package plexmeta

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"strings"
	"testing"
)

func TestSplitMetadataBatchJSONKeepsContainerAndOnlySelectedItem(t *testing.T) {
	body := []byte("\ufeff \n{" +
		"\"rootExtra\":\"keep\"," +
		"\"MediaContainer\":{" +
		"\"size\":2,\"containerExtra\":{\"nested\":true}," +
		"\"Metadata\":[" +
		"{\"ratingKey\":\"20\",\"title\":\"second\",\"itemExtra\":\"keep-20\"}," +
		"{\"ratingKey\":10,\"title\":\"first\",\"itemExtra\":\"keep-10\"}" +
		"]}} \n")

	result, err := SplitMetadataBatch(body, "application/json", []string{"10", "20"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 {
		t.Fatalf("result count = %d, want 2", len(result))
	}
	for _, key := range []string{"10", "20"} {
		output := result[key]
		if !bytes.HasPrefix(output, []byte("\ufeff \n")) || !bytes.HasSuffix(output, []byte(" \n")) {
			t.Fatalf("JSON envelope whitespace/BOM was not retained for %s: %q", key, output[:min(len(output), 16)])
		}
		var root map[string]json.RawMessage
		if err := json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(output, []byte{0xef, 0xbb, 0xbf})), &root); err != nil {
			t.Fatalf("decode %s: %v", key, err)
		}
		var container map[string]json.RawMessage
		if err := json.Unmarshal(root["MediaContainer"], &container); err != nil {
			t.Fatal(err)
		}
		var size int
		if err := json.Unmarshal(container["size"], &size); err != nil || size != 1 {
			t.Fatalf("size[%s] = %s, err=%v", key, container["size"], err)
		}
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(container["Metadata"], &items); err != nil || len(items) != 1 {
			t.Fatalf("Metadata[%s] = %s, err=%v", key, container["Metadata"], err)
		}
		var ratingKey string
		if err := json.Unmarshal(items[0]["ratingKey"], &ratingKey); err != nil {
			var numeric json.Number
			if err := json.Unmarshal(items[0]["ratingKey"], &numeric); err != nil {
				t.Fatal(err)
			}
			ratingKey = numeric.String()
		}
		if ratingKey != key {
			t.Fatalf("selected ratingKey = %q, want %q", ratingKey, key)
		}
		if string(container["containerExtra"]) != `{"nested":true}` || string(items[0]["itemExtra"]) != `"keep-`+key+`"` {
			t.Fatalf("unknown fields not preserved in %s: container=%s item=%s", key, container["containerExtra"], items[0]["itemExtra"])
		}
	}
}

func TestSplitMetadataBatchJSONSupportsSingleMetadataObject(t *testing.T) {
	body := []byte(`{"MediaContainer":{"size":9,"Metadata":{"ratingKey":"42","title":"one"}}}`)
	result, err := SplitMetadataBatch(body, "application/json", []string{"42"})
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(result["42"], &root); err != nil {
		t.Fatal(err)
	}
	var container map[string]json.RawMessage
	if err := json.Unmarshal(root["MediaContainer"], &container); err != nil {
		t.Fatal(err)
	}
	if string(container["Metadata"]) != `{"ratingKey":"42","title":"one"}` {
		t.Fatalf("single Metadata object changed shape: %s", container["Metadata"])
	}
}

func TestSplitMetadataBatchXMLKeepsNonItemChildrenAndSelectedItem(t *testing.T) {
	body := []byte("\ufeff  <?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<MediaContainer size=\"2\" containerExtra=\"keep\">\n" +
		"  <Context type=\"collection\"><UnknownField value=\"keep\"/></Context>\n" +
		"  <Video ratingKey=\"20\" title=\"second\" itemExtra=\"keep-20\"/>\n" +
		"  <Video ratingKey=\"10\" title=\"first\" itemExtra=\"keep-10\"/>\n" +
		"</MediaContainer>\n")

	result, err := SplitMetadataBatch(body, "application/xml", []string{"10", "20"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 {
		t.Fatalf("result count = %d, want 2", len(result))
	}
	for _, key := range []string{"10", "20"} {
		output := result[key]
		if !bytes.HasPrefix(output, []byte{0xef, 0xbb, 0xbf}) || !bytes.Contains(output, []byte("Context")) || !bytes.Contains(output, []byte(`containerExtra="keep"`)) {
			t.Fatalf("XML envelope or non-item child not preserved for %s: %s", key, output)
		}
		var root batchXMLNode
		decoder := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(output, []byte{0xef, 0xbb, 0xbf})))
		for {
			token, err := decoder.Token()
			if err != nil {
				t.Fatal(err)
			}
			if start, ok := token.(xml.StartElement); ok {
				if err := decoder.DecodeElement(&root, &start); err != nil {
					t.Fatal(err)
				}
				break
			}
		}
		if got := xmlAttributeValue(root.start.Attr, "size"); got != "1" {
			t.Fatalf("XML size[%s] = %q", key, got)
		}
		var itemCount int
		var selected string
		var nonItemCount int
		for _, child := range root.children {
			if child.kind != batchXMLElement {
				continue
			}
			ratingKey, present, err := xmlBatchRatingKey(child.element.start.Attr)
			if err != nil {
				t.Fatal(err)
			}
			if !present {
				nonItemCount++
				continue
			}
			itemCount++
			selected = ratingKey
		}
		if itemCount != 1 || selected != key || nonItemCount != 1 {
			t.Fatalf("XML children[%s] itemCount=%d selected=%q nonItemCount=%d", key, itemCount, selected, nonItemCount)
		}
	}
}

