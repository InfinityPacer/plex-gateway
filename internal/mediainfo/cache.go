package mediainfo

import (
	"sort"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

const defaultCacheEntries = 10_000

type cacheEntry struct {
	record             Record
	lastAccess         atomic.Int64
	retainUntil        atomic.Int64
	lastTouchScheduled atomic.Int64
}

type touchReservation struct {
	entry     *cacheEntry
	previous  int64
	scheduled int64
}

// Cache is a bounded L1 record cache. Records are detached at the boundary so
// callers cannot mutate cached media payloads, while recency drives eviction.
type Cache struct {
	records *lru.Cache[string, *cacheEntry]
}

// NewCache creates a bounded L1 cache with the default entry limit.
func NewCache(records []Record, now time.Time) *Cache {
	return NewCacheWithLimit(records, now, defaultCacheEntries)
}

// NewCacheWithLimit creates a bounded L1 cache and restores retained records.
func NewCacheWithLimit(records []Record, now time.Time, maxEntries int) *Cache {
	if maxEntries <= 0 {
		maxEntries = defaultCacheEntries
	}
	recordsByKey, err := lru.New[string, *cacheEntry](maxEntries)
	if err != nil {
		panic("create MediaInfo L1 cache: " + err.Error())
	}
	cache := &Cache{records: recordsByKey}
	cache.restore(records, now)
	return cache
}

// restore installs the most recently accessed retained records at startup.
func (cache *Cache) restore(records []Record, now time.Time) {
	retained := append([]Record(nil), records...)
	sort.Slice(retained, func(left, right int) bool {
		return retained[left].LastAccessedAt.Before(retained[right].LastAccessedAt)
	})
	for _, record := range retained {
		if !record.Retained(now) {
			continue
		}
		cache.records.Add(record.Key.cacheKey(), newCacheEntry(record))
	}
}

// Get returns a detached fresh record for an exact stable key.
func (cache *Cache) Get(key Key, now time.Time) (Record, bool) {
	cacheKey, _, record, ok := cache.peek(key)
	if !ok || !record.Fresh(now) {
		return Record{}, false
	}
	cache.promote(cacheKey)
	return record, true
}

// GetKnown returns a detached retained record that may need revalidation.
func (cache *Cache) GetKnown(key Key, now time.Time) (Record, bool) {
	cacheKey, _, record, ok := cache.peek(key)
	if !ok || !record.Retained(now) {
		return Record{}, false
	}
	cache.promote(cacheKey)
	return record, true
}

func (cache *Cache) peek(key Key) (string, *cacheEntry, Record, bool) {
	if cache == nil || key.Validate() != nil {
		return "", nil, Record{}, false
	}
	cacheKey := key.cacheKey()
	entry, ok := cache.records.Peek(cacheKey)
	if !ok {
		return "", nil, Record{}, false
	}
	return cacheKey, entry, entry.snapshot(), true
}

func (cache *Cache) promote(cacheKey string) {
	_, _ = cache.records.Get(cacheKey)
}

// Touch extends one retained entry's access window without rewriting its media
// payload or rebuilding the cache map.
func (cache *Cache) Touch(key Key, accessedAt, retainUntil time.Time) (Record, bool) {
	if cache == nil || key.Validate() != nil || accessedAt.IsZero() || !retainUntil.After(accessedAt) {
		return Record{}, false
	}
	cacheKey, entry, record, ok := cache.peek(key)
	if !ok || !record.Retained(accessedAt) {
		return Record{}, false
	}
	current, ok := cache.records.Get(cacheKey)
	if !ok {
		return Record{}, false
	}
	if current != entry {
		entry = current
		if !entry.snapshot().Retained(accessedAt) {
			return Record{}, false
		}
	}
	storeLater(&entry.lastAccess, accessedAt.UnixNano())
	storeLater(&entry.retainUntil, retainUntil.UnixNano())
	return entry.snapshot(), true
}

func (cache *Cache) reserveTouch(key Key, accessedAt time.Time, interval time.Duration) (touchReservation, bool) {
	if cache == nil || key.Validate() != nil || accessedAt.IsZero() || interval <= 0 {
		return touchReservation{}, false
	}
	entry, ok := cache.records.Peek(key.cacheKey())
	if !ok {
		return touchReservation{}, false
	}
	scheduled := accessedAt.UnixNano()
	for {
		previous := entry.lastTouchScheduled.Load()
		if scheduled <= previous || scheduled-previous < interval.Nanoseconds() {
			return touchReservation{}, false
		}
		if entry.lastTouchScheduled.CompareAndSwap(previous, scheduled) {
			return touchReservation{entry: entry, previous: previous, scheduled: scheduled}, true
		}
	}
}

func (reservation touchReservation) release() {
	if reservation.entry != nil {
		reservation.entry.lastTouchScheduled.CompareAndSwap(reservation.scheduled, reservation.previous)
	}
}

// Put publishes one complete retained record. The least recently accessed
// entry is evicted when the configured capacity is exceeded.
func (cache *Cache) Put(record Record, now time.Time) bool {
	if cache == nil || !record.Retained(now) {
		return false
	}
	cache.records.Add(record.Key.cacheKey(), newCacheEntry(record))
	return true
}

func newCacheEntry(record Record) *cacheEntry {
	entry := &cacheEntry{record: cloneRecord(record)}
	entry.lastAccess.Store(record.LastAccessedAt.UnixNano())
	entry.retainUntil.Store(record.RetainUntil.UnixNano())
	entry.lastTouchScheduled.Store(record.LastAccessedAt.UnixNano())
	return entry
}

func (entry *cacheEntry) snapshot() Record {
	record := cloneRecord(entry.record)
	record.LastAccessedAt = time.Unix(0, entry.lastAccess.Load()).UTC()
	record.RetainUntil = time.Unix(0, entry.retainUntil.Load()).UTC()
	return record
}

func storeLater(target *atomic.Int64, value int64) {
	for {
		current := target.Load()
		if value <= current || target.CompareAndSwap(current, value) {
			return
		}
	}
}

// Len returns the number of records currently held in L1.
func (cache *Cache) Len() int {
	if cache == nil {
		return 0
	}
	return cache.records.Len()
}

// Purge removes every in-memory record. Durable records are managed by the
// store so maintenance can reset both cache tiers under one service boundary.
func (cache *Cache) Purge() {
	if cache != nil {
		cache.records.Purge()
	}
}

func cloneRecord(record Record) Record {
	cloned := record
	cloned.Media.Streams = append([]Stream(nil), record.Media.Streams...)
	for index := range cloned.Media.Streams {
		if record.Media.Streams[index].DolbyVision != nil {
			dolbyVision := *record.Media.Streams[index].DolbyVision
			cloned.Media.Streams[index].DolbyVision = &dolbyVision
		}
		if record.Media.Streams[index].MasteringDisplay != nil {
			masteringDisplay := *record.Media.Streams[index].MasteringDisplay
			cloned.Media.Streams[index].MasteringDisplay = &masteringDisplay
		}
		if record.Media.Streams[index].ContentLightLevel != nil {
			contentLightLevel := *record.Media.Streams[index].ContentLightLevel
			cloned.Media.Streams[index].ContentLightLevel = &contentLightLevel
		}
	}
	return cloned
}
