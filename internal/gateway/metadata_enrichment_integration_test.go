package gateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/database"
	"github.com/InfinityPacer/plex-gateway/internal/mediainfo"
	"github.com/InfinityPacer/plex-gateway/internal/metrics"
	"github.com/InfinityPacer/plex-gateway/internal/partcache"
	"github.com/InfinityPacer/plex-gateway/internal/pathmap"
	"github.com/InfinityPacer/plex-gateway/internal/resolver"
)

type delayedIntegrationProvider struct {
	calls   atomic.Int64
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (*delayedIntegrationProvider) Descriptor() mediainfo.ProviderDescriptor {
	return mediainfo.ProviderDescriptor{Name: "integration", Revision: "v1"}
}

func (provider *delayedIntegrationProvider) Probe(ctx context.Context, _ mediainfo.ProviderRequest) (mediainfo.ProviderResult, error) {
	provider.calls.Add(1)
	provider.once.Do(func() { close(provider.started) })
	select {
	case <-ctx.Done():
		return mediainfo.ProviderResult{}, ctx.Err()
	case <-provider.release:
	}
	return mediainfo.ProviderResult{Media: completeProjectionMedia()}, nil
}

func TestGatewayColdMetadataTimeoutContinuesProbeAndWarmsNextResponse(t *testing.T) {
	raw := []byte(`<MediaContainer><Video ratingKey="42"><Media><Part id="9" file="/media/cloud/episode.strm"><Stream id="101" streamType="1" index="0"/></Part></Media></Video></MediaContainer>`)
	gzippedRaw := gzipBytes(t, raw)
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/library/metadata/42" {
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("ETag", `"plex-etag"`)
		body := raw
		if strings.Contains(request.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			body = gzippedRaw
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer plex.Close()

	directory := t.TempDir()
	strmTarget := "http://mediavault:7811/redirect/pick/episode.mkv"
	if err := os.WriteFile(filepath.Join(directory, "episode.strm"), []byte(strmTarget+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mapper, err := pathmap.New([]pathmap.Mapping{{PlexPrefix: "/media/cloud", LocalPrefix: directory}})
	if err != nil {
		t.Fatal(err)
	}
	controlResolver, err := resolver.NewMediaVaultSTRMResolver("http://mediavault:7811", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	gatewayDB, err := database.OpenSQLite(t.Context(), filepath.Join(directory, "plex-gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := mediainfo.NewSQLiteStore(t.Context(), gatewayDB)
	if err != nil {
		_ = gatewayDB.Close()
		t.Fatal(err)
	}
	provider := &delayedIntegrationProvider{started: make(chan struct{}), release: make(chan struct{})}
	service, err := mediainfo.NewService(mediainfo.ServiceOptions{
		Cache: mediainfo.NewCache(nil, time.Now()), Store: store, Provider: provider,
		PlexServerID: "integration-server", Concurrency: 1,
		InteractiveQueueSize: 4, BackgroundQueueSize: 4,
		ProbeTimeout: time.Second, RecordTTL: time.Hour, RecordRetention: 24 * time.Hour,
		BackgroundUserAgent: "integration-background",
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		_ = gatewayDB.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			t.Error(err)
		}
		if err := gatewayDB.Close(); err != nil {
			t.Error(err)
		}
	})

	plexURL, err := url.Parse(plex.URL)
	if err != nil {
		t.Fatal(err)
	}
	registry := metrics.New()
	handler := New(Options{
		Upstream: plexURL, PartCache: partcache.New(time.Hour), PathMapper: mapper,
		Resolver: controlResolver, Metrics: registry, CloudExtensions: []string{".strm"},
		MediaInfoEnabled: true, MediaInfoStatus: service.Status, MediaInfoService: service,
		MediaInfoColdWait: 20 * time.Millisecond, MediaInfoResponseMaxBytes: 1 << 20,
		MediaInfoEnrichmentConcurrency: 1,
	})

	first := httptest.NewRecorder()
	startedAt := time.Now()
	handler.ServeHTTP(first, authenticatedMetadataRequest(http.MethodGet))
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("cold fail-open took %s", elapsed)
	}
	if first.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), raw) || first.Header().Get("ETag") != `"plex-etag"` {
		t.Fatalf("cold response status=%d headers=%#v body=%s", first.Code, first.Header(), first.Body.Bytes())
	}
	select {
	case <-provider.started:
	default:
		t.Fatal("cold request did not start the background probe")
	}
	close(provider.release)

	fingerprint, err := mediainfo.FingerprintSTRMTarget(strmTarget)
	if err != nil {
		t.Fatal(err)
	}
	key := mediainfo.Key{PlexServerID: "integration-server", PartID: "9", STRMFingerprint: fingerprint}
	waitForPersistedMediaInfo(t, store, key)

	second := httptest.NewRecorder()
	warmRequest := authenticatedMetadataRequest(http.MethodGet)
	warmRequest.Header.Set("Accept-Encoding", "gzip")
	handler.ServeHTTP(second, warmRequest)
	if second.Code != http.StatusOK || bytes.Equal(second.Body.Bytes(), raw) {
		t.Fatalf("warm response status=%d body=%s", second.Code, second.Body.Bytes())
	}
	decodedWarm := gunzipBytes(t, second.Body.Bytes())
	if second.Header().Get("Content-Encoding") != "gzip" || second.Header().Get("ETag") != "" || !bytes.Contains(decodedWarm, []byte(`container="mkv"`)) {
		t.Fatalf("warm response headers=%#v body=%s", second.Header(), second.Body.Bytes())
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls.Load())
	}
	snapshot := registry.Snapshot()
	if snapshot.MediaInfoFailOpenTotal != 1 || snapshot.MediaInfoEnrichedTotal != 1 {
		t.Fatalf("metrics = %#v", snapshot)
	}

	for _, requestPath := range []string{
		"/library/parts/999/1/file.strm",
		"/video/:/transcode/universal/decision",
		"/video/:/transcode/universal/start.mpd",
		"/:/timeline",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, authenticatedRequest(http.MethodGet, requestPath))
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("non-metadata routes triggered MediaInfo provider: calls=%d", provider.calls.Load())
	}
}

func waitForPersistedMediaInfo(t *testing.T, store *mediainfo.SQLiteStore, key mediainfo.Key) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if record, found, err := store.Get(t.Context(), key); err != nil {
			t.Fatal(err)
		} else if found && record.Media.Complete {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("MediaInfo worker did not persist the completed probe")
}
