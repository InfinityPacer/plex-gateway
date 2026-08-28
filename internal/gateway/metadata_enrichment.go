package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/InfinityPacer/plex-gateway/internal/mediainfo"
	"github.com/InfinityPacer/plex-gateway/internal/metrics"
	"github.com/InfinityPacer/plex-gateway/internal/pathmap"
	"github.com/InfinityPacer/plex-gateway/internal/plexmeta"
	"github.com/InfinityPacer/plex-gateway/internal/resolver"
)

const (
	defaultMediaInfoColdWait        = 5 * time.Second
	defaultMediaInfoResponseLimit   = 8 << 20
	defaultMediaInfoEnrichmentSlots = 4
)

type mediaInfoEnsurer interface {
	Get(mediainfo.Key) (mediainfo.Record, bool)
	Ensure(context.Context, mediainfo.Request) (mediainfo.Record, error)
}

// metadataEnrichmentOptions keeps analysis collaborators outside the playback
// handlers. A missing collaborator disables enrichment without changing Plex
// proxy behavior.
type metadataEnrichmentOptions struct {
	Service         mediaInfoEnsurer
	Mapper          *pathmap.Mapper
	Resolver        resolver.ControlResolver
	CloudExtensions []string
	ColdWait        time.Duration
	ResponseLimit   int64
	Concurrency     int
	Metrics         *metrics.Metrics
}

type metadataEnrichmentHandler struct {
	next            http.Handler
	service         mediaInfoEnsurer
	mapper          *pathmap.Mapper
	resolver        resolver.ControlResolver
	cloudExtensions map[string]struct{}
	coldWait        time.Duration
	responseLimit   int64
	waiters         chan struct{}
	metrics         *metrics.Metrics
}

func newMetadataEnrichmentHandler(options metadataEnrichmentOptions, next http.Handler) http.Handler {
	if next == nil || options.Service == nil || options.Mapper == nil || options.Resolver == nil {
		return next
	}
	coldWait := options.ColdWait
	if coldWait <= 0 {
		coldWait = defaultMediaInfoColdWait
	}
	responseLimit := options.ResponseLimit
	if responseLimit <= 0 {
		responseLimit = defaultMediaInfoResponseLimit
	}
	concurrency := options.Concurrency
	if concurrency <= 0 {
		concurrency = defaultMediaInfoEnrichmentSlots
	}
	registry := options.Metrics
	if registry == nil {
		registry = metrics.New()
	}
	return &metadataEnrichmentHandler{
		next: next, service: options.Service, mapper: options.Mapper, resolver: options.Resolver,
		cloudExtensions: normalizedExtensionSet(options.CloudExtensions), coldWait: coldWait,
		responseLimit: responseLimit, waiters: make(chan struct{}, concurrency), metrics: registry,
	}
}

