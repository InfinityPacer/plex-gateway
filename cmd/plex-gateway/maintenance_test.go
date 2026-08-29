package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/mediainfo"
)

type maintenanceStore struct {
	resets atomic.Int64
}

func (*maintenanceStore) Put(context.Context, mediainfo.Record) error { return nil }

func (*maintenanceStore) Get(context.Context, mediainfo.Key) (mediainfo.Record, bool, error) {
	return mediainfo.Record{}, false, nil
}

func (*maintenanceStore) Touch(context.Context, mediainfo.Key, time.Time, time.Time) error {
	return nil
}

func (store *maintenanceStore) BackupAndDeleteAll(
	_ context.Context,
	backupDir string,
	_ time.Time,
) (mediainfo.ResetResult, error) {
	store.resets.Add(1)
	return mediainfo.ResetResult{DeletedRecords: 3, BackupPath: filepath.Join(backupDir, "backup.db")}, nil
}

type maintenanceProvider struct{}

func (maintenanceProvider) Descriptor() mediainfo.ProviderDescriptor {
	return mediainfo.ProviderDescriptor{
		Name: mediainfo.ProviderMediaVaultFFProbe, Revision: mediainfo.ProviderRevisionFFProbeJSONV3,
	}
}

func (maintenanceProvider) Probe(context.Context, mediainfo.ProviderRequest) (mediainfo.ProviderResult, error) {
	return mediainfo.ProviderResult{}, nil
}

func TestMaintenanceHandlerHotResetsFromLoopbackOnly(t *testing.T) {
	store := &maintenanceStore{}
	service, err := mediainfo.NewService(mediainfo.ServiceOptions{
		Cache: mediainfo.NewCache(nil, time.Now()), Store: store, Provider: maintenanceProvider{},
		PlexServerID: "server", BackgroundUserAgent: "Infuse/1.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(t.Context())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := newMaintenanceHandler(http.NotFoundHandler(), service, "/app_data/plex-gateway.db", logger)

	remoteRequest := httptest.NewRequest(http.MethodPost, mediaInfoCacheResetPath, nil)
	remoteRequest.RemoteAddr = "203.0.113.10:1234"
	remoteResponse := httptest.NewRecorder()
	handler.ServeHTTP(remoteResponse, remoteRequest)
	if remoteResponse.Code != http.StatusNotFound || store.resets.Load() != 0 {
		t.Fatalf("remote response=%d resets=%d", remoteResponse.Code, store.resets.Load())
	}

	localRequest := httptest.NewRequest(http.MethodPost, mediaInfoCacheResetPath, nil)
	localRequest.RemoteAddr = "127.0.0.1:1234"
	localResponse := httptest.NewRecorder()
	handler.ServeHTTP(localResponse, localRequest)
	if localResponse.Code != http.StatusOK || store.resets.Load() != 1 {
		t.Fatalf("local response=%d resets=%d body=%s", localResponse.Code, store.resets.Load(), localResponse.Body.String())
	}
	var result cacheResetResponse
	if err := json.Unmarshal(localResponse.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" || result.DeletedRecords != 3 || result.Backup != "backup.db" {
		t.Fatalf("response = %#v", result)
	}
}

func TestRequestMediaInfoCacheResetUsesLocalListener(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != mediaInfoCacheResetPath {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(cacheResetResponse{Status: "ok", DeletedRecords: 2, Backup: "backup.db"})
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LISTEN_ADDR", parsed.Host)
	if err := requestMediaInfoCacheReset(); err != nil {
		t.Fatal(err)
	}
}

func TestLoopbackRemoteRejectsForwardedOrMalformedAddresses(t *testing.T) {
	for _, address := range []string{"127.0.0.1:1", "[::1]:1"} {
		if !loopbackRemote(address) {
			t.Fatalf("loopback address rejected: %s", address)
		}
	}
	for _, address := range []string{"203.0.113.1:1", "127.0.0.1", strings.Repeat("x", 100)} {
		if loopbackRemote(address) {
			t.Fatalf("non-loopback address accepted: %s", address)
		}
	}
}
