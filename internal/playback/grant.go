package playback

import (
	"crypto/sha256"
	"encoding/binary"
	"strconv"
	"sync"
	"time"
)

// SessionIdentity is the first stable Plex playback-session identifier shared
// by a decision and its corresponding universal start request.
type SessionIdentity struct {
	Name  string
	Value string
}

// Attempt is the normalized identity of one Plex Media/Part playback attempt.
// Client-specific omission rules belong in the protocol adapter that constructs
// this value, not in grant storage.
type Attempt struct {
	MetadataPath string
	MediaIndex   int
	PartIndex    int
	Session      SessionIdentity
}

// Valid reports whether the Plex item and exact Media/Part selection are
// normalized. A decision may still be useful when no session identity exists.
func (a Attempt) Valid() bool {
	return a.MetadataPath != "" && a.MediaIndex >= 0 && a.PartIndex >= 0
}

// Correlatable reports whether decision and start requests can safely share a
// short-lived grant.
func (a Attempt) Correlatable() bool {
	return a.Valid() && a.Session.Name != "" && a.Session.Value != ""
}

// Grant binds a normalized playback attempt to one recently accepted Direct
// Play Part. The expiry remains owned by the store.
type Grant struct {
	Part      PreparedPart
	expiresAt time.Time
}

// GrantStore is a bounded in-memory bridge between Plex decision and universal
// start requests. It stores only hashed attempt identities.
type GrantStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	limit   int
	entries map[[sha256.Size]byte]Grant
}

// NewGrantStore creates a bounded store. Non-positive values use conservative
// defaults appropriate for short-lived Plex playback decisions.
func NewGrantStore(ttl time.Duration, limit int) *GrantStore {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if limit <= 0 {
		limit = 4096
	}
	return &GrantStore{
		ttl:     ttl,
		limit:   limit,
		entries: make(map[[sha256.Size]byte]Grant),
	}
}

// Put publishes a Direct Play grant for one normalized attempt.
func (s *GrantStore) Put(attempt Attempt, part PreparedPart) bool {
	key, ok := attemptKey(attempt)
	if s == nil || !ok || part.Part.ID == "" || part.Part.Key == "" ||
		part.Part.File == "" || part.Target == "" {
		return false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for candidate, grant := range s.entries {
		if !grant.expiresAt.After(now) {
			delete(s.entries, candidate)
		}
	}
	if _, exists := s.entries[key]; !exists && len(s.entries) >= s.limit {
		var oldestKey [sha256.Size]byte
		var oldestExpiry time.Time
		for candidate, grant := range s.entries {
			if oldestExpiry.IsZero() || grant.expiresAt.Before(oldestExpiry) {
				oldestKey = candidate
				oldestExpiry = grant.expiresAt
			}
		}
		delete(s.entries, oldestKey)
	}
	s.entries[key] = Grant{Part: part, expiresAt: now.Add(s.ttl)}
	return true
}

// Get returns a live grant without extending its original decision lifetime.
func (s *GrantStore) Get(attempt Attempt) (Grant, bool) {
	key, ok := attemptKey(attempt)
	if s == nil || !ok {
		return Grant{}, false
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, found := s.entries[key]
	if !found {
		return Grant{}, false
	}
	if !grant.expiresAt.After(now) {
		delete(s.entries, key)
		return Grant{}, false
	}
	return grant, true
}

// Delete revokes any previous grant before Plex evaluates a new decision for
// the same normalized attempt.
func (s *GrantStore) Delete(attempt Attempt) {
	key, ok := attemptKey(attempt)
	if s == nil || !ok {
		return
	}
	s.mu.Lock()
	delete(s.entries, key)
	s.mu.Unlock()
}

func attemptKey(attempt Attempt) ([sha256.Size]byte, bool) {
	var zero [sha256.Size]byte
	if !attempt.Correlatable() {
		return zero, false
	}
	fields := []string{
		"path", attempt.MetadataPath,
		"mediaIndex", strconv.Itoa(attempt.MediaIndex),
		"partIndex", strconv.Itoa(attempt.PartIndex),
		attempt.Session.Name, attempt.Session.Value,
	}
	hash := sha256.New()
	var length [8]byte
	for _, field := range fields {
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(field))
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, true
}
