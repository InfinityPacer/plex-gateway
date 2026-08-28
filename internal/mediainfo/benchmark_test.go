package mediainfo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func BenchmarkMediaInfoL1Get(b *testing.B) {
	now := time.Now()
	record := completeRecord(now)
	cache := NewCache([]Record{record}, now)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, ok := cache.Get(record.Key, now); !ok {
			b.Fatal("cache miss")
		}
	}
}

func BenchmarkMediaInfoL1PutAtCapacity(b *testing.B) {
	now := time.Now()
	records := make([]Record, defaultCacheEntries)
	for index := range records {
		records[index] = completeRecord(now)
		records[index].Key.PartID = strconv.Itoa(index)
	}
	cache := NewCache(records, now)
	record := completeRecord(now)
	b.ReportAllocs()
	b.ResetTimer()
	for index := range b.N {
		record.Key.PartID = strconv.Itoa(defaultCacheEntries + index)
		if !cache.Put(record, now) {
			b.Fatal("cache rejected retained record")
		}
	}
}

func BenchmarkMediaInfoSQLiteGet(b *testing.B) {
	now := time.Now()
	record := completeRecord(now)
	store, err := OpenSQLite(context.Background(), filepath.Join(b.TempDir(), "mediainfo.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := store.Close(); err != nil {
			b.Error(err)
		}
	})
	if err := store.Put(context.Background(), record); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, ok, err := store.Get(context.Background(), record.Key); err != nil || !ok {
			b.Fatalf("SQLite Get() ok=%t err=%v", ok, err)
		}
	}
}

func BenchmarkMediaInfoHTTPFFProbe(b *testing.B) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		b.Skip("ffmpeg is unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		b.Skip("ffprobe is unavailable")
	}
	directory := b.TempDir()
	mediaPath := filepath.Join(directory, "sample.mkv")
	command := exec.CommandContext(b.Context(), ffmpeg,
		"-v", "error",
		"-f", "lavfi", "-i", "color=c=black:s=320x180:r=24:d=1",
		"-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=48000:duration=1",
		"-c:v", "mpeg4", "-c:a", "aac", "-shortest", mediaPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		b.Fatalf("generate media: %v: %s", err, output)
	}
	server := httptest.NewServer(http.FileServer(http.Dir(directory)))
	b.Cleanup(server.Close)
	prober, err := NewFFProber(FFProbeOptions{
		Binary: ffprobe, Timeout: 5 * time.Second, ProbeSize: 2 << 20,
		AnalyzeDuration: time.Second, OutputLimit: 1 << 20,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		media, err := prober.Probe(b.Context(), server.URL+"/sample.mkv", "Infuse-Library/8.4.4")
		if err != nil {
			b.Fatal(err)
		}
		if !media.Complete {
			b.Fatal("incomplete MediaInfo")
		}
	}
}
