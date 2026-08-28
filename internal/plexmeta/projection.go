package plexmeta

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/InfinityPacer/plex-gateway/internal/mediainfo"
)

// Target identifies the single Plex item and reports whether the response is
// missing a whitelisted technical field that a MediaInfo record can project.
// A missing Stream is not itself a reason to enrich: projection never creates
// Stream elements because their Plex identity is not known to the gateway.
type Target struct {
	Part Part
	// NeedsEnrichment is true when a whitelisted field is absent from the
	// selected Media, Part, or matchable existing Stream elements.
	NeedsEnrichment bool
}

// SelectEnrichmentTarget strictly selects one ratingKey item and its only
// Media/Part pair. It performs no response mutation and is suitable for
// deciding whether a probe is needed before loading MediaInfo.
func SelectEnrichmentTarget(body []byte, contentType, ratingKey string) (Target, error) {
	_, selection, err := parseProjection(body, contentType, ratingKey)
	if err != nil {
		return Target{}, err
	}
	return selection.target(), nil
}

// SelectEnrichmentPart is the strict Part-only form of
// SelectEnrichmentTarget. The response must contain exactly one requested
// ratingKey item, one Media, and one Part.
func SelectEnrichmentPart(body []byte, contentType, ratingKey string) (Part, error) {
	target, err := SelectEnrichmentTarget(body, contentType, ratingKey)
	if err != nil {
		return Part{}, err
	}
	return target.Part, nil
}

// EnrichMetadata fills only absent whitelisted Media, Part, and already
// present Stream attributes. SelectEnrichmentTarget supplies the exact Part
// to callers that need to build a probe request before this function runs.
// The input body is never modified in place. When no field is filled, the
// original bytes are returned unchanged.
func EnrichMetadata(body []byte, contentType, ratingKey string, media mediainfo.Media) ([]byte, bool, error) {
	document, selection, err := parseProjection(body, contentType, ratingKey)
	if err != nil {
		return nil, false, err
	}
	changed, err := selection.enrich(media)
	if err != nil {
		return nil, false, err
	}
	if !changed {
		return append([]byte(nil), body...), false, nil
	}
	result, err := document.encode()
	if err != nil {
		return nil, false, err
	}
	return result, true, nil
}

type projectionDocument interface {
	selectTarget(string) (projectionSelection, error)
	encode() ([]byte, error)
}

type projectionSelection interface {
	target() Target
	enrich(mediainfo.Media) (bool, error)
}

func parseProjection(body []byte, contentType, ratingKey string) (projectionDocument, projectionSelection, error) {
	ratingKey, err := normalizeProjectionRatingKey(ratingKey)
	if err != nil {
		return nil, nil, err
	}
	trimmed := trimProjectionBody(body)
	if len(trimmed) == 0 {
		return nil, nil, errors.New("Plex metadata response is empty")
	}
	if strings.Contains(strings.ToLower(contentType), "json") || trimmed[0] == '{' || trimmed[0] == '[' {
		document, err := parseJSONProjectionDocument(body)
		if err != nil {
			return nil, nil, err
		}
		selection, err := document.selectTarget(ratingKey)
		if err != nil {
			return nil, nil, err
		}
		return document, selection, nil
	}
	document, err := parseXMLProjectionDocument(body)
	if err != nil {
		return nil, nil, err
	}
	selection, err := document.selectTarget(ratingKey)
	if err != nil {
		return nil, nil, err
	}
	return document, selection, nil
}

func normalizeProjectionRatingKey(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "/?#\x00\r\n") {
		return "", errors.New("rating key must be a single path segment")
	}
	return value, nil
}

func trimProjectionBody(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	trimmed = bytes.TrimPrefix(trimmed, []byte{0xef, 0xbb, 0xbf})
	return bytes.TrimSpace(trimmed)
}

// projectionAttribute is deliberately narrower than mediainfo.Media. The
// whitelist contains only descriptive technical Plex fields; identifiers and
// playback-selection fields are never generated.
type projectionAttribute struct {
	name   string
	value  string
	number bool
}

func textProjectionAttribute(name, value string) projectionAttribute {
	return projectionAttribute{name: name, value: value}
}

func integerProjectionAttribute(name string, value int64) projectionAttribute {
	return projectionAttribute{name: name, value: strconv.FormatInt(value, 10), number: true}
}

