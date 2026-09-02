package gateway

import (
	"net/http"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/playback"
)

// universalStartHandler converts an eligible STRM start request into the same
// direct CDN redirect used by Part playback. Uncertain requests remain Plex's
// responsibility, including every local-media and genuine transcode request.
type universalStartHandler struct {
	grants   *playback.GrantStore
	playback *playbackHandler
	tickets  *controlTicketStore
}

func (h *universalStartHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	attempt, _, validAttempt := normalizePlaybackAttempt(r)
	grant, granted := h.grants.Get(attempt)
	if !validAttempt || !granted || !hasPlexToken(r) {
		h.playback.plex.ServeHTTP(w, r)
		return
	}
	if acceptsShimWeaveControl(r) {
		err := h.playback.service.Authorize(playback.PlayInput{
			Request: r, Part: grant.Part, PartReference: grant.Part.Part.Key,
			RefreshCacheOnAuthorize: true,
		})
		if err != nil {
			h.playback.fallback(w, r, grant.Part.Part.ID, "shimweave_authorization")
			return
		}
		descriptor, err := h.tickets.Issue(attempt, grant.Part, playback.ResolverRequest(r))
		if err != nil {
			h.playback.fallback(w, r, grant.Part.Part.ID, "shimweave_ticket")
			return
		}
		writeShimWeaveDescriptor(w, descriptor)
		return
	}
	h.playback.servePrepared(w, r, grant.Part, grant.Part.Part.Key, true, started, "universal_start")
}
