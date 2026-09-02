package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/metrics"
	"github.com/InfinityPacer/plex-gateway/internal/playback"
	"github.com/InfinityPacer/plex-gateway/internal/resolver"
)

const (
	shimWeaveProtocol             = "control-v1"
	shimWeaveAcceptHeader         = "X-ShimWeave-Accept"
	shimWeaveProtocolHeader       = "X-ShimWeave-Protocol"
	shimWeaveSourceIDHeader       = "X-ShimWeave-Source-Id"
	shimWeaveControlURLHeader     = "X-ShimWeave-Control-Url"
	shimWeaveControlTokenHeader   = "X-ShimWeave-Control-Token"
	shimWeaveMediaURLHeader       = "X-ShimWeave-Media-Url"
	shimWeaveControlPath          = "/_shimweave/control/v1/range"
	defaultControlTicketIdleTTL   = 30 * time.Minute
	defaultControlTicketMaxTTL    = 24 * time.Hour
	defaultControlTicketStoreSize = 4096
	maxControlRequestHeaderBytes  = 64 << 10
)

type shimWeaveDescriptor struct {
	SourceID     string
	ControlPath  string
	ControlToken string
}

type controlTicket struct {
	Token        string
	SourceID     string
	Part         playback.PreparedPart
	Request      resolver.PlaybackRequest
	AttemptKey   [sha256.Size]byte
	ExpiresAt    time.Time
	MaxExpiresAt time.Time
}

// controlTicketStore owns short-lived bearer capabilities for one authorized
// Plex decision/start attempt. Reads renew only the idle deadline and never
// extend the absolute lifetime, so a long active stream survives the decision
// Grant without becoming a permanent credential.
type controlTicketStore struct {
	mu        sync.Mutex
	idleTTL   time.Duration
	maxTTL    time.Duration
	limit     int
	entries   map[string]controlTicket
	byAttempt map[[sha256.Size]byte]string
	now       func() time.Time
	random    func(int) (string, error)
}

func newControlTicketStore(idleTTL, maxTTL time.Duration, limit int) *controlTicketStore {
	if idleTTL <= 0 {
		idleTTL = defaultControlTicketIdleTTL
	}
	if maxTTL <= 0 {
		maxTTL = defaultControlTicketMaxTTL
	}
	if maxTTL < idleTTL {
		maxTTL = idleTTL
	}
	if limit <= 0 {
		limit = defaultControlTicketStoreSize
	}
	return &controlTicketStore{
		idleTTL:   idleTTL,
		maxTTL:    maxTTL,
		limit:     limit,
		entries:   make(map[string]controlTicket),
		byAttempt: make(map[[sha256.Size]byte]string),
		now:       time.Now,
		random:    randomControlID,
	}
}

func (s *controlTicketStore) Issue(attempt playback.Attempt, part playback.PreparedPart, request resolver.PlaybackRequest) (shimWeaveDescriptor, error) {
	if s == nil || !attempt.Correlatable() || part.Part.ID == "" || part.Part.Key == "" || part.Target == "" {
		return shimWeaveDescriptor{}, errors.New("control ticket input is incomplete")
	}
	if playbackRequestHeaderBytes(request) > maxControlRequestHeaderBytes {
		return shimWeaveDescriptor{}, errors.New("control ticket request headers exceed the limit")
	}
	now := s.now()
	sourceID := shimWeaveSourceID(part)
	attemptKey := shimWeaveAttemptKey(attempt)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if token := s.byAttempt[attemptKey]; token != "" {
		if ticket, found := s.entries[token]; found && ticket.SourceID == sourceID {
			ticket.Request = clonePlaybackRequest(request)
			ticket.ExpiresAt = minTime(now.Add(s.idleTTL), ticket.MaxExpiresAt)
			s.entries[token] = ticket
			return descriptorForTicket(ticket), nil
		}
		s.deleteLocked(token)
	}
	if len(s.entries) >= s.limit {
		s.evictLocked()
	}
	var token string
	for range 4 {
		candidate, err := s.random(32)
		if err != nil {
			return shimWeaveDescriptor{}, err
		}
		if _, exists := s.entries[candidate]; !exists {
			token = candidate
			break
		}
	}
	if token == "" {
		return shimWeaveDescriptor{}, errors.New("control ticket ID collision")
	}
	maximum := now.Add(s.maxTTL)
	ticket := controlTicket{
		Token:        token,
		SourceID:     sourceID,
		Part:         part,
		Request:      clonePlaybackRequest(request),
		AttemptKey:   attemptKey,
		ExpiresAt:    minTime(now.Add(s.idleTTL), maximum),
		MaxExpiresAt: maximum,
	}
	s.entries[token] = ticket
	s.byAttempt[attemptKey] = token
	return descriptorForTicket(ticket), nil
}