func mediaProjectionAttributes(media mediainfo.Media) []projectionAttribute {
	attributes := make([]projectionAttribute, 0, 14)
	if media.Container != "" {
		attributes = append(attributes, textProjectionAttribute("container", media.Container))
	}
	if media.DurationMS > 0 {
		attributes = append(attributes, integerProjectionAttribute("duration", media.DurationMS))
	}
	if media.Bitrate > 0 {
		attributes = append(attributes, integerProjectionAttribute("bitrate", media.Bitrate))
	}
	if media.Width > 0 {
		attributes = append(attributes, integerProjectionAttribute("width", int64(media.Width)))
	}
	if media.Height > 0 {
		attributes = append(attributes, integerProjectionAttribute("height", int64(media.Height)))
	}
	if media.AspectRatio != "" {
		attributes = append(attributes, textProjectionAttribute("aspectRatio", media.AspectRatio))
	}
	if media.AudioChannels > 0 {
		attributes = append(attributes, integerProjectionAttribute("audioChannels", int64(media.AudioChannels)))
	}
	if media.AudioCodec != "" {
		attributes = append(attributes, textProjectionAttribute("audioCodec", media.AudioCodec))
	}
	if media.VideoCodec != "" {
		attributes = append(attributes, textProjectionAttribute("videoCodec", media.VideoCodec))
	}
	if media.VideoResolution != "" {
		attributes = append(attributes, textProjectionAttribute("videoResolution", media.VideoResolution))
	}
	if media.FrameRate != "" {
		attributes = append(attributes, textProjectionAttribute("videoFrameRate", media.FrameRate))
	}
	if media.VideoProfile != "" {
		attributes = append(attributes, textProjectionAttribute("videoProfile", media.VideoProfile))
	}
	if media.AudioProfile != "" {
		attributes = append(attributes, textProjectionAttribute("audioProfile", media.AudioProfile))
	}
	return attributes
}

func partProjectionAttributes(media mediainfo.Media) []projectionAttribute {
	attributes := make([]projectionAttribute, 0, 5)
	if media.DurationMS > 0 {
		attributes = append(attributes, integerProjectionAttribute("duration", media.DurationMS))
	}
	if media.Size > 0 {
		attributes = append(attributes, integerProjectionAttribute("size", media.Size))
	}
	if media.Container != "" {
		attributes = append(attributes, textProjectionAttribute("container", media.Container))
	}
	if media.VideoProfile != "" {
		attributes = append(attributes, textProjectionAttribute("videoProfile", media.VideoProfile))
	}
	if media.AudioProfile != "" {
		attributes = append(attributes, textProjectionAttribute("audioProfile", media.AudioProfile))
	}
	return attributes
}

func streamProjectionAttributes(stream mediainfo.Stream) []projectionAttribute {
	attributes := make([]projectionAttribute, 0, 20)
	if stream.Codec != "" {
		attributes = append(attributes, textProjectionAttribute("codec", stream.Codec))
	}
	if stream.Profile != "" {
		attributes = append(attributes, textProjectionAttribute("profile", stream.Profile))
	}
	if stream.Level > 0 {
		attributes = append(attributes, integerProjectionAttribute("level", int64(stream.Level)))
	}
	if stream.Bitrate > 0 {
		attributes = append(attributes, integerProjectionAttribute("bitrate", stream.Bitrate))
	}
	if stream.Width > 0 {
		attributes = append(attributes, integerProjectionAttribute("width", int64(stream.Width)))
	}
	if stream.Height > 0 {
		attributes = append(attributes, integerProjectionAttribute("height", int64(stream.Height)))
	}
	frameRate := firstProjectionString(stream.FrameRate, stream.AverageFrameRate)
	if frameRate != "" {
		attributes = append(attributes, textProjectionAttribute("frameRate", frameRate))
	}
	if stream.ReferenceFrames > 0 {
		attributes = append(attributes, integerProjectionAttribute("refFrames", int64(stream.ReferenceFrames)))
	}
	if stream.PixelFormat != "" {
		attributes = append(attributes, textProjectionAttribute("pixelFormat", stream.PixelFormat))
	}
	if stream.BitDepth > 0 {
		attributes = append(attributes, integerProjectionAttribute("bitDepth", int64(stream.BitDepth)))
	}
	if stream.ColorSpace != "" {
		attributes = append(attributes, textProjectionAttribute("colorSpace", stream.ColorSpace))
	}
	if stream.ColorRange != "" {
		attributes = append(attributes, textProjectionAttribute("colorRange", stream.ColorRange))
	}
	if stream.ColorPrimaries != "" {
		attributes = append(attributes, textProjectionAttribute("colorPrimaries", stream.ColorPrimaries))
	}
	if stream.ColorTransfer != "" {
		attributes = append(attributes, textProjectionAttribute("colorTrc", stream.ColorTransfer))
	}
	if stream.ChromaLocation != "" {
		attributes = append(attributes, textProjectionAttribute("chromaLocation", stream.ChromaLocation))
	}
	if stream.SampleAspectRatio != "" {
		attributes = append(attributes, textProjectionAttribute("sampleAspectRatio", stream.SampleAspectRatio))
	}
	if stream.DisplayAspectRatio != "" {
		attributes = append(attributes, textProjectionAttribute("displayAspectRatio", stream.DisplayAspectRatio))
	}
	if stream.SampleRate > 0 {
		attributes = append(attributes, integerProjectionAttribute("samplingRate", int64(stream.SampleRate)))
	}
	if stream.Channels > 0 {
		attributes = append(attributes, integerProjectionAttribute("channels", int64(stream.Channels)))
	}
	if stream.ChannelLayout != "" {
		attributes = append(attributes, textProjectionAttribute("channelLayout", stream.ChannelLayout))
	}
	if stream.Language != "" {
		attributes = append(attributes, textProjectionAttribute("language", stream.Language))
	}
	if stream.Title != "" {
		attributes = append(attributes, textProjectionAttribute("title", stream.Title))
	}
	return attributes
}

