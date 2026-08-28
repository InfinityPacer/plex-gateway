package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/config"
	"github.com/InfinityPacer/plex-gateway/internal/database"
	"github.com/InfinityPacer/plex-gateway/internal/gateway"
	"github.com/InfinityPacer/plex-gateway/internal/mediainfo"
	"github.com/InfinityPacer/plex-gateway/internal/metrics"
	"github.com/InfinityPacer/plex-gateway/internal/partcache"
	"github.com/InfinityPacer/plex-gateway/internal/pathmap"
	"github.com/InfinityPacer/plex-gateway/internal/resolver"
	"github.com/InfinityPacer/plex-gateway/internal/trace"
	"github.com/InfinityPacer/plex-gateway/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version.String())
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	logger := newLogger(cfg.LogLevel)
	if err := gateway.ValidatePlexTrustBoundary(context.Background(), cfg.PlexURL, cfg.PartProbeTimeout); err != nil {
		return fmt.Errorf("validate Plex authentication boundary: %w", err)
	}
	registry := metrics.New()
	cache := partcache.New(cfg.PartTTL)
	mapper, err := pathmap.New(cfg.PathMappings)
	if err != nil {
		return fmt.Errorf("create path mapper: %w", err)
	}
	var strmResolver resolver.ControlResolver
	if cfg.MediaVaultURL != nil {
		client := &http.Client{Timeout: cfg.ResolverTimeout}
		strmResolver, err = resolver.NewMediaVaultSTRMResolver(cfg.MediaVaultURL.String(), client, 0)
		if err != nil {
			return fmt.Errorf("create MediaVault resolver: %w", err)
		}
	}
	databaseStore, databaseReason := initializeDatabase(
		context.Background(), cfg.DatabasePath, cfg.MediaInfo.Enabled && strmResolver != nil, logger,
	)
	mediaInfoService, mediaInfoReason := initializeMediaInfo(
		context.Background(), databaseStore, databaseReason, cfg.MediaInfo, cfg.PlexURL, cfg.PartProbeTimeout, strmResolver, registry, logger,
	)
	defer closeRuntime(mediaInfoService, databaseStore, cfg.ShutdownTimeout, logger)
	mediaInfoStatus := func() mediainfo.Status {
		return mediaInfoService.Status()
	}
	handler := gateway.New(gateway.Options{
		Upstream:         cfg.PlexURL,
		Logger:           logger,
		Tracer:           trace.New(cfg.TraceEnabled, logger),
		PartCache:        cache,
		PathMapper:       mapper,
		Resolver:         strmResolver,
		Metrics:          registry,
		CloudExtensions:  cfg.CloudExtensions,
		ObserveMaxBytes:  cfg.ObserveMaxBytes,
		PartProbeTimeout: cfg.PartProbeTimeout,
		MetadataGuard: gateway.MetadataGuardOptions{
			Enabled:              cfg.MetadataGuard.Enabled,
			GlobalConcurrency:    cfg.MetadataGuard.GlobalConcurrency,
			PerClientConcurrency: cfg.MetadataGuard.PerClientConcurrency,
			BatchEnabled:         cfg.MetadataGuard.BatchEnabled,
			BatchConcurrency:     cfg.MetadataGuard.BatchConcurrency,
			QueueTimeout:         cfg.MetadataGuard.QueueTimeout,
		},
		MediaInfoEnabled:               cfg.MediaInfo.Enabled,
		MediaInfoStatus:                mediaInfoStatus,
		MediaInfoService:               mediaInfoService,
		MediaInfoColdWait:              cfg.MediaInfo.ColdWait,
		MediaInfoResponseMaxBytes:      cfg.MediaInfo.ResponseMaxBytes,
		MediaInfoEnrichmentConcurrency: cfg.MediaInfo.EnrichmentWaiters,
	})
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErrors := make(chan error, 1)
	go func() {
		logger.Info("gateway_started",
			"version", version.String(),
			"listen", cfg.ListenAddr,
			"upstream_scheme", cfg.PlexURL.Scheme,
			"cloud_redirect", cfg.MediaVaultURL != nil,
			"path_mappings", len(cfg.PathMappings),
			"trace", cfg.TraceEnabled,
			"metadata_guard", cfg.MetadataGuard.Enabled,
			"metadata_batch_guard", cfg.MetadataGuard.BatchEnabled,
			"mediainfo_enabled", cfg.MediaInfo.Enabled,
			"mediainfo_available", mediaInfoService != nil,
			"mediainfo_reason", mediaInfoReason,
		)
		serveErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve gateway: %w", err)
	case <-ctx.Done():
		logger.Info("gateway_shutdown_started")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown gateway: %w", err)
	}
	logger.Info("gateway_stopped")
	return nil
}

