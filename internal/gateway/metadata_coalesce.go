package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/metrics"
	"github.com/InfinityPacer/plex-gateway/internal/plexmeta"
)

const (
	defaultMetadataCoalesceWindow        = 3 * time.Millisecond
	defaultMetadataCoalesceMaxItems      = 32
	defaultMetadataCoalesceTimeout       = 5 * time.Second
	defaultMetadataCoalesceResponseLimit = 8 << 20
	defaultMetadataCoalesceMaxGroups     = 256
	defaultMetadataCoalesceMaxWaiters    = 128
	defaultMetadataCoalesceCooldown      = time.Second
	maximumMetadataCoalesceWindow        = 100 * time.Millisecond
	maximumMetadataCoalesceItems         = 64
)

// MetadataCoalesceOptions controls a bounded aggregation window for equivalent
// single-item metadata reads. Every unsafe or ambiguous response falls back to
// the original requests through the same upstream admission guard.
type MetadataCoalesceOptions struct {
	Enabled    bool
	Window     time.Duration
	MaxItems   int
	Timeout    time.Duration
	OnMetadata func(*http.Request, []byte, string)
}

type metadataCoalescer struct {
	next       http.Handler
	window     time.Duration
	maxItems   int
	timeout    time.Duration
	bodyLimit  int64
	maxGroups  int
	maxWaiters int
	onMetadata func(*http.Request, []byte, string)
	metrics    *metrics.Metrics
	logger     *slog.Logger

	mu     sync.Mutex
	groups map[[sha256.Size]byte]*metadataCoalesceGroup
	failed map[[sha256.Size]byte]time.Time
}

type metadataCoalesceGroup struct {
	key            [sha256.Size]byte
	representative *http.Request
	items          map[string][]*metadataCoalesceCall
	order          []string
	timer          *time.Timer
	waiters        int

	lifecycleMu sync.Mutex
	cancel      context.CancelFunc
}

type metadataCoalesceCall struct {
	writer  http.ResponseWriter
	request *http.Request
	result  chan metadataCoalesceResult

	mu     sync.Mutex
	active bool
	group  *metadataCoalesceGroup
}

type metadataCoalesceResult struct {
	direct    bool
	followers []*metadataCoalesceCall
	headers   http.Header
	status    int
	body      []byte
}

type metadataCoalesceContextKey struct{}