func firstProjectionString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

var mediaProjectionAttributeNames = []string{
	"container", "duration", "bitrate", "width", "height", "aspectRatio",
	"audioChannels", "audioCodec", "videoCodec", "videoResolution", "videoFrameRate",
	"videoProfile", "audioProfile",
}

var partProjectionAttributeNames = []string{"duration", "size", "container", "videoProfile", "audioProfile"}

var commonStreamProjectionAttributeNames = []string{
	"codec", "profile", "level", "bitrate", "language", "title",
}

var videoStreamProjectionAttributeNames = []string{
	"width", "height", "frameRate", "refFrames", "pixelFormat", "bitDepth",
	"colorSpace", "colorRange", "colorPrimaries", "colorTrc", "chromaLocation",
	"sampleAspectRatio", "displayAspectRatio",
}

var audioStreamProjectionAttributeNames = []string{"samplingRate", "channels", "channelLayout"}

type streamKey struct {
	typeName string
	index    int
}

type streamProjection struct {
	key        streamKey
	attributes []projectionAttribute
}

func buildStreamProjections(media mediainfo.Media) ([]streamProjection, error) {
	projections := make([]streamProjection, 0, len(media.Streams))
	seen := make(map[streamKey]struct{}, len(media.Streams))
	for _, stream := range media.Streams {
		key, ok := mediaStreamKey(stream.Type, stream.Index)
		if !ok {
			continue
		}
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("MediaInfo contains duplicate stream identity %s/%d", key.typeName, key.index)
		}
		seen[key] = struct{}{}
		projections = append(projections, streamProjection{
			key:        key,
			attributes: streamProjectionAttributes(stream),
		})
	}
	return projections, nil
}

func mediaStreamKey(typeName string, index int) (streamKey, bool) {
	if index < 0 {
		return streamKey{}, false
	}
	typeName = canonicalStreamType(typeName)
	if typeName == "" {
		return streamKey{}, false
	}
	return streamKey{typeName: typeName, index: index}, true
}

func canonicalStreamType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "video":
		return "1"
	case "2", "audio":
		return "2"
	case "3", "subtitle", "subtitles":
		return "3"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func streamProjectionAttributeNames(typeName string) []string {
	names := append([]string(nil), commonStreamProjectionAttributeNames...)
	switch typeName {
	case "1":
		names = append(names, videoStreamProjectionAttributeNames...)
	case "2":
		names = append(names, audioStreamProjectionAttributeNames...)
	}
	return names
}

// XML projection -----------------------------------------------------------

type xmlProjectionDocument struct {
	nodes []*xmlProjectionNode
}

type xmlProjectionNode struct {
	element *xmlProjectionElement
	token   xml.Token
}

type xmlProjectionElement struct {
	start    xml.StartElement
	children []*xmlProjectionNode
}

func parseXMLProjectionDocument(body []byte) (*xmlProjectionDocument, error) {
	decoder := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf})))
	document := &xmlProjectionDocument{}
	stack := make([]*xmlProjectionElement, 0, 8)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse Plex XML metadata: %w", err)
		}
		switch typed := token.(type) {
		case xml.StartElement:
			element := &xmlProjectionElement{start: copyXMLStartElement(typed)}
			node := &xmlProjectionNode{element: element}
			appendXMLProjectionNode(document, stack, node)
			stack = append(stack, element)
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, errors.New("Plex XML metadata has an unexpected closing element")
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			data := append(xml.CharData(nil), typed...)
			if len(stack) == 0 && len(bytes.TrimSpace(data)) != 0 {
				return nil, errors.New("Plex XML metadata has non-whitespace text outside the root")
			}
			appendXMLProjectionNode(document, stack, &xmlProjectionNode{token: data})
		case xml.Comment:
			appendXMLProjectionNode(document, stack, &xmlProjectionNode{token: append(xml.Comment(nil), typed...)})
		case xml.Directive:
			appendXMLProjectionNode(document, stack, &xmlProjectionNode{token: append(xml.Directive(nil), typed...)})
		case xml.ProcInst:
			appendXMLProjectionNode(document, stack, &xmlProjectionNode{token: xml.ProcInst{
				Target: typed.Target,
				Inst:   append([]byte(nil), typed.Inst...),
			}})
		}
	}
	if len(stack) != 0 {
		return nil, errors.New("Plex XML metadata has an unterminated element")
	}
	if len(xmlProjectionElements(document.nodes)) != 1 {
		return nil, errors.New("Plex XML metadata must have one root element")
	}
	return document, nil
}

