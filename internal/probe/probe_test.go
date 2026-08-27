package probe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseXMLParts(t *testing.T) {
	body := []byte(`<MediaContainer><Video><Media><Part id="123" key="/library/parts/123/7/file.strm" file="/media/cloud/A.strm" /></Media></Video></MediaContainer>`)
	parts, err := ParseParts(body, "application/xml")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].ID != "123" || parts[0].File != "/media/cloud/A.strm" {
		t.Fatalf("parts = %#v", parts)
	}
}

func TestParseJSONParts(t *testing.T) {
	body := []byte(`{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":456,"key":"/library/parts/456/8/file.strm","file":"/media/cloud/B.strm"}]}]}]}}`)
	parts, err := ParseParts(body, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].ID != "456" {
		t.Fatalf("parts = %#v", parts)
	}
}

func TestCompareUsesPartIDAsStableKey(t *testing.T) {
	baseline := Report{GeneratedAt: time.Unix(1, 0), Parts: []Part{{ID: "1", Key: "/old", File: "/a.strm"}, {ID: "2", Key: "/same", File: "/b.strm"}}}
	current := Report{Parts: []Part{{ID: "1", Key: "/new", File: "/a.strm"}, {ID: "3", Key: "/added", File: "/c.strm"}}}

	comparison := Compare(baseline, current)
	if len(comparison.Changed) != 1 || comparison.Changed[0].ID != "1" {
		t.Fatalf("changed = %#v", comparison.Changed)
	}
	if len(comparison.Removed) != 1 || comparison.Removed[0].ID != "2" {
		t.Fatalf("removed = %#v", comparison.Removed)
	}
	if len(comparison.Added) != 1 || comparison.Added[0].ID != "3" {
		t.Fatalf("added = %#v", comparison.Added)
	}
}

func TestFetchMetadataReportExcludesOriginAndToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "private-token" {
			t.Fatal("probe token was not sent in the request header")
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<MediaContainer><Video><Media><Part id="123" key="/library/parts/123/7/file" file="/private/media/Test.strm" /></Media></Video></MediaContainer>`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "private-token", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	report, err := client.FetchMetadata(context.Background(), "42")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Contains(text, server.URL) || strings.Contains(text, "private-token") || strings.Contains(text, "plex_origin") {
		t.Fatalf("report leaked origin or token: %s", text)
	}
	if !strings.Contains(text, "/private/media/Test.strm") {
		t.Fatalf("report did not retain Part path for local analysis: %s", text)
	}
}
