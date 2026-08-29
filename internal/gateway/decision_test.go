package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/mediainfo"
	"github.com/InfinityPacer/plex-gateway/internal/metrics"
	"github.com/InfinityPacer/plex-gateway/internal/partcache"
	"github.com/InfinityPacer/plex-gateway/internal/pathmap"
	"github.com/InfinityPacer/plex-gateway/internal/playback"
	"github.com/InfinityPacer/plex-gateway/internal/plexmeta"
	"github.com/InfinityPacer/plex-gateway/internal/resolver"
)

type decisionMediaInfoStub struct {
	record        mediainfo.Record
	memoryHit     bool
	waitForCancel bool
	ensureCalls   int
	memoryCalls   int
	request       mediainfo.Request
}

func (stub *decisionMediaInfoStub) GetMemory(mediainfo.Key) (mediainfo.Record, bool) {
	stub.memoryCalls++
	return stub.record, stub.memoryHit
}

func (stub *decisionMediaInfoStub) Ensure(ctx context.Context, request mediainfo.Request) (mediainfo.Record, error) {
	stub.ensureCalls++
	stub.request = request
	if stub.waitForCancel {
		<-ctx.Done()
		return mediainfo.Record{}, ctx.Err()
	}
	return stub.record, nil
}

func TestCloudDecisionProjectsMediaInfoWithoutClientSpecificPolicy(t *testing.T) {
	clients := []struct {
		name      string
		product   string
		platform  string
		userAgent string
	}{
		{name: "Infuse", product: "Infuse-Direct", platform: "iOS", userAgent: "Infuse-Direct/8.5.3"},
		{name: "Plex iOS", product: "Plex for iOS", platform: "iOS", userAgent: "PlexMobile/8.0"},
		{name: "Plex Apple TV", product: "Plex for Apple TV", platform: "tvOS", userAgent: "PlexTV/8.0"},
	}
	var projectedBody string
	for _, client := range clients {
		t.Run(client.name, func(t *testing.T) {
			media := completeProjectionMedia()
			media.Size = 987654321
			mediaInfo := &decisionMediaInfoStub{record: mediainfo.Record{Media: media}}
			handler := newDecisionProjectionHandler(t, mediaInfo, 100*time.Millisecond, nil)
			request := decisionProjectionRequest()
			request.Header.Set("User-Agent", client.userAgent)
			request.Header.Set("X-Plex-Product", client.product)
			request.Header.Set("X-Plex-Platform", client.platform)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `decision="directplay"`) ||
				!strings.Contains(response.Body.String(), `size="987654321"`) ||
				!strings.Contains(response.Body.String(), `container="mkv"`) ||
				!strings.Contains(response.Body.String(), `<Stream`) {
				t.Fatalf("status=%d headers=%#v body=%s", response.Code, response.Header(), response.Body.String())
			}
			if projectedBody == "" {
				projectedBody = response.Body.String()
			} else if response.Body.String() != projectedBody {
				t.Fatalf("client-specific decision projection:\nwant %s\n got %s", projectedBody, response.Body.String())
			}
			if mediaInfo.ensureCalls != 1 || mediaInfo.request.Key.PartID != "9" || mediaInfo.request.RatingKey != "42" || mediaInfo.request.ClientUserAgent != client.userAgent {
				t.Fatalf("MediaInfo request = %#v calls=%d", mediaInfo.request, mediaInfo.ensureCalls)
			}
			if response.Header().Get("ETag") != "" || response.Header().Get("Content-Length") == "" {
				t.Fatalf("enriched decision headers = %#v", response.Header())
			}
		})
	}
}