func copyXMLStartElement(start xml.StartElement) xml.StartElement {
	attributes := make([]xml.Attr, len(start.Attr))
	copy(attributes, start.Attr)
	return xml.StartElement{Name: start.Name, Attr: attributes}
}

func appendXMLProjectionNode(document *xmlProjectionDocument, stack []*xmlProjectionElement, node *xmlProjectionNode) {
	if len(stack) == 0 {
		document.nodes = append(document.nodes, node)
		return
	}
	parent := stack[len(stack)-1]
	parent.children = append(parent.children, node)
}

func xmlProjectionElements(nodes []*xmlProjectionNode) []*xmlProjectionElement {
	result := make([]*xmlProjectionElement, 0, len(nodes))
	for _, node := range nodes {
		if node.element != nil {
			result = append(result, node.element)
		}
	}
	return result
}

func directXMLElements(parent *xmlProjectionElement, localName string) []*xmlProjectionElement {
	result := make([]*xmlProjectionElement, 0)
	for _, node := range parent.children {
		if node.element != nil && node.element.start.Name.Local == localName {
			result = append(result, node.element)
		}
	}
	return result
}

func (document *xmlProjectionDocument) selectTarget(ratingKey string) (projectionSelection, error) {
	roots := xmlProjectionElements(document.nodes)
	if len(roots) != 1 || roots[0].start.Name.Local != "MediaContainer" {
		return nil, errors.New("Plex XML metadata has no unique MediaContainer root")
	}
	videos := directXMLElements(roots[0], "Video")
	if len(videos) != 1 {
		return nil, errors.New("Plex XML metadata must contain one Video item")
	}
	item := videos[0]
	itemRatingKey, present, err := xmlAttribute(item.start.Attr, "ratingKey")
	if err != nil {
		return nil, err
	}
	if !present || itemRatingKey == "" || itemRatingKey != ratingKey {
		return nil, errors.New("Plex XML metadata ratingKey does not match the requested item")
	}
	mediaItems := directXMLElements(item, "Media")
	if len(mediaItems) != 1 {
		return nil, errors.New("Plex XML metadata must contain one Media")
	}
	parts := directXMLElements(mediaItems[0], "Part")
	if len(parts) != 1 {
		return nil, errors.New("Plex XML metadata must contain one Part")
	}
	part, err := partFromXML(parts[0])
	if err != nil {
		return nil, err
	}
	needs, err := xmlTargetNeedsEnrichment(mediaItems[0], parts[0])
	if err != nil {
		return nil, err
	}
	return &xmlProjectionSelection{
		value: Target{Part: part, NeedsEnrichment: needs},
		media: mediaItems[0],
		part:  parts[0],
	}, nil
}

type xmlProjectionSelection struct {
	value Target
	media *xmlProjectionElement
	part  *xmlProjectionElement
}

func (selection *xmlProjectionSelection) target() Target {
	return selection.value
}

func (selection *xmlProjectionSelection) enrich(media mediainfo.Media) (bool, error) {
	streamProjections, err := buildStreamProjections(media)
	if err != nil {
		return false, err
	}
	changed := addMissingXMLAttributes(selection.media, mediaProjectionAttributes(media))
	changed = addMissingXMLAttributes(selection.part, partProjectionAttributes(media)) || changed
	streamElements := directXMLElements(selection.part, "Stream")
	streamIndex, err := indexXMLStreams(streamElements)
	if err != nil {
		return false, err
	}
	for _, projection := range streamProjections {
		stream, ok := streamIndex[projection.key]
		if !ok {
			continue
		}
		changed = addMissingXMLAttributes(stream, projection.attributes) || changed
	}
	return changed, nil
}

func partFromXML(element *xmlProjectionElement) (Part, error) {
	id, present, err := xmlAttribute(element.start.Attr, "id")
	if err != nil {
		return Part{}, err
	}
	if !present || id == "" {
		return Part{}, errors.New("selected Plex Part has no id")
	}
	key, _, err := xmlAttribute(element.start.Attr, "key")
	if err != nil {
		return Part{}, err
	}
	file, present, err := xmlAttribute(element.start.Attr, "file")
	if err != nil {
		return Part{}, err
	}
	if !present || file == "" {
		return Part{}, errors.New("selected Plex Part has no file")
	}
	return Part{ID: id, Key: key, File: file}, nil
}

