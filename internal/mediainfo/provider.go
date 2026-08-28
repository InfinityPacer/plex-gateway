package mediainfo

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/InfinityPacer/plex-gateway/internal/resolver"
)

// ProviderRequest contains the ephemeral transport context needed to analyze
// one STRM target. UserAgent is not persisted in the resulting record.
type ProviderRequest struct {
	Target    string
	UserAgent string
}

// ProviderResult carries normalized media data and optional stronger content
// identity supplied by the backing provider.
type ProviderResult struct {
	ContentFingerprint string
	Media              Media
}

// ProviderDescriptor identifies one provider output contract. A revision
// change invalidates records whose normalized interpretation is incompatible.
type ProviderDescriptor struct {
	Name     string
	Revision string
}

func (descriptor ProviderDescriptor) validate() error {
	if strings.TrimSpace(descriptor.Name) == "" || strings.TrimSpace(descriptor.Revision) == "" {
		return errors.New("MediaInfo provider descriptor is invalid")
	}
	return nil
}

// Provider is the replaceable source boundary for MediaInfo. Queueing,
// persistence, freshness, and Plex projection do not depend on its transport.
type Provider interface {
	Descriptor() ProviderDescriptor
	Probe(context.Context, ProviderRequest) (ProviderResult, error)
}

// MediaVaultFFProbeProvider resolves a MediaVault STRM control target and probes
// the resulting CDN URL with the same User-Agent.
type MediaVaultFFProbeProvider struct {
	resolver resolver.ControlResolver
	prober   Prober
}

// NewMediaVaultFFProbeProvider builds the default fallback provider.
func NewMediaVaultFFProbeProvider(controlResolver resolver.ControlResolver, prober Prober) (*MediaVaultFFProbeProvider, error) {
	if controlResolver == nil || prober == nil {
		return nil, ErrServiceUnavailable
	}
	return &MediaVaultFFProbeProvider{resolver: controlResolver, prober: prober}, nil
}

// Descriptor returns the durable compatibility identity for this provider.
func (*MediaVaultFFProbeProvider) Descriptor() ProviderDescriptor {
	return ProviderDescriptor{Name: ProviderMediaVaultFFProbe, Revision: ProviderRevisionFFProbeJSONV1}
}

// Probe forwards only the transport headers required for deterministic
// resolution and probing. Plex credentials never reach the CDN probe.
func (provider *MediaVaultFFProbeProvider) Probe(ctx context.Context, request ProviderRequest) (ProviderResult, error) {
	if provider == nil || provider.resolver == nil || provider.prober == nil {
		return ProviderResult{}, ErrServiceUnavailable
	}
	request.Target = strings.TrimSpace(request.Target)
	request.UserAgent = strings.TrimSpace(request.UserAgent)
	if request.Target == "" || request.UserAgent == "" || strings.ContainsAny(request.UserAgent, "\r\n") {
		return ProviderResult{}, errors.New("MediaInfo provider request is invalid")
	}
	header := make(http.Header)
	header.Set("User-Agent", request.UserAgent)
	header.Set("Accept-Encoding", "identity")
	directURL, err := provider.resolver.ResolveTarget(ctx, request.Target, resolver.PlaybackRequest{
		Method: http.MethodGet,
		Header: header,
	})
	if err != nil {
		return ProviderResult{}, errors.New("resolve MediaInfo target")
	}
	media, err := provider.prober.Probe(ctx, directURL.String(), request.UserAgent)
	if err != nil {
		return ProviderResult{}, err
	}
	return ProviderResult{Media: media}, nil
}