func newMetadataCoalescer(options MetadataCoalesceOptions, next http.Handler, registry *metrics.Metrics, logger *slog.Logger) http.Handler {
	if next == nil || !options.Enabled {
		return next
	}
	window := options.Window
	if window <= 0 {
		window = defaultMetadataCoalesceWindow
	} else if window > maximumMetadataCoalesceWindow {
		window = maximumMetadataCoalesceWindow
	}
	maxItems := options.MaxItems
	if maxItems == 1 {
		return next
	}
	if maxItems <= 0 {
		maxItems = defaultMetadataCoalesceMaxItems
	} else if maxItems > maximumMetadataCoalesceItems {
		maxItems = maximumMetadataCoalesceItems
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultMetadataCoalesceTimeout
	}
	if registry == nil {
		registry = metrics.New()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &metadataCoalescer{
		next: next, window: window, maxItems: maxItems, timeout: timeout,
		bodyLimit: defaultMetadataCoalesceResponseLimit, onMetadata: options.OnMetadata,
		maxGroups: defaultMetadataCoalesceMaxGroups, maxWaiters: defaultMetadataCoalesceMaxWaiters,
		metrics: registry, logger: logger, groups: make(map[[sha256.Size]byte]*metadataCoalesceGroup),
		failed: make(map[[sha256.Size]byte]time.Time),
	}
}

func (coalescer *metadataCoalescer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	ratingKey, eligible := metadataCoalesceRatingKey(request)
	if !eligible {
		coalescer.next.ServeHTTP(writer, request)
		return
	}

	call := &metadataCoalesceCall{
		writer: writer, request: request, result: make(chan metadataCoalesceResult, 1), active: true,
	}
	coalescer.metrics.IncMetadataCoalesceOffered()
	if !coalescer.enqueue(metadataCoalesceSignature(request), ratingKey, call) {
		coalescer.next.ServeHTTP(writer, request)
		return
	}

	var result metadataCoalesceResult
	select {
	case result = <-call.result:
	case <-request.Context().Done():
		if call.cancel() {
			return
		}
		result = <-call.result
	}
	coalescer.serveResult(call, result)
}

func (coalescer *metadataCoalescer) enqueue(key [sha256.Size]byte, ratingKey string, call *metadataCoalesceCall) bool {
	coalescer.mu.Lock()
	if deadline, blocked := coalescer.failed[key]; blocked {
		if time.Now().Before(deadline) {
			coalescer.mu.Unlock()
			return false
		}
		delete(coalescer.failed, key)
	}
	group := coalescer.groups[key]
	if group == nil {
		if len(coalescer.groups) >= coalescer.maxGroups {
			coalescer.mu.Unlock()
			return false
		}
		group = &metadataCoalesceGroup{
			key:            key,
			representative: call.request,
			items:          make(map[string][]*metadataCoalesceCall),
		}
		coalescer.groups[key] = group
		group.timer = time.AfterFunc(coalescer.window, func() {
			coalescer.flush(key, group)
		})
	}
	if group.waiters >= coalescer.maxWaiters {
		coalescer.mu.Unlock()
		return false
	}
	call.group = group
	group.waiters++
	if _, exists := group.items[ratingKey]; !exists {
		group.order = append(group.order, ratingKey)
	}
	group.items[ratingKey] = append(group.items[ratingKey], call)
	full := len(group.order) >= coalescer.maxItems
	if full {
		delete(coalescer.groups, key)
		group.timer.Stop()
	}
	coalescer.mu.Unlock()

	if full {
		go coalescer.execute(group)
	}
	return true
}

func (coalescer *metadataCoalescer) flush(key [sha256.Size]byte, group *metadataCoalesceGroup) {
	coalescer.mu.Lock()
	if coalescer.groups[key] != group {
		coalescer.mu.Unlock()
		return
	}
	delete(coalescer.groups, key)
	coalescer.mu.Unlock()
	coalescer.execute(group)
}

func (coalescer *metadataCoalescer) execute(group *metadataCoalesceGroup) {
	order, items, representative, liveCalls := liveMetadataCoalesceItems(group)
	if liveCalls == 0 {
		return
	}
	if liveCalls == 1 {
		for _, ratingKey := range order {
			items[ratingKey][0].dispatch(metadataCoalesceResult{direct: true})
			return
		}
	}

	baseContext := context.WithoutCancel(representative.Context())
	ctx, cancel := context.WithTimeout(baseContext, coalescer.timeout)
	ctx = context.WithValue(ctx, metadataCoalesceContextKey{}, true)
	group.installCancel(cancel)
	defer group.clearCancel()
	defer cancel()
	if !group.anyActive() {
		return
	}

	coalescer.metrics.IncMetadataCoalesceBatch(len(order))
	coalescer.metrics.IncMetadataCoalesceActive()
	responses, headers, status, err := coalescer.requestBatch(ctx, representative, order)
	coalescer.metrics.DecMetadataCoalesceActive()
	if err != nil {
		if !group.anyActive() {
			return
		}
		coalescer.metrics.IncMetadataCoalesceFallback()
		coalescer.markFailed(group.key)
		coalescer.logger.Warn("metadata_coalesce_fallback", "reason", metadataCoalesceErrorKind(err))
		coalescer.fallback(order, items)
		return
	}

	wireBodies := make(map[string][]byte, len(order))
	responseHeaders := make(map[string]http.Header, len(order))
	for _, ratingKey := range order {
		decoded := responses[ratingKey]
		wireBody, encodeErr := encodeMetadataBody(decoded, headers.Get("Content-Encoding"))
		if encodeErr != nil {
			coalescer.metrics.IncMetadataCoalesceFallback()
			coalescer.markFailed(group.key)
			coalescer.logger.Warn("metadata_coalesce_fallback", "reason", "encode")
			coalescer.fallback(order, items)
			return
		}
		wireBodies[ratingKey] = wireBody
		responseHeaders[ratingKey] = metadataCoalesceResponseHeaders(headers, len(wireBody))
	}
	for _, ratingKey := range order {
		decoded := responses[ratingKey]
		if coalescer.onMetadata != nil {
			coalescer.observe(items[ratingKey][0].request, decoded, responseHeaders[ratingKey].Get("Content-Type"))
		}
		for _, call := range items[ratingKey] {
			call.dispatch(metadataCoalesceResult{
				headers: responseHeaders[ratingKey], status: status, body: wireBodies[ratingKey],
			})
		}
	}
}

func liveMetadataCoalesceItems(group *metadataCoalesceGroup) ([]string, map[string][]*metadataCoalesceCall, *http.Request, int) {
	order := make([]string, 0, len(group.order))
	items := make(map[string][]*metadataCoalesceCall, len(group.items))
	var representative *http.Request
	liveCalls := 0
	for _, ratingKey := range group.order {
		for _, call := range group.items[ratingKey] {
			if !call.isActive() {
				continue
			}
			if representative == nil {
				representative = call.request
			}
			items[ratingKey] = append(items[ratingKey], call)
			liveCalls++
		}
		if len(items[ratingKey]) != 0 {
			order = append(order, ratingKey)
		}
	}
	return order, items, representative, liveCalls
}

func (coalescer *metadataCoalescer) requestBatch(
	ctx context.Context,
	request *http.Request,
	ratingKeys []string,
) (responses map[string][]byte, headers http.Header, status int, err error) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		if recovered == http.ErrAbortHandler {
			responses, headers, status = nil, nil, 0
			err = errors.New("metadata coalesce upstream body aborted")
			return
		}
		panic(recovered)
	}()
	if request == nil || len(ratingKeys) == 0 {
		return nil, nil, 0, errors.New("metadata coalesce request is empty")
	}
	batchRequest := request.Clone(ctx)
	batchRequest.URL.Path = "/library/metadata/" + strings.Join(ratingKeys, ",")
	batchRequest.URL.RawPath = ""
	batchRequest.RequestURI = batchRequest.URL.RequestURI()

	capture := newMetadataCoalesceCapture(coalescer.bodyLimit)
	coalescer.next.ServeHTTP(capture, batchRequest)
	if err := ctx.Err(); err != nil {
		return nil, nil, 0, err
	}
	if capture.overflow {
		return nil, nil, 0, errors.New("metadata coalesce response exceeds limit")
	}
	if capture.statusCode() != http.StatusOK || capture.header.Get("Trailer") != "" || capture.header.Get("Content-Range") != "" {
		return nil, nil, 0, fmt.Errorf("metadata coalesce upstream status %d", capture.statusCode())
	}
	decoded, encoding, err := decodeMetadataBody(capture.body.Bytes(), capture.header.Get("Content-Encoding"), coalescer.bodyLimit)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("decode metadata coalesce response: %w", err)
	}
	responses, err = plexmeta.SplitMetadataBatch(decoded, capture.header.Get("Content-Type"), ratingKeys)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("split metadata coalesce response: %w", err)
	}
	headers = capture.header.Clone()
	headers.Set("Content-Encoding", encoding)
	if encoding == "" {
		headers.Del("Content-Encoding")
	}
	return responses, headers, capture.statusCode(), nil
}