func xmlAttribute(attributes []xml.Attr, localName string) (string, bool, error) {
	value := ""
	present := false
	for _, attribute := range attributes {
		if attribute.Name.Local != localName {
			continue
		}
		if present {
			return "", false, fmt.Errorf("Plex XML metadata has duplicate %s attributes", localName)
		}
		value = attribute.Value
		present = true
	}
	return value, present, nil
}

func xmlTargetNeedsEnrichment(media, part *xmlProjectionElement) (bool, error) {
	needs := false
	for _, name := range mediaProjectionAttributeNames {
		_, present, err := xmlAttribute(media.start.Attr, name)
		if err != nil {
			return false, err
		}
		if !present {
			needs = true
		}
	}
	for _, name := range partProjectionAttributeNames {
		_, present, err := xmlAttribute(part.start.Attr, name)
		if err != nil {
			return false, err
		}
		if !present {
			needs = true
		}
	}
	streams := directXMLElements(part, "Stream")
	if _, err := indexXMLStreams(streams); err != nil {
		return false, err
	}
	for _, stream := range streams {
		key, present, err := xmlStreamIdentity(stream)
		if err != nil {
			return false, err
		}
		if !present {
			continue
		}
		for _, name := range streamProjectionAttributeNames(key.typeName) {
			_, present, err := xmlAttribute(stream.start.Attr, name)
			if err != nil {
				return false, err
			}
			if !present {
				needs = true
			}
		}
	}
	return needs, nil
}

func indexXMLStreams(streams []*xmlProjectionElement) (map[streamKey]*xmlProjectionElement, error) {
	result := make(map[streamKey]*xmlProjectionElement, len(streams))
	for _, stream := range streams {
		key, present, err := xmlStreamIdentity(stream)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("Plex metadata has duplicate Stream identity %s/%d", key.typeName, key.index)
		}
		result[key] = stream
	}
	return result, nil
}

func xmlStreamIdentity(stream *xmlProjectionElement) (streamKey, bool, error) {
	typeName, typePresent, err := xmlAttribute(stream.start.Attr, "streamType")
	if err != nil {
		return streamKey{}, false, err
	}
	indexValue, indexPresent, err := xmlAttribute(stream.start.Attr, "index")
	if err != nil {
		return streamKey{}, false, err
	}
	if !typePresent || !indexPresent {
		return streamKey{}, false, nil
	}
	if strings.TrimSpace(typeName) == "" {
		return streamKey{}, false, errors.New("Plex metadata Stream has an empty streamType")
	}
	index, err := strconv.Atoi(strings.TrimSpace(indexValue))
	if err != nil || index < 0 {
		return streamKey{}, false, errors.New("Plex metadata Stream has an invalid index")
	}
	key, ok := mediaStreamKey(typeName, index)
	if !ok {
		return streamKey{}, false, errors.New("Plex metadata Stream has an invalid identity")
	}
	return key, true, nil
}

func addMissingXMLAttributes(element *xmlProjectionElement, attributes []projectionAttribute) bool {
	changed := false
	for _, attribute := range attributes {
		_, present, err := xmlAttribute(element.start.Attr, attribute.name)
		if err != nil {
			// The target validation inspects all whitelisted attributes before
			// mutation. This branch is unreachable for a valid selection.
			continue
		}
		if present {
			continue
		}
		element.start.Attr = append(element.start.Attr, xml.Attr{
			Name:  xml.Name{Local: attribute.name},
			Value: attribute.value,
		})
		changed = true
	}
	return changed
}

func (document *xmlProjectionDocument) encode() ([]byte, error) {
	var buffer bytes.Buffer
	encoder := xml.NewEncoder(&buffer)
	for _, node := range document.nodes {
		if node.element != nil {
			if err := encodeXMLProjectionElement(encoder, node.element); err != nil {
				return nil, fmt.Errorf("encode Plex XML metadata: %w", err)
			}
			continue
		}
		if err := encoder.EncodeToken(node.token); err != nil {
			return nil, fmt.Errorf("encode Plex XML metadata: %w", err)
		}
	}
	if err := encoder.Flush(); err != nil {
		return nil, fmt.Errorf("flush Plex XML metadata: %w", err)
	}
	return buffer.Bytes(), nil
}

func encodeXMLProjectionElement(encoder *xml.Encoder, element *xmlProjectionElement) error {
	if err := encoder.EncodeToken(element.start); err != nil {
		return err
	}
	for _, node := range element.children {
		if node.element != nil {
			if err := encodeXMLProjectionElement(encoder, node.element); err != nil {
				return err
			}
			continue
		}
		if err := encoder.EncodeToken(node.token); err != nil {
			return err
		}
	}
	return encoder.EncodeToken(element.start.End())
}

// JSON projection ----------------------------------------------------------

type jsonProjectionDocument struct {
	root   jsonObject
	prefix []byte
	suffix []byte
}

