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

func BenchmarkMediaInfoServiceGetMemory(b *testing.B) {
	now := time.Now().UTC()
	record := completeRecord(now)
	service, err := NewService(ServiceOptions{
		Cache: NewCache([]Record{record}, now), Store: &fakeRecordStore{},
		Provider: &fakeProvider{}, PlexServerID: record.Key.PlexServerID,
		BackgroundUserAgent: "Infuse-Library/8.5.1", Now: func() time.Time { return now },
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			b.Error(err)
		}
	})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, ok := service.GetMemory(record.Key); !ok {
			b.Fatal("service L1 miss")
		}
	}
}

func BenchmarkMediaInfoServiceOfferFresh(b *testing.B) {
	now := time.Now().UTC()
	record := completeRecord(now)
	request := Request{
		Key: record.Key, RatingKey: record.RatingKey, Target: "https://control.example.test/fresh",
		Priority: PriorityPlayback, ClientUserAgent: "Infuse-Library/8.5.1",
	}
	service, err := NewService(ServiceOptions{
		Cache: NewCache([]Record{record}, now), Store: &fakeRecordStore{},
		Provider: &fakeProvider{}, PlexServerID: record.Key.PlexServerID,
		BackgroundUserAgent: "Infuse-Library/8.5.1", Now: func() time.Time { return now },
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			b.Error(err)
		}
	})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if result := service.Offer(request); result.Disposition != SubmitFreshCache || result.Err != nil {
			b.Fatalf("Offer() = %#v", result)
		}
	}
}

func BenchmarkMediaInfoServiceOfferJoin(b *testing.B) {
	prober := &blockingProber{started: make(chan string, 1), release: make(chan struct{})}
	service, err := NewService(ServiceOptions{
		Cache: NewCache(nil, time.Now()), Store: &fakeRecordStore{},
		Provider: &fakeProvider{prober: prober}, PlexServerID: "server",
		BackgroundUserAgent: "Infuse-Library/8.5.1", BackgroundInterval: time.Millisecond,
	})
	if err != nil {
		b.Fatal(err)
	}
	request := testRequest("benchmark-join", PriorityPlayback)
	if result := service.Offer(request); result.Err != nil {
		b.Fatal(result.Err)
	}
	<-prober.started
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			b.Error(err)
		}
	})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if result := service.Offer(request); result.Disposition != SubmitJoinedExistingFlight || result.Err != nil {
			b.Fatalf("Offer() = %#v", result)
		}
	}
}

func BenchmarkFingerprintSTRMTarget(b *testing.B) {
	const target = "http://mediavault:7811/redirect/pickcode/movie.mkv"
	b.ReportAllocs()
	for range b.N {
		if _, err := FingerprintSTRMTarget(target); err != nil {
			b.Fatal(err)
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
	store, gatewayDB := openTestSQLiteStore(b, filepath.Join(b.TempDir(), "plex-gateway.db"))
	b.Cleanup(func() {
		if err := gatewayDB.Close(); err != nil {
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
