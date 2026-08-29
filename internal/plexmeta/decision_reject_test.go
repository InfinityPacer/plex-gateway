package plexmeta

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIncompatibleDecisionBuildsXMLAndJSONEnvelopes(t *testing.T) {
	contentType, body := IncompatibleDecision("application/xml")
	if contentType != "application/xml; charset=utf-8" || !strings.Contains(string(body), `transcodeDecisionCode="4005"`) || strings.Contains(string(body), "<Video") {
		t.Fatalf("XML decision content_type=%q body=%s", contentType, body)
	}

	contentType, body = IncompatibleDecision("application/json")
	if contentType != "application/json; charset=utf-8" {
		t.Fatalf("JSON content type = %q", contentType)
	}
	var value struct {
		MediaContainer struct {
			Size               int `json:"size"`
			DirectPlayDecision int `json:"directPlayDecisionCode"`
			TranscodeDecision  int `json:"transcodeDecisionCode"`
			GeneralDecision    int `json:"generalDecisionCode"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	if value.MediaContainer.Size != 0 || value.MediaContainer.DirectPlayDecision != 1000 || value.MediaContainer.TranscodeDecision != 4005 || value.MediaContainer.GeneralDecision != 2000 {
		t.Fatalf("JSON decision = %#v", value.MediaContainer)
	}
}