type jsonObject map[string]json.RawMessage

func parseJSONProjectionDocument(body []byte) (*jsonProjectionDocument, error) {
	core, prefix, suffix, err := splitJSONProjectionBody(body)
	if err != nil {
		return nil, err
	}
	root, err := decodeJSONProjectionObject(core)
	if err != nil {
		return nil, fmt.Errorf("parse Plex JSON metadata: %w", err)
	}
	return &jsonProjectionDocument{root: root, prefix: prefix, suffix: suffix}, nil
}

func splitJSONProjectionBody(body []byte) ([]byte, []byte, []byte, error) {
	leading := len(body) - len(bytes.TrimLeft(body, " \t\r\n"))
	start := leading
	if bytes.HasPrefix(body[start:], []byte{0xef, 0xbb, 0xbf}) {
		start += 3
	}
	withoutBOM := body[start:]
	left := len(withoutBOM) - len(bytes.TrimLeft(withoutBOM, " \t\r\n"))
	coreWithSuffix := withoutBOM[left:]
	right := len(coreWithSuffix) - len(bytes.TrimRight(coreWithSuffix, " \t\r\n"))
	if len(coreWithSuffix)-right == 0 {
		return nil, nil, nil, errors.New("Plex JSON metadata response is empty")
	}
	prefix := append([]byte(nil), body[:start+left]...)
	suffix := append([]byte(nil), coreWithSuffix[len(coreWithSuffix)-right:]...)
	return coreWithSuffix[:len(coreWithSuffix)-right], prefix, suffix, nil
}

func decodeJSONProjectionObject(raw []byte) (jsonObject, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, errors.New("Plex JSON metadata value is not an object")
	}
	result := make(jsonObject)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("Plex JSON metadata object has an invalid key")
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("Plex JSON metadata has duplicate %s fields", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		result[key] = append(json.RawMessage(nil), value...)
	}
	if token, err := decoder.Token(); err != nil {
		return nil, err
	} else if delimiter, ok := token.(json.Delim); !ok || delimiter != '}' {
		return nil, errors.New("Plex JSON metadata object is not closed")
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) || token != nil {
		return nil, errors.New("Plex JSON metadata has trailing values")
	}
	return result, nil
}

func decodeJSONProjectionArray(raw []byte) ([]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '[' {
		return nil, errors.New("Plex JSON metadata value is not an array")
	}
	result := make([]json.RawMessage, 0)
	for decoder.More() {
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		result = append(result, append(json.RawMessage(nil), value...))
	}
	if token, err := decoder.Token(); err != nil {
		return nil, err
	} else if delimiter, ok := token.(json.Delim); !ok || delimiter != ']' {
		return nil, errors.New("Plex JSON metadata array is not closed")
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) || token != nil {
		return nil, errors.New("Plex JSON metadata has trailing values")
	}
	return result, nil
}

type jsonProjectionList struct {
	items []*jsonObject
	array bool
}

func decodeJSONProjectionList(raw json.RawMessage, name string) (jsonProjectionList, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return jsonProjectionList{}, fmt.Errorf("Plex JSON metadata %s is empty", name)
	}
	if trimmed[0] == '{' {
		object, err := decodeJSONProjectionObject(trimmed)
		if err != nil {
			return jsonProjectionList{}, fmt.Errorf("parse Plex JSON %s: %w", name, err)
		}
		return jsonProjectionList{items: []*jsonObject{&object}}, nil
	}
	if trimmed[0] != '[' {
		return jsonProjectionList{}, fmt.Errorf("Plex JSON metadata %s is neither an object nor an array", name)
	}
	values, err := decodeJSONProjectionArray(trimmed)
	if err != nil {
		return jsonProjectionList{}, fmt.Errorf("parse Plex JSON %s: %w", name, err)
	}
	result := jsonProjectionList{items: make([]*jsonObject, 0, len(values)), array: true}
	for _, value := range values {
		object, err := decodeJSONProjectionObject(value)
		if err != nil {
			return jsonProjectionList{}, fmt.Errorf("Plex JSON metadata %s contains a non-object item: %w", name, err)
		}
		result.items = append(result.items, &object)
	}
	return result, nil
}

func encodeJSONProjectionList(list jsonProjectionList) (json.RawMessage, error) {
	if list.array {
		values := make([]jsonObject, 0, len(list.items))
		for _, item := range list.items {
			values = append(values, *item)
		}
		return json.Marshal(values)
	}
	if len(list.items) != 1 {
		return nil, errors.New("Plex JSON metadata object list is not unique")
	}
	return json.Marshal(*list.items[0])
}