func (s *controlTicketStore) Lease(token string) (controlTicket, bool) {
	if s == nil || token == "" {
		return controlTicket{}, false
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	ticket, found := s.entries[token]
	if !found || !ticket.ExpiresAt.After(now) || !ticket.MaxExpiresAt.After(now) {
		if found {
			s.deleteLocked(token)
		}
		return controlTicket{}, false
	}
	ticket.ExpiresAt = minTime(now.Add(s.idleTTL), ticket.MaxExpiresAt)
	s.entries[token] = ticket
	ticket.Request = clonePlaybackRequest(ticket.Request)
	return ticket, true
}

func (s *controlTicketStore) pruneLocked(now time.Time) {
	for token, ticket := range s.entries {
		if !ticket.ExpiresAt.After(now) || !ticket.MaxExpiresAt.After(now) {
			s.deleteLocked(token)
		}
	}
}

func (s *controlTicketStore) evictLocked() {
	var oldestToken string
	var oldestExpiry time.Time
	for token, ticket := range s.entries {
		if oldestExpiry.IsZero() || ticket.ExpiresAt.Before(oldestExpiry) {
			oldestToken = token
			oldestExpiry = ticket.ExpiresAt
		}
	}
	if oldestToken != "" {
		s.deleteLocked(oldestToken)
	}
}

func (s *controlTicketStore) deleteLocked(token string) {
	ticket, found := s.entries[token]
	if !found {
		return
	}
	delete(s.entries, token)
	if s.byAttempt[ticket.AttemptKey] == token {
		delete(s.byAttempt, ticket.AttemptKey)
	}
}

type shimWeaveControlHandler struct {
	store   *controlTicketStore
	service *playback.Service
	metrics *metrics.Metrics
	logger  *slog.Logger
}

// ServeHTTP resolves one Range control request into an ephemeral CDN URL. The
// Gateway returns no media body and never follows or persists the signed URL;
// the browser starts a separate credential-free request to the media origin.
func (h *shimWeaveControlHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, ok := readControlToken(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ticket, found := h.store.Lease(token)
	if !found {
		http.NotFound(w, r)
		return
	}
	request, err := mergeControlRequest(ticket.Request, r)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	started := time.Now()
	result, err := h.service.Resolve(r.Context(), ticket.Part, request)
	if err != nil {
		if requestCanceled(r, err) {
			return
		}
		if h.metrics != nil {
			h.metrics.IncRedirectFailure()
			h.metrics.ObserveResolverLatency(result.ResolverLatency)
		}
		if h.logger != nil {
			h.logger.Warn("shimweave_control_failed", "part", ticket.Part.Part.ID, "error_kind", errorKind(err))
		}
		http.Error(w, http.StatusText(http.StatusBadGateway), http.StatusBadGateway)
		return
	}
	if requestFinished(r) {
		return
	}
	if h.metrics != nil {
		h.metrics.IncRedirectSuccess()
		h.metrics.ObserveResolverLatency(result.ResolverLatency)
		h.metrics.ObserveRedirectLatency(time.Since(started))
	}
	w.Header().Set(shimWeaveMediaURLHeader, result.DirectURL.String())
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func acceptsShimWeaveControl(request *http.Request) bool {
	if request == nil {
		return false
	}
	for _, value := range request.Header.Values(shimWeaveAcceptHeader) {
		for _, item := range strings.Split(value, ",") {
			if strings.TrimSpace(item) == shimWeaveProtocol {
				return true
			}
		}
	}
	return false
}

func writeShimWeaveDescriptor(w http.ResponseWriter, descriptor shimWeaveDescriptor) {
	w.Header().Set(shimWeaveProtocolHeader, shimWeaveProtocol)
	w.Header().Set(shimWeaveSourceIDHeader, descriptor.SourceID)
	w.Header().Set(shimWeaveControlURLHeader, descriptor.ControlPath)
	w.Header().Set(shimWeaveControlTokenHeader, descriptor.ControlToken)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func shimWeaveSourceID(part playback.PreparedPart) string {
	hash := sha256.New()
	for _, field := range []string{part.Part.ID, part.Part.Key, part.Target} {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(field))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func descriptorForTicket(ticket controlTicket) shimWeaveDescriptor {
	return shimWeaveDescriptor{
		SourceID:     ticket.SourceID,
		ControlPath:  shimWeaveControlPath,
		ControlToken: ticket.Token,
	}
}

func clonePlaybackRequest(request resolver.PlaybackRequest) resolver.PlaybackRequest {
	return resolver.PlaybackRequest{Method: request.Method, Header: request.Header.Clone()}
}

func mergeControlRequest(base resolver.PlaybackRequest, current *http.Request) (resolver.PlaybackRequest, error) {
	merged := clonePlaybackRequest(base)
	if merged.Header == nil {
		merged.Header = make(http.Header)
	}
	ranges := current.Header.Values("Range")
	if len(ranges) != 1 || strings.TrimSpace(ranges[0]) == "" {
		return resolver.PlaybackRequest{}, errors.New("control request must contain one Range header")
	}
	merged.Header.Set("Range", ranges[0])
	merged.Method = http.MethodGet
	return merged, nil
}

func readControlToken(request *http.Request) (string, bool) {
	if request == nil {
		return "", false
	}
	values := request.Header.Values(shimWeaveControlTokenHeader)
	if len(values) != 1 {
		return "", false
	}
	token := strings.TrimSpace(values[0])
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	return token, err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == token
}

func randomControlID(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func minTime(first, second time.Time) time.Time {
	if first.Before(second) {
		return first
	}
	return second
}

func shimWeaveAttemptKey(attempt playback.Attempt) [sha256.Size]byte {
	hash := sha256.New()
	var length [8]byte
	for _, field := range []string{
		attempt.MetadataPath,
		strconv.Itoa(attempt.MediaIndex),
		strconv.Itoa(attempt.PartIndex),
		attempt.Session.Name,
		attempt.Session.Value,
	} {
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(field))
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func playbackRequestHeaderBytes(request resolver.PlaybackRequest) int {
	total := len(request.Method)
	for name, values := range request.Header {
		total += len(name)
		for _, value := range values {
			total += len(value)
		}
	}
	return total
}
