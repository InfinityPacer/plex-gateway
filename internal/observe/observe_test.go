package observe

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestRoundTripperPreservesXMLBodyAndObservesParts(t *testing.T) {
	raw := []byte(`<MediaContainer><Video><Media><Part id="123" key="/library/parts/123/7/file.strm" file="/media/cloud/A.strm" /></Media></Video></MediaContainer>`)
	var observations []Observation
	transport := NewRoundTripper(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/xml; charset=utf-8"}},
			Body:       io.NopCloser(bytes.NewReader(raw)),
			Request:    request,
		}, nil
	}), Config{
		MetadataPaths: []string{"/library/metadata/*"},
		MaxBodyBytes:  int64(len(raw)),
		OnParts: func(observation Observation) {
			observations = append(observations, observation)
		},
	})

	request := httptest.NewRequest(http.MethodGet, "http://plex.test/library/metadata/42?includeMarkers=1", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, raw) {
		t.Fatalf("body changed: got %q, want %q", body, raw)
	}
	if len(observations) != 1 || len(observations[0].Parts) != 1 {
		t.Fatalf("observations = %#v", observations)
	}
	part := observations[0].Parts[0]
	if part.ID != "123" || part.Key != "/library/parts/123/7/file.strm" || part.File != "/media/cloud/A.strm" {
		t.Fatalf("part = %#v", part)
	}
	if observations[0].Request != request || observations[0].Response != response {
		t.Fatal("observation did not retain request/response identity")
	}
}

func TestRoundTripperPreservesGzipBodyAndObservesParts(t *testing.T) {
	plain := []byte(`{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":456,"key":"/library/parts/456/8/file.strm","file":"/media/cloud/B.strm"}]}]}]}}`)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	raw := compressed.Bytes()
	var observations []Observation
	transport := NewRoundTripper(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type":     []string{"application/json"},
				"Content-Encoding": []string{"gzip"},
			},
			Body:    io.NopCloser(bytes.NewReader(raw)),
			Request: request,
		}, nil
	}), Config{
		MetadataPaths: []string{"/library/metadata/"},
		MaxBodyBytes:  int64(len(plain)),
		OnParts: func(observation Observation) {
			observations = append(observations, observation)
		},
	})

	response, err := transport.RoundTrip(httptest.NewRequest(http.MethodGet, "http://plex.test/library/metadata/42", nil))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if !bytes.Equal(body, raw) {
		t.Fatal("gzip wire body was changed")
	}
	if len(observations) != 1 || len(observations[0].Parts) != 1 || observations[0].Parts[0].ID != "456" {
		t.Fatalf("observations = %#v", observations)
	}
}

func TestRoundTripperFailsOpenWhenBodyExceedsLimit(t *testing.T) {
	raw := []byte(`<MediaContainer><Video><Media><Part id="123" file="/media/cloud/A.strm" /></Media></Video></MediaContainer>`)
	called := false
	transport := NewRoundTripper(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/xml"}},
			Body:       io.NopCloser(bytes.NewReader(raw)),
			Request:    request,
		}, nil
	}), Config{
		MetadataPaths: []string{"/library/metadata/*"},
		MaxBodyBytes:  int64(len(raw) - 1),
		OnParts: func(Observation) {
			called = true
		},
	})

	response, err := transport.RoundTrip(httptest.NewRequest(http.MethodGet, "http://plex.test/library/metadata/42", nil))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if !bytes.Equal(body, raw) {
		t.Fatal("body changed after observation limit was exceeded")
	}
	if called {
		t.Fatal("observer callback ran for an over-limit body")
	}
}

func TestRoundTripperFailsOpenOnParseFailure(t *testing.T) {
	raw := []byte(`{"MediaContainer":`)
	called := false
	transport := NewRoundTripper(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(raw)),
			Request:    request,
		}, nil
	}), Config{
		MetadataPaths: []string{"/library/metadata/*"},
		OnParts: func(Observation) {
			called = true
		},
	})

	response, err := transport.RoundTrip(httptest.NewRequest(http.MethodGet, "http://plex.test/library/metadata/42", nil))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if !bytes.Equal(body, raw) {
		t.Fatal("body changed after parse failure")
	}
	if called {
		t.Fatal("observer callback ran after parse failure")
	}
}

func TestRoundTripperFailsOpenOnEarlyClose(t *testing.T) {
	raw := []byte(`<MediaContainer><Video><Media><Part id="123" file="/media/cloud/A.strm" /></Media></Video></MediaContainer>`)
	called := false
	transport := NewRoundTripper(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/xml"}},
			Body:       io.NopCloser(bytes.NewReader(raw)),
			Request:    request,
		}, nil
	}), Config{
		MetadataPaths: []string{"/library/metadata/*"},
		OnParts: func(Observation) {
			called = true
		},
	})

	response, err := transport.RoundTrip(httptest.NewRequest(http.MethodGet, "http://plex.test/library/metadata/42", nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("observer callback ran after early close")
	}
}

func TestRoundTripperSkipsObservationBeforeWrappingBody(t *testing.T) {
	raw := []byte(`<MediaContainer><Video><Media><Part id="123" file="/media/cloud/A.strm" /></Media></Video></MediaContainer>`)
	called := false
	transport := NewRoundTripper(roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/xml"}},
			Body:       io.NopCloser(bytes.NewReader(raw)),
			Request:    request,
		}, nil
	}), Config{
		MetadataPaths: []string{"/library/metadata/*"},
		SkipRequest:   func(*http.Request) bool { return true },
		OnParts: func(Observation) {
			called = true
		},
	})

	response, err := transport.RoundTrip(httptest.NewRequest(http.MethodGet, "http://plex.test/library/metadata/42", nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, wrapped := response.Body.(*observedBody); wrapped {
		t.Fatal("skipped request body was wrapped for observation")
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if called {
		t.Fatal("observer callback ran for a skipped request")
	}
}

func TestRoundTripperSkipsUnconfiguredOrUnsuccessfulResponses(t *testing.T) {
	for _, test := range []struct {
		name        string
		path        string
		statusCode  int
		contentType string
	}{
		{name: "other path", path: "/library/parts/123/file", statusCode: http.StatusOK, contentType: "application/xml"},
		{name: "error status", path: "/library/metadata/42", statusCode: http.StatusInternalServerError, contentType: "application/xml"},
		{name: "other content", path: "/library/metadata/42", statusCode: http.StatusOK, contentType: "video/mkv"},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			transport := NewRoundTripper(roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: test.statusCode,
					Header:     http.Header{"Content-Type": []string{test.contentType}},
					Body:       io.NopCloser(bytes.NewReader([]byte(`<MediaContainer/>`))),
					Request:    request,
				}, nil
			}), Config{
				MetadataPaths: []string{"/library/metadata/*"},
				OnParts: func(Observation) {
					called = true
				},
			})
			response, err := transport.RoundTrip(httptest.NewRequest(http.MethodGet, "http://plex.test"+test.path, nil))
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if called {
				t.Fatal("observer callback ran for an ineligible response")
			}
		})
	}
}
