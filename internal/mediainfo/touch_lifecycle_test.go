package mediainfo

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

type scriptedTouchStore struct {
	fakeRecordStore
	started chan Key
	results chan error
}

func (store *scriptedTouchStore) Touch(ctx context.Context, key Key, accessedAt, retainUntil time.Time) error {
	if store.started != nil {
		select {
		case store.started <- key:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if store.results != nil {
		select {
		case err := <-store.results:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return store.fakeRecordStore.Touch(ctx, key, accessedAt, retainUntil)
}

type cancelAwareTouchStore struct {
	fakeRecordStore
	started  chan struct{}
	canceled chan struct{}
}

func (store *cancelAwareTouchStore) Touch(ctx context.Context, _ Key, _, _ time.Time) error {
	if store.started != nil {
		select {
		case store.started <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	<-ctx.Done()
	if store.canceled != nil {
		select {
		case store.canceled <- struct{}{}:
		default:
		}
	}
	return ctx.Err()
}

func TestServiceRequeuesTouchAfterStoreError(t *testing.T) {
	const touchInterval = time.Hour
	now := time.Unix(1_800_000_000, 0).UTC()
	request := testRequest("touch-error", PriorityInteractive)
	record := completeRecord(now.Add(-2 * touchInterval))
	record.Key = request.Key
	store := &scriptedTouchStore{
		started: make(chan Key, 2),
		results: make(chan error, 2),
	}
	store.results <- errors.New("touch unavailable")
	store.results <- nil
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache([]Record{record}, now), Store: store,
		Provider: &fakeProvider{prober: &blockingProber{}}, PlexServerID: "server",
		RecordRetention: 24 * time.Hour, TouchInterval: touchInterval,
		Now: func() time.Time { return now },
	})

	if _, ok := service.Get(request.Key); !ok {
		t.Fatal("first cache lookup missed")
	}
	select {
	case got := <-store.started:
		if got != request.Key {
			t.Fatalf("first touch key = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("first touch was not attempted")
	}
	waitForTouchSchedule(t, service.cache, request.Key, record.LastAccessedAt.UnixNano())

	if _, ok := service.Get(request.Key); !ok {
		t.Fatal("second cache lookup missed")
	}
	select {
	case got := <-store.started:
		if got != request.Key {
			t.Fatalf("second touch key = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("touch was not requeued after Store.Touch failed")
	}
}

func TestCacheTouchReservationReleaseDoesNotResetReplacedEntry(t *testing.T) {
	const touchInterval = time.Hour
	now := time.Unix(1_800_000_000, 0).UTC()
	record := completeRecord(now)
	cache := NewCache([]Record{record}, now)
	scheduled := now.Add(touchInterval)
	reservation, ok := cache.reserveTouch(record.Key, scheduled, touchInterval)
	if !ok {
		t.Fatal("failed to reserve original entry")
	}

	replacement := record
	replacement.LastAccessedAt = scheduled
	replacement.RetainUntil = scheduled.Add(180 * 24 * time.Hour)
	if !cache.Put(replacement, scheduled) {
		t.Fatal("failed to replace retained entry")
	}
	reservation.release()

	tooSoon := scheduled.Add(touchInterval - time.Nanosecond)
	if _, ok := cache.reserveTouch(record.Key, tooSoon, touchInterval); ok {
		t.Fatal("old reservation reset the replacement entry")
	}
	due := scheduled.Add(touchInterval)
	newReservation, ok := cache.reserveTouch(record.Key, due, touchInterval)
	if !ok {
		t.Fatal("replacement entry could not reserve its next touch")
	}
	newReservation.release()
}

func TestCacheTouchReservationReleaseAfterEvictionDoesNotResetReinsertedEntry(t *testing.T) {
	const touchInterval = time.Hour
	now := time.Unix(1_800_000_000, 0).UTC()
	record := completeRecord(now)
	cache := NewCacheWithLimit([]Record{record}, now, 1)
	scheduled := now.Add(touchInterval)
	reservation, ok := cache.reserveTouch(record.Key, scheduled, touchInterval)
	if !ok {
		t.Fatal("failed to reserve evicted entry")
	}

	other := completeRecord(scheduled)
	other.Key.PartID = "other"
	if !cache.Put(other, scheduled) {
		t.Fatal("failed to insert eviction candidate")
	}
	if _, ok := cache.GetKnown(record.Key, scheduled); ok {
		t.Fatal("reserved entry was not evicted")
	}

	reinserted := record
	reinserted.LastAccessedAt = scheduled
	reinserted.RetainUntil = scheduled.Add(180 * 24 * time.Hour)
	if !cache.Put(reinserted, scheduled) {
		t.Fatal("failed to reinsert evicted entry")
	}
	reservation.release()

	tooSoon := scheduled.Add(touchInterval - time.Nanosecond)
	if _, ok := cache.reserveTouch(record.Key, tooSoon, touchInterval); ok {
		t.Fatal("old reservation reset the reinserted entry")
	}
	due := scheduled.Add(touchInterval)
	newReservation, ok := cache.reserveTouch(record.Key, due, touchInterval)
	if !ok {
		t.Fatal("reinserted entry could not reserve its next touch")
	}
	newReservation.release()
}

func TestServiceCloseCancelsPendingTouches(t *testing.T) {
	const touchInterval = time.Hour
	now := time.Unix(1_800_000_000, 0).UTC()
	first := completeRecord(now.Add(-2 * touchInterval))
	first.Key.PlexServerID = "server"
	first.Key.PartID = "close-first"
	second := completeRecord(now.Add(-2 * touchInterval))
	second.Key.PlexServerID = "server"
	second.Key.PartID = "close-second"
	store := &cancelAwareTouchStore{
		started:  make(chan struct{}, 2),
		canceled: make(chan struct{}, 1),
	}
	service := newServiceForTest(t, ServiceOptions{
		Cache: NewCache([]Record{first, second}, now), Store: store,
		Provider: &fakeProvider{prober: &blockingProber{}}, PlexServerID: "server",
		RecordRetention: 24 * time.Hour, TouchInterval: touchInterval,
		Now: func() time.Time { return now },
	})
	if _, ok := service.Get(first.Key); !ok {
		t.Fatal("first cache lookup missed")
	}
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("first touch was not started")
	}
	if _, ok := service.Get(second.Key); !ok {
		t.Fatal("second cache lookup missed")
	}
	if got := len(service.touches); got != 1 {
		t.Fatalf("pending touch queue length = %d, want 1", got)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := service.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.canceled:
	case <-time.After(time.Second):
		t.Fatal("Store.Touch did not observe service cancellation")
	}
	waitForTouchSchedule(t, service.cache, first.Key, first.LastAccessedAt.UnixNano())
	waitForTouchSchedule(t, service.cache, second.Key, second.LastAccessedAt.UnixNano())
}

func waitForTouchSchedule(t *testing.T, cache *Cache, key Key, expected int64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		_, entry, _, ok := cache.peek(key)
		if ok && entry.lastTouchScheduled.Load() == expected {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("touch reservation was not released: got entry=%t", ok)
		}
		runtime.Gosched()
	}
}
