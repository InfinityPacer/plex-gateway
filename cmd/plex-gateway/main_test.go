package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/config"
	"github.com/InfinityPacer/plex-gateway/internal/metrics"
	"github.com/InfinityPacer/plex-gateway/internal/pathmap"
	"github.com/InfinityPacer/plex-gateway/internal/playback"
	"github.com/InfinityPacer/plex-gateway/internal/resolver"
)

type countingControlResolver struct {
	calls atomic.Int64
}

func (control *countingControlResolver) ReadTarget(string) (string, error) {
	control.calls.Add(1)
	return "", errors.New("unexpected STRM read")
}

func (control *countingControlResolver) ResolveTarget(context.Context, string, resolver.PlaybackRequest) (resolver.DirectURL, error) {
	control.calls.Add(1)
	return resolver.DirectURL{}, errors.New("unexpected MediaVault resolve")
}

func TestInitializeMediaInfoDoesNotProbeWithoutTask(t *testing.T) {
	unexpectedPlexPath := make(chan string, 1)
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/identity":
			_, _ = io.WriteString(w, `<MediaContainer machineIdentifier="test-server"/>`)
		case "/library/sections":
			if r.Header.Get("X-Plex-Token") != "management-token" {
				http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"MediaContainer":{"Metadata":[]}}`)
		default:
			select {
			case unexpectedPlexPath <- r.URL.Path:
			default:
			}
			http.NotFound(w, r)
		}
	}))
	defer plex.Close()
	plexURL, err := url.Parse(plex.URL)
	if err != nil {
		t.Fatal(err)
	}

	directory := t.TempDir()
	ffprobePath := filepath.Join(directory, "ffprobe")
	if err := os.WriteFile(ffprobePath, []byte("#!/bin/sh\n: > \"$0.called\"\nexit 99\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	control := &countingControlResolver{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	databaseStore, databaseReason := initializeDatabase(t.Context(), filepath.Join(directory, "plex-gateway.db"), true, logger)
	service, reason := initializeMediaInfo(t.Context(), databaseStore, databaseReason, config.MediaInfoConfig{
		Enabled:            true,
		FFProbePath:        ffprobePath,
		ProbeTimeout:       time.Second,
		ProbeSize:          1 << 20,
		AnalyzeDuration:    time.Second,
		OutputMaxBytes:     1 << 20,
		Concurrency:        1,
		PlaybackQueueSize:  2,
		NeighborQueueSize:  2,
		MetadataQueueSize:  2,
		PendingTTL:         time.Minute,
		BackgroundInterval: time.Millisecond,
		RecordTTL:          24 * time.Hour,
		RecordRetention:    48 * time.Hour,
		L1MaxEntries:       10,
		NegativeTTL:        time.Minute,
		UserAgent:          "plex-gateway-test",
	}, plexURL, time.Second, control, metrics.New(), logger)
	if service == nil || databaseStore == nil || reason != "ready" {
		t.Fatalf("initializeMediaInfo() service=%v store=%v reason=%q", service != nil, databaseStore != nil, reason)
	}
	if status := service.Status(); !status.Available || status.ActiveProbes != 0 ||
		status.InteractiveQueued != 0 || status.BackgroundQueued != 0 {
		t.Fatalf("unexpected MediaInfo status %#v", status)
	}
	if got := control.calls.Load(); got != 0 {
		t.Fatalf("resolver calls = %d", got)
	}
	select {
	case path := <-unexpectedPlexPath:
		t.Fatalf("unexpected Plex path %q", path)
	default:
	}
	if _, err := os.Stat(ffprobePath + ".called"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ffprobe process unexpectedly started: %v", err)
	}
	mapper, err := pathmap.New([]pathmap.Mapping{{PlexPrefix: "/media/cloud", LocalPrefix: directory}})
	if err != nil {
		t.Fatal(err)
	}
	partPreparer := playback.NewPartPreparer(mapper, control, []string{".strm"})
	invalidTokenService, reason := initializePrewarm(
		t.Context(), "invalid-token", plexURL, time.Second, partPreparer, service,
		2, 3, metrics.New(), logger,
	)
	if invalidTokenService == nil || reason != "current_only_plex_token_invalid" ||
		!invalidTokenService.Status().Available || invalidTokenService.Status().NeighborAvailable {
		t.Fatalf("invalid token prewarm service=%v reason=%q status=%#v", invalidTokenService != nil, reason, invalidTokenService.Status())
	}
	closeContext, closeCancel := context.WithTimeout(t.Context(), time.Second)
	if err := invalidTokenService.Close(closeContext); err != nil {
		closeCancel()
		t.Fatal(err)
	}
	closeCancel()
	prewarmService, reason := initializePrewarm(
		t.Context(), "management-token", plexURL, time.Second, partPreparer, service,
		2, 3, metrics.New(), logger,
	)
	if prewarmService == nil || reason != "ready" || !prewarmService.Status().Available {
		t.Fatalf("prewarm service=%v reason=%q", prewarmService != nil, reason)
	}
	closeRuntime(prewarmService, service, databaseStore, time.Second, logger)
}
