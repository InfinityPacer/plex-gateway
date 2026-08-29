package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/mediainfo"
	"github.com/InfinityPacer/plex-gateway/internal/metrics"
	"github.com/InfinityPacer/plex-gateway/internal/pathmap"
	"github.com/InfinityPacer/plex-gateway/internal/resolver"
)

type fakeMediaInfoEnsurer struct {
	gets         atomic.Int64
	memoryGets   atomic.Int64
	mu           sync.Mutex
	offered      []mediainfo.Request
	memoryFn     func(mediainfo.Key) (mediainfo.Record, bool)
	getFn        func(mediainfo.Key) (mediainfo.Record, bool)
	getContextFn func(context.Context, mediainfo.Key) (mediainfo.Record, bool)
}

func (ensurer *fakeMediaInfoEnsurer) GetMemory(key mediainfo.Key) (mediainfo.Record, bool) {
	ensurer.memoryGets.Add(1)
	if ensurer.memoryFn == nil {
		return mediainfo.Record{}, false
	}
	return ensurer.memoryFn(key)
}

func (ensurer *fakeMediaInfoEnsurer) GetContext(ctx context.Context, key mediainfo.Key) (mediainfo.Record, bool) {
	ensurer.gets.Add(1)
	if ensurer.getContextFn != nil {
		return ensurer.getContextFn(ctx, key)
	}
	if ensurer.getFn == nil {
		return mediainfo.Record{}, false
	}
	return ensurer.getFn(key)
}

func TestMetadataEnrichmentCollectionFailsOpenWhenCacheStoreStalls(t *testing.T) {
	fixture := newMetadataEnrichmentFixture(t)
	raw := []byte(`<MediaContainer size="1"><Video ratingKey="42"><Media><Part id="9" file="/media/cloud/episode.strm"/></Media></Video></MediaContainer>`)
	ensurer := &fakeMediaInfoEnsurer{
		getContextFn: func(ctx context.Context, _ mediainfo.Key) (mediainfo.Record, bool) {
			<-ctx.Done()
			return mediainfo.Record{}, false
		},
	}
	handler := fixture.handler(ensurer, metrics.New(), metadataBodyHandler(raw, "application/xml"))
	request := authenticatedRequest(http.MethodGet, "/library/metadata/41/children")
	started := time.Now()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cache-only collection blocked for %s", elapsed)
	}
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), raw) {
		t.Fatalf("response = %d %q", response.Code, response.Body.Bytes())
	}
}

func (ensurer *fakeMediaInfoEnsurer) Offer(request mediainfo.Request) mediainfo.SubmitResult {
	ensurer.mu.Lock()
	ensurer.offered = append(ensurer.offered, request)
	ensurer.mu.Unlock()
	return mediainfo.SubmitResult{Disposition: mediainfo.SubmitNewlyQueued}
}

func (ensurer *fakeMediaInfoEnsurer) offers() []mediainfo.Request {
	ensurer.mu.Lock()
	defer ensurer.mu.Unlock()
	return append([]mediainfo.Request(nil), ensurer.offered...)
}

func TestMetadataEnrichmentAddsMissingXMLFields(t *testing.T) {
	fixture := newMetadataEnrichmentFixture(t)
	raw := []byte(`<MediaContainer size="1"><Video ratingKey="42" type="episode"><Media><Part id="9" key="/library/parts/9/1/file.strm" file="/media/cloud/episode.strm"><Stream id="101" streamType="1" index="0"/></Part></Media></Video></MediaContainer>`)
	registry := metrics.New()
	ensurer := &fakeMediaInfoEnsurer{memoryFn: func(key mediainfo.Key) (mediainfo.Record, bool) {
		return mediainfo.Record{Key: key, Media: completeProjectionMedia()}, true
	}}
	handler := fixture.handler(ensurer, registry, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		w.Header().Set("ETag", `"plex-etag"`)
		w.Header().Set("Last-Modified", "Wed, 01 Jan 2025 00:00:00 GMT")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	}))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedMetadataRequest(http.MethodGet))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if bytes.Equal(response.Body.Bytes(), raw) {
		t.Fatal("metadata body was not enriched")
	}
	if !strings.Contains(response.Body.String(), `container="mkv"`) || !strings.Contains(response.Body.String(), `codec="hevc"`) {
		t.Fatalf("enriched XML missing expected fields: %s", response.Body.String())
	}
	if response.Header().Get("ETag") != "" || response.Header().Get("Last-Modified") != "" {
		t.Fatalf("stale validators retained: %#v", response.Header())
	}
	if response.Header().Get("Cache-Control") != "private, no-cache" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
		t.Fatalf("mutated headers = %#v", response.Header())
	}
	if ensurer.memoryGets.Load() != 1 || len(ensurer.offers()) != 0 {
		t.Fatalf("memory gets=%d offers=%d", ensurer.memoryGets.Load(), len(ensurer.offers()))
	}
	snapshot := registry.Snapshot()
	if snapshot.MediaInfoEnrichedTotal != 1 || snapshot.MediaInfoWaitActive != 0 || snapshot.MediaInfoFailOpenTotal != 0 {
		t.Fatalf("metrics = %#v", snapshot)
	}
}

