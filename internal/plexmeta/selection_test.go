package plexmeta

import "testing"

func TestSelectPartUsesHierarchicalXMLIndices(t *testing.T) {
	body := []byte(`<MediaContainer><Video><Media><Part id="10" key="/parts/10" file="/local/A.mkv"/><Part id="11" key="/parts/11" file="/local/B.mkv"/></Media><Media><Part id="20" key="/parts/20" file="/cloud/C.strm"/><Part id="21" key="/parts/21" file="/cloud/D.strm"/></Media></Video></MediaContainer>`)

	part, err := SelectPart(body, "application/xml", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if part.ID != "21" || part.Key != "/parts/21" || part.File != "/cloud/D.strm" {
		t.Fatalf("part = %#v", part)
	}
}

func TestSelectPartUsesHierarchicalJSONIndices(t *testing.T) {
	body := []byte(`{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":10,"key":"/parts/10","file":"/local/A.mkv"}]},{"Part":[{"id":20,"key":"/parts/20","file":"/cloud/C.strm"},{"id":21,"key":"/parts/21","file":"/cloud/D.strm"}]}]}]}}`)

	part, err := SelectPart(body, "application/json", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if part.ID != "21" || part.Key != "/parts/21" || part.File != "/cloud/D.strm" {
		t.Fatalf("part = %#v", part)
	}
}

func TestSelectPartRejectsAmbiguousOrInvalidIndices(t *testing.T) {
	body := []byte(`<MediaContainer><Video><Media><Part id="10" file="/local/A.mkv"/></Media></Video></MediaContainer>`)
	for _, test := range []struct {
		name       string
		mediaIndex int
		partIndex  int
	}{
		{name: "automatic media", mediaIndex: -1, partIndex: 0},
		{name: "automatic part", mediaIndex: 0, partIndex: -1},
		{name: "media out of range", mediaIndex: 1, partIndex: 0},
		{name: "part out of range", mediaIndex: 0, partIndex: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := SelectPart(body, "application/xml", test.mediaIndex, test.partIndex); err == nil {
				t.Fatal("SelectPart succeeded")
			}
		})
	}
}

func TestSelectUniquePartRequiresOneMediaAndOnePart(t *testing.T) {
	for _, test := range []struct {
		name        string
		contentType string
		body        string
		wantID      string
	}{
		{name: "unique XML", contentType: "application/xml", body: `<MediaContainer><Video><Media><Part id="10" key="/parts/10" file="/cloud/A.strm"/></Media></Video></MediaContainer>`, wantID: "10"},
		{name: "unique JSON", contentType: "application/json", body: `{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":20,"key":"/parts/20","file":"/cloud/B.strm"}]}]}]}}`, wantID: "20"},
		{name: "multiple media", contentType: "application/xml", body: `<MediaContainer><Video><Media><Part id="10" file="/cloud/A.strm"/></Media><Media><Part id="20" file="/cloud/B.strm"/></Media></Video></MediaContainer>`},
		{name: "multiple parts", contentType: "application/json", body: `{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":10,"file":"/cloud/A.strm"},{"id":20,"file":"/cloud/B.strm"}]}]}]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			part, err := SelectUniquePart([]byte(test.body), test.contentType)
			if test.wantID == "" {
				if err == nil {
					t.Fatalf("SelectUniquePart() = %#v, want error", part)
				}
				return
			}
			if err != nil || part.ID != test.wantID {
				t.Fatalf("SelectUniquePart() = %#v, %v", part, err)
			}
		})
	}
}
