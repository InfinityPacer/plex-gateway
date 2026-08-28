package mediainfo

import (
	"context"
	"net/http"
	"testing"

	"github.com/InfinityPacer/plex-gateway/internal/resolver"
)

type capturingResolver struct {
	playback resolver.PlaybackRequest
}

func (capture *capturingResolver) ReadTarget(string) (string, error) { return "", nil }

func (capture *capturingResolver) ResolveTarget(_ context.Context, _ string, playback resolver.PlaybackRequest) (resolver.DirectURL, error) {
	capture.playback = playback
	return resolver.DirectURL{URL: "https://cdn.example.test/movie.mkv"}, nil
}

type capturingProber struct {
	target    string
	userAgent string
}

func (capture *capturingProber) Probe(_ context.Context, target, userAgent string) (Media, error) {
	capture.target = target
	capture.userAgent = userAgent
	return Media{
		Complete: true, Container: "mkv", DurationMS: 60_000,
		Streams: []Stream{{Index: 0, Type: "video", Codec: "hevc", Width: 1920, Height: 1080}},
	}, nil
}

func TestMediaVaultProviderUsesSameClientUserAgentForRedirectAndProbe(t *testing.T) {
	control := &capturingResolver{}
	probe := &capturingProber{}
	provider, err := NewMediaVaultFFProbeProvider(control, probe)
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Probe(t.Context(), ProviderRequest{
		Target: "pickcode", UserAgent: "Infuse-Library/8.4.4",
	})
	if err != nil {
		t.Fatal(err)
	}
	if control.playback.Method != http.MethodGet ||
		control.playback.Header.Get("User-Agent") != "Infuse-Library/8.4.4" ||
		control.playback.Header.Get("Accept-Encoding") != "identity" {
		t.Fatalf("resolver request = %#v", control.playback)
	}
	if len(control.playback.Header) != 2 {
		t.Fatalf("unexpected resolver headers = %#v", control.playback.Header)
	}
	if probe.target != "https://cdn.example.test/movie.mkv" || probe.userAgent != "Infuse-Library/8.4.4" {
		t.Fatalf("probe target=%q User-Agent=%q", probe.target, probe.userAgent)
	}
	if provider.Descriptor() != (ProviderDescriptor{Name: ProviderMediaVaultFFProbe, Revision: ProviderRevisionFFProbeJSONV2}) || !result.Media.Complete {
		t.Fatalf("provider result = %#v", result)
	}
}
