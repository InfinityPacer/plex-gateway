package gateway

import (
	"net/http"
	"strings"

	"github.com/InfinityPacer/plex-gateway/internal/mediainfo"
)

type playbackVeto func(*http.Request, mediainfo.Media) (reason string, reject bool)

func newPlaybackVeto(enabled bool) playbackVeto {
	if !enabled {
		return nil
	}
	return vetoAppleTVDolbyVisionProfile5
}

// vetoAppleTVDolbyVisionProfile5 is an optional compatibility override for
// deployments where this exact client and media combination is known to fail.
// Unknown, incomplete, or conflicting facts always preserve Plex's decision.
func vetoAppleTVDolbyVisionProfile5(request *http.Request, media mediainfo.Media) (string, bool) {
	if !media.Complete || request == nil || request.URL == nil {
		return "", false
	}
	product, productOK := plexRequestFact(request, "X-Plex-Product")
	platform, platformOK := plexRequestFact(request, "X-Plex-Platform")
	if !productOK || !platformOK || !strings.EqualFold(product, "Plex for Apple TV") || !strings.EqualFold(platform, "tvOS") {
		return "", false
	}
	var video *mediainfo.Stream
	for index := range media.Streams {
		if !strings.EqualFold(strings.TrimSpace(media.Streams[index].Type), "video") {
			continue
		}
		if video != nil {
			return "", false
		}
		video = &media.Streams[index]
	}
	if video == nil || !strings.EqualFold(strings.TrimSpace(video.HDRFormat), "dolby_vision") || video.DolbyVision == nil {
		return "", false
	}
	if video.DolbyVision.Profile != 5 || video.DolbyVision.BLCompatID != 0 {
		return "", false
	}
	return "dolby_vision_profile_5", true
}

func plexRequestFact(request *http.Request, name string) (string, bool) {
	queryValues, queryPresent := request.URL.Query()[name]
	headerValues := request.Header.Values(name)
	if queryPresent && (len(queryValues) != 1 || strings.TrimSpace(queryValues[0]) == "") {
		return "", false
	}
	if len(headerValues) > 1 || len(headerValues) == 1 && strings.TrimSpace(headerValues[0]) == "" {
		return "", false
	}
	if queryPresent && len(headerValues) == 1 && queryValues[0] != headerValues[0] {
		return "", false
	}
	if queryPresent {
		return strings.TrimSpace(queryValues[0]), true
	}
	if len(headerValues) == 1 {
		return strings.TrimSpace(headerValues[0]), true
	}
	return "", false
}
