package plexmeta

import "testing"

func TestIsDirectPlayDecisionRequiresMatchingPart(t *testing.T) {
	expected := Part{ID: "21", Key: "/library/parts/21/1/file"}
	tests := []struct {
		name        string
		contentType string
		body        string
		want        bool
	}{
		{name: "xml direct play", contentType: "application/xml", body: `<MediaContainer><Video><Media videoDecision="directplay"><Part id="21" key="/library/parts/21/1/file" decision="directplay"/></Media></Video></MediaContainer>`, want: true},
		{name: "json direct play", contentType: "application/json", body: `{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":21,"key":"/library/parts/21/1/file","decision":"directplay"}]}]}]}}`, want: true},
		{name: "transcode", contentType: "application/xml", body: `<MediaContainer><Video><Media videoDecision="transcode"><Part id="21" key="/library/parts/21/1/file" decision="transcode"/></Media></Video></MediaContainer>`},
		{name: "different part", contentType: "application/xml", body: `<MediaContainer><Video><Media><Part id="22" key="/library/parts/22/1/file" decision="directplay"/></Media></Video></MediaContainer>`},
		{name: "conflicting identifier", contentType: "application/xml", body: `<MediaContainer><Video><Media><Part id="21" key="/library/parts/22/1/file" decision="directplay"/></Media></Video></MediaContainer>`},
		{name: "missing identifier", contentType: "application/xml", body: `<MediaContainer><Video><Media><Part decision="directplay"/></Media></Video></MediaContainer>`},
		{name: "truncated", contentType: "application/xml", body: `<MediaContainer><Video><Media><Part id="21" key="/library/parts/21/1/file" decision="directplay"/>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsDirectPlayDecision([]byte(test.body), test.contentType, expected); got != test.want {
				t.Fatalf("IsDirectPlayDecision() = %v, want %v", got, test.want)
			}
		})
	}
}
