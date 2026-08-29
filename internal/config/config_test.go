package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadRejectsCredentialInPlexURL(t *testing.T) {
	t.Setenv("PLEX_URL", "http://token@example.test:32400")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "must not contain credentials") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "")
	t.Setenv("MEDIAVAULT_URL", "")
	t.Setenv("PATH_MAPPINGS", "")
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("TRACE_ENABLED", "")

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.ListenAddr != ":32400" {
		t.Fatalf("ListenAddr = %q", got.ListenAddr)
	}
	if got.PlexURL.String() != "http://plex:32400" {
		t.Fatalf("PlexURL = %q", got.PlexURL)
	}
	if got.PlexToken != "" {
		t.Fatal("PlexToken unexpectedly configured")
	}
	if !got.TraceEnabled {
		t.Fatal("TraceEnabled = false")
	}
	if !got.MetadataGuard.Enabled || got.MetadataGuard.GlobalConcurrency != 8 || got.MetadataGuard.PerClientConcurrency != 4 || !got.MetadataGuard.BatchEnabled || got.MetadataGuard.BatchConcurrency != 3 || got.MetadataGuard.QueueTimeout != 10*time.Second {
		t.Fatalf("unexpected metadata guard defaults: %#v", got.MetadataGuard)
	}
	if got.DatabasePath != "./data/plex-gateway.db" {
		t.Fatalf("DatabasePath = %q", got.DatabasePath)
	}
	if !got.MediaInfo.Enabled || got.MediaInfo.ProbeTimeout != 20*time.Second || got.MediaInfo.Concurrency != 1 || got.MediaInfo.UserAgent != "Infuse-Library/8.5.1" || got.MediaInfo.ColdWait != 5*time.Second || got.MediaInfo.ResponseMaxBytes != 8<<20 || got.MediaInfo.EnrichmentWaiters != 8 || got.MediaInfo.PrewarmBefore != 2 || got.MediaInfo.PrewarmAfter != 3 || got.MediaInfo.PrewarmInterval != 5*time.Second {
		t.Fatalf("unexpected MediaInfo defaults: %#v", got.MediaInfo)
	}
	if got.MediaVaultURL != nil || len(got.PathMappings) != 0 {
		t.Fatalf("cloud redirect unexpectedly enabled: %#v", got)
	}
	if got.PartTTL != 24*time.Hour || got.ResolverTimeout != 15*time.Second || got.PartProbeTimeout != 15*time.Second {
		t.Fatalf("unexpected durations: part=%s resolver=%s probe=%s", got.PartTTL, got.ResolverTimeout, got.PartProbeTimeout)
	}
	if !reflect.DeepEqual(got.CloudExtensions, []string{".strm"}) {
		t.Fatalf("unexpected cloud extensions: %v", got.CloudExtensions)
	}
}