func TestCloudDecisionMediaInfoTimeoutFailsOpen(t *testing.T) {
	mediaInfo := &decisionMediaInfoStub{waitForCancel: true}
	handler := newDecisionProjectionHandler(t, mediaInfo, 10*time.Millisecond, nil)
	response := httptest.NewRecorder()
	started := time.Now()

	handler.ServeHTTP(response, decisionProjectionRequest())

	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("decision fail-open took %s", elapsed)
	}
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `size="301"`) || strings.Contains(response.Body.String(), `container="mkv"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if mediaInfo.ensureCalls != 1 {
		t.Fatalf("Ensure calls = %d", mediaInfo.ensureCalls)
	}
}

func TestWriteIncompatibleDecisionClearsRepresentationHeadersAndHonorsHEAD(t *testing.T) {
	for _, test := range []struct {
		name   string
		method string
		accept string
	}{
		{name: "XML GET", method: http.MethodGet, accept: "application/xml"},
		{name: "JSON GET", method: http.MethodGet, accept: "application/json"},
		{name: "XML HEAD", method: http.MethodHead, accept: "application/xml"},
		{name: "JSON HEAD", method: http.MethodHead, accept: "application/json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			for _, name := range []string{
				"Accept-Ranges", "Content-Encoding", "Content-MD5", "Content-Range", "Digest",
				"ETag", "Last-Modified", "Trailer", "Transfer-Encoding", "Vary",
			} {
				response.Header().Set(name, "stale")
			}
			request := httptest.NewRequest(test.method, "/video/:/transcode/universal/decision", nil)
			request.Header.Set("Accept", test.accept)

			writeIncompatibleDecision(response, request)

			contentType, body := plexmeta.IncompatibleDecision(test.accept)
			if response.Code != http.StatusOK || response.Header().Get("Content-Type") != contentType ||
				response.Header().Get("Content-Length") != strconv.Itoa(len(body)) || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d headers=%#v", response.Code, response.Header())
			}
			wantBody := string(body)
			if test.method == http.MethodHead {
				wantBody = ""
			}
			if response.Body.String() != wantBody {
				t.Fatalf("body=%q want=%q", response.Body.String(), wantBody)
			}
			for _, name := range []string{
				"Accept-Ranges", "Content-Encoding", "Content-MD5", "Content-Range", "Digest",
				"ETag", "Last-Modified", "Trailer", "Transfer-Encoding", "Vary",
			} {
				if got := response.Header().Get(name); got != "" {
					t.Errorf("%s = %q, want empty", name, got)
				}
			}
		})
	}
}

func TestCloudDecisionJSONProjectionClearsRepresentationHeaders(t *testing.T) {
	media := completeProjectionMedia()
	mediaInfo := &decisionMediaInfoStub{record: freshDecisionRecord(media), memoryHit: true}
	handler := newDecisionProjectionHandler(t, mediaInfo, 100*time.Millisecond, nil).(*decisionHandler)
	handler.plex = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for _, name := range []string{
			"Accept-Ranges", "Content-MD5", "Content-Range", "Digest", "ETag", "Last-Modified",
			"Trailer", "Transfer-Encoding", "Vary",
		} {
			w.Header().Set(name, "stale")
		}
		w.Header().Set("Content-Length", "1")
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"Media":[{"decision":"directplay","Part":[{"id":9,"key":"/library/parts/9/1/file","size":301,"decision":"directplay"}]}]}]}}`))
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, decisionProjectionRequest())

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"container":"mkv"`) {
		t.Fatalf("status=%d headers=%#v body=%s", response.Code, response.Header(), response.Body.String())
	}
	if response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("body headers=%#v body_len=%d", response.Header(), response.Body.Len())
	}
	for _, name := range []string{
		"Accept-Ranges", "Content-Encoding", "Content-MD5", "Content-Range", "Digest", "ETag",
		"Last-Modified", "Trailer", "Transfer-Encoding", "Vary",
	} {
		if got := response.Header().Get(name); got != "" {
			t.Errorf("%s = %q, want empty", name, got)
		}
	}
}

func TestCloudDecisionVetoReplacesGzipResponseWithIdentityBody(t *testing.T) {
	media := completeProjectionMedia()
	media.Streams[0].HDRFormat = "dolby_vision"
	media.Streams[0].DolbyVision = &mediainfo.DolbyVision{Profile: 5, BLCompatID: 0}
	mediaInfo := &decisionMediaInfoStub{record: freshDecisionRecord(media), memoryHit: true}
	handler := newDecisionProjectionHandler(t, mediaInfo, 100*time.Millisecond, newPlaybackVeto(true)).(*decisionHandler)
	handler.plex = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("Content-Encoding", "gzip")
		for _, name := range []string{
			"Accept-Ranges", "Content-MD5", "Content-Range", "Digest", "ETag", "Last-Modified",
			"Trailer", "Transfer-Encoding", "Vary",
		} {
			w.Header().Set(name, "stale")
		}
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		_, _ = writer.Write([]byte(`<MediaContainer><Video><Media decision="directplay"><Part id="9" key="/library/parts/9/1/file" size="301" decision="directplay"/></Media></Video></MediaContainer>`))
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Length", strconv.Itoa(compressed.Len()))
		_, _ = w.Write(compressed.Bytes())
	})
	request := decisionProjectionRequest()
	request.Header.Set("Accept", "application/xml")
	request.Header.Set("X-Plex-Product", "Plex for Apple TV")
	request.Header.Set("X-Plex-Platform", "tvOS")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `transcodeDecisionCode="4005"`) || response.Header().Get("Content-Encoding") != "" {
		t.Fatalf("status=%d headers=%#v body=%s", response.Code, response.Header(), response.Body.String())
	}
	if response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
		t.Fatalf("Content-Length=%q body_len=%d", response.Header().Get("Content-Length"), response.Body.Len())
	}
	for _, name := range []string{
		"Accept-Ranges", "Content-MD5", "Content-Range", "Digest", "ETag", "Last-Modified",
		"Trailer", "Transfer-Encoding", "Vary",
	} {
		if got := response.Header().Get(name); got != "" {
			t.Errorf("%s = %q, want empty", name, got)
		}
	}
}

func TestCloudDecisionOptionalVetoUsesExistingFreshMediaInfo(t *testing.T) {
	media := completeProjectionMedia()
	media.Streams[0].HDRFormat = "dolby_vision"
	media.Streams[0].DolbyVision = &mediainfo.DolbyVision{Profile: 5, BLCompatID: 0}
	mediaInfo := &decisionMediaInfoStub{record: freshDecisionRecord(media), memoryHit: true}
	handler := newDecisionProjectionHandler(t, mediaInfo, 100*time.Millisecond, newPlaybackVeto(true))
	request := decisionProjectionRequest()
	request.Header.Set("X-Plex-Product", "Plex for Apple TV")
	request.Header.Set("X-Plex-Platform", "tvOS")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `transcodeDecisionCode="4005"`) || strings.Contains(response.Body.String(), `decision="directplay"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if mediaInfo.memoryCalls != 1 || mediaInfo.ensureCalls != 0 {
		t.Fatalf("GetMemory calls=%d Ensure calls=%d", mediaInfo.memoryCalls, mediaInfo.ensureCalls)
	}
}