func TestMetadataEnrichmentPreservesGzipJSON(t *testing.T) {
	fixture := newMetadataEnrichmentFixture(t)
	rawJSON := []byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"42","type":"episode","Media":[{"Part":[{"id":"9","key":"/library/parts/9/1/file.strm","file":"/media/cloud/episode.strm","Stream":[{"id":"101","streamType":1,"index":0}]}]}]}]}}`)
	wire := gzipBytes(t, rawJSON)
	ensurer := &fakeMediaInfoEnsurer{memoryFn: func(key mediainfo.Key) (mediainfo.Record, bool) {
		return mediainfo.Record{Key: key, Media: completeProjectionMedia()}, true
	}}
	handler := fixture.handler(ensurer, metrics.New(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(wire)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(wire)
	}))

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedMetadataRequest(http.MethodGet))
	if response.Header().Get("Content-Encoding") != "gzip" || response.Header().Get("Content-Length") != strconv.Itoa(response.Body.Len()) {
		t.Fatalf("gzip headers = %#v", response.Header())
	}
	decoded := gunzipBytes(t, response.Body.Bytes())
	var value map[string]any
	if err := json.Unmarshal(decoded, &value); err != nil {
		t.Fatalf("decode enriched JSON: %v", err)
	}
	if !bytes.Contains(decoded, []byte(`"container":"mkv"`)) || !bytes.Contains(decoded, []byte(`"codec":"hevc"`)) {
		t.Fatalf("enriched JSON missing expected fields: %s", decoded)
	}
}

func TestMetadataEnrichmentKeepsBackgroundLibrarySyncCacheOnly(t *testing.T) {
	fixture := newMetadataEnrichmentFixture(t)
	raw := fixture.xmlBody()
	ensurer := &fakeMediaInfoEnsurer{}
	handler := fixture.handler(ensurer, metrics.New(), metadataBodyHandler(raw, "application/xml"))
	request := authenticatedRequest(http.MethodGet, "/library/metadata/42?skipRefresh=1")
	request.Header.Set("X-Plex-Product", "Infuse-Library")

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), raw) {
		t.Fatalf("response = %d %q", response.Code, response.Body.Bytes())
	}
	if ensurer.gets.Load() != 1 || len(ensurer.offers()) != 0 {
		t.Fatalf("Get calls = %d", ensurer.gets.Load())
	}
}

func TestMetadataEnrichmentUsesCachedRecordForBackgroundLibrarySync(t *testing.T) {
	fixture := newMetadataEnrichmentFixture(t)
	ensurer := &fakeMediaInfoEnsurer{getFn: func(key mediainfo.Key) (mediainfo.Record, bool) {
		return mediainfo.Record{Key: key, Media: completeProjectionMedia()}, true
	}}
	handler := fixture.handler(ensurer, metrics.New(), metadataBodyHandler(fixture.xmlBody(), "application/xml"))
	request := authenticatedRequest(http.MethodGet, "/library/metadata/42?skipRefresh=1")
	request.Header.Set("X-Plex-Product", "Infuse-Library")

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `container="mkv"`) {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if ensurer.gets.Load() != 1 {
		t.Fatalf("Get calls = %d", ensurer.gets.Load())
	}
}

func TestMetadataEnrichmentUsesOnlyCachedRecordsForCollection(t *testing.T) {
	fixture := newMetadataEnrichmentFixture(t)
	raw := []byte(`<MediaContainer size="2"><Video ratingKey="42"><Media><Part id="9" file="/media/cloud/episode.strm"/></Media></Video><Video ratingKey="43"><Media container="mp4"><Part id="10" file="/media/local/movie.mp4"/></Media></Video></MediaContainer>`)
	ensurer := &fakeMediaInfoEnsurer{
		getFn: func(key mediainfo.Key) (mediainfo.Record, bool) {
			if key.PartID != "9" {
				t.Fatalf("unexpected cached Part = %#v", key)
			}
			return mediainfo.Record{Key: key, Media: completeProjectionMedia()}, true
		},
	}
	handler := fixture.handler(ensurer, metrics.New(), metadataBodyHandler(raw, "application/xml"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(http.MethodGet, "/library/metadata/7/children"))

	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`ratingKey="42"><Media container="mkv"`)) {
		t.Fatalf("response = %d %q", response.Code, response.Body.Bytes())
	}
	if !bytes.Contains(response.Body.Bytes(), []byte(`ratingKey="43"><Media container="mp4"`)) || bytes.Contains(response.Body.Bytes(), []byte(`ratingKey="43"><Media container="mkv"`)) {
		t.Fatalf("local collection item changed: %s", response.Body.Bytes())
	}
	if ensurer.gets.Load() != 1 {
		t.Fatalf("Get calls = %d", ensurer.gets.Load())
	}
}

func TestMetadataEnrichmentLeavesCollectionUnchangedOnCacheMiss(t *testing.T) {
	fixture := newMetadataEnrichmentFixture(t)
	raw := []byte(`<MediaContainer size="1"><Video ratingKey="42"><Media><Part id="9" file="/media/cloud/episode.strm"/></Media></Video></MediaContainer>`)
	ensurer := &fakeMediaInfoEnsurer{getFn: func(mediainfo.Key) (mediainfo.Record, bool) {
		return mediainfo.Record{}, false
	}}
	handler := fixture.handler(ensurer, metrics.New(), metadataBodyHandler(raw, "application/xml"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(http.MethodGet, "/library/metadata/7/children"))

	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), raw) {
		t.Fatalf("response = %d %q", response.Code, response.Body.Bytes())
	}
	if ensurer.gets.Load() != 1 {
		t.Fatalf("Get calls = %d", ensurer.gets.Load())
	}
}

func TestMediaInfoCacheOnlyRequestRequiresLibraryProductAndSkipRefresh(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		product string
		want    bool
	}{
		{name: "background library sync", path: "/library/metadata/42?skipRefresh=1", product: "Infuse-Library", want: true},
		{name: "case insensitive product", path: "/library/metadata/42?skipRefresh=1", product: "INFUSE-LIBRARY", want: true},
		{name: "interactive library request", path: "/library/metadata/42", product: "Infuse-Library", want: false},
		{name: "ordinary client with skip refresh", path: "/library/metadata/42?skipRefresh=1", product: "Plex for iOS", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			request.Header.Set("X-Plex-Product", test.product)
			if got := mediaInfoCacheOnlyRequest(request); got != test.want {
				t.Fatalf("mediaInfoCacheOnlyRequest() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestMetadataEnrichmentColdMissReturnsOriginalAndOffersP2(t *testing.T) {
	fixture := newMetadataEnrichmentFixture(t)
	raw := fixture.xmlBody()
	ensurer := &fakeMediaInfoEnsurer{}
	handler := newMetadataEnrichmentHandler(metadataEnrichmentOptions{
		Service: ensurer, Mapper: fixture.mapper, Resolver: fixture.resolver,
		CloudExtensions: []string{".strm"},
		ResponseLimit:   1 << 20, Concurrency: 1, Metrics: metrics.New(),
	}, metadataBodyHandler(raw, "application/xml"))

	response := httptest.NewRecorder()
	started := time.Now()
	handler.ServeHTTP(response, authenticatedMetadataRequest(http.MethodGet))
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("cold miss took %s", elapsed)
	}
	if !bytes.Equal(response.Body.Bytes(), raw) || response.Header().Get("ETag") != `"plex-etag"` {
		t.Fatalf("fail-open response changed: headers=%#v body=%s", response.Header(), response.Body.Bytes())
	}
	offered := ensurer.offers()
	if len(offered) != 1 || offered[0].Priority != mediainfo.PriorityMetadata || offered[0].Key.PartID != "9" {
		t.Fatalf("P2 offers = %#v", offered)
	}
}

func TestMetadataEnrichmentRejectsFingerprintChange(t *testing.T) {
	fixture := newMetadataEnrichmentFixture(t)
	raw := fixture.xmlBody()
	ensurer := &fakeMediaInfoEnsurer{memoryFn: func(key mediainfo.Key) (mediainfo.Record, bool) {
		if err := os.WriteFile(fixture.strmPath, []byte("http://mediavault:7811/redirect/changed/file.mkv\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return mediainfo.Record{Key: key, Media: completeProjectionMedia()}, true
	}}
	handler := fixture.handler(ensurer, metrics.New(), metadataBodyHandler(raw, "application/xml"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedMetadataRequest(http.MethodGet))
	if !bytes.Equal(response.Body.Bytes(), raw) {
		t.Fatalf("response changed after STRM identity changed: %s", response.Body.Bytes())
	}
}

func TestMetadataEnrichmentPoolSaturationSkipsWaiting(t *testing.T) {
	fixture := newMetadataEnrichmentFixture(t)
	raw := fixture.xmlBody()
	started := make(chan struct{})
	release := make(chan struct{})
	ensurer := &fakeMediaInfoEnsurer{memoryFn: func(key mediainfo.Key) (mediainfo.Record, bool) {
		return mediainfo.Record{Key: key, Media: completeProjectionMedia()}, true
	}}
	registry := metrics.New()
	var upstreamCalls atomic.Int64
	handler := newMetadataEnrichmentHandler(metadataEnrichmentOptions{
		Service: ensurer, Mapper: fixture.mapper, Resolver: fixture.resolver,
		CloudExtensions: []string{".strm"},
		ResponseLimit:   1 << 20, Concurrency: 1, Metrics: registry,
	}, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if upstreamCalls.Add(1) == 1 {
			close(started)
			<-release
		}
		metadataBodyHandler(raw, "application/xml").ServeHTTP(w, request)
	}))

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, authenticatedMetadataRequest(http.MethodGet))
		firstDone <- response
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first enrichment did not start")
	}
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, authenticatedMetadataRequest(http.MethodGet))
	if !bytes.Equal(second.Body.Bytes(), raw) {
		t.Fatalf("saturated response=%s", second.Body.Bytes())
	}
	close(release)
	select {
	case first := <-firstDone:
		if bytes.Equal(first.Body.Bytes(), raw) {
			t.Fatal("first response was not enriched")
		}
	case <-time.After(time.Second):
		t.Fatal("first enrichment did not finish")
	}
	if registry.Snapshot().MediaInfoWaitRejectedTotal != 1 {
		t.Fatalf("metrics = %#v", registry.Snapshot())
	}
}

func TestMetadataEnrichmentUsesL1RecordAfterColdOffer(t *testing.T) {
	fixture := newMetadataEnrichmentFixture(t)
	raw := fixture.xmlBody()
	var lookups atomic.Int64
	ensurer := &fakeMediaInfoEnsurer{
		memoryFn: func(key mediainfo.Key) (mediainfo.Record, bool) {
			if lookups.Add(1) == 1 {
				return mediainfo.Record{}, false
			}
			return mediainfo.Record{Key: key, Media: completeProjectionMedia()}, true
		},
	}
	handler := newMetadataEnrichmentHandler(metadataEnrichmentOptions{
		Service: ensurer, Mapper: fixture.mapper, Resolver: fixture.resolver,
		CloudExtensions: []string{".strm"},
		ResponseLimit:   1 << 20, Concurrency: 2, Metrics: metrics.New(),
	}, metadataBodyHandler(raw, "application/xml"))

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, authenticatedMetadataRequest(http.MethodGet))
	if !bytes.Equal(first.Body.Bytes(), raw) {
		t.Fatalf("cold response = %s", first.Body.Bytes())
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, authenticatedMetadataRequest(http.MethodGet))
	if !strings.Contains(second.Body.String(), `container="mkv"`) {
		t.Fatalf("cached response was not enriched: %s", second.Body.String())
	}
	if offered := ensurer.offers(); len(offered) != 1 || offered[0].Priority != mediainfo.PriorityMetadata {
		t.Fatalf("offers = %#v", offered)
	}
}

func TestMetadataEnrichmentColdBurstReturnsWithoutSynchronousProbe(t *testing.T) {
	fixture := newMetadataEnrichmentFixture(t)
	raw := fixture.xmlBody()
	ensurer := &fakeMediaInfoEnsurer{}
	registry := metrics.New()
	enrichment := newMetadataEnrichmentHandler(metadataEnrichmentOptions{
		Service: ensurer, Mapper: fixture.mapper, Resolver: fixture.resolver,
		CloudExtensions: []string{".strm"},
		ResponseLimit:   1 << 20, Concurrency: 64, Metrics: registry,
	}, metadataBodyHandler(raw, "application/xml"))
	handler := newMetadataGuard(MetadataGuardOptions{
		Enabled: true, GlobalConcurrency: 16, PerClientConcurrency: 4,
		QueueTimeout: 500 * time.Millisecond,
	}, enrichment, registry, nil)

	const burst = 64
	responses := make(chan *httptest.ResponseRecorder, burst)
	var group sync.WaitGroup
	group.Add(burst)
	for range burst {
		go func() {
			defer group.Done()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, authenticatedMetadataRequest(http.MethodGet))
			responses <- response
		}()
	}
	burstDone := make(chan struct{})
	go func() {
		group.Wait()
		close(burstDone)
	}()
	select {
	case <-burstDone:
	case <-time.After(time.Second):
		t.Fatal("cold metadata burst remained queued")
	}
	close(responses)
	for response := range responses {
		if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), raw) {
			t.Fatalf("burst response = %d %q", response.Code, response.Body.Bytes())
		}
	}
	if offered := ensurer.offers(); len(offered) != burst {
		t.Fatalf("background offers = %d, want %d", len(offered), burst)
	}
	if got := ensurer.gets.Load(); got != 0 {
		t.Fatalf("cold burst performed %d durable-capable lookups", got)
	}
	if snapshot := registry.Snapshot(); snapshot.MetadataGuardTimeoutsTotal != 0 || snapshot.MediaInfoWaitRejectedTotal != 0 {
		t.Fatalf("metrics = %#v", snapshot)
	}
}

func TestMetadataEnrichmentBypassesIneligibleAndUnbufferableResponses(t *testing.T) {
	fixture := newMetadataEnrichmentFixture(t)
	raw := fixture.xmlBody()
	oversized := bytes.Repeat([]byte("x"), 1025)
	ensurer := &fakeMediaInfoEnsurer{}
	tests := []struct {
		name       string
		request    *http.Request
		next       http.Handler
		wantStatus int
		wantBody   []byte
	}{
		{name: "HEAD", request: authenticatedMetadataRequest(http.MethodHead), next: metadataBodyHandler(raw, "application/xml"), wantStatus: http.StatusOK},
		{name: "batch", request: authenticatedRequest(http.MethodGet, "/library/metadata/42,43"), next: metadataBodyHandler(raw, "application/xml"), wantStatus: http.StatusOK, wantBody: raw},
		{name: "children", request: authenticatedRequest(http.MethodGet, "/library/metadata/42/children"), next: metadataBodyHandler(raw, "application/xml"), wantStatus: http.StatusOK, wantBody: raw},
		{name: "HEAD children", request: authenticatedRequest(http.MethodHead, "/library/metadata/42/children"), next: metadataBodyHandler(raw, "application/xml"), wantStatus: http.StatusOK},
		{name: "range children", request: rangeChildrenRequest(), next: metadataBodyHandler(raw, "application/xml"), wantStatus: http.StatusOK, wantBody: raw},
		{name: "missing token children", request: httptest.NewRequest(http.MethodGet, "/library/metadata/42/children", nil), next: metadataBodyHandler(raw, "application/xml"), wantStatus: http.StatusOK, wantBody: raw},
		{name: "Part playback", request: authenticatedRequest(http.MethodGet, "/library/parts/9/1/file.strm"), next: metadataBodyHandler(raw, "application/xml"), wantStatus: http.StatusOK, wantBody: raw},
		{name: "decision", request: authenticatedRequest(http.MethodGet, "/video/:/transcode/universal/decision"), next: metadataBodyHandler(raw, "application/xml"), wantStatus: http.StatusOK, wantBody: raw},
		{name: "universal start", request: authenticatedRequest(http.MethodGet, "/video/:/transcode/universal/start.mpd"), next: metadataBodyHandler(raw, "application/xml"), wantStatus: http.StatusOK, wantBody: raw},
		{name: "timeline", request: authenticatedRequest(http.MethodGet, "/:/timeline"), next: metadataBodyHandler(raw, "application/xml"), wantStatus: http.StatusOK, wantBody: raw},
		{name: "missing token", request: httptest.NewRequest(http.MethodGet, "/library/metadata/42", nil), next: metadataBodyHandler(raw, "application/xml"), wantStatus: http.StatusOK, wantBody: raw},
		{name: "conflicting token", request: conflictingTokenRequest(), next: metadataBodyHandler(raw, "application/xml"), wantStatus: http.StatusOK, wantBody: raw},
		{name: "range", request: rangeMetadataRequest(), next: metadataBodyHandler(raw, "application/xml"), wantStatus: http.StatusOK, wantBody: raw},
		{name: "unsupported encoding", request: authenticatedMetadataRequest(http.MethodGet), next: encodedMetadataHandler(raw, "br"), wantStatus: http.StatusOK, wantBody: raw},
		{name: "oversized", request: authenticatedMetadataRequest(http.MethodGet), next: metadataBodyHandler(oversized, "application/xml"), wantStatus: http.StatusOK, wantBody: oversized},
		{name: "not modified", request: authenticatedMetadataRequest(http.MethodGet), next: metadataStatusHandler(http.StatusNotModified, nil), wantStatus: http.StatusNotModified},
		{name: "partial content", request: authenticatedMetadataRequest(http.MethodGet), next: metadataStatusHandler(http.StatusPartialContent, []byte("partial")), wantStatus: http.StatusPartialContent, wantBody: []byte("partial")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newMetadataEnrichmentHandler(metadataEnrichmentOptions{
				Service: ensurer, Mapper: fixture.mapper, Resolver: fixture.resolver,
				CloudExtensions: []string{".strm"},
				ResponseLimit:   1024, Concurrency: 1, Metrics: metrics.New(),
			}, test.next)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, test.request)
			if response.Code != test.wantStatus || !bytes.Equal(response.Body.Bytes(), test.wantBody) {
				t.Fatalf("response status=%d body=%q, want status=%d body=%q", response.Code, response.Body.Bytes(), test.wantStatus, test.wantBody)
			}
		})
	}
	if offered := ensurer.offers(); len(offered) != 0 {
		t.Fatalf("ineligible responses offered %d probes", len(offered))
	}
}

func TestMetadataEnrichmentAdmitsExistingStreamHDRAndSTRMControlSize(t *testing.T) {
	fixture := newMetadataEnrichmentFixture(t)
	media := completeProjectionMedia()
	media.Size = 987654321
	media.Bitrate = 29430119
	media.Streams[0] = mediainfo.Stream{
		Index: 0, Type: "video", Codec: "hevc", Profile: "Main 10", Bitrate: 27510000,
		Width: 3840, Height: 2160, FrameRate: "24000/1001", PixelFormat: "yuv420p10le", BitDepth: 10,
		ColorSpace: "bt2020nc", ColorRange: "tv", ColorPrimaries: "bt2020", ColorTransfer: "smpte2084",
		HDRFormat: "dolby_vision",
		DolbyVision: &mediainfo.DolbyVision{
			VersionMajor: 1, Profile: 7, Level: 6, RPUPresent: 1,
			ELPresent: 1, BLPresent: 1, BLCompatID: 6,
		},
	}
	tests := []struct {
		name        string
		contentType string
		body        []byte
		want        [][]byte
	}{
		{
			name: "XML", contentType: "application/xml",
			body: []byte(`<MediaContainer><Video ratingKey="42"><Media container="mkv" duration="60000" bitrate="8000" width="3840" height="2160" aspectRatio="16:9" audioChannels="6" audioCodec="aac" videoCodec="hevc" videoResolution="4k" videoFrameRate="23.976" videoProfile="Main 10" audioProfile="LC"><Part id="9" file="/media/cloud/episode.strm" duration="60000" size="301" container="mkv" videoProfile="Main 10" audioProfile="LC"><Stream id="101" streamType="1" index="0" codec="hevc" profile="Main 10" level="153" bitrate="8000" languageCode="eng" title="Video" width="3840" height="2160" frameRate="23.976" refFrames="4" pixelFormat="yuv420p10le" bitDepth="10" colorSpace="bt2020nc" colorRange="tv" colorPrimaries="bt2020" colorTrc="smpte2084" chromaLocation="left" sampleAspectRatio="1:1" displayAspectRatio="16:9"/></Part></Media></Video></MediaContainer>`),
			want: [][]byte{[]byte(`size="987654321"`), []byte(`displayTitle="4K DoVi/HDR10"`), []byte(`DOVIPresent="1"`)},
		},
		{
			name: "JSON", contentType: "application/json",
			body: []byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"42","Media":[{"container":"mkv","duration":60000,"bitrate":8000,"width":3840,"height":2160,"aspectRatio":"16:9","audioChannels":6,"audioCodec":"aac","videoCodec":"hevc","videoResolution":"4k","videoFrameRate":"23.976","videoProfile":"Main 10","audioProfile":"LC","Part":[{"id":9,"file":"/media/cloud/episode.strm","duration":60000,"size":301,"container":"mkv","videoProfile":"Main 10","audioProfile":"LC","Stream":[{"id":101,"streamType":1,"index":0,"codec":"hevc","profile":"Main 10","level":153,"bitrate":8000,"languageCode":"eng","title":"Video","width":3840,"height":2160,"frameRate":23.976,"refFrames":4,"pixelFormat":"yuv420p10le","bitDepth":10,"colorSpace":"bt2020nc","colorRange":"tv","colorPrimaries":"bt2020","colorTrc":"smpte2084","chromaLocation":"left","sampleAspectRatio":"1:1","displayAspectRatio":"16:9"}]}]}]}]}}`),
			want: [][]byte{[]byte(`"size":987654321`), []byte(`"displayTitle":"4K DoVi/HDR10"`), []byte(`"DOVIPresent":true`)},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ensurer := &fakeMediaInfoEnsurer{memoryFn: func(key mediainfo.Key) (mediainfo.Record, bool) {
				return mediainfo.Record{Key: key, Media: media}, true
			}}
			handler := fixture.handler(ensurer, metrics.New(), metadataBodyHandler(test.body, test.contentType))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, authenticatedMetadataRequest(http.MethodGet))
			if ensurer.memoryGets.Load() != 1 || len(ensurer.offers()) != 0 {
				t.Fatalf("memory gets=%d offers=%d", ensurer.memoryGets.Load(), len(ensurer.offers()))
			}
			for _, expected := range test.want {
				if !bytes.Contains(response.Body.Bytes(), expected) {
					t.Fatalf("response missing %q: %s", expected, response.Body.Bytes())
				}
			}
		})
	}
}