func (handler *metadataEnrichmentHandler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	ratingKey, eligible := mediaInfoMetadataRequest(request)
	if !eligible {
		handler.next.ServeHTTP(w, request)
		return
	}
	select {
	case handler.waiters <- struct{}{}:
		handler.metrics.IncMediaInfoWaitActive()
		defer func() {
			<-handler.waiters
			handler.metrics.DecMediaInfoWaitActive()
		}()
	default:
		handler.metrics.IncMediaInfoWaitRejected()
		handler.next.ServeHTTP(w, request)
		return
	}

	capture := newBoundedResponseCapture(w, handler.responseLimit)
	handler.next.ServeHTTP(capture, request)
	if capture.passthrough {
		return
	}
	rawBody := capture.body.Bytes()
	decoded, encoding, err := decodeMetadataBody(rawBody, capture.header.Get("Content-Encoding"), handler.responseLimit)
	if err != nil {
		handler.failOpen(capture, rawBody)
		return
	}
	target, err := plexmeta.SelectEnrichmentTarget(decoded, capture.header.Get("Content-Type"), ratingKey)
	if err != nil {
		handler.failOpen(capture, rawBody)
		return
	}
	if !target.NeedsEnrichment || !handler.cloudPart(target.Part) {
		handler.replay(capture, rawBody)
		return
	}
	localPath, err := handler.mapper.Resolve(target.Part.File)
	if err != nil {
		handler.failOpen(capture, rawBody)
		return
	}
	strmTarget, err := handler.resolver.ReadTarget(localPath)
	if err != nil {
		handler.failOpen(capture, rawBody)
		return
	}
	fingerprint, err := mediainfo.FingerprintSTRMTarget(strmTarget)
	if err != nil {
		handler.failOpen(capture, rawBody)
		return
	}
	key := mediainfo.Key{PartID: target.Part.ID, STRMFingerprint: fingerprint}
	var record mediainfo.Record
	if mediaInfoCacheOnlyRequest(request) {
		var found bool
		record, found = handler.service.Get(key)
		if !found {
			handler.replay(capture, rawBody)
			return
		}
	} else {
		waitContext, cancel := context.WithTimeout(request.Context(), handler.coldWait)
		record, err = handler.service.Ensure(waitContext, mediainfo.Request{
			Key: key, RatingKey: ratingKey, Target: strmTarget,
			Priority: mediainfo.PriorityInteractive, ClientUserAgent: request.UserAgent(),
		})
		cancel()
		if err != nil {
			handler.failOpen(capture, rawBody)
			return
		}
	}
	currentTarget, err := handler.resolver.ReadTarget(localPath)
	if err != nil {
		handler.failOpen(capture, rawBody)
		return
	}
	currentFingerprint, err := mediainfo.FingerprintSTRMTarget(currentTarget)
	if err != nil || currentFingerprint != fingerprint {
		handler.failOpen(capture, rawBody)
		return
	}
	enriched, changed, err := plexmeta.EnrichMetadata(decoded, capture.header.Get("Content-Type"), ratingKey, record.Media)
	if err != nil || !changed {
		handler.failOpen(capture, rawBody)
		return
	}
	wireBody, err := encodeMetadataBody(enriched, encoding)
	if err != nil {
		handler.failOpen(capture, rawBody)
		return
	}
	headers := capture.header.Clone()
	for _, name := range []string{"ETag", "Last-Modified", "Content-MD5", "Digest"} {
		headers.Del(name)
	}
	if !hasCacheDirective(headers.Get("Cache-Control"), "no-store") {
		headers.Set("Cache-Control", "private, no-cache")
	}
	headers.Set("Content-Length", strconv.Itoa(len(wireBody)))
	writeCapturedResponse(w, headers, capture.statusCode(), wireBody)
	handler.metrics.IncMediaInfoEnriched()
}

func (handler *metadataEnrichmentHandler) cloudPart(part plexmeta.Part) bool {
	_, ok := handler.cloudExtensions[strings.ToLower(path.Ext(part.File))]
	return ok
}

func (handler *metadataEnrichmentHandler) failOpen(capture *boundedResponseCapture, rawBody []byte) {
	handler.replay(capture, rawBody)
	handler.metrics.IncMediaInfoFailOpen()
}

func (handler *metadataEnrichmentHandler) replay(capture *boundedResponseCapture, rawBody []byte) {
	writeCapturedResponse(capture.destination, capture.header, capture.statusCode(), rawBody)
}

func mediaInfoMetadataRequest(request *http.Request) (string, bool) {
	if request == nil || request.URL == nil || request.Method != http.MethodGet || request.Header.Get("Range") != "" {
		return "", false
	}
	_, tokenPresent, tokenValid := requestIdentity(request, "X-Plex-Token")
	if !tokenPresent || !tokenValid || !isMetadataItemPath(request.URL.Path) {
		return "", false
	}
	return strings.TrimPrefix(request.URL.Path, "/library/metadata/"), true
}

// mediaInfoCacheOnlyRequest identifies background library crawlers that may
// enumerate every item. They can consume an existing record, while active
// playback remains responsible for admitting new remote analysis work.
func mediaInfoCacheOnlyRequest(request *http.Request) bool {
	if request == nil || request.URL == nil {
		return false
	}
	product := strings.ToLower(strings.TrimSpace(request.Header.Get("X-Plex-Product")))
	_, skipsRefresh := request.URL.Query()["skipRefresh"]
	return skipsRefresh && strings.HasSuffix(product, "-library")
}

