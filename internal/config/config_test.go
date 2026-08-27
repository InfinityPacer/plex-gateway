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
	if got.TraceEnabled {
		t.Fatal("TraceEnabled = true")
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

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.MediaVaultURL.String() != "http://mediavault:7811" || len(got.PathMappings) != 2 {
		t.Fatalf("cloud configuration = %#v", got)
	}
	if !reflect.DeepEqual(got.CloudExtensions, []string{".strm"}) {
		t.Fatalf("CloudExtensions = %v", got.CloudExtensions)
	}
	if got.PartTTL != 12*time.Hour || got.ResolverTimeout != 7*time.Second || got.ObserveMaxBytes != 4096 || got.PartProbeTimeout != 3*time.Second {
		t.Fatalf("cloud durations/limit = %#v", got)
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