func (coalescer *metadataCoalescer) fallback(order []string, items map[string][]*metadataCoalesceCall) {
	for _, ratingKey := range order {
		calls := items[ratingKey]
		for index, leader := range calls {
			followers := make([]*metadataCoalesceCall, 0, len(calls)-1)
			followers = append(followers, calls[:index]...)
			followers = append(followers, calls[index+1:]...)
			if leader.dispatch(metadataCoalesceResult{direct: true, followers: followers}) {
				break
			}
		}
	}
}

func (coalescer *metadataCoalescer) markFailed(key [sha256.Size]byte) {
	now := time.Now()
	coalescer.mu.Lock()
	if len(coalescer.failed) >= coalescer.maxGroups {
		for candidate, deadline := range coalescer.failed {
			if !now.Before(deadline) {
				delete(coalescer.failed, candidate)
			}
		}
	}
	if len(coalescer.failed) < coalescer.maxGroups {
		coalescer.failed[key] = now.Add(defaultMetadataCoalesceCooldown)
	}
	coalescer.mu.Unlock()
}

func (coalescer *metadataCoalescer) observe(request *http.Request, body []byte, contentType string) {
	defer func() { _ = recover() }()
	coalescer.onMetadata(request, body, contentType)
}

func metadataCoalesceRatingKey(request *http.Request) (string, bool) {
	if request == nil || request.URL == nil || request.Method != http.MethodGet || request.ContentLength > 0 ||
		request.URL.RawPath != "" || request.Body != nil && request.Body != http.NoBody || len(request.TransferEncoding) != 0 {
		return "", false
	}
	if classifyMetadataRequest(request) != metadataRequestSingle {
		return "", false
	}
	for _, header := range []string{"Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range"} {
		if request.Header.Get(header) != "" {
			return "", false
		}
	}
	_, tokenPresent, tokenValid := requestIdentity(request, "X-Plex-Token")
	if !tokenPresent || !tokenValid {
		return "", false
	}
	return strings.TrimPrefix(request.URL.Path, "/library/metadata/"), true
}

