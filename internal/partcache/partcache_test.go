package partcache

import (
	"sync"
	"testing"
	"time"
)

func TestPutGetAndLen(t *testing.T) {
	cache := New(time.Minute)
	want := PartInfo{
		PartID:       "123",
		PlexFilePath: "/media/cloud/Movie.strm",
		PartKey:      "/library/parts/123/7/file",
		UpdatedAt:    time.Unix(100, 0),
	}
	cache.Put(want)

	got, ok := cache.Get("123")
	if !ok {
		t.Fatal("Get() returned a miss")
	}
	if got != want {
		t.Fatalf("Get() = %#v, want %#v", got, want)
	}
	if cache.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", cache.Len())
	}
	if _, ok := cache.Get("missing"); ok {
		t.Fatal("Get(missing) returned a hit")
	}
}

func TestExpiredEntriesAreUnavailableAndRemoved(t *testing.T) {
	cache := New(10 * time.Millisecond)
	cache.Put(PartInfo{PartID: "123"})
	time.Sleep(25 * time.Millisecond)

	if _, ok := cache.Get("123"); ok {
		t.Fatal("Get() returned an expired entry")
	}
	if got := cache.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0", got)
	}
}

func TestPutSweepsExpiredUnrelatedEntries(t *testing.T) {
	cache := New(time.Millisecond)
	cache.Put(PartInfo{PartID: "expired", PlexFilePath: "/cloud/expired.strm"})
	time.Sleep(5 * time.Millisecond)
	cache.Put(PartInfo{PartID: "live", PlexFilePath: "/cloud/live.strm"})

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if _, exists := cache.entries["expired"]; exists {
		t.Fatal("expired entry was retained after opportunistic sweep")
	}
	if _, exists := cache.entries["live"]; !exists {
		t.Fatal("new entry was removed by opportunistic sweep")
	}
}

func TestEmptyPartIDIsIgnored(t *testing.T) {
	cache := New(time.Minute)
	cache.Put(PartInfo{PlexFilePath: "/media/cloud/empty.strm"})
	if got := cache.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0", got)
	}
	if _, ok := cache.Get(""); ok {
		t.Fatal("Get(\"\") returned a hit")
	}
}

func TestConcurrentAccess(t *testing.T) {
	cache := New(time.Minute)
	const workers = 16
	const iterations = 200
	var group sync.WaitGroup
	group.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func(worker int) {
			defer group.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				partID := string(rune('a' + worker%8))
				cache.Put(PartInfo{PartID: partID})
				_, _ = cache.Get(partID)
				_ = cache.Len()
			}
		}(worker)
	}
	group.Wait()
	if got := cache.Len(); got == 0 {
		t.Fatal("Len() = 0 after concurrent Put calls")
	}
}