func TestCloudDecisionDisabledVetoPreservesDV5WithSameMediaInfoIO(t *testing.T) {
	media := completeProjectionMedia()
	media.Streams[0].HDRFormat = "dolby_vision"
	media.Streams[0].DolbyVision = &mediainfo.DolbyVision{Profile: 5, BLCompatID: 0}
	mediaInfo := &decisionMediaInfoStub{record: freshDecisionRecord(media), memoryHit: true}
	handler := newDecisionProjectionHandler(t, mediaInfo, 100*time.Millisecond, nil)
	request := decisionProjectionRequest()
	request.Header.Set("X-Plex-Product", "Plex for Apple TV")
	request.Header.Set("X-Plex-Platform", "tvOS")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `decision="directplay"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if mediaInfo.memoryCalls != 1 || mediaInfo.ensureCalls != 0 {
		t.Fatalf("GetMemory calls=%d Ensure calls=%d", mediaInfo.memoryCalls, mediaInfo.ensureCalls)
	}
}

func TestCloudDecisionOptionalVetoAbstainsOnStaleMediaInfo(t *testing.T) {
	media := completeProjectionMedia()
	media.Streams[0].HDRFormat = "dolby_vision"
	media.Streams[0].DolbyVision = &mediainfo.DolbyVision{Profile: 5, BLCompatID: 0}
	now := time.Now().UTC()
	record := freshDecisionRecord(media)
	record.ProbedAt = now.Add(-2 * time.Hour)
	record.ExpiresAt = now.Add(-time.Hour)
	mediaInfo := &decisionMediaInfoStub{record: record, memoryHit: true}
	handler := newDecisionProjectionHandler(t, mediaInfo, 100*time.Millisecond, newPlaybackVeto(true))
	request := decisionProjectionRequest()
	request.Header.Set("X-Plex-Product", "Plex for Apple TV")
	request.Header.Set("X-Plex-Platform", "tvOS")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `decision="directplay"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestCloudDecisionVetoDoesNotOverrideCompletePlexDecision(t *testing.T) {
	media := completeProjectionMedia()
	media.Streams[0].HDRFormat = "dolby_vision"
	media.Streams[0].DolbyVision = &mediainfo.DolbyVision{Profile: 5, BLCompatID: 0}
	mediaInfo := &decisionMediaInfoStub{record: freshDecisionRecord(media), memoryHit: true}
	handler := newDecisionProjectionHandler(t, mediaInfo, 100*time.Millisecond, newPlaybackVeto(true)).(*decisionHandler)
	base := []byte(`<MediaContainer><Video><Media id="11" decision="directplay"><Part id="9" key="/library/parts/9/1/file" size="301" decision="directplay"/></Media></Video></MediaContainer>`)
	complete, changed, err := plexmeta.EnrichDecision(base, "application/xml", plexmeta.Part{ID: "9", Key: "/library/parts/9/1/file"}, media)
	if err != nil || !changed {
		t.Fatalf("prepare complete decision changed=%v err=%v", changed, err)
	}
	handler.plex = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write(complete)
	})
	request := decisionProjectionRequest()
	request.Header.Set("X-Plex-Product", "Plex for Apple TV")
	request.Header.Set("X-Plex-Platform", "tvOS")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `decision="directplay"`) || strings.Contains(response.Body.String(), `transcodeDecisionCode="4005"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func freshDecisionRecord(media mediainfo.Media) mediainfo.Record {
	now := time.Now().UTC()
	return mediainfo.Record{
		Key:      mediainfo.Key{PlexServerID: "plex", PartID: "9", STRMFingerprint: strings.Repeat("a", 64)},
		Provider: mediainfo.ProviderMediaVaultFFProbe, ProviderRevision: mediainfo.ProviderRevisionFFProbeJSONV3,
		SchemaVersion: mediainfo.SchemaVersion, Media: media,
		ProbedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
		LastAccessedAt: now, RetainUntil: now.Add(24 * time.Hour),
	}
}

func newDecisionProjectionHandler(
	t *testing.T,
	mediaInfo decisionMediaInfoService,
	coldWait time.Duration,
	veto playbackVeto,
) http.Handler {
	t.Helper()
	localRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(localRoot, "episode.strm"), []byte("http://mediavault.invalid/redirect/pick/episode.mkv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mapper, err := pathmap.New([]pathmap.Mapping{{PlexPrefix: "/media/cloud", LocalPrefix: localRoot}})
	if err != nil {
		t.Fatal(err)
	}
	controlResolver, err := resolver.NewMediaVaultSTRMResolver("http://mediavault.invalid", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	metadataServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/library/metadata/42" {
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":9,"key":"/library/parts/9/1/file","file":"/media/cloud/episode.strm"}]}]}]}}`))
	}))
	t.Cleanup(metadataServer.Close)
	upstream, err := url.Parse(metadataServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	cloudPlayback := playback.New(playback.Options{
		Cache: partcache.New(time.Hour), Mapper: mapper, Resolver: controlResolver,
		AuthorizePart:   func(*http.Request, string, string) (bool, error) { return true, nil },
		CloudExtensions: []string{".strm"},
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &decisionHandler{
		plex: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/video/:/transcode/universal/decision" || request.URL.Query().Get("directPlay") != "1" {
				http.NotFound(w, request)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			w.Header().Set("Content-Length", "1")
			w.Header().Set("ETag", `"stale"`)
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media id="11" decision="directplay"><Part id="9" key="/library/parts/9/1/file" size="301" decision="directplay"/></Media></Video></MediaContainer>`))
		}),
		probe: &decisionMetadataProbe{
			upstream: upstream,
			client:   &http.Client{Timeout: time.Second},
			maxBytes: 1 << 20,
		},
		service: cloudPlayback, mediaInfo: mediaInfo, coldWait: coldWait, veto: veto,
		grants: playback.NewGrantStore(time.Minute, 16), logger: logger, metrics: metrics.New(),
	}
}

func decisionProjectionRequest() *http.Request {
	query := url.Values{
		"path":                       {"/library/metadata/42"},
		"mediaIndex":                 {"0"},
		"partIndex":                  {"0"},
		"directPlay":                 {"0"},
		"directStream":               {"0"},
		"X-Plex-Playback-Session-Id": {"decision-projection-session"},
	}
	request := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?"+query.Encode(), nil)
	request.Header.Set("X-Plex-Token", "client-token")
	return request
}

func TestCloudDecisionForcesDirectPlayAndContinuesThroughPartRedirect(t *testing.T) {
	localRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(localRoot, "D.strm"), []byte("http://public.invalid/redirect/pickcode/D.mkv\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var mediaVaultRequests int
	mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaVaultRequests++
		if r.URL.Path != "/redirect/pickcode/D.mkv" {
			t.Fatalf("MediaVault path = %q", r.URL.Path)
		}
		w.Header().Set("Location", "https://cdn.invalid/D.mkv?signature=private")
		w.WriteHeader(http.StatusFound)
	}))
	defer mediaVault.Close()

	var metadataRequests, decisionRequests, partRequests int
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/42":
			metadataRequests++
			if r.Method != http.MethodGet || r.URL.Query().Get("X-Plex-Token") != "query-token" {
				t.Fatalf("metadata request = %s %s", r.Method, r.URL.RequestURI())
			}
			if r.Header.Get("X-Plex-Token") != "header-token" || r.Header.Get("Cookie") != "plex=session" {
				t.Fatalf("metadata credentials were not preserved")
			}
			if r.Header.Get("X-Plex-Client-Identifier") != "client-id" {
				t.Fatalf("metadata client identifier = %q", r.Header.Get("X-Plex-Client-Identifier"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":10,"key":"/library/parts/10/1/file","file":"/media/local/A.mkv"}]},{"Part":[{"id":20,"key":"/library/parts/20/1/file","file":"/media/cloud/C.strm"},{"id":21,"key":"/library/parts/21/1/file","file":"/media/cloud/D.strm"}]}]}]}}`))
		case "/video/:/transcode/universal/decision":
			decisionRequests++
			query := r.URL.Query()
			if got := query["directPlay"]; len(got) != 1 || got[0] != "1" {
				t.Fatalf("directPlay = %#v", got)
			}
			if got := query["directStream"]; len(got) != 1 || got[0] != "1" {
				t.Fatalf("directStream = %#v", got)
			}
			if got := query["hasMDE"]; len(got) != 1 || got[0] != "1" {
				t.Fatalf("hasMDE = %#v", got)
			}
			if got := query["profileExtra"]; len(got) != 2 || got[0] != "first" || got[1] != "second" {
				t.Fatalf("profileExtra = %#v", got)
			}
			if r.Header.Get("X-Plex-Client-Identifier") != "client-id" || r.Header.Get("Cookie") != "plex=session" {
				t.Fatal("decision headers were not preserved")
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media decision="directplay"><Part decision="directplay" key="/library/parts/21/1/file"/></Media></Video></MediaContainer>`))
		case "/library/parts/21/1/file":
			partRequests++
			if r.Header.Get("X-Plex-Token") != "header-token" {
				http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
				return
			}
			w.Header().Set("Location", "http://public.invalid/redirect/pickcode/D.mkv")
			w.WriteHeader(http.StatusMovedPermanently)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, _, cache := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: localRoot,
	}})
	query := url.Values{}
	query.Set("path", "/library/metadata/42")
	query.Set("mediaIndex", "1")
	query.Set("partIndex", "1")
	query.Add("directPlay", "0")
	query.Add("directPlay", "0")
	query.Set("directStream", "0")
	query.Set("hasMDE", "0")
	query.Add("hasMDE", "1")
	query.Add("profileExtra", "first")
	query.Add("profileExtra", "second")
	query.Set("X-Plex-Token", "query-token")
	decisionRequest := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?"+query.Encode(), nil)
	decisionRequest.Header.Set("X-Plex-Token", "header-token")
	decisionRequest.Header.Set("X-Plex-Client-Identifier", "client-id")
	decisionRequest.Header.Set("Cookie", "plex=session")
	decisionResponse := httptest.NewRecorder()
	handler.ServeHTTP(decisionResponse, decisionRequest)

	if decisionResponse.Code != http.StatusOK || !strings.Contains(decisionResponse.Body.String(), `decision="directplay"`) {
		t.Fatalf("decision status = %d body = %q", decisionResponse.Code, decisionResponse.Body.String())
	}
	if part, ok := cache.Get("21"); !ok || part.PlexFilePath != "/media/cloud/D.strm" {
		t.Fatalf("selected Part was not cached: %#v, %v", part, ok)
	}

	partRequest := httptest.NewRequest(http.MethodGet, "/library/parts/21/1/file", nil)
	partRequest.Header.Set("X-Plex-Token", "header-token")
	partResponse := httptest.NewRecorder()
	handler.ServeHTTP(partResponse, partRequest)
	if partResponse.Code != http.StatusFound || partResponse.Header().Get("Location") != "https://cdn.invalid/D.mkv?signature=private" {
		t.Fatalf("Part status = %d Location = %q", partResponse.Code, partResponse.Header().Get("Location"))
	}
	if metadataRequests != 1 || decisionRequests != 1 || partRequests != 1 || mediaVaultRequests != 1 {
		t.Fatalf("requests metadata=%d decision=%d part=%d MediaVault=%d", metadataRequests, decisionRequests, partRequests, mediaVaultRequests)
	}
}

func TestCloudDecisionInfersOmittedMediaIndexFromUniqueMetadata(t *testing.T) {
	localRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(localRoot, "Unique.strm"), []byte("http://public.invalid/redirect/pickcode/Unique.mkv\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://cdn.invalid/Unique.mkv?signature=private")
		w.WriteHeader(http.StatusFound)
	}))
	defer mediaVault.Close()

	var decisionRequests, partRequests int
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/42":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":20,"key":"/library/parts/20/1/file","file":"/media/cloud/Unique.strm"}]}]}]}}`))
		case "/video/:/transcode/universal/decision":
			decisionRequests++
			query := r.URL.Query()
			if query.Get("mediaIndex") != "0" || query.Get("partIndex") != "0" || query.Get("directPlay") != "1" || query.Get("hasMDE") != "1" {
				t.Fatalf("adapted decision query = %q", r.URL.RawQuery)
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media decision="directplay"><Part decision="directplay" key="/library/parts/20/1/file"/></Media></Video></MediaContainer>`))
		case "/library/parts/20/1/file":
			partRequests++
			w.Header().Set("Location", "http://public.invalid/redirect/pickcode/Unique.mkv")
			w.WriteHeader(http.StatusMovedPermanently)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, _, _ := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: localRoot,
	}})
	query := url.Values{
		"path":                       {"/library/metadata/42"},
		"partIndex":                  {"0"},
		"directPlay":                 {"0"},
		"directStream":               {"0"},
		"X-Plex-Playback-Session-Id": {"apple-tv-session"},
	}
	decisionRequest := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?"+query.Encode(), nil)
	decisionRequest.Header.Set("X-Plex-Token", "client-token")
	decisionResponse := httptest.NewRecorder()
	handler.ServeHTTP(decisionResponse, decisionRequest)
	if decisionResponse.Code != http.StatusOK {
		t.Fatalf("decision status = %d body = %q", decisionResponse.Code, decisionResponse.Body.String())
	}

	startRequest := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/start.m3u8?"+query.Encode(), nil)
	startRequest.Header.Set("X-Plex-Token", "client-token")
	startResponse := httptest.NewRecorder()
	handler.ServeHTTP(startResponse, startRequest)
	if startResponse.Code != http.StatusFound || startResponse.Header().Get("Location") != "https://cdn.invalid/Unique.mkv?signature=private" {
		t.Fatalf("start status = %d Location = %q", startResponse.Code, startResponse.Header().Get("Location"))
	}
	if decisionRequests != 1 || partRequests != 1 {
		t.Fatalf("decision requests = %d, part requests = %d", decisionRequests, partRequests)
	}
}

func TestOmittedMediaIndexWithAmbiguousMetadataRemainsUnchanged(t *testing.T) {
	originalQuery := "path=%2Flibrary%2Fmetadata%2F42&partIndex=0&directPlay=0&directStream=0"
	var metadataRequests int
	var decisionQuery string
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/42":
			metadataRequests++
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="10" file="/media/cloud/A.strm"/></Media><Media><Part id="20" file="/media/cloud/B.strm"/></Media></Video></MediaContainer>`))
		case "/video/:/transcode/universal/decision":
			decisionQuery = r.URL.RawQuery
			http.Error(w, "original decision", http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, _, _ := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: t.TempDir(),
	}})
	request := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?"+originalQuery, nil)
	request.Header.Set("X-Plex-Token", "client-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || metadataRequests != 1 || decisionQuery != originalQuery {
		t.Fatalf("status=%d metadata=%d query=%q", response.Code, metadataRequests, decisionQuery)
	}
}