func normalizedExtensionSet(extensions []string) map[string]struct{} {
	if len(extensions) == 0 {
		extensions = []string{".strm"}
	}
	result := make(map[string]struct{}, len(extensions))
	for _, extension := range extensions {
		extension = strings.ToLower(strings.TrimSpace(extension))
		if extension != "" {
			result[extension] = struct{}{}
		}
	}
	return result
}

func hasCacheDirective(value, directive string) bool {
	for _, component := range strings.Split(value, ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(component), "=")
		if strings.EqualFold(name, directive) {
			return true
		}
	}
	return false
}

func decodeMetadataBody(raw []byte, rawEncoding string, limit int64) ([]byte, string, error) {
	encoding := strings.ToLower(strings.TrimSpace(rawEncoding))
	switch encoding {
	case "", "identity":
		return raw, encoding, nil
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, "", err
		}
		defer reader.Close()
		decoded, err := io.ReadAll(io.LimitReader(reader, limit+1))
		if err != nil || int64(len(decoded)) > limit {
			return nil, "", errors.New("metadata gzip body exceeds limit")
		}
		return decoded, encoding, nil
	default:
		return nil, "", errors.New("metadata content encoding is unsupported")
	}
}

func encodeMetadataBody(decoded []byte, encoding string) ([]byte, error) {
	if encoding != "gzip" {
		return decoded, nil
	}
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write(decoded); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

type boundedResponseCapture struct {
	destination http.ResponseWriter
	header      http.Header
	body        bytes.Buffer
	status      int
	limit       int64
	passthrough bool
}

func newBoundedResponseCapture(destination http.ResponseWriter, limit int64) *boundedResponseCapture {
	return &boundedResponseCapture{destination: destination, header: make(http.Header), limit: limit}
}

func (capture *boundedResponseCapture) Header() http.Header { return capture.header }

func (capture *boundedResponseCapture) WriteHeader(status int) {
	if capture.status != 0 {
		return
	}
	capture.status = status
	if status != http.StatusOK || capture.header.Get("Trailer") != "" || capture.header.Get("Content-Range") != "" {
		capture.startPassthrough()
	}
}

func (capture *boundedResponseCapture) Write(value []byte) (int, error) {
	if capture.status == 0 {
		capture.WriteHeader(http.StatusOK)
	}
	if capture.passthrough {
		return capture.destination.Write(value)
	}
	if int64(capture.body.Len())+int64(len(value)) > capture.limit {
		if err := capture.startPassthrough(); err != nil {
			return 0, err
		}
		return capture.destination.Write(value)
	}
	return capture.body.Write(value)
}

// Flush is intentionally deferred while a bounded metadata body is captured.
// A passthrough response retains the downstream flusher behavior.
func (capture *boundedResponseCapture) Flush() {
	if !capture.passthrough {
		return
	}
	if flusher, ok := capture.destination.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (capture *boundedResponseCapture) statusCode() int {
	if capture.status == 0 {
		return http.StatusOK
	}
	return capture.status
}

func (capture *boundedResponseCapture) startPassthrough() error {
	if capture.passthrough {
		return nil
	}
	capture.passthrough = true
	writeHeader(capture.destination.Header(), capture.header)
	capture.destination.WriteHeader(capture.statusCode())
	if capture.body.Len() == 0 {
		return nil
	}
	_, err := capture.destination.Write(capture.body.Bytes())
	capture.body.Reset()
	return err
}

func writeCapturedResponse(destination http.ResponseWriter, header http.Header, status int, body []byte) {
	writeHeader(destination.Header(), header)
	destination.WriteHeader(status)
	_, _ = destination.Write(body)
}

func writeHeader(destination, source http.Header) {
	for name := range destination {
		destination.Del(name)
	}
	for name, values := range source {
		destination[name] = append([]string(nil), values...)
	}
}
