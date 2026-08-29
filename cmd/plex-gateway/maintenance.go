package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/mediainfo"
)

const mediaInfoCacheResetPath = "/_plex-gateway/maintenance/mediainfo-cache/reset"

type cacheResetResponse struct {
	Status         string `json:"status"`
	DeletedRecords int64  `json:"deleted_records"`
	Backup         string `json:"backup"`
}

// newMaintenanceHandler exposes container-local maintenance without widening
// the public Gateway API. Remote callers receive no proxy fallback for the
// reserved path.
func newMaintenanceHandler(
	next http.Handler,
	service *mediainfo.Service,
	databasePath string,
	logger *slog.Logger,
) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+mediaInfoCacheResetPath, func(w http.ResponseWriter, request *http.Request) {
		if !loopbackRemote(request.RemoteAddr) {
			http.NotFound(w, request)
			return
		}
		if service == nil {
			http.Error(w, "MediaInfo service unavailable", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(request.Context(), 30*time.Second)
		defer cancel()
		result, err := service.ResetCache(ctx, filepath.Join(filepath.Dir(databasePath), "backups"))
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, mediainfo.ErrCacheResetInProgress) {
				status = http.StatusConflict
			}
			logger.Warn("mediainfo_cache_reset_failed", "error_kind", "maintenance")
			http.Error(w, "MediaInfo cache reset failed", status)
			return
		}
		logger.Info("mediainfo_cache_reset", "deleted_records", result.DeletedRecords)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(cacheResetResponse{
			Status: "ok", DeletedRecords: result.DeletedRecords, Backup: filepath.Base(result.BackupPath),
		})
	})
	mux.Handle("/", next)
	return mux
}

func loopbackRemote(remoteAddress string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddress))
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func requestMediaInfoCacheReset() error {
	endpoint, err := localGatewayEndpoint(mediaInfoCacheResetPath)
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create MediaInfo cache reset request: %w", err)
	}
	client := &http.Client{Timeout: 35 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("request MediaInfo cache reset: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("read MediaInfo cache reset response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("MediaInfo cache reset returned HTTP %d", response.StatusCode)
	}
	var result cacheResetResponse
	if err := json.Unmarshal(body, &result); err != nil || result.Status != "ok" {
		return errors.New("MediaInfo cache reset returned an invalid response")
	}
	fmt.Printf("mediainfo_cache_reset deleted=%d backup=%s\n", result.DeletedRecords, result.Backup)
	return nil
}

func localGatewayEndpoint(path string) (string, error) {
	listenAddr := strings.TrimSpace(os.Getenv("LISTEN_ADDR"))
	if listenAddr == "" {
		listenAddr = ":32400"
	}
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("parse LISTEN_ADDR: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + path, nil
}
