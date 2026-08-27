package playback

import (
	"testing"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/plexmeta"
)

func TestGrantStoreIsSessionScopedAndBounded(t *testing.T) {
	store := NewGrantStore(time.Minute, 2)
	part := grantTestPart()
	withoutSession := grantTestAttempt("")
	if store.Put(withoutSession, part) {
		t.Fatal("grant without a session identity was accepted")
	}

	first := grantTestAttempt("first")
	second := grantTestAttempt("second")
	third := grantTestAttempt("third")
	if !store.Put(first, part) || !store.Put(second, part) {
		t.Fatal("valid grants were rejected")
	}
	firstKey, _ := attemptKey(first)
	store.mu.Lock()
	grant := store.entries[firstKey]
	grant.expiresAt = time.Now().Add(time.Second)
	store.entries[firstKey] = grant
	store.mu.Unlock()
	if !store.Put(third, part) {
		t.Fatal("third grant was rejected")
	}
	if len(store.entries) != 2 {
		t.Fatalf("grant count = %d", len(store.entries))
	}
	if _, ok := store.Get(first); ok {
		t.Fatal("oldest grant was not evicted")
	}
	got, ok := store.Get(third)
	if !ok || got.Part != part {
		t.Fatalf("grant = %#v, found = %v", got, ok)
	}
	if _, ok := store.Get(grantTestAttempt("different")); ok {
		t.Fatal("different session reused a grant")
	}
}

func TestGrantStoreRevokesAndExpiresEntries(t *testing.T) {
	store := NewGrantStore(time.Nanosecond, 1)
	attempt := grantTestAttempt("session")
	part := grantTestPart()
	if !store.Put(attempt, part) {
		t.Fatal("grant was rejected")
	}
	time.Sleep(time.Millisecond)
	if _, ok := store.Get(attempt); ok {
		t.Fatal("expired grant remained available")
	}

	store = NewGrantStore(time.Minute, 1)
	if !store.Put(attempt, part) {
		t.Fatal("grant was rejected")
	}
	store.Delete(attempt)
	if _, ok := store.Get(attempt); ok {
		t.Fatal("revoked grant remained available")
	}
}

func grantTestAttempt(session string) Attempt {
	return Attempt{
		MetadataPath: "/library/metadata/42",
		MediaIndex:   0,
		PartIndex:    0,
		Session:      SessionIdentity{Name: "X-Plex-Playback-Session-Id", Value: session},
	}
}

func grantTestPart() PreparedPart {
	return PreparedPart{
		Part: plexmeta.Part{
			ID:   "21",
			Key:  "/library/parts/21/1/file",
			File: "/media/cloud/D.strm",
		},
		Target: "http://mediavault.invalid/redirect/pickcode/D.mkv",
	}
}
