// Package playback owns the cloud Direct Play use case shared by Plex Part
// requests and universal playback sessions.
package playback

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/partcache"
	"github.com/InfinityPacer/plex-gateway/internal/pathmap"
	"github.com/InfinityPacer/plex-gateway/internal/plexmeta"
	"github.com/InfinityPacer/plex-gateway/internal/resolver"
)

// PreparationState describes whether a Plex Part can enter cloud playback.
type PreparationState uint8

const (
	PreparationUnavailable PreparationState = iota
	PreparationMissing
	PreparationLocal
	PreparationFailed
	PreparationReady
)

// PreparedPart binds the Plex identity used for authorization to the exact
// control target read from its mapped STRM file.
type PreparedPart struct {
	Part      plexmeta.Part
	RatingKey string
	Target    string
}

// Preparation is a typed result so HTTP adapters can preserve transparent
// Plex fallback without reconstructing path and STRM rules.
type Preparation struct {
	State  PreparationState
	Part   PreparedPart
	Reason string
	Cloud  bool
}

// FailureKind identifies the use-case step that prevented a direct redirect.
type FailureKind string

const (
	FailureAuthorization FailureKind = "authorization"
	FailureResolver      FailureKind = "resolver"
	FailureEmptyLocation FailureKind = "empty_location"
)

// Failure preserves the underlying network or cancellation cause while giving
// HTTP adapters a stable operational reason that contains no target URL.
type Failure struct {
	Kind FailureKind
	Err  error
}

func (f *Failure) Error() string {
	if f == nil {
		return "playback failed"
	}
	return "playback " + string(f.Kind) + " failed"
}

func (f *Failure) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.Err
}

// AuthorizePartFunc asks Plex whether the current request may access an exact
// Part and STRM control target.
type AuthorizePartFunc func(request *http.Request, partReference, expectedTarget string) (bool, error)

// PlayInput describes one prepared Part request after the HTTP adapter has
// established its Plex protocol identity.
type PlayInput struct {
	Request                 *http.Request
	Part                    PreparedPart
	PartReference           string
	RefreshCacheOnAuthorize bool
}

// PlayResult contains the direct redirect and the measured resolver portion of
// the control path. ResolverLatency is also populated for resolver failures.
type PlayResult struct {
	DirectURL       resolver.DirectURL
	ResolverLatency time.Duration
}

// Options contains the concrete collaborators of the cloud playback use case.
type Options struct {
	Cache           *partcache.Cache
	Mapper          *pathmap.Mapper
	Resolver        resolver.ControlResolver
	AuthorizePart   AuthorizePartFunc
	CloudExtensions []string
}

// Service prepares, authorizes, and resolves cloud Parts. It never writes an
// HTTP response; protocol adapters remain responsible for redirect or fallback.
type Service struct {
	cache         *partcache.Cache
	resolver      resolver.ControlResolver
	authorizePart AuthorizePartFunc
	preparer      *PartPreparer
}

// PartPreparer owns the pure cloud-file classification, path mapping, and STRM
// target read shared by live playback and speculative analysis.
type PartPreparer struct {
	mapper          *pathmap.Mapper
	resolver        resolver.ControlResolver
	cloudExtensions map[string]struct{}
}

// NewPartPreparer creates the side-effect-bounded preparation boundary. It
// never contacts Plex, MediaVault, or the media origin.
func NewPartPreparer(mapper *pathmap.Mapper, controlResolver resolver.ControlResolver, cloudExtensions []string) *PartPreparer {
	return &PartPreparer{
		mapper: mapper, resolver: controlResolver,
		cloudExtensions: normalizeExtensions(cloudExtensions),
	}
}

// New creates a playback service. Missing cloud collaborators intentionally
// produce PreparationUnavailable so a proxy-only deployment remains valid.
func New(options Options) *Service {
	return &Service{
		cache: options.Cache, resolver: options.Resolver, authorizePart: options.AuthorizePart,
		preparer: NewPartPreparer(options.Mapper, options.Resolver, options.CloudExtensions),
	}
}

// PrepareCached looks up a Part by its stable Plex ID and applies the same
// preparation rules used by universal playback decisions.
func (s *Service) PrepareCached(partID string) Preparation {
	if !s.enabled() {
		return Preparation{State: PreparationUnavailable, Reason: "cloud_disabled"}
	}
	info, found := s.cache.Get(partID)
	if !found {
		return Preparation{State: PreparationMissing, Reason: "cache_miss"}
	}
	preparation := s.Prepare(plexmeta.Part{ID: info.PartID, Key: info.PartKey, File: info.PlexFilePath})
	preparation.Part.RatingKey = info.RatingKey
	return preparation
}

// Prepare maps a cloud Part to its local STRM file and reads one validated
// control target without contacting Plex, MediaVault, or the media origin.
func (s *Service) Prepare(partInfo plexmeta.Part) Preparation {
	if s == nil || s.preparer == nil {
		return Preparation{State: PreparationUnavailable, Reason: "cloud_disabled"}
	}
	return s.preparer.Prepare(partInfo)
}

