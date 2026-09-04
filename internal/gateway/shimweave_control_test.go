package gateway

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/pathmap"
	"github.com/InfinityPacer/plex-gateway/internal/playback"
	"github.com/InfinityPacer/plex-gateway/internal/plexmeta"
	"github.com/InfinityPacer/plex-gateway/internal/resolver"
)

func TestShimWeaveControlNegotiationKeepsMediaBytesOffGateway(t *testing.T) {
	localRoot := writeTranscodeSTRM(t)
	var mediaVaultRequests, partRequests atomic.Int64
	mediaVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaVaultRequests.Add(1)
		if !strings.HasPrefix(r.Header.Get("Range"), "bytes=") || r.Header.Get("User-Agent") != "ShimWeave Test Browser" {
			t.Fatalf("MediaVault headers = %#v", r.Header)
		}
		if r.Header.Get("X-Plex-Token") != "header-token" ||
			r.Header.Get("Cookie") != "plex=session" ||
			r.Header.Get("Authorization") != "Bearer client-credential" {
			t.Fatalf("Plex context was not retained: %#v", r.Header)
		}
		if r.Header.Get(shimWeaveControlTokenHeader) != "" {
			t.Fatal("control bearer was forwarded to MediaVault")
		}
		w.Header().Set("Location", "https://cdn.invalid/D.mkv?signature=private")
		w.WriteHeader(http.StatusFound)
	}))
	defer mediaVault.Close()

	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case transcodeMetadataPath:
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="21" key="/library/parts/21/1/file" file="/media/cloud/D.strm"/></Media></Video></MediaContainer>`))
		case "/video/:/transcode/universal/decision":
			writeDirectPlayDecision(w)
		case transcodePartPath:
			partRequests.Add(1)
			w.Header().Set("Location", transcodeControlURL)
			w.WriteHeader(http.StatusMovedPermanently)
		default:
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()

	handler, _, _ := newCloudHandler(t, plex.URL, mediaVault.URL, []pathmap.Mapping{{
		PlexPrefix: "/media/cloud", LocalPrefix: localRoot,
	}})
	query := transcodeQuery()
	performCloudDecision(t, handler, query, http.StatusOK)

	start := newTranscodeRequest(http.MethodGet, "/video/:/transcode/universal/start.mpd", query)
	start.Header.Set(shimWeaveAcceptHeader, shimWeaveProtocol)
	start.Header.Set("User-Agent", "ShimWeave Test Browser")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, start)

	if response.Code != http.StatusNoContent {
		t.Fatalf("start status = %d", response.Code)
	}
	if response.Header().Get(shimWeaveProtocolHeader) != shimWeaveProtocol {
		t.Fatalf("protocol = %q", response.Header().Get(shimWeaveProtocolHeader))
	}
	controlPath := response.Header().Get(shimWeaveControlURLHeader)
	controlToken := response.Header().Get(shimWeaveControlTokenHeader)
	if controlPath != shimWeaveControlPath || controlToken == "" ||
		response.Header().Get(shimWeaveSourceIDHeader) == "" {
		t.Fatalf("descriptor headers = %#v", response.Header())
	}
	if strings.Contains(controlPath, controlToken) {
		t.Fatal("bearer control token leaked into the request path")
	}
	if mediaVaultRequests.Load() != 0 || partRequests.Load() != 1 {
		t.Fatalf("MediaVault=%d Part=%d", mediaVaultRequests.Load(), partRequests.Load())
	}

	for offset := range 20 {
		rangeRequest := httptest.NewRequest(http.MethodGet, controlPath, nil)
		rangeRequest.Header.Set("Range", "bytes="+strconv.Itoa(offset)+"-524287")
		rangeRequest.Header.Set("User-Agent", "ShimWeave Test Browser")
		rangeRequest.Header.Set(shimWeaveControlTokenHeader, controlToken)
		rangeRequest.Header.Set("X-Plex-Token", "forged-token")
		rangeRequest.Header.Set("Cookie", "forged=session")
		rangeRequest.Header.Set("Authorization", "Bearer forged")
		rangeResponse := httptest.NewRecorder()
		handler.ServeHTTP(rangeResponse, rangeRequest)
		if rangeResponse.Code != http.StatusNoContent || rangeResponse.Header().Get(shimWeaveMediaURLHeader) != "https://cdn.invalid/D.mkv?signature=private" || rangeResponse.Header().Get("Location") != "" || rangeResponse.Body.Len() != 0 {
			t.Fatalf("range status=%d mediaURL=%q Location=%q body=%d", rangeResponse.Code, rangeResponse.Header().Get(shimWeaveMediaURLHeader), rangeResponse.Header().Get("Location"), rangeResponse.Body.Len())
		}
	}
	if mediaVaultRequests.Load() != 20 || partRequests.Load() != 1 {
		t.Fatalf("MediaVault=%d Part=%d", mediaVaultRequests.Load(), partRequests.Load())
	}

	repeated := httptest.NewRecorder()
	handler.ServeHTTP(repeated, start.Clone(start.Context()))
	if repeated.Header().Get(shimWeaveControlURLHeader) != controlPath ||
		repeated.Header().Get(shimWeaveControlTokenHeader) != controlToken {
		t.Fatalf("repeated descriptor changed: first=%#v repeated=%#v", response.Header(), repeated.Header())
	}
}

func TestControlTicketRenewsIdleLifetimeButNotAbsoluteLifetime(t *testing.T) {
	store := newControlTicketStore(time.Minute, 3*time.Minute, 2)
	now := time.Unix(1_000, 0)
	store.now = func() time.Time { return now }
	sequence := 0
	store.random = func(_ int) (string, error) {
		sequence++
		return strings.Repeat(string(rune('a'+sequence)), 24), nil
	}
	attempt := playback.Attempt{
		MetadataPath: "/library/metadata/42", MediaIndex: 0, PartIndex: 0,
		Session: playback.SessionIdentity{Name: "X-Plex-Playback-Session-Id", Value: "playback"},
	}
	descriptor, err := store.Issue(attempt, controlTestPart(), resolver.PlaybackRequest{
		Method: http.MethodGet, Header: http.Header{"User-Agent": {"browser"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	token := descriptor.ControlToken

	now = now.Add(50 * time.Second)
	if _, found := store.Lease(token); !found {
		t.Fatal("live ticket was not renewed")
	}
	now = now.Add(50 * time.Second)
	if _, found := store.Lease(token); !found {
		t.Fatal("renewed ticket expired at its original idle deadline")
	}
	now = time.Unix(1_000, 0).Add(3*time.Minute + time.Nanosecond)
	if _, found := store.Lease(token); found {
		t.Fatal("ticket exceeded its absolute lifetime")
	}
}

func TestControlTicketSurvivesTheShortDecisionGrantWindow(t *testing.T) {
	store := newControlTicketStore(30*time.Minute, time.Hour, 1)
	now := time.Unix(2_000, 0)
	store.now = func() time.Time { return now }
	sequence := 0
	store.random = func(_ int) (string, error) {
		sequence++
		return strings.Repeat(string(rune('k'+sequence)), 24), nil
	}
	attempt := playback.Attempt{
		MetadataPath: "/library/metadata/42", MediaIndex: 0, PartIndex: 0,
		Session: playback.SessionIdentity{Name: "X-Plex-Playback-Session-Id", Value: "pause-test"},
	}
	descriptor, err := store.Issue(attempt, controlTestPart(), resolver.PlaybackRequest{})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(6 * time.Minute)
	token := descriptor.ControlToken
	if _, found := store.Lease(token); !found {
		t.Fatal("active playback capability expired with the five-minute decision Grant")
	}
}

func TestControlTicketCoalescesOneAttemptAndSeparatesNormalizedAttempts(t *testing.T) {
	store := newControlTicketStore(time.Minute, time.Hour, 4)
	firstAttempt := playback.Attempt{
		MetadataPath: "/library/metadata/42", MediaIndex: 0, PartIndex: 0,
		Session: playback.SessionIdentity{Name: "X-Plex-Playback-Session-Id", Value: "first"},
	}
	first, err := store.Issue(firstAttempt, controlTestPart(), resolver.PlaybackRequest{})
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := store.Issue(firstAttempt, controlTestPart(), resolver.PlaybackRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if repeated.ControlToken != first.ControlToken {
		t.Fatal("one Plex playback attempt did not reuse its control identity")
	}

	secondAttempt := firstAttempt
	secondAttempt.Session.Value = "second"
	second, err := store.Issue(secondAttempt, controlTestPart(), resolver.PlaybackRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if second.ControlToken == first.ControlToken {
		t.Fatal("a different normalized Plex attempt reused the previous control identity")
	}
}

func TestShimWeaveHeaderDoesNotChangeAnUnrecognizedStartRequest(t *testing.T) {
	var upstreamRequests atomic.Int64
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		if r.Header.Get(shimWeaveAcceptHeader) != shimWeaveProtocol {
			t.Fatalf("negotiation header was not transparently forwarded")
		}
		w.Header().Set("Content-Type", "application/dash+xml")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer plex.Close()

	handler, _, _ := newCloudHandler(t, plex.URL, "http://mediavault.invalid:7811", nil)
	request := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/start.mpd?path=%2Flibrary%2Fmetadata%2F42&mediaIndex=0&partIndex=0", nil)
	request.Header.Set(shimWeaveAcceptHeader, shimWeaveProtocol)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted || response.Header().Get(shimWeaveProtocolHeader) != "" || upstreamRequests.Load() != 1 {
		t.Fatalf("status=%d protocol=%q upstream=%d", response.Code, response.Header().Get(shimWeaveProtocolHeader), upstreamRequests.Load())
	}
}

func controlTestPart() playback.PreparedPart {
	return playback.PreparedPart{
		Part: plexmeta.Part{
			ID: "21", Key: "/library/parts/21/1/file", File: "/media/cloud/D.strm",
		},
		Target: "http://mediavault.invalid/redirect/pickcode/D.mkv",
	}
}

func TestControlTicketSourceIdentityChangesWithMaterializedPart(t *testing.T) {
	base := playback.PreparedPart{
		Part:   plexmeta.Part{ID: "21", Key: "/library/parts/21/1/file", File: "/media/D.strm"},
		Target: "https://media.invalid/redirect/first",
	}
	changedKey := base
	changedKey.Part.Key = "/library/parts/21/2/file"
	changedTarget := base
	changedTarget.Target = "https://media.invalid/redirect/second"
	if shimWeaveSourceID(base) == shimWeaveSourceID(changedKey) ||
		shimWeaveSourceID(base) == shimWeaveSourceID(changedTarget) {
		t.Fatal("source identity did not include Part change stamp and STRM target")
	}
}

func TestControlTicketRejectsOversizedAuthorizationContext(t *testing.T) {
	store := newControlTicketStore(time.Minute, time.Hour, 1)
	attempt := playback.Attempt{
		MetadataPath: "/library/metadata/42", MediaIndex: 0, PartIndex: 0,
		Session: playback.SessionIdentity{Name: "X-Plex-Playback-Session-Id", Value: "playback"},
	}
	header := make(http.Header)
	header.Set("X-Large", strings.Repeat("x", maxControlRequestHeaderBytes))
	if _, err := store.Issue(attempt, controlTestPart(), resolver.PlaybackRequest{Header: header}); err == nil {
		t.Fatal("oversized request headers were retained by the ticket store")
	}
}

func TestControlEndpointRequiresHeaderTokenAndRange(t *testing.T) {
	store := newControlTicketStore(time.Minute, time.Hour, 1)
	store.random = func(bytes int) (string, error) {
		return randomControlID(bytes)
	}
	attempt := playback.Attempt{
		MetadataPath: "/library/metadata/42", MediaIndex: 0, PartIndex: 0,
		Session: playback.SessionIdentity{Name: "X-Plex-Playback-Session-Id", Value: "playback"},
	}
	descriptor, err := store.Issue(attempt, controlTestPart(), resolver.PlaybackRequest{})
	if err != nil {
		t.Fatal(err)
	}
	handler := &shimWeaveControlHandler{store: store, service: &playback.Service{}}

	withoutToken := httptest.NewRecorder()
	handler.ServeHTTP(withoutToken, httptest.NewRequest(http.MethodGet, descriptor.ControlPath, nil))
	if withoutToken.Code != http.StatusNotFound {
		t.Fatalf("missing token status = %d", withoutToken.Code)
	}

	withoutRangeRequest := httptest.NewRequest(http.MethodGet, descriptor.ControlPath, nil)
	withoutRangeRequest.Header.Set(shimWeaveControlTokenHeader, descriptor.ControlToken)
	withoutRange := httptest.NewRecorder()
	handler.ServeHTTP(withoutRange, withoutRangeRequest)
	if withoutRange.Code != http.StatusBadRequest {
		t.Fatalf("missing Range status = %d", withoutRange.Code)
	}
}
