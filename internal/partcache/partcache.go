// Package partcache stores the short-lived relationship between a Plex Part
// identifier and the STRM path observed in Plex metadata.
package partcache

import (
	"sync"
	"time"
)

// PartInfo is the metadata needed to resolve one Plex media Part. PartID is
// the stable cache key; PartKey is retained as observed because its change
// stamp may vary after a library refresh.
type PartInfo struct {
	PartID       string
	RatingKey    string
	PlexFilePath string
	PartKey      string
	UpdatedAt    time.Time
}

type entry struct {
	info      PartInfo
	expiresAt time.Time
}

// Cache is a concurrency-safe in-memory TTL cache. Reads and writes perform a
// bounded-frequency full sweep so inactive Part IDs do not accumulate after
// their TTL expires.
type Cache struct {
	mu            sync.Mutex
	ttl           time.Duration
	sweepInterval time.Duration
	nextSweep     time.Time
	entries       map[string]entry
}

// New creates a cache with the supplied entry TTL. A non-positive TTL creates
// a cache whose entries expire immediately; callers that load configuration
// should validate a positive duration before constructing the cache.
func New(ttl time.Duration) *Cache {
	now := time.Now()
	interval := ttl
	if interval <= 0 || interval > 5*time.Minute {
		interval = 5 * time.Minute
	}
	return &Cache{
		ttl:           ttl,
		sweepInterval: interval,
		nextSweep:     now.Add(interval),
		entries:       make(map[string]entry),
	}
}

// Put stores a PartInfo until the configured TTL elapses. Empty PartID values
// are ignored because they cannot be addressed by the Plex Part endpoint.
func (c *Cache) Put(info PartInfo) {
	if c == nil || info.PartID == "" {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepIfDue(now)
	if info.RatingKey == "" {
		if current, exists := c.entries[info.PartID]; exists && now.Before(current.expiresAt) {
			info.RatingKey = current.info.RatingKey
		}
	}
	c.entries[info.PartID] = entry{
		info:      info,
		expiresAt: now.Add(c.ttl),
	}
}

// Get returns a copy of the cached PartInfo when it is present and unexpired.
// Expired entries are removed while holding the same lock used by Put.
func (c *Cache) Get(partID string) (PartInfo, bool) {
	if c == nil || partID == "" {
		return PartInfo{}, false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepIfDue(now)
	item, ok := c.entries[partID]
	if !ok {
		return PartInfo{}, false
	}
	if !now.Before(item.expiresAt) {
		delete(c.entries, partID)
		return PartInfo{}, false
	}
	return item.info, true
}

// Len returns the number of currently live entries and removes expired ones.
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepExpired(now)
	return len(c.entries)
}

func (c *Cache) sweepIfDue(now time.Time) {
	if now.Before(c.nextSweep) {
		return
	}
	c.sweepExpired(now)
	c.nextSweep = now.Add(c.sweepInterval)
}

func (c *Cache) sweepExpired(now time.Time) {
	for partID, item := range c.entries {
		if !now.Before(item.expiresAt) {
			delete(c.entries, partID)
		}
	}
}
