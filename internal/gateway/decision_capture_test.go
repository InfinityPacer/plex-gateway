package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

func TestDecisionCaptureReplacesGzipBodyAndEntityHeaders(t *testing.T) {
	destination := httptest.NewRecorder()
	capture := newDecisionResponseCapture(destination, 1<<20)
	capture.Header().Set("Content-Encoding", "gzip")
	capture.Header().Set("Content-Length", "1")
	capture.Header().Set("ETag", `"stale"`)
	capture.WriteHeader(http.StatusOK)

	var original bytes.Buffer
	writer := gzip.NewWriter(&original)
	_, _ = writer.Write([]byte("original"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	_, _ = capture.Write(original.Bytes())

	if err := capture.replaceDecodedBody([]byte("enriched")); err != nil {
		t.Fatal(err)
	}
	if err := capture.commit(); err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(destination.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if string(decoded) != "enriched" || destination.Header().Get("ETag") != "" || destination.Header().Get("Content-Length") != strconv.Itoa(destination.Body.Len()) {
		t.Fatalf("headers=%#v body=%q", destination.Header(), decoded)
	}
}

func TestNormalizePlaybackAttemptUsesStableSessionIdentity(t *testing.T) {
	decision := grantTestRequest("")
	decision.Header.Set("X-Plex-Playback-Session-Id", "header-session")
	setGrantTestQuery(decision, "session", "auxiliary-decision")
	attempt, _, ok := normalizePlaybackAttempt(decision)
	if !ok || !attempt.Correlatable() || attempt.Session.Value != "header-session" {
		t.Fatalf("attempt = %#v, valid = %v", attempt, ok)
	}

	start := grantTestRequest("")
	start.Header.Set("X-Plex-Playback-Session-Id", "header-session")
	setGrantTestQuery(start, "session", "auxiliary-start")
	startAttempt, _, ok := normalizePlaybackAttempt(start)
	if !ok || startAttempt != attempt {
		t.Fatal("stable playback identity did not survive auxiliary session changes")
	}

	conflict := grantTestRequest("query-session")
	conflict.Header.Set("X-Plex-Playback-Session-Id", "header-session")
	if _, _, ok := normalizePlaybackAttempt(conflict); ok {
		t.Fatal("conflicting query and header identity was accepted")
	}

	withoutSession := grantTestRequest("")
	withoutSessionAttempt, _, ok := normalizePlaybackAttempt(withoutSession)
	if !ok || withoutSessionAttempt.Correlatable() {
		t.Fatalf("sessionless attempt = %#v, valid = %v", withoutSessionAttempt, ok)
	}
}

func TestDecisionResponseCaptureDecodesBoundedGzip(t *testing.T) {
	raw := []byte(`<MediaContainer><Video><Media><Part id="21" key="/library/parts/21/1/file" decision="directplay"/></Media></Video></MediaContainer>`)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	capture := newDecisionResponseCapture(recorder, int64(len(raw)))
	capture.Header().Set("Content-Encoding", "gzip")
	if _, err := capture.Write(compressed.Bytes()); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
		t.Fatal("bounded decision response became visible before commit")
	}
	if !capture.successful() || !bytes.Equal(capture.body(), raw) {
		t.Fatalf("successful=%v body=%q", capture.successful(), capture.body())
	}
	if err := capture.commit(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recorder.Body.Bytes(), compressed.Bytes()) {
		t.Fatal("committed response did not preserve the wire body")
	}

	overflow := newDecisionResponseCapture(httptest.NewRecorder(), int64(len(compressed.Bytes())-1))
	if _, err := overflow.Write(compressed.Bytes()); err != nil {
		t.Fatal(err)
	}
	if overflow.successful() {
		t.Fatal("over-limit decision response was accepted")
	}
}

func setGrantTestQuery(request *http.Request, name, value string) {
	query := request.URL.Query()
	query.Set(name, value)
	request.URL.RawQuery = query.Encode()
}

func grantTestRequest(sessionID string) *http.Request {
	query := url.Values{
		"path":       {"/library/metadata/42"},
		"mediaIndex": {"0"},
		"partIndex":  {"0"},
	}
	if sessionID != "" {
		query.Set("X-Plex-Playback-Session-Id", sessionID)
	}
	return httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?"+query.Encode(), nil)
}
