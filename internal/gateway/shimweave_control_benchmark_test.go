package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/playback"
	"github.com/InfinityPacer/plex-gateway/internal/resolver"
)

func BenchmarkControlTicketStore(b *testing.B) {
	store := newControlTicketStore(time.Hour, 24*time.Hour, 1024)
	attempt := playback.Attempt{
		MetadataPath: "/library/metadata/42",
		MediaIndex:   0,
		PartIndex:    0,
		Session: playback.SessionIdentity{
			Name:  "X-Plex-Playback-Session-Id",
			Value: "benchmark-playback",
		},
	}
	request := resolver.PlaybackRequest{
		Method: http.MethodGet,
		Header: http.Header{
			"User-Agent":   {"ShimWeave Benchmark"},
			"X-Plex-Token": {"benchmark-token"},
		},
	}
	descriptor, err := store.Issue(attempt, controlTestPart(), request)
	if err != nil {
		b.Fatal(err)
	}

	b.Run("issue-reuse", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := store.Issue(attempt, controlTestPart(), request); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("lease", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, found := store.Lease(descriptor.ControlToken); !found {
				b.Fatal("control ticket expired during benchmark")
			}
		}
	})

	b.Run("lease-parallel", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(parallel *testing.PB) {
			for parallel.Next() {
				if _, found := store.Lease(descriptor.ControlToken); !found {
					b.Fatal("control ticket expired during benchmark")
				}
			}
		})
	})
}