func TestSplitMetadataBatchRejectsMissingDuplicateAndInvalidShapes(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		keys        []string
	}{
		{
			name:        "JSON missing requested item",
			contentType: "application/json",
			body:        `{"MediaContainer":{"Metadata":[{"ratingKey":"1"}]}}`,
			keys:        []string{"1", "2"},
		},
		{
			name:        "JSON duplicate response item",
			contentType: "application/json",
			body:        `{"MediaContainer":{"Metadata":[{"ratingKey":"1"},{"ratingKey":1}]}}`,
			keys:        []string{"1"},
		},
		{
			name:        "JSON unexpected response item",
			contentType: "application/json",
			body:        `{"MediaContainer":{"Metadata":[{"ratingKey":"1"},{"ratingKey":"2"}]}}`,
			keys:        []string{"1"},
		},
		{
			name:        "JSON duplicate ratingKey field",
			contentType: "application/json",
			body:        `{"MediaContainer":{"Metadata":[{"ratingKey":"1","ratingKey":"1"}]}}`,
			keys:        []string{"1"},
		},
		{
			name:        "JSON item without ratingKey",
			contentType: "application/json",
			body:        `{"MediaContainer":{"Metadata":[{"title":"unknown"}]}}`,
			keys:        []string{"1"},
		},
		{
			name:        "XML missing requested item",
			contentType: "application/xml",
			body:        `<MediaContainer><Video ratingKey="1"/></MediaContainer>`,
			keys:        []string{"1", "2"},
		},
		{
			name:        "XML duplicate response item",
			contentType: "application/xml",
			body:        `<MediaContainer><Video ratingKey="1"/><Video ratingKey="1"/></MediaContainer>`,
			keys:        []string{"1"},
		},
		{
			name:        "XML unexpected response item",
			contentType: "application/xml",
			body:        `<MediaContainer><Video ratingKey="1"/><Video ratingKey="2"/></MediaContainer>`,
			keys:        []string{"1"},
		},
		{
			name:        "XML duplicate ratingKey attributes",
			contentType: "application/xml",
			body:        `<MediaContainer><Video ratingKey="1" ratingKey="2"/></MediaContainer>`,
			keys:        []string{"1"},
		},
		{
			name:        "XML unexpected text",
			contentType: "application/xml",
			body:        `<MediaContainer>unexpected<Video ratingKey="1"/></MediaContainer>`,
			keys:        []string{"1"},
		},
		{
			name:        "malformed XML",
			contentType: "application/xml",
			body:        `<MediaContainer><Video ratingKey="1"></MediaContainer>`,
			keys:        []string{"1"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := SplitMetadataBatch([]byte(test.body), test.contentType, test.keys)
			if err == nil || result != nil {
				t.Fatalf("result=%v err=%v, want an atomic error", result, err)
			}
		})
	}

	for _, keys := range [][]string{nil, {}, {""}, {"1", "1"}, {"1/2"}, {"1,2"}, {" 1"}, {"1\n"}} {
		if result, err := SplitMetadataBatch([]byte(`<MediaContainer><Video ratingKey="1"/></MediaContainer>`), "application/xml", keys); err == nil || result != nil {
			t.Fatalf("keys=%q result=%v err=%v, want invalid-key error", keys, result, err)
		}
	}
}

func TestSplitMetadataBatchIgnoresNonItemXMLChildren(t *testing.T) {
	body := []byte(`<MediaContainer><Context name="preserve"/><Extension value="keep"/><Video ratingKey="1"/></MediaContainer>`)
	result, err := SplitMetadataBatch(body, "application/xml", []string{"1"})
	if err != nil {
		t.Fatalf("non-item child should be retained, err=%v", err)
	}
	if !strings.Contains(string(result["1"]), `Context name="preserve"`) || !strings.Contains(string(result["1"]), `Extension value="keep"`) {
		t.Fatalf("unexpected XML children in output: %s", result["1"])
	}
}

func xmlAttributeValue(attributes []xml.Attr, name string) string {
	for _, attribute := range attributes {
		if attribute.Name.Local == name {
			return attribute.Value
		}
	}
	return ""
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}