func metadataCoalesceSignature(request *http.Request) [sha256.Size]byte {
	digest := sha256.New()
	writeMetadataCoalesceValue(digest, request.Method)
	writeMetadataCoalesceValue(digest, request.Proto)
	writeMetadataCoalesceValue(digest, request.Host)
	writeMetadataCoalesceValue(digest, request.URL.RawQuery)
	writeMetadataCoalesceValue(digest, metadataCoalesceRemoteIP(request.RemoteAddr))
	headerNames := make([]string, 0, len(request.Header))
	for name := range request.Header {
		headerNames = append(headerNames, http.CanonicalHeaderKey(name))
	}
	sort.Strings(headerNames)
	for _, name := range headerNames {
		writeMetadataCoalesceValue(digest, name)
		for _, value := range request.Header.Values(name) {
			writeMetadataCoalesceValue(digest, value)
		}
	}
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result
}

func writeMetadataCoalesceValue(destination hash.Hash, value string) {
	_, _ = destination.Write([]byte(strconv.Itoa(len(value))))
	_, _ = destination.Write([]byte{':'})
	_, _ = destination.Write([]byte(value))
}

func metadataCoalesceRemoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}

func metadataCoalesceResponseHeaders(source http.Header, contentLength int) http.Header {
	headers := source.Clone()
	for _, name := range []string{"Content-Length", "ETag", "Last-Modified", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest", "Content-Range", "Trailer"} {
		headers.Del(name)
	}
	if !hasCacheDirective(headers.Get("Cache-Control"), "no-store") {
		headers.Set("Cache-Control", "private, no-cache")
	}
	headers.Set("Content-Length", strconv.Itoa(contentLength))
	return headers
}

func metadataCoalesceErrorKind(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "status"):
		return "status"
	case strings.Contains(message, "exceeds"):
		return "limit"
	case strings.Contains(message, "decode"):
		return "decode"
	case strings.Contains(message, "split"):
		return "split"
	default:
		return "upstream"
	}
}

func isMetadataCoalesceUpstreamRequest(request *http.Request) bool {
	return request != nil && request.Context().Value(metadataCoalesceContextKey{}) == true
}

func (call *metadataCoalesceCall) isActive() bool {
	call.mu.Lock()
	defer call.mu.Unlock()
	return call.active
}

func (call *metadataCoalesceCall) cancel() bool {
	call.mu.Lock()
	if !call.active {
		call.mu.Unlock()
		return false
	}
	call.active = false
	group := call.group
	call.mu.Unlock()
	if group != nil {
		group.cancelIfIdle()
	}
	return true
}

func (call *metadataCoalesceCall) dispatch(result metadataCoalesceResult) bool {
	call.mu.Lock()
	defer call.mu.Unlock()
	if !call.active {
		return false
	}
	call.active = false
	call.result <- result
	return true
}

