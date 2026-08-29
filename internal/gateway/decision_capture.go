package gateway

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// decisionResponseCapture mirrors every response write while retaining a
// bounded wire copy. The copy is eligible for a grant only after Plex has
// completed a successful response and the full body fits within the limit.
type decisionResponseCapture struct {
	http.ResponseWriter
	limit       int64
	status      int
	captured    bytes.Buffer
	overflow    bool
	passthrough bool
	committed   bool
	writeFailed bool
}

func newDecisionResponseCapture(writer http.ResponseWriter, limit int64) *decisionResponseCapture {
	return &decisionResponseCapture{ResponseWriter: writer, limit: limit}
}

func (w *decisionResponseCapture) WriteHeader(status int) {
	if status < http.StatusOK {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status == 0 {
		w.status = status
	}
}

func (w *decisionResponseCapture) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.passthrough {
		written, err := w.ResponseWriter.Write(body)
		if err != nil || written != len(body) {
			w.writeFailed = true
		}
		return written, err
	}
	if int64(w.captured.Len()+len(body)) <= w.limit {
		return w.captured.Write(body)
	}

	w.overflow = true
	w.passthrough = true
	w.commitHeader()
	if w.captured.Len() > 0 {
		if _, err := w.ResponseWriter.Write(w.captured.Bytes()); err != nil {
			w.writeFailed = true
			return 0, err
		}
		w.captured.Reset()
	}
	written, err := w.ResponseWriter.Write(body)
	if err != nil || written != len(body) {
		w.writeFailed = true
	}
	return written, err
}

// Flush intentionally holds bounded decision responses until grant state is
// committed. Once the response exceeds the inspection limit, it becomes an
// ordinary pass-through response and flushes normally without creating a grant.
func (w *decisionResponseCapture) Flush() {
	if !w.passthrough {
		return
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *decisionResponseCapture) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *decisionResponseCapture) successful() bool {
	return w.status >= http.StatusOK && w.status < http.StatusMultipleChoices && !w.overflow && !w.writeFailed
}

// commit makes a bounded response visible after its associated grant decision
// has been finalized. It is idempotent because ReverseProxy error handling may
// attempt more than one terminal write.
func (w *decisionResponseCapture) commit() error {
	if w.committed || w.passthrough {
		return nil
	}
	w.committed = true
	w.commitHeader()
	if w.captured.Len() == 0 {
		return nil
	}
	_, err := w.ResponseWriter.Write(w.captured.Bytes())
	if err != nil {
		w.writeFailed = true
	}
	return err
}

func (w *decisionResponseCapture) commitHeader() {
	if w.committed && w.passthrough {
		return
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *decisionResponseCapture) body() []byte {
	if !strings.EqualFold(strings.TrimSpace(w.Header().Get("Content-Encoding")), "gzip") {
		return w.captured.Bytes()
	}
	reader, err := gzip.NewReader(bytes.NewReader(w.captured.Bytes()))
	if err != nil {
		return nil
	}
	defer reader.Close()
	decoded, err := io.ReadAll(io.LimitReader(reader, w.limit+1))
	if err != nil || int64(len(decoded)) > w.limit {
		return nil
	}
	return decoded
}

// replaceDecodedBody swaps one complete bounded decision response before it is
// exposed to the client. Wire encoding is preserved while stale entity
// validators are removed because the response representation has changed.
func (w *decisionResponseCapture) replaceDecodedBody(body []byte) error {
	if !w.successful() || w.committed || w.passthrough {
		return errors.New("decision response is not replaceable")
	}
	wireBody, err := encodeMetadataBody(body, w.Header().Get("Content-Encoding"))
	if err != nil {
		return err
	}
	if int64(len(wireBody)) > w.limit {
		return errors.New("enriched decision response exceeds the limit")
	}
	w.captured.Reset()
	_, _ = w.captured.Write(wireBody)
	for _, name := range []string{"ETag", "Last-Modified", "Content-MD5", "Digest"} {
		w.Header().Del(name)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(wireBody)))
	return nil
}

var _ interface{ Unwrap() http.ResponseWriter } = (*decisionResponseCapture)(nil)