func (document *jsonProjectionDocument) selectTarget(ratingKey string) (projectionSelection, error) {
	containerRaw, ok := document.root["MediaContainer"]
	if !ok {
		return nil, errors.New("Plex JSON metadata has no MediaContainer")
	}
	container, err := decodeJSONProjectionObject(bytes.TrimSpace(containerRaw))
	if err != nil {
		return nil, fmt.Errorf("parse Plex JSON MediaContainer: %w", err)
	}
	metadataRaw, ok := container["Metadata"]
	if !ok {
		return nil, errors.New("Plex JSON metadata has no Metadata item")
	}
	metadata, err := decodeJSONProjectionList(metadataRaw, "Metadata")
	if err != nil {
		return nil, err
	}
	if len(metadata.items) != 1 {
		return nil, errors.New("Plex JSON metadata must contain one Metadata item")
	}
	item := metadata.items[0]
	itemRatingKey, err := requiredJSONScalarString(*item, "ratingKey")
	if err != nil {
		return nil, err
	}
	if itemRatingKey != ratingKey {
		return nil, errors.New("Plex JSON metadata ratingKey does not match the requested item")
	}
	mediaRaw, ok := (*item)["Media"]
	if !ok {
		return nil, errors.New("Plex JSON metadata has no Media")
	}
	mediaItems, err := decodeJSONProjectionList(mediaRaw, "Media")
	if err != nil {
		return nil, err
	}
	if len(mediaItems.items) != 1 {
		return nil, errors.New("Plex JSON metadata must contain one Media")
	}
	mediaItem := mediaItems.items[0]
	partRaw, ok := (*mediaItem)["Part"]
	if !ok {
		return nil, errors.New("Plex JSON metadata has no Part")
	}
	parts, err := decodeJSONProjectionList(partRaw, "Part")
	if err != nil {
		return nil, err
	}
	if len(parts.items) != 1 {
		return nil, errors.New("Plex JSON metadata must contain one Part")
	}
	partItem := parts.items[0]
	part, err := partFromJSON(partItem)
	if err != nil {
		return nil, err
	}
	needs, err := jsonTargetNeedsEnrichment(mediaItem, partItem)
	if err != nil {
		return nil, err
	}
	return &jsonProjectionSelection{
		value:     Target{Part: part, NeedsEnrichment: needs},
		document:  document,
		container: container,
		metadata:  metadata,
		media:     mediaItems,
		parts:     parts,
		mediaItem: mediaItem,
		part:      partItem,
	}, nil
}

type jsonProjectionSelection struct {
	value     Target
	document  *jsonProjectionDocument
	container jsonObject
	metadata  jsonProjectionList
	media     jsonProjectionList
	parts     jsonProjectionList
	mediaItem *jsonObject
	part      *jsonObject
}

func (selection *jsonProjectionSelection) target() Target {
	return selection.value
}

