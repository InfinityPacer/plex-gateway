package playback

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/partcache"
	"github.com/InfinityPacer/plex-gateway/internal/pathmap"
	"github.com/InfinityPacer/plex-gateway/internal/plexmeta"
	"github.com/InfinityPacer/plex-gateway/internal/resolver"
)

func TestServicePreparationOwnsCloudClassificationAndTargetRead(t *testing.T) {
	cache := partcache.New(time.Hour)
	localRoot := t.TempDir()
	mapper, err := pathmap.New([]pathmap.Mapping{{PlexPrefix: "/media/cloud", LocalPrefix: localRoot}})
	if err != nil {
		t.Fatal(err)
	}
	control := &stubControlResolver{target: "http://mediavault.invalid/redirect/pick"}
	service := New(Options{
		Cache:         cache,
		Mapper:        mapper,
		Resolver:      control,
		AuthorizePart: func(*http.Request, string, string) (bool, error) { return true, nil },
	})

	if got := service.PrepareCached("missing"); got.State != PreparationMissing || got.Reason != "cache_miss" {
		t.Fatalf("missing preparation = %#v", got)
	}
	cache.Put(partcache.PartInfo{PartID: "local", PlexFilePath: "/media/local/Movie.mkv"})
	if got := service.PrepareCached("local"); got.State != PreparationLocal {
		t.Fatalf("local preparation = %#v", got)
	}
	cache.Put(partcache.PartInfo{
		PartID:       "cloud",
		RatingKey:    "42",
		PartKey:      "/library/parts/cloud/1/file",
		PlexFilePath: "/media/cloud/Movie.strm",
	})
	got := service.PrepareCached("cloud")
	if got.State != PreparationReady || got.Part.Target != control.target || got.Part.RatingKey != "42" {
		t.Fatalf("cloud preparation = %#v", got)
	}
	if control.readPath != filepath.Join(localRoot, "Movie.strm") {
		t.Fatalf("STRM path = %q", control.readPath)
	}
	if got := service.Prepare(plexmeta.Part{ID: "unmapped", File: "/other/Movie.strm"}); got.State != PreparationFailed || got.Reason != "path_mapping" {
		t.Fatalf("unmapped preparation = %#v", got)
	}
	if got := New(Options{}).PrepareCached("cloud"); got.State != PreparationUnavailable {
		t.Fatalf("disabled preparation = %#v", got)
	}
}

func TestServicePlayPreservesClientContextAndRefreshesCache(t *testing.T) {
	cache := partcache.New(time.Hour)
	control := &stubControlResolver{
		target:    "http://mediavault.invalid/redirect/pick",
		directURL: resolver.DirectURL{URL: "https://cdn.invalid/Movie.mkv?signature=private"},
	}
	part := PreparedPart{
		Part:   plexmeta.Part{ID: "21", Key: "/library/parts/21/1/file", File: "/media/cloud/Movie.strm"},
		Target: control.target,
	}
	service := New(Options{
		Cache:    cache,
		Mapper:   mustMapper(t),
		Resolver: control,
		AuthorizePart: func(_ *http.Request, reference, target string) (bool, error) {
			if reference != part.Part.Key || target != part.Target {
				t.Fatalf("authorization reference=%q target=%q", reference, target)
			}
			return true, nil
		},
	})
	request := httptest.NewRequest(http.MethodHead,
		"/video/:/transcode/universal/start.mpd?X-Plex-Token=query-token&X-Plex-Session-Id=session&Accept-Language=zh-CN", nil)
	request.Header.Set("X-Plex-Token", "header-token")
	request.Header.Set("Authorization", "Bearer client-credential")
	request.Header.Set("Cookie", "plex=session")
	request.Header.Set("Range", "bytes=100-200")

	result, err := service.Play(PlayInput{
		Request:                 request,
		Part:                    part,
		PartReference:           part.Part.Key,
		RefreshCacheOnAuthorize: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.DirectURL != control.directURL {
		t.Fatalf("play result = %#v", result)
	}
	if control.playback.Method != http.MethodGet || control.playback.Header.Get("X-Plex-Token") != "header-token" ||
		control.playback.Header.Get("X-Plex-Session-Id") != "session" ||
		control.playback.Header.Get("Accept-Language") != "zh-CN" ||
		control.playback.Header.Get("Authorization") != "Bearer client-credential" ||
		control.playback.Header.Get("Cookie") != "plex=session" ||
		control.playback.Header.Get("Range") != "bytes=100-200" {
		t.Fatalf("resolver request = %#v", control.playback)
	}
	if cached, ok := cache.Get(part.Part.ID); !ok || cached.PartKey != part.Part.Key || cached.RatingKey != part.RatingKey {
		t.Fatalf("refreshed cache = %#v, found = %v", cached, ok)
	}
}

func TestServicePlayReturnsTypedFailures(t *testing.T) {
	part := PreparedPart{
		Part:   plexmeta.Part{ID: "21", Key: "/library/parts/21/1/file", File: "/media/cloud/Movie.strm"},
		Target: "http://mediavault.invalid/redirect/pick",
	}
	request := httptest.NewRequest(http.MethodGet, "/library/parts/21/1/file", nil)

	authorizationErr := errors.New("Plex unavailable")
	service := New(Options{
		Cache:    partcache.New(time.Hour),
		Mapper:   mustMapper(t),
		Resolver: &stubControlResolver{},
		AuthorizePart: func(*http.Request, string, string) (bool, error) {
			return false, authorizationErr
		},
	})
	_, err := service.Play(PlayInput{Request: request, Part: part, PartReference: part.Part.Key})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Kind != FailureAuthorization || !errors.Is(err, authorizationErr) {
		t.Fatalf("authorization error = %#v", err)
	}

	service.authorizePart = func(*http.Request, string, string) (bool, error) { return true, nil }
	service.resolver = &stubControlResolver{resolveErr: context.DeadlineExceeded}
	result, err := service.Play(PlayInput{Request: request, Part: part, PartReference: part.Part.Key})
	if !errors.As(err, &failure) || failure.Kind != FailureResolver ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("resolver error = %#v, result = %#v", err, result)
	}

	service.resolver = &stubControlResolver{}
	_, err = service.Play(PlayInput{Request: request, Part: part, PartReference: part.Part.Key})
	if !errors.As(err, &failure) || failure.Kind != FailureEmptyLocation {
		t.Fatalf("empty location error = %#v", err)
	}
}

type stubControlResolver struct {
	target     string
	readPath   string
	directURL  resolver.DirectURL
	resolveErr error
	playback   resolver.PlaybackRequest
}

func (r *stubControlResolver) ReadTarget(path string) (string, error) {
	r.readPath = path
	return r.target, nil
}

func (r *stubControlResolver) ResolveTarget(_ context.Context, _ string, playback resolver.PlaybackRequest) (resolver.DirectURL, error) {
	r.playback = playback
	return r.directURL, r.resolveErr
}

func mustMapper(t *testing.T) *pathmap.Mapper {
	t.Helper()
	mapper, err := pathmap.New([]pathmap.Mapping{{PlexPrefix: "/media/cloud", LocalPrefix: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	return mapper
}