// Prepare applies cloud Part rules without playback authorization or direct
// URL resolution.
func (preparer *PartPreparer) Prepare(partInfo plexmeta.Part) Preparation {
	if preparer == nil || preparer.mapper == nil || preparer.resolver == nil {
		return Preparation{State: PreparationUnavailable, Reason: "cloud_disabled"}
	}
	prepared := PreparedPart{Part: partInfo}
	if partInfo.ID == "" || partInfo.File == "" {
		return Preparation{State: PreparationFailed, Part: prepared, Reason: "invalid_part"}
	}
	if _, ok := preparer.cloudExtensions[strings.ToLower(path.Ext(partInfo.File))]; !ok {
		return Preparation{State: PreparationLocal, Part: prepared}
	}
	localPath, err := preparer.mapper.Resolve(partInfo.File)
	if err != nil {
		return Preparation{State: PreparationFailed, Part: prepared, Reason: "path_mapping", Cloud: true}
	}
	target, err := preparer.resolver.ReadTarget(localPath)
	if err != nil {
		return Preparation{State: PreparationFailed, Part: prepared, Reason: "strm_target", Cloud: true}
	}
	prepared.Target = target
	return Preparation{State: PreparationReady, Part: prepared, Cloud: true}
}

// Remember refreshes the derived Part cache after Plex metadata has selected or
// authorized a Part. Plex remains the source of truth for every stored field.
func (s *Service) Remember(part PreparedPart) {
	partInfo := part.Part
	if s == nil || s.cache == nil || partInfo.ID == "" || partInfo.File == "" {
		return
	}
	s.cache.Put(partcache.PartInfo{
		PartID:       partInfo.ID,
		RatingKey:    part.RatingKey,
		PlexFilePath: partInfo.File,
		PartKey:      partInfo.Key,
		UpdatedAt:    time.Now().UTC(),
	})
}

// Play authorizes the prepared target through Plex, then resolves it through
// MediaVault. It never follows or downloads the returned media URL.
func (s *Service) Play(input PlayInput) (PlayResult, error) {
	if err := s.Authorize(input); err != nil {
		return PlayResult{}, err
	}
	return s.Resolve(input.Request.Context(), input.Part, ResolverRequest(input.Request))
}

// Authorize validates that one client may access the exact Plex Part and STRM
// target before any direct URL is resolved or reusable control capability is
// created for that playback.
func (s *Service) Authorize(input PlayInput) error {
	if s == nil || s.authorizePart == nil || input.Request == nil {
		return &Failure{Kind: FailureAuthorization, Err: errors.New("playback service is unavailable")}
	}
	authorized, err := s.authorizePart(input.Request, input.PartReference, input.Part.Target)
	if err != nil || !authorized {
		return &Failure{Kind: FailureAuthorization, Err: err}
	}
	if input.RefreshCacheOnAuthorize {
		s.Remember(input.Part)
	}
	return nil
}

// Resolve exchanges one already-authorized STRM control target for a direct
// URL. Callers must keep the returned URL ephemeral and must not proxy media
// bytes through the Gateway.
func (s *Service) Resolve(ctx context.Context, part PreparedPart, request resolver.PlaybackRequest) (PlayResult, error) {
	if s == nil || s.resolver == nil {
		return PlayResult{}, &Failure{Kind: FailureResolver, Err: errors.New("playback service is unavailable")}
	}
	started := time.Now()
	directURL, err := s.resolver.ResolveTarget(ctx, part.Target, request)
	result := PlayResult{DirectURL: directURL, ResolverLatency: time.Since(started)}
	if err != nil {
		return result, &Failure{Kind: FailureResolver, Err: err}
	}
	if strings.TrimSpace(directURL.String()) == "" {
		return result, &Failure{Kind: FailureEmptyLocation}
	}
	return result, nil
}

func (s *Service) enabled() bool {
	return s != nil && s.cache != nil && s.preparer != nil && s.preparer.mapper != nil &&
		s.preparer.resolver != nil && s.resolver != nil && s.authorizePart != nil
}

// ResolverRequest snapshots the active client semantics for a trusted control
// request, including query-carried Plex context. The returned headers are
// independent from the caller and may be retained for a bounded playback
// capability; direct-link resolution always uses GET.
func ResolverRequest(request *http.Request) resolver.PlaybackRequest {
	header := request.Header.Clone()
	for name, values := range request.URL.Query() {
		headerName, ok := plexContextHeaderName(name)
		if !ok || len(header.Values(headerName)) != 0 {
			continue
		}
		header[headerName] = append([]string(nil), values...)
	}
	return resolver.PlaybackRequest{Method: http.MethodGet, Header: header}
}

func plexContextHeaderName(name string) (string, bool) {
	if strings.EqualFold(name, "Accept-Language") {
		return "Accept-Language", true
	}
	const prefix = "X-Plex-"
	if len(name) <= len(prefix) || !strings.EqualFold(name[:len(prefix)], prefix) {
		return "", false
	}
	for _, character := range name[len(prefix):] {
		if character != '-' && (character < '0' || character > '9') &&
			(character < 'A' || character > 'Z') && (character < 'a' || character > 'z') {
			return "", false
		}
	}
	return http.CanonicalHeaderKey(name), true
}

func normalizeExtensions(extensions []string) map[string]struct{} {
	if len(extensions) == 0 {
		extensions = []string{".strm"}
	}
	result := make(map[string]struct{}, len(extensions))
	for _, extension := range extensions {
		extension = strings.ToLower(strings.TrimSpace(extension))
		if extension != "" {
			result[extension] = struct{}{}
		}
	}
	return result
}