func (selection *jsonProjectionSelection) enrich(media mediainfo.Media) (bool, error) {
	streamProjections, err := buildStreamProjections(media)
	if err != nil {
		return false, err
	}
	changed, err := addMissingJSONAttributes(*selection.mediaItem, mediaProjectionAttributes(media))
	if err != nil {
		return false, err
	}
	partChanged, err := addMissingJSONAttributes(*selection.part, partProjectionAttributes(media))
	if err != nil {
		return false, err
	}
	changed = changed || partChanged
	streamRaw, exists := (*selection.part)["Stream"]
	if !exists {
		return changed, nil
	}
	streams, err := decodeJSONProjectionList(streamRaw, "Stream")
	if err != nil {
		return false, err
	}
	streamIndex, err := indexJSONStreams(streams.items)
	if err != nil {
		return false, err
	}
	for _, projection := range streamProjections {
		stream, ok := streamIndex[projection.key]
		if !ok {
			continue
		}
		streamChanged, err := addMissingJSONAttributes(*stream, projection.attributes)
		if err != nil {
			return false, err
		}
		changed = changed || streamChanged
	}
	if changed {
		encoded, err := encodeJSONProjectionList(streams)
		if err != nil {
			return false, err
		}
		(*selection.part)["Stream"] = encoded
	}
	if changed {
		if err := selection.commit(); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func (selection *jsonProjectionSelection) commit() error {
	parts, err := encodeJSONProjectionList(selection.parts)
	if err != nil {
		return err
	}
	(*selection.mediaItem)["Part"] = parts
	media, err := encodeJSONProjectionList(selection.media)
	if err != nil {
		return err
	}
	// The metadata list has exactly one item by selection contract.
	(*selection.metadata.items[0])["Media"] = media
	metadata, err := encodeJSONProjectionList(selection.metadata)
	if err != nil {
		return err
	}
	selection.container["Metadata"] = metadata
	container, err := json.Marshal(selection.container)
	if err != nil {
		return err
	}
	selection.document.root["MediaContainer"] = container
	return nil
}

func partFromJSON(object *jsonObject) (Part, error) {
	id, err := requiredJSONScalarString(*object, "id")
	if err != nil {
		return Part{}, err
	}
	file, err := requiredJSONString(*object, "file")
	if err != nil {
		return Part{}, err
	}
	key, err := optionalJSONString(*object, "key")
	if err != nil {
		return Part{}, err
	}
	return Part{ID: id, Key: key, File: file}, nil
}

func requiredJSONScalarString(object jsonObject, name string) (string, error) {
	raw, ok := object[name]
	if !ok {
		return "", fmt.Errorf("Plex JSON metadata has no %s", name)
	}
	value, ok := jsonRawScalarString(raw)
	if !ok || value == "" {
		return "", fmt.Errorf("Plex JSON metadata %s is not a non-empty scalar", name)
	}
	return value, nil
}

func requiredJSONString(object jsonObject, name string) (string, error) {
	raw, ok := object[name]
	if !ok {
		return "", fmt.Errorf("Plex JSON metadata has no %s", name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", fmt.Errorf("Plex JSON metadata %s is not a non-empty string", name)
	}
	return value, nil
}

func optionalJSONString(object jsonObject, name string) (string, error) {
	raw, ok := object[name]
	if !ok {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("Plex JSON metadata %s is not a string", name)
	}
	return value, nil
}

func jsonRawScalarString(raw json.RawMessage) (string, bool) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", false
	}
	return stringValue(value)
}

func jsonTargetNeedsEnrichment(media, part *jsonObject) (bool, error) {
	needs := false
	for _, name := range mediaProjectionAttributeNames {
		if _, present := (*media)[name]; !present {
			needs = true
		}
	}
	for _, name := range partProjectionAttributeNames {
		if _, present := (*part)[name]; !present {
			needs = true
		}
	}
	streamRaw, exists := (*part)["Stream"]
	if !exists {
		return needs, nil
	}
	streams, err := decodeJSONProjectionList(streamRaw, "Stream")
	if err != nil {
		return false, err
	}
	if _, err := indexJSONStreams(streams.items); err != nil {
		return false, err
	}
	for _, stream := range streams.items {
		key, present, err := jsonStreamIdentity(*stream)
		if err != nil {
			return false, err
		}
		if !present {
			continue
		}
		for _, name := range streamProjectionAttributeNames(key.typeName) {
			if _, present := (*stream)[name]; !present {
				needs = true
			}
		}
	}
	return needs, nil
}

func indexJSONStreams(streams []*jsonObject) (map[streamKey]*jsonObject, error) {
	result := make(map[streamKey]*jsonObject, len(streams))
	for _, stream := range streams {
		key, present, err := jsonStreamIdentity(*stream)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("Plex metadata has duplicate Stream identity %s/%d", key.typeName, key.index)
		}
		result[key] = stream
	}
	return result, nil
}

func jsonStreamIdentity(object jsonObject) (streamKey, bool, error) {
	typeRaw, typePresent := object["streamType"]
	indexRaw, indexPresent := object["index"]
	if !typePresent || !indexPresent {
		return streamKey{}, false, nil
	}
	typeName, ok := jsonRawScalarString(typeRaw)
	if !ok || strings.TrimSpace(typeName) == "" {
		return streamKey{}, false, errors.New("Plex JSON metadata Stream has an invalid streamType")
	}
	indexValue, ok := jsonRawScalarString(indexRaw)
	if !ok {
		return streamKey{}, false, errors.New("Plex JSON metadata Stream has an invalid index")
	}
	index, err := strconv.Atoi(strings.TrimSpace(indexValue))
	if err != nil || index < 0 {
		return streamKey{}, false, errors.New("Plex JSON metadata Stream has an invalid index")
	}
	key, ok := mediaStreamKey(typeName, index)
	if !ok {
		return streamKey{}, false, errors.New("Plex JSON metadata Stream has an invalid identity")
	}
	return key, true, nil
}

func addMissingJSONAttributes(object jsonObject, attributes []projectionAttribute) (bool, error) {
	changed := false
	for _, attribute := range attributes {
		if _, present := object[attribute.name]; present {
			continue
		}
		var raw []byte
		var err error
		if attribute.number {
			raw = []byte(attribute.value)
		} else {
			raw, err = json.Marshal(attribute.value)
			if err != nil {
				return false, err
			}
		}
		object[attribute.name] = append(json.RawMessage(nil), raw...)
		changed = true
	}
	return changed, nil
}

func (document *jsonProjectionDocument) encode() ([]byte, error) {
	encoded, err := json.Marshal(document.root)
	if err != nil {
		return nil, fmt.Errorf("encode Plex JSON metadata: %w", err)
	}
	result := make([]byte, 0, len(document.prefix)+len(encoded)+len(document.suffix))
	result = append(result, document.prefix...)
	result = append(result, encoded...)
	result = append(result, document.suffix...)
	return result, nil
}
