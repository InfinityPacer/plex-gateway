package plexmeta

import (
	"encoding/json"
	"strings"
)

const (
	incompatibleDirectPlayDecisionCode = 1000
	incompatibleTranscodeDecisionCode  = 4005
	incompatibleGeneralDecisionCode    = 2000
)

// IncompatibleDecision builds the minimal Plex decision envelope observed for
// a source that cannot be Direct Played or transcoded. It intentionally omits
// Video, Media, Part, and session identities so a reject cannot expose or start
// another playback path.
func IncompatibleDecision(accept string) (contentType string, body []byte) {
	if strings.Contains(strings.ToLower(accept), "json") {
		body, _ = json.Marshal(map[string]any{
			"MediaContainer": map[string]any{
				"size":                   0,
				"directPlayDecisionCode": incompatibleDirectPlayDecisionCode,
				"transcodeDecisionCode":  incompatibleTranscodeDecisionCode,
				"generalDecisionCode":    incompatibleGeneralDecisionCode,
			},
		})
		return "application/json; charset=utf-8", body
	}
	return "application/xml; charset=utf-8", []byte(
		`<?xml version="1.0" encoding="UTF-8"?>` +
			`<MediaContainer size="0" directPlayDecisionCode="1000" transcodeDecisionCode="4005" generalDecisionCode="2000"></MediaContainer>`,
	)
}
