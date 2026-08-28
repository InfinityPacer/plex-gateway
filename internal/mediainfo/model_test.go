package mediainfo

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFingerprintSTRMIsStableAndContentSensitive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "movie.strm")
	if err := os.WriteFile(path, []byte("https://example.test/redirect/abc/movie.mkv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := FingerprintSTRM(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := FingerprintSTRM(path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 64 {
		t.Fatalf("fingerprints = %q, %q", first, second)
	}
	if err := os.WriteFile(path, []byte("https://example.test/redirect/def/movie.mkv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := FingerprintSTRM(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("fingerprint did not change with STRM content")
	}
}

func TestFingerprintSTRMNormalizesFormattingAndMediaVaultOrigin(t *testing.T) {
	directory := t.TempDir()
	firstPath := filepath.Join(directory, "first.strm")
	secondPath := filepath.Join(directory, "second.strm")
	if err := os.WriteFile(firstPath, []byte("\r\nhttps://old.example.test/redirect/pick/movie.mkv?path=%2Fa&pickcode=abc\r\nignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, []byte("https://new.example.test/redirect/pick/movie.mkv?pickcode=abc&path=%2Fa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := FingerprintSTRM(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := FingerprintSTRM(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("normalized fingerprints differ: %q != %q", first, second)
	}
}

func TestFingerprintSTRMTargetMatchesFileFingerprint(t *testing.T) {
	target := "http://mediavault:7811/redirect/example/file.mkv"
	path := filepath.Join(t.TempDir(), "sample.strm")
	if err := os.WriteFile(path, []byte("\n"+target+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile, err := FingerprintSTRM(path)
	if err != nil {
		t.Fatal(err)
	}
	fromTarget, err := FingerprintSTRMTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	if fromTarget != fromFile {
		t.Fatalf("target fingerprint = %q, file fingerprint = %q", fromTarget, fromFile)
	}
}

func TestCacheReturnsDetachedFreshRecord(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	record := completeRecord(now)
	cache := NewCache([]Record{record}, now)

	got, ok := cache.Get(record.Key, now)
	if !ok {
		t.Fatal("cache miss")
	}
	got.Media.Streams[0].Codec = "changed"
	got.Media.Streams[0].DolbyVision.Profile = 5
	got.Media.Streams[0].MasteringDisplay.MaxLuminance = "changed"
	got.Media.Streams[0].ContentLightLevel.MaxContent = 1
	again, ok := cache.Get(record.Key, now)
	if !ok || again.Media.Streams[0].Codec != "hevc" ||
		again.Media.Streams[0].DolbyVision.Profile != 8 ||
		again.Media.Streams[0].MasteringDisplay.MaxLuminance != "1000/1" ||
		again.Media.Streams[0].ContentLightLevel.MaxContent != 1000 {
		t.Fatalf("cached record was mutated: %#v", again)
	}
	if _, ok := cache.Get(record.Key, record.RetainUntil); ok {
		t.Fatal("expired record returned")
	}
}

func TestCacheBoundsEntriesAndEvictsLeastRecentlyUsed(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	first := completeRecord(now.Add(-2 * time.Hour))
	first.Key.PartID = "first"
	second := completeRecord(now.Add(-time.Hour))
	second.Key.PartID = "second"
	cache := NewCacheWithLimit([]Record{first, second}, now, 2)
	if _, ok := cache.Touch(first.Key, now, now.Add(180*24*time.Hour)); !ok {
		t.Fatal("failed to touch first record")
	}
	third := completeRecord(now)
	third.Key.PartID = "third"
	if !cache.Put(third, now) || cache.Len() != 2 {
		t.Fatalf("cache size = %d", cache.Len())
	}
	if _, ok := cache.GetKnown(second.Key, now); ok {
		t.Fatal("least recently used record was not evicted")
	}
	if _, ok := cache.GetKnown(first.Key, now); !ok {
		t.Fatal("recently touched record was evicted")
	}
}

func TestCacheTouchExtendsRetentionWithoutRefreshingMedia(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	record := completeRecord(now.Add(-179 * 24 * time.Hour))
	record.ExpiresAt = now.Add(-149 * 24 * time.Hour)
	record.RetainUntil = now.Add(24 * time.Hour)
	cache := NewCache([]Record{record}, now)
	touched, ok := cache.Touch(record.Key, now, now.Add(180*24*time.Hour))
	if !ok || !touched.RetainUntil.Equal(now.Add(180*24*time.Hour)) {
		t.Fatalf("Touch() = %#v, %t", touched, ok)
	}
	if touched.Fresh(now) {
		t.Fatal("retention touch incorrectly refreshed probe freshness")
	}
}

func TestCacheDoesNotPromoteUnretainedEntry(t *testing.T) {
	createdAt := time.Unix(1_800_000_000, 0).UTC()
	expired := completeRecord(createdAt)
	expired.Key.PartID = "expired"
	expired.ExpiresAt = createdAt.Add(time.Hour)
	expired.RetainUntil = createdAt.Add(2 * time.Hour)
	retained := completeRecord(createdAt)
	retained.Key.PartID = "retained"
	retained.LastAccessedAt = createdAt.Add(time.Hour)
	cache := NewCacheWithLimit([]Record{expired, retained}, createdAt, 2)

	later := createdAt.Add(3 * time.Hour)
	if _, ok := cache.GetKnown(expired.Key, later); ok {
		t.Fatal("unretained record returned")
	}
	inserted := completeRecord(later)
	inserted.Key.PartID = "inserted"
	if !cache.Put(inserted, later) {
		t.Fatal("cache rejected retained record")
	}
	if _, ok := cache.GetKnown(retained.Key, later); !ok {
		t.Fatal("invalid lookup promoted an unretained entry over a retained entry")
	}
}

func completeRecord(now time.Time) Record {
	return Record{
		Key: Key{
			PlexServerID:    "server-1",
			PartID:          "123",
			STRMFingerprint: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		RatingKey:        "456",
		Provider:         ProviderMediaVaultFFProbe,
		ProviderRevision: ProviderRevisionFFProbeJSONV3,
		SchemaVersion:    SchemaVersion,
		Media: Media{
			Complete:   true,
			Container:  "matroska",
			DurationMS: 90_000,
			Streams: []Stream{{
				Index: 0, Type: "video", Codec: "hevc", Width: 3840, Height: 2160,
				DolbyVision:       &DolbyVision{Profile: 8},
				MasteringDisplay:  &MasteringDisplay{MaxLuminance: "1000/1"},
				ContentLightLevel: &ContentLightLevel{MaxContent: 1000},
			}},
		},
		ProbedAt:       now,
		ExpiresAt:      now.Add(30 * 24 * time.Hour),
		LastAccessedAt: now,
		RetainUntil:    now.Add(180 * 24 * time.Hour),
	}
}
