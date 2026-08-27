package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/config"
	"github.com/InfinityPacer/plex-gateway/internal/gateway"
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
			QueueTimeout:         cfg.MetadataGuard.QueueTimeout,
		},
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