func TestLoadCloudConfiguration(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "management-token")
	t.Setenv("MEDIAVAULT_URL", "http://mediavault:7811")
	t.Setenv("PATH_MAPPINGS", `[
		{"plex_prefix":"/media/cloud","local_prefix":"/media/cloud"},
		{"plex_prefix":"/media/cloud-secondary","local_prefix":"/mnt/cloud-secondary"}
	]`)
	t.Setenv("CLOUD_EXTENSIONS", ".strm,.STRM")
	t.Setenv("PART_CACHE_TTL", "12h")
	t.Setenv("RESOLVER_TIMEOUT", "7s")
	t.Setenv("OBSERVE_MAX_BYTES", "4096")
	t.Setenv("PART_PROBE_TIMEOUT", "3s")
	t.Setenv("METADATA_GUARD_ENABLED", "true")
	t.Setenv("METADATA_GUARD_GLOBAL_CONCURRENCY", "6")
	t.Setenv("METADATA_GUARD_CLIENT_CONCURRENCY", "3")
	t.Setenv("METADATA_GUARD_BATCH_ENABLED", "true")
	t.Setenv("METADATA_GUARD_BATCH_CONCURRENCY", "2")
	t.Setenv("METADATA_GUARD_QUEUE_TIMEOUT", "5s")
	t.Setenv("MEDIAINFO_ENABLED", "true")
	t.Setenv("DATABASE_PATH", "/app_data/test.db")
	t.Setenv("MEDIAINFO_FFPROBE_PATH", "/usr/bin/ffprobe")
	t.Setenv("MEDIAINFO_PROBE_TIMEOUT", "12s")
	t.Setenv("MEDIAINFO_PROBE_SIZE", "4194304")
	t.Setenv("MEDIAINFO_ANALYZE_DURATION", "3s")
	t.Setenv("MEDIAINFO_OUTPUT_MAX_BYTES", "1048576")
	t.Setenv("MEDIAINFO_CONCURRENCY", "2")
	t.Setenv("MEDIAINFO_INTERACTIVE_QUEUE_SIZE", "64")
	t.Setenv("MEDIAINFO_BACKGROUND_QUEUE_SIZE", "128")
	t.Setenv("MEDIAINFO_RECORD_TTL", "168h")
	t.Setenv("MEDIAINFO_RECORD_RETENTION", "720h")
	t.Setenv("MEDIAINFO_L1_MAX_ENTRIES", "5000")
	t.Setenv("MEDIAINFO_NEGATIVE_TTL", "10m")
	t.Setenv("MEDIAINFO_USER_AGENT", "plex-gateway-test")
	t.Setenv("MEDIAINFO_COLD_WAIT", "4s")
	t.Setenv("MEDIAINFO_RESPONSE_MAX_BYTES", "2097152")
	t.Setenv("MEDIAINFO_ENRICHMENT_CONCURRENCY", "3")
	t.Setenv("MEDIAINFO_PREWARM_BEFORE", "4")
	t.Setenv("MEDIAINFO_PREWARM_AFTER", "6")
	t.Setenv("MEDIAINFO_PREWARM_INTERVAL", "9s")

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.MediaVaultURL.String() != "http://mediavault:7811" || len(got.PathMappings) != 2 {
		t.Fatalf("cloud configuration = %#v", got)
	}
	if got.PlexToken != "management-token" {
		t.Fatalf("PlexToken = %q", got.PlexToken)
	}
	if !reflect.DeepEqual(got.CloudExtensions, []string{".strm"}) {
		t.Fatalf("CloudExtensions = %v", got.CloudExtensions)
	}
	if got.PartTTL != 12*time.Hour || got.ResolverTimeout != 7*time.Second || got.ObserveMaxBytes != 4096 || got.PartProbeTimeout != 3*time.Second {
		t.Fatalf("cloud durations/limit = %#v", got)
	}
	if !got.MetadataGuard.Enabled || got.MetadataGuard.GlobalConcurrency != 6 || got.MetadataGuard.PerClientConcurrency != 3 || !got.MetadataGuard.BatchEnabled || got.MetadataGuard.BatchConcurrency != 2 || got.MetadataGuard.QueueTimeout != 5*time.Second {
		t.Fatalf("metadata guard = %#v", got.MetadataGuard)
	}
	if got.DatabasePath != "/app_data/test.db" {
		t.Fatalf("DatabasePath = %q", got.DatabasePath)
	}
	if !got.MediaInfo.Enabled || got.MediaInfo.FFProbePath != "/usr/bin/ffprobe" || got.MediaInfo.ProbeTimeout != 12*time.Second || got.MediaInfo.ProbeSize != 4194304 || got.MediaInfo.AnalyzeDuration != 3*time.Second || got.MediaInfo.OutputMaxBytes != 1048576 || got.MediaInfo.Concurrency != 2 || got.MediaInfo.InteractiveQueueSize != 64 || got.MediaInfo.BackgroundQueueSize != 128 || got.MediaInfo.RecordTTL != 168*time.Hour || got.MediaInfo.RecordRetention != 720*time.Hour || got.MediaInfo.L1MaxEntries != 5000 || got.MediaInfo.NegativeTTL != 10*time.Minute || got.MediaInfo.UserAgent != "plex-gateway-test" || got.MediaInfo.ColdWait != 4*time.Second || got.MediaInfo.ResponseMaxBytes != 2097152 || got.MediaInfo.EnrichmentWaiters != 3 || got.MediaInfo.PrewarmBefore != 4 || got.MediaInfo.PrewarmAfter != 6 || got.MediaInfo.PrewarmInterval != 9*time.Second {
		t.Fatalf("MediaInfo configuration = %#v", got.MediaInfo)
	}
}

func TestLoadRejectsPlexTokenWithLineBreak(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "secret\nvalue")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "line breaks") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsMediaInfoRetentionBelowFreshTTL(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("MEDIAINFO_RECORD_TTL", "720h")
	t.Setenv("MEDIAINFO_RECORD_RETENTION", "168h")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "must not be shorter") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsMetadataClientLimitAboveGlobalLimit(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("METADATA_GUARD_GLOBAL_CONCURRENCY", "2")
	t.Setenv("METADATA_GUARD_CLIENT_CONCURRENCY", "3")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "must not exceed") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRequiresCompleteCloudConfiguration(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("MEDIAVAULT_URL", "http://mediavault:7811")
	t.Setenv("PATH_MAPPINGS", "")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "configured together") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsOversizedPrewarmWindow(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("MEDIAINFO_PREWARM_BEFORE", "25")
	t.Setenv("MEDIAINFO_PREWARM_AFTER", "26")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "total at most 50") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadAllowsCurrentOnlyPrewarm(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("MEDIAINFO_PREWARM_BEFORE", "0")
	t.Setenv("MEDIAINFO_PREWARM_AFTER", "0")

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.MediaInfo.PrewarmBefore != 0 || got.MediaInfo.PrewarmAfter != 0 {
		t.Fatalf("prewarm window = %d/%d", got.MediaInfo.PrewarmBefore, got.MediaInfo.PrewarmAfter)
	}
}