func TestMetadataEnrichmentSkipsLocalAndCompleteCloudParts(t *testing.T) {
	fixture := newMetadataEnrichmentFixture(t)
	ensurer := &fakeMediaInfoEnsurer{}
	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "local Part",
			body: []byte(`<MediaContainer><Video ratingKey="42"><Media><Part id="9" file="/media/local/movie.mkv"/></Media></Video></MediaContainer>`),
		},
		{
			name: "complete cloud Part",
			body: []byte(`<MediaContainer><Video ratingKey="42"><Media container="mkv" duration="60000" bitrate="8000" width="3840" height="2160" aspectRatio="16:9" audioChannels="6" audioCodec="aac" videoCodec="hevc" videoResolution="4k" videoFrameRate="23.976" videoProfile="Main 10" audioProfile="LC"><Part id="9" file="/media/cloud/episode.strm" duration="60000" size="2000000" container="mkv" videoProfile="Main 10" audioProfile="LC"><Stream id="101" streamType="1" index="0" codec="hevc" profile="Main 10" level="153" bitrate="8000" language="eng" title="Video" width="3840" height="2160" frameRate="23.976" refFrames="4" pixelFormat="yuv420p10le" bitDepth="10" colorSpace="bt2020nc" colorRange="tv" colorPrimaries="bt2020" colorTrc="smpte2084" chromaLocation="left" sampleAspectRatio="1:1" displayAspectRatio="16:9" displayTitle="4K HDR10" extendedDisplayTitle="4K HDR10 (HEVC Main 10)"/></Part></Media></Video></MediaContainer>`),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := fixture.handler(ensurer, metrics.New(), metadataBodyHandler(test.body, "application/xml"))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, authenticatedMetadataRequest(http.MethodGet))
			if !bytes.Equal(response.Body.Bytes(), test.body) || response.Header().Get("ETag") != `"plex-etag"` {
				t.Fatalf("response changed: headers=%#v body=%s", response.Header(), response.Body.Bytes())
			}
		})
	}
	if ensurer.memoryGets.Load() != 0 || len(ensurer.offers()) != 0 {
		t.Fatalf("skipped Parts memory gets=%d offers=%d", ensurer.memoryGets.Load(), len(ensurer.offers()))
	}
}

