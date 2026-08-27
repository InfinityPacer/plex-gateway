package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/pathmap"
)

const (
	defaultListenAddr        = ":32400"
	defaultReadHeaderTimeout = 15 * time.Second
	defaultIdleTimeout       = 90 * time.Second
	defaultShutdownTimeout   = 15 * time.Second
	defaultPartTTL           = 24 * time.Hour
	defaultResolverTimeout   = 15 * time.Second
	defaultObserveMaxBytes   = 8 << 20
	defaultPartProbeTimeout  = 15 * time.Second
)

// Config contains process-level settings for transparent Plex proxying and
// optional STRM redirect handling. Credentials remain request-scoped and are
// never loaded into this structure.
type Config struct {
	ListenAddr        string
	PlexURL           *url.URL
	MediaVaultURL     *url.URL
	PathMappings      []pathmap.Mapping
	CloudExtensions   []string
	PartTTL           time.Duration
	ResolverTimeout   time.Duration
	ObserveMaxBytes   int64
	PartProbeTimeout  time.Duration
	LogLevel          string
	TraceEnabled      bool
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

// Load reads environment configuration and rejects values that would make the
// proxy ambiguous or accidentally embed credentials in durable configuration.
func Load() (Config, error) {
	plexRaw := strings.TrimSpace(os.Getenv("PLEX_URL"))
	if plexRaw == "" {
		return Config{}, errors.New("PLEX_URL is required")
	}

	plexURL, err := url.Parse(plexRaw)
	if err != nil {
		return Config{}, fmt.Errorf("parse PLEX_URL: %w", err)
	}
	if plexURL.Scheme != "http" && plexURL.Scheme != "https" {
		return Config{}, errors.New("PLEX_URL must use http or https")
	}
	if plexURL.Host == "" {
		return Config{}, errors.New("PLEX_URL must include a host")
	}
	if plexURL.User != nil {
		return Config{}, errors.New("PLEX_URL must not contain credentials")
	}
	plexURL.RawQuery = ""
	plexURL.Fragment = ""

	mediaVaultURL, err := optionalHTTPURL("MEDIAVAULT_URL")
	if err != nil {
		return Config{}, err
	}
	pathMappings, err := envPathMappings("PATH_MAPPINGS")
	if err != nil {
		return Config{}, err
	}
	if (mediaVaultURL == nil) != (len(pathMappings) == 0) {
		return Config{}, errors.New("MEDIAVAULT_URL and PATH_MAPPINGS must be configured together")
	}

	partTTL, err := envDuration("PART_CACHE_TTL", defaultPartTTL)
	if err != nil {
		return Config{}, err
	}
	resolverTimeout, err := envDuration("RESOLVER_TIMEOUT", defaultResolverTimeout)
	if err != nil {
		return Config{}, err
	}
	observeMaxBytes, err := envPositiveInt64("OBSERVE_MAX_BYTES", defaultObserveMaxBytes)
	if err != nil {
		return Config{}, err
	}
	partProbeTimeout, err := envDuration("PART_PROBE_TIMEOUT", defaultPartProbeTimeout)
	if err != nil {
		return Config{}, err
	}
	cloudExtensions, err := envExtensions("CLOUD_EXTENSIONS", []string{".strm"})
	if err != nil {
		return Config{}, err
	}

	traceEnabled, err := envBool("TRACE_ENABLED", false)
	if err != nil {
		return Config{}, err
	}

	readHeaderTimeout, err := envDuration("READ_HEADER_TIMEOUT", defaultReadHeaderTimeout)
	if err != nil {
		return Config{}, err
	}
	idleTimeout, err := envDuration("IDLE_TIMEOUT", defaultIdleTimeout)
	if err != nil {
		return Config{}, err
	}
	shutdownTimeout, err := envDuration("SHUTDOWN_TIMEOUT", defaultShutdownTimeout)
	if err != nil {
		return Config{}, err
	}

	listenAddr := strings.TrimSpace(os.Getenv("LISTEN_ADDR"))
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	}
	logLevel := strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL")))
	if logLevel == "" {
		logLevel = "info"
	}
	if logLevel != "debug" && logLevel != "info" && logLevel != "warn" && logLevel != "error" {
		return Config{}, fmt.Errorf("unsupported LOG_LEVEL %q", logLevel)
	}

	return Config{
		ListenAddr:        listenAddr,
		PlexURL:           plexURL,
		MediaVaultURL:     mediaVaultURL,
		PathMappings:      pathMappings,
		CloudExtensions:   cloudExtensions,
		PartTTL:           partTTL,
		ResolverTimeout:   resolverTimeout,
		ObserveMaxBytes:   observeMaxBytes,
		PartProbeTimeout:  partProbeTimeout,
		LogLevel:          logLevel,
		TraceEnabled:      traceEnabled,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ShutdownTimeout:   shutdownTimeout,
	}, nil
}

func optionalHTTPURL(name string) (*url.URL, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("%s must be an absolute HTTP(S) URL", name)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("%s must not contain credentials, query, or fragment", name)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed, nil
}

func envPathMappings(name string) ([]pathmap.Mapping, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil, nil
	}
	var mappings []pathmap.Mapping
	if err := json.Unmarshal([]byte(raw), &mappings); err != nil {
		return nil, fmt.Errorf("parse %s JSON: %w", name, err)
	}
	mapper, err := pathmap.New(mappings)
	if err != nil {
		return nil, fmt.Errorf("validate %s: %w", name, err)
	}
	return mapper.Mappings(), nil
}

func envExtensions(name string, fallback []string) ([]string, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return append([]string(nil), fallback...), nil
	}
	seen := make(map[string]struct{})
	var extensions []string
	for _, item := range strings.Split(raw, ",") {
		extension := strings.ToLower(strings.TrimSpace(item))
		if extension == "" || extension[0] != '.' || strings.ContainsAny(extension, `/\\\x00`) {
			return nil, fmt.Errorf("parse %s: invalid extension %q", name, item)
		}
		if _, exists := seen[extension]; exists {
			continue
		}
		seen[extension] = struct{}{}
		extensions = append(extensions, extension)
	}
	if len(extensions) == 0 {
		return nil, fmt.Errorf("%s must contain at least one extension", name)
	}
	return extensions, nil
}

func envBool(name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", name, err)
	}
	return value, nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	if value <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return value, nil
}

func envPositiveInt64(name string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	if value <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return value, nil
}
