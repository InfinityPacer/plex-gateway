package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/InfinityPacer/plex-gateway/internal/mediainfo"
)

func TestPlaybackVetoIsAbsentWhenDisabled(t *testing.T) {
	if veto := newPlaybackVeto(false); veto != nil {
		t.Fatal("disabled playback veto is not nil")
	}
}

func TestPlaybackVetoRejectsOnlyExactAppleTVDolbyVisionProfile5(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/decision", nil)
	request.Header.Set("X-Plex-Product", "Plex for Apple TV")
	request.Header.Set("X-Plex-Platform", "tvOS")
	media := mediainfo.Media{
		Complete: true,
		Streams: []mediainfo.Stream{{
			Type: "video", HDRFormat: "dolby_vision",
			DolbyVision: &mediainfo.DolbyVision{Profile: 5, BLCompatID: 0},
		}},
	}
	veto := newPlaybackVeto(true)
	if reason, reject := veto(request, media); !reject || reason != "dolby_vision_profile_5" {
		t.Fatalf("veto() = %q, %v", reason, reject)
	}

	request.Header.Set("X-Plex-Product", "Infuse")
	if reason, reject := veto(request, media); reject || reason != "" {
		t.Fatalf("Infuse veto() = %q, %v", reason, reject)
	}
}

func TestPlaybackVetoAbstainsOnConflictingClientFacts(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/decision?X-Plex-Platform=tvOS", nil)
	request.Header.Set("X-Plex-Product", "Plex for Apple TV")
	request.Header.Set("X-Plex-Platform", "iOS")
	media := mediainfo.Media{
		Complete: true,
		Streams: []mediainfo.Stream{{
			Type: "video", HDRFormat: "dolby_vision",
			DolbyVision: &mediainfo.DolbyVision{Profile: 5, BLCompatID: 0},
		}},
	}
	if reason, reject := newPlaybackVeto(true)(request, media); reject || reason != "" {
		t.Fatalf("veto() = %q, %v", reason, reject)
	}
}