type metadataEnrichmentFixture struct {
	mapper   *pathmap.Mapper
	resolver resolver.ControlResolver
	strmPath string
	target   string
}

func newMetadataEnrichmentFixture(t *testing.T) metadataEnrichmentFixture {
	t.Helper()
	directory := t.TempDir()
	strmPath := filepath.Join(directory, "episode.strm")
	target := "http://mediavault:7811/redirect/pick/episode.mkv"
	if err := os.WriteFile(strmPath, []byte(target+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mapper, err := pathmap.New([]pathmap.Mapping{{PlexPrefix: "/media/cloud", LocalPrefix: directory}})
	if err != nil {
		t.Fatal(err)
	}
	strmResolver, err := resolver.NewMediaVaultSTRMResolver("http://mediavault:7811", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return metadataEnrichmentFixture{mapper: mapper, resolver: strmResolver, strmPath: strmPath, target: target}
}

func (fixture metadataEnrichmentFixture) handler(ensurer mediaInfoEnsurer, registry *metrics.Metrics, next http.Handler) http.Handler {
	return newMetadataEnrichmentHandler(metadataEnrichmentOptions{
		Service: ensurer, Mapper: fixture.mapper, Resolver: fixture.resolver,
		CloudExtensions: []string{".strm"},
		ResponseLimit:   1 << 20, Concurrency: 2, Metrics: registry,
	}, next)
}

func (fixture metadataEnrichmentFixture) xmlBody() []byte {
	return []byte(`<MediaContainer size="1"><Video ratingKey="42" type="episode"><Media><Part id="9" key="/library/parts/9/1/file.strm" file="/media/cloud/episode.strm"><Stream id="101" streamType="1" index="0"/></Part></Media></Video></MediaContainer>`)
}

func completeProjectionMedia() mediainfo.Media {
	return mediainfo.Media{
		Complete: true, Container: "mkv", DurationMS: 60_000, Size: 1_000_000,
		Bitrate: 8_000, VideoCodec: "hevc", Width: 3840, Height: 2160,
		Streams: []mediainfo.Stream{{Index: 0, Type: "video", Codec: "hevc", Width: 3840, Height: 2160, HDRFormat: "dolby_vision"}},
	}
}

func authenticatedMetadataRequest(method string) *http.Request {
	return authenticatedRequest(method, "/library/metadata/42")
}

func authenticatedRequest(method, path string) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set("X-Plex-Token", "client-token")
	request.Header.Set("User-Agent", "Infuse-Test/1.0")
	return request
}

func conflictingTokenRequest() *http.Request {
	request := authenticatedRequest(http.MethodGet, "/library/metadata/42?X-Plex-Token=query-token")
	request.Header.Set("X-Plex-Token", "header-token")
	return request
}

func rangeMetadataRequest() *http.Request {
	request := authenticatedMetadataRequest(http.MethodGet)
	request.Header.Set("Range", "bytes=0-100")
	return request
}

func rangeChildrenRequest() *http.Request {
	request := authenticatedRequest(http.MethodGet, "/library/metadata/42/children")
	request.Header.Set("Range", "bytes=0-100")
	return request
}

func metadataBodyHandler(body []byte, contentType string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("ETag", `"plex-etag"`)
		w.WriteHeader(http.StatusOK)
		if request.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	})
}

func encodedMetadataHandler(body []byte, encoding string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("Content-Encoding", encoding)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}

func metadataStatusHandler(status int, body []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if status == http.StatusPartialContent {
			w.Header().Set("Content-Range", "bytes 0-6/100")
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

func gzipBytes(t *testing.T, body []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func gunzipBytes(t *testing.T, body []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}