func TestLocalDecisionRemainsUnchanged(t *testing.T) {
	originalQuery := "path=%2Flibrary%2Fmetadata%2F42&partIndex=0&directPlay=0&directStream=0"
	var metadataRequests int
	var decisionQuery string
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/42":
			metadataRequests++
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="10" key="/library/parts/10/1/file" file="/media/local/A.mkv"/></Media></Video></MediaContainer>`))
		case "/video/:/transcode/universal/decision":
			decisionQuery = r.URL.RawQuery
			http.Error(w, "original decision", http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, _, _ := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: t.TempDir(),
	}})
	request := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?"+originalQuery, nil)
	request.Header.Set("X-Plex-Token", "client-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || metadataRequests != 1 || decisionQuery != originalQuery {
		t.Fatalf("status=%d metadata=%d decision_query=%q", response.Code, metadataRequests, decisionQuery)
	}
}

func TestAmbiguousDecisionIndicesFailOpenWithoutMetadataProbe(t *testing.T) {
	for _, test := range []struct {
		name  string
		query string
	}{
		{name: "automatic media", query: "path=%2Flibrary%2Fmetadata%2F42&mediaIndex=-1&partIndex=0&directPlay=0&directStream=0"},
		{name: "automatic part", query: "path=%2Flibrary%2Fmetadata%2F42&mediaIndex=0&partIndex=-1&directPlay=0&directStream=0"},
		{name: "missing part", query: "path=%2Flibrary%2Fmetadata%2F42&mediaIndex=0&directPlay=0&directStream=0"},
		{name: "duplicate media", query: "path=%2Flibrary%2Fmetadata%2F42&mediaIndex=0&mediaIndex=1&partIndex=0&directPlay=0&directStream=0"},
		{name: "empty media", query: "path=%2Flibrary%2Fmetadata%2F42&mediaIndex=&partIndex=0&directPlay=0&directStream=0"},
		{name: "empty part", query: "path=%2Flibrary%2Fmetadata%2F42&partIndex=&directPlay=0&directStream=0"},
		{name: "duplicate part", query: "path=%2Flibrary%2Fmetadata%2F42&partIndex=0&partIndex=1&directPlay=0&directStream=0"},
		{name: "omitted media with nonzero part", query: "path=%2Flibrary%2Fmetadata%2F42&partIndex=1&directPlay=0&directStream=0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var metadataRequests int
			var decisionQuery string
			plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/library/metadata/42" {
					metadataRequests++
				}
				if r.URL.Path == "/video/:/transcode/universal/decision" {
					decisionQuery = r.URL.RawQuery
					w.WriteHeader(http.StatusNoContent)
					return
				}
				http.NotFound(w, r)
			}))
			defer plex.Close()

			handler, _, _ := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
				PlexPrefix:  "/media/cloud",
				LocalPrefix: t.TempDir(),
			}})
			request := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?"+test.query, nil)
			request.Header.Set("X-Plex-Token", "client-token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusNoContent || metadataRequests != 0 || decisionQuery != test.query {
				t.Fatalf("status=%d metadata=%d query=%q", response.Code, metadataRequests, decisionQuery)
			}
		})
	}
}

func TestDecisionMetadataFailureFallsBackToOriginalRequest(t *testing.T) {
	originalQuery := "path=%2Flibrary%2Fmetadata%2F42&mediaIndex=0&partIndex=0&directPlay=0&directStream=0"
	var decisionQuery string
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/42":
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		case "/video/:/transcode/universal/decision":
			decisionQuery = r.URL.RawQuery
			http.Error(w, "original decision", http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, _, _ := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: t.TempDir(),
	}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?"+originalQuery, nil))
	if response.Code != http.StatusBadRequest || decisionQuery != originalQuery {
		t.Fatalf("status = %d decision query = %q", response.Code, decisionQuery)
	}
}

func TestCloudDecisionWithoutPlexTokenDoesNotProbeMetadata(t *testing.T) {
	var metadataRequests, decisionRequests int
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/42":
			metadataRequests++
			w.WriteHeader(http.StatusOK)
		case "/video/:/transcode/universal/decision":
			decisionRequests++
			w.WriteHeader(http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, _, _ := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
		PlexPrefix:  "/media/cloud",
		LocalPrefix: t.TempDir(),
	}})
	query := url.Values{
		"path":       {"/library/metadata/42"},
		"mediaIndex": {"0"},
		"partIndex":  {"0"},
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?"+query.Encode(), nil))

	if recorder.Code != http.StatusTeapot || metadataRequests != 0 || decisionRequests != 1 {
		t.Fatalf("status=%d metadata=%d decision=%d", recorder.Code, metadataRequests, decisionRequests)
	}
}

func TestDecisionRequiresReadableValidSTRMTarget(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
	}{
		{name: "missing file"},
		{name: "invalid target", content: "ftp://media.invalid/Movie.mkv\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			localRoot := t.TempDir()
			if test.content != "" {
				if err := os.WriteFile(filepath.Join(localRoot, "Movie.strm"), []byte(test.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var decisionQuery string
			plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/library/metadata/42":
					w.Header().Set("Content-Type", "application/xml")
					_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="10" key="/library/parts/10/1/file" file="/media/cloud/Movie.strm"/></Media></Video></MediaContainer>`))
				case "/video/:/transcode/universal/decision":
					decisionQuery = r.URL.RawQuery
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			}))
			defer plex.Close()

			handler, _, _ := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", []pathmap.Mapping{{
				PlexPrefix:  "/media/cloud",
				LocalPrefix: localRoot,
			}})
			originalQuery := "path=%2Flibrary%2Fmetadata%2F42&mediaIndex=0&partIndex=0&directPlay=0&directStream=0"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?"+originalQuery, nil))

			if response.Code != http.StatusNoContent || decisionQuery != originalQuery {
				t.Fatalf("status=%d query=%q", response.Code, decisionQuery)
			}
		})
	}
}