func initializeMediaInfo(
	ctx context.Context,
	databaseStore *database.SQLite,
	databaseReason string,
	cfg config.MediaInfoConfig,
	plexURL *url.URL,
	identityTimeout time.Duration,
	strmResolver resolver.ControlResolver,
	registry *metrics.Metrics,
	logger *slog.Logger,
) (*mediainfo.Service, string) {
	if !cfg.Enabled {
		return nil, "disabled"
	}
	if strmResolver == nil {
		return nil, "cloud_unavailable"
	}
	if databaseStore == nil {
		return nil, databaseReason
	}
	plexServerID, err := gateway.ReadPlexServerIdentity(ctx, plexURL, identityTimeout)
	if err != nil {
		logger.Warn("mediainfo_unavailable", "reason", "plex_identity")
		return nil, "plex_identity_unavailable"
	}
	prober, err := mediainfo.NewFFProber(mediainfo.FFProbeOptions{
		Binary: cfg.FFProbePath, Timeout: cfg.ProbeTimeout, ProbeSize: cfg.ProbeSize,
		AnalyzeDuration: cfg.AnalyzeDuration, OutputLimit: cfg.OutputMaxBytes,
	})
	if err != nil {
		logger.Warn("mediainfo_unavailable", "reason", "ffprobe")
		return nil, "ffprobe_unavailable"
	}
	provider, err := mediainfo.NewMediaVaultFFProbeProvider(strmResolver, prober)
	if err != nil {
		logger.Warn("mediainfo_unavailable", "reason", "provider")
		return nil, "provider_unavailable"
	}
	store, err := mediainfo.NewSQLiteStore(ctx, databaseStore)
	if err != nil {
		logger.Warn("mediainfo_unavailable", "reason", "migration")
		return nil, "migration_failed"
	}
	now := time.Now().UTC()
	if _, err := store.DeleteUnretained(ctx, now); err != nil {
		logger.Warn("mediainfo_store_gc_failed", "error_kind", "storage")
	}
	records, err := store.LoadCompatibleRetained(ctx, now, cfg.L1MaxEntries, provider.Descriptor())
	if err != nil {
		logger.Warn("mediainfo_unavailable", "reason", "restore")
		return nil, "restore_failed"
	}
	cache := mediainfo.NewCacheWithLimit(records, now, cfg.L1MaxEntries)
	service, err := mediainfo.NewService(mediainfo.ServiceOptions{
		Cache: cache, Store: store, Janitor: store, Provider: provider, PlexServerID: plexServerID,
		Logger: logger, Metrics: registry, Concurrency: cfg.Concurrency,
		InteractiveQueueSize: cfg.InteractiveQueueSize, BackgroundQueueSize: cfg.BackgroundQueueSize,
		ProbeTimeout: cfg.ProbeTimeout, RecordTTL: cfg.RecordTTL,
		RecordRetention: cfg.RecordRetention, NegativeTTL: cfg.NegativeTTL,
		BackgroundUserAgent: cfg.UserAgent,
	})
	if err != nil {
		logger.Warn("mediainfo_unavailable", "reason", "worker")
		return nil, "worker_unavailable"
	}
	return service, "ready"
}

func initializeDatabase(ctx context.Context, path string, required bool, logger *slog.Logger) (*database.SQLite, string) {
	if !required {
		return nil, "unused"
	}
	store, err := database.OpenSQLite(ctx, path)
	if err != nil {
		logger.Warn("database_unavailable", "reason", "storage")
		return nil, "storage_unavailable"
	}
	return store, "ready"
}

func closeRuntime(service *mediainfo.Service, store *database.SQLite, timeout time.Duration, logger *slog.Logger) {
	serviceClosed := true
	if service != nil {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		if err := service.Close(ctx); err != nil {
			serviceClosed = false
			logger.Warn("mediainfo_shutdown_incomplete", "error_kind", "timeout")
		}
		cancel()
	}
	if store != nil && serviceClosed {
		if err := store.Close(); err != nil {
			logger.Warn("mediainfo_store_close_failed", "error_kind", "storage")
		}
	}
}

func newLogger(level string) *slog.Logger {
	var slogLevel slog.Level
	switch level {
	case "debug":
		slogLevel = slog.LevelDebug
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		slogLevel = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slogLevel}))
}

func healthcheck() error {
	listenAddr := strings.TrimSpace(os.Getenv("LISTEN_ADDR"))
	if listenAddr == "" {
		listenAddr = ":32400"
	}
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return fmt.Errorf("parse LISTEN_ADDR: %w", err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	endpoint := "http://" + net.JoinHostPort(host, port) + "/health"
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(endpoint)
	if err != nil {
		return fmt.Errorf("health request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health returned HTTP %d", response.StatusCode)
	}
	return nil
}
