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
		if r.URL.Path != "/identity" {
			select {
			case unexpectedPlexPath <- r.URL.Path:
			default:
			}
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `<MediaContainer machineIdentifier="test-server"/>`)
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
	service, store, reason := initializeMediaInfo(t.Context(), config.MediaInfoConfig{
		Enabled:              true,
		DatabasePath:         filepath.Join(directory, "mediainfo.db"),
		FFProbePath:          ffprobePath,
		ProbeTimeout:         time.Second,
		ProbeSize:            1 << 20,
		AnalyzeDuration:      time.Second,
		OutputMaxBytes:       1 << 20,
		Concurrency:          1,
		InteractiveQueueSize: 2,
		BackgroundQueueSize:  2,
		RecordTTL:            24 * time.Hour,
		RecordRetention:      48 * time.Hour,
		L1MaxEntries:         10,
		NegativeTTL:          time.Minute,
		UserAgent:            "plex-gateway-test",
	}, plexURL, time.Second, control, metrics.New(), logger)
	if service == nil || store == nil || reason != "ready" {
		t.Fatalf("initializeMediaInfo() service=%v store=%v reason=%q", service != nil, store != nil, reason)
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
	closeMediaInfo(service, store, time.Second, logger)
}