func (coalescer *metadataCoalescer) serveResult(call *metadataCoalesceCall, result metadataCoalesceResult) {
	if !result.direct {
		writeCapturedResponse(call.writer, result.headers, result.status, result.body)
		return
	}
	if len(result.followers) == 0 {
		coalescer.next.ServeHTTP(call.writer, call.request)
		return
	}

	capture := newMetadataCoalesceTee(call.writer, coalescer.bodyLimit)
	panicked := true
	defer func() {
		if panicked || !capture.reusable() {
			coalescer.dispatchFallbackError(result.followers)
			return
		}
		response := metadataCoalesceResult{
			headers: capture.Header().Clone(), status: capture.statusCode(), body: capture.body.Bytes(),
		}
		for _, follower := range result.followers {
			follower.dispatch(response)
		}
	}()
	coalescer.next.ServeHTTP(capture, call.request)
	panicked = false
}

func (coalescer *metadataCoalescer) dispatchFallbackError(calls []*metadataCoalesceCall) {
	body := []byte(http.StatusText(http.StatusBadGateway) + "\n")
	result := metadataCoalesceResult{
		headers: http.Header{
			"Content-Type":   []string{"text/plain; charset=utf-8"},
			"Content-Length": []string{strconv.Itoa(len(body))},
		},
		status: http.StatusBadGateway,
		body:   body,
	}
	for _, call := range calls {
		call.dispatch(result)
	}
}

func (group *metadataCoalesceGroup) anyActive() bool {
	for _, calls := range group.items {
		for _, call := range calls {
			if call.isActive() {
				return true
			}
		}
	}
	return false
}

func (group *metadataCoalesceGroup) installCancel(cancel context.CancelFunc) {
	group.lifecycleMu.Lock()
	group.cancel = cancel
	if !group.anyActive() {
		cancel()
	}
	group.lifecycleMu.Unlock()
}

func (group *metadataCoalesceGroup) clearCancel() {
	group.lifecycleMu.Lock()
	group.cancel = nil
	group.lifecycleMu.Unlock()
}

func (group *metadataCoalesceGroup) cancelIfIdle() {
	group.lifecycleMu.Lock()
	if group.cancel != nil && !group.anyActive() {
		group.cancel()
	}
	group.lifecycleMu.Unlock()
}

type metadataCoalesceCapture struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	limit    int64
	overflow bool
}

type metadataCoalesceTee struct {
	destination http.ResponseWriter
	body        bytes.Buffer
	status      int
	limit       int64
	overflow    bool
	failed      bool
}

func newMetadataCoalesceTee(destination http.ResponseWriter, limit int64) *metadataCoalesceTee {
	return &metadataCoalesceTee{destination: destination, limit: limit}
}

func (capture *metadataCoalesceTee) Header() http.Header { return capture.destination.Header() }

func (capture *metadataCoalesceTee) WriteHeader(status int) {
	if capture.status == 0 {
		capture.status = status
	}
	capture.destination.WriteHeader(status)
}

func (capture *metadataCoalesceTee) Write(value []byte) (int, error) {
	if capture.status == 0 {
		capture.WriteHeader(http.StatusOK)
	}
	count, err := capture.destination.Write(value)
	if err != nil || count != len(value) {
		capture.failed = true
	}
	if !capture.overflow && int64(capture.body.Len())+int64(count) <= capture.limit {
		_, _ = capture.body.Write(value[:count])
	} else {
		capture.overflow = true
	}
	return count, err
}

func (capture *metadataCoalesceTee) statusCode() int {
	if capture.status == 0 {
		return http.StatusOK
	}
	return capture.status
}

func (capture *metadataCoalesceTee) reusable() bool {
	return !capture.failed && !capture.overflow && capture.Header().Get("Trailer") == ""
}

func newMetadataCoalesceCapture(limit int64) *metadataCoalesceCapture {
	return &metadataCoalesceCapture{header: make(http.Header), limit: limit}
}

func (capture *metadataCoalesceCapture) Header() http.Header { return capture.header }

func (capture *metadataCoalesceCapture) WriteHeader(status int) {
	if capture.status == 0 {
		capture.status = status
	}
}

func (capture *metadataCoalesceCapture) Write(value []byte) (int, error) {
	if capture.status == 0 {
		capture.status = http.StatusOK
	}
	if capture.overflow || int64(capture.body.Len())+int64(len(value)) > capture.limit {
		capture.overflow = true
		return len(value), nil
	}
	return capture.body.Write(value)
}

func (capture *metadataCoalesceCapture) statusCode() int {
	if capture.status == 0 {
		return http.StatusOK
	}
	return capture.status
}
