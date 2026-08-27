package trace

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Tracer emits request-order evidence without recording authentication tokens,
// query values, cookies, remote addresses, or response bodies.
type Tracer struct {
	enabled bool
	logger  *slog.Logger
	serial  atomic.Uint64
}

func New(enabled bool, logger *slog.Logger) *Tracer {
	return &Tracer{enabled: enabled, logger: logger}
}

// Middleware records a bounded, privacy-preserving summary after each request.
func (t *Tracer) Middleware(next http.Handler) http.Handler {
	if !t.enabled {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		sequence := t.serial.Add(1)
		writer := &responseWriter{ResponseWriter: w}

		next.ServeHTTP(writer, r)

		status := writer.status
		if status == 0 {
			status = http.StatusOK
		}
		t.logger.Info("plex_trace",
			"sequence", sequence,
			"method", r.Method,
			"path", r.URL.EscapedPath(),
			"query_keys", queryKeys(r),
			"plex_product", cleanHeader(r.Header.Get("X-Plex-Product")),
			"plex_platform", cleanHeader(r.Header.Get("X-Plex-Platform")),
			"plex_device", cleanHeader(r.Header.Get("X-Plex-Device")),
			"user_agent", cleanHeader(r.UserAgent()),
			"status", status,
			"response_content_type", cleanHeader(writer.Header().Get("Content-Type")),
			"response_bytes", writer.bytes,
			"latency_ms", time.Since(started).Milliseconds(),
		)
	})
}

func queryKeys(r *http.Request) []string {
	values := r.URL.Query()
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func cleanHeader(value string) string {
	value = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' {
			return ' '
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	if len(value) > 160 {
		return value[:160]
	}
	return value
}

type responseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

// Unwrap lets net/http discover optional capabilities provided by the original
// writer while the trace wrapper records status and byte counts.
func (w *responseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *responseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *responseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hijacker.Hijack()
}

func (w *responseWriter) Push(target string, options *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}

func (w *responseWriter) ReadFrom(src io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if readerFrom, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := readerFrom.ReadFrom(src)
		w.bytes += n
		return n, err
	}
	n, err := io.Copy(struct{ io.Writer }{w}, src)
	return n, err
}
