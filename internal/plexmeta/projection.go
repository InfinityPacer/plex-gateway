package plexmeta

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/InfinityPacer/plex-gateway/internal/mediainfo"
)

// Target identifies one Plex item and reports whether the endpoint-specific
// native response shape is missing a whitelisted technical field. A missing
// Stream in item metadata is projectable, but generated Stream elements never
// claim Plex-owned IDs or playback-selection state.
type Target struct {
	RatingKey string
	Part      Part
	// NeedsEnrichment follows the endpoint contract: collection targets consider
	// Media/Part only, while item targets also consider descriptive Stream data.
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
	target := selection.target()
	streamNeedsEnrichment, err := selection.streamsNeedEnrichment()
	if err != nil {
		return Target{}, err
	}
	target.NeedsEnrichment = target.NeedsEnrichment || streamNeedsEnrichment
	return target, nil
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

// EnrichMetadata fills only absent whitelisted Media, Part, and matching
// Stream attributes. When item metadata has no Stream elements, descriptive
// elements are created without Plex-owned IDs or playback-selection state.
// The input body is never modified in place. When no field is filled, the
// original bytes are returned unchanged.
func EnrichMetadata(body []byte, contentType, ratingKey string, media mediainfo.Media) ([]byte, bool, error) {
	document, selection, err := parseProjection(body, contentType, ratingKey)
	if err != nil {
		return nil, false, err
	}
	changed, err := selection.enrich(media, true)
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

// SelectEnrichmentTargets returns independently projectable items from a Plex
// collection response. Items with multiple Media versions or Parts are left
// untouched because a cache record cannot be mapped to them without guessing.
func SelectEnrichmentTargets(body []byte, contentType string) ([]Target, error) {
	document, err := parseProjectionDocument(body, contentType)
	if err != nil {
		return nil, err
	}
	selections, err := document.selectTargets()
	if err != nil {
		return nil, err
	}
	targets := make([]Target, 0, len(selections))
	for _, selection := range selections {
		targets = append(targets, selection.target())
	}
	return targets, nil
}

// EnrichMetadataTargets projects cached MediaInfo summaries into matching
// items of one collection response. Plex collection responses normally omit
// Stream elements, so collection projection never reads, creates, or changes
// them. Existing Stream elements retain Plex's native representation.
func EnrichMetadataTargets(body []byte, contentType string, records map[string]mediainfo.Media) ([]byte, int, error) {
	document, err := parseProjectionDocument(body, contentType)
	if err != nil {
		return nil, 0, err
	}
	selections, err := document.selectTargets()
	if err != nil {
		return nil, 0, err
	}
	changedItems := 0
	for _, selection := range selections {
		media, ok := records[selection.target().RatingKey]
		if !ok {
			continue
		}
		changed, err := selection.enrich(media, false)
		if err != nil {
			return nil, 0, err
		}
		if changed {
			changedItems++
		}
	}
	if changedItems == 0 {
		return append([]byte(nil), body...), 0, nil
	}
	result, err := document.encode()
	if err != nil {
		return nil, 0, err
	}
	return result, changedItems, nil
}

type projectionDocument interface {
	selectTarget(string) (projectionSelection, error)
	selectTargets() ([]projectionSelection, error)
	encode() ([]byte, error)
}

type projectionSelection interface {
	target() Target
	streamsNeedEnrichment() (bool, error)
	enrich(mediainfo.Media, bool) (bool, error)
}

func parseProjection(body []byte, contentType, ratingKey string) (projectionDocument, projectionSelection, error) {
	ratingKey, err := normalizeProjectionRatingKey(ratingKey)
	if err != nil {
		return nil, nil, err
	}
	document, err := parseProjectionDocument(body, contentType)
	if err != nil {
		return nil, nil, err
	}
	selection, err := document.selectTarget(ratingKey)
	if err != nil {
		return nil, nil, err
	}
	return document, selection, nil
}

func parseProjectionDocument(body []byte, contentType string) (projectionDocument, error) {
	trimmed := trimProjectionBody(body)
	if len(trimmed) == 0 {
		return nil, errors.New("Plex metadata response is empty")
	}
	if strings.Contains(strings.ToLower(contentType), "json") || trimmed[0] == '{' || trimmed[0] == '[' {
		document, err := parseJSONProjectionDocument(body)
		if err != nil {
			return nil, err
		}
		return document, nil
	}
	document, err := parseXMLProjectionDocument(body)
	if err != nil {
		return nil, err
	}
	return document, nil
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
	name    string
	value   string
	number  bool
	boolean bool
}

func textProjectionAttribute(name, value string) projectionAttribute {
	return projectionAttribute{name: name, value: value}
}

func integerProjectionAttribute(name string, value int64) projectionAttribute {
	return projectionAttribute{name: name, value: strconv.FormatInt(value, 10), number: true}
}

func numberProjectionAttribute(name, value string) projectionAttribute {
	return projectionAttribute{name: name, value: value, number: true}
}

func booleanProjectionAttribute(name string, value bool) projectionAttribute {
	if value {
		return projectionAttribute{name: name, value: "1", boolean: true}
	}
	return projectionAttribute{name: name, value: "0", boolean: true}
}

const strmControlSizeThreshold int64 = 1 << 20

// bitrateKbps converts ffprobe's bits-per-second value to Plex's integer
// kilobits-per-second representation. A positive sub-kilobit value remains
// visible as one rather than being rounded down to zero.
func bitrateKbps(value int64) int64 {
	if value <= 0 {
		return 0
	}
	value /= 1000
	if value == 0 {
		return 1
	}
	return value
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
		attributes = append(attributes, integerProjectionAttribute("bitrate", bitrateKbps(media.Bitrate)))
	}
	if media.Width > 0 {
		attributes = append(attributes, integerProjectionAttribute("width", int64(media.Width)))
	}
	if media.Height > 0 {
		attributes = append(attributes, integerProjectionAttribute("height", int64(media.Height)))
	}
	if aspectRatio := plexAspectRatio(media.AspectRatio); aspectRatio != "" {
		attributes = append(attributes, numberProjectionAttribute("aspectRatio", aspectRatio))
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
	if frameRate := plexMediaFrameRate(media.FrameRate); frameRate != "" {
		attributes = append(attributes, textProjectionAttribute("videoFrameRate", frameRate))
	}
	if profile := plexProfile(media.VideoProfile); profile != "" {
		attributes = append(attributes, textProjectionAttribute("videoProfile", profile))
	}
	if profile := plexProfile(media.AudioProfile); profile != "" {
		attributes = append(attributes, textProjectionAttribute("audioProfile", profile))
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
	if profile := plexProfile(media.VideoProfile); profile != "" {
		attributes = append(attributes, textProjectionAttribute("videoProfile", profile))
	}
	if profile := plexProfile(media.AudioProfile); profile != "" {
		attributes = append(attributes, textProjectionAttribute("audioProfile", profile))
	}
	return attributes
}

func streamProjectionAttributes(stream mediainfo.Stream) []projectionAttribute {
	attributes := make([]projectionAttribute, 0, 35)
	if stream.Codec != "" {
		attributes = append(attributes, textProjectionAttribute("codec", plexCodec(stream.Codec)))
	}
	if codecID := plexCodecID(stream.CodecTag); codecID != "" {
		attributes = append(attributes, textProjectionAttribute("codecID", codecID))
	}
	if profile := plexProfile(stream.Profile); profile != "" {
		attributes = append(attributes, textProjectionAttribute("profile", profile))
	}
	if stream.Level > 0 {
		attributes = append(attributes, integerProjectionAttribute("level", int64(stream.Level)))
	}
	if stream.Bitrate > 0 {
		attributes = append(attributes, integerProjectionAttribute("bitrate", bitrateKbps(stream.Bitrate)))
	}
	if stream.Width > 0 {
		attributes = append(attributes, integerProjectionAttribute("width", int64(stream.Width)))
	}
	if stream.Height > 0 {
		attributes = append(attributes, integerProjectionAttribute("height", int64(stream.Height)))
	}
	frameRate := plexFrameRate(firstProjectionString(stream.FrameRate, stream.AverageFrameRate))
	if frameRate != "" {
		attributes = append(attributes, projectionAttribute{name: "frameRate", value: frameRate, number: true})
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
	if stream.ChannelLayout != "" && canonicalStreamType(stream.Type) == "2" {
		attributes = append(attributes, textProjectionAttribute("audioChannelLayout", stream.ChannelLayout))
	}
	if code, ok := plexLanguageCode(stream.Language); ok {
		attributes = append(attributes, textProjectionAttribute("languageCode", code))
	}
	if stream.Title != "" {
		attributes = append(attributes, textProjectionAttribute("title", stream.Title))
	}
	if canonicalStreamType(stream.Type) == "1" {
		if displayTitle, extendedDisplayTitle := videoDisplayTitles(stream); displayTitle != "" {
			attributes = append(attributes, textProjectionAttribute("displayTitle", displayTitle))
			if extendedDisplayTitle != "" {
				attributes = append(attributes, textProjectionAttribute("extendedDisplayTitle", extendedDisplayTitle))
			}
		}
	}
	if stream.DolbyVision != nil && canonicalStreamType(stream.Type) == "1" {
		attributes = append(attributes, dolbyVisionProjectionAttributes(stream.DolbyVision)...)
	}
	attributes = append(attributes, dispositionProjectionAttributes(stream.Disposition)...)
	return attributes
}

func plexCodecID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "[0]") || strings.EqualFold(value, "0x0000") {
		return ""
	}
	for index := 0; index < len(value); index++ {
		if value[index] < 0x21 || value[index] > 0x7e {
			return ""
		}
	}
	return value
}

// plexProfile follows the lowercase profile vocabulary emitted by Plex for
// Media, Part, and Stream technical fields. Display titles retain ffprobe's
// presentation casing independently.
func plexProfile(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// plexAspectRatio converts ffprobe ratios to Plex's two-decimal numeric Media
// representation. Stream displayAspectRatio remains the original ratio text.
func plexAspectRatio(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	numerator, denominator, ratio := strings.Cut(value, ":")
	if !ratio {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || parsed <= 0 || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return ""
		}
		return strconv.FormatFloat(parsed, 'f', 2, 64)
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(numerator), 64)
	if err != nil || n <= 0 {
		return ""
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(denominator), 64)
	if err != nil || d <= 0 {
		return ""
	}
	return strconv.FormatFloat(n/d, 'f', 2, 64)
}

// plexCodec keeps the normalized codec vocabulary expected by Plex clients.
// ffprobe may expose a container-specific subtitle codec name that Plex
// represents using its short format name.
func plexCodec(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "hdmv_pgs_subtitle", "pgs":
		return "pgs"
	case "subrip":
		return "srt"
	default:
		return value
	}
}

func plexLanguageCode(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if len(value) != 2 && len(value) != 3 {
		return "", false
	}
	for index := 0; index < len(value); index++ {
		if (value[index] < 'a' || value[index] > 'z') && (value[index] < 'A' || value[index] > 'Z') {
			return "", false
		}
	}
	return strings.ToLower(value), true
}

func videoDisplayTitles(stream mediainfo.Stream) (string, string) {
	label := ""
	switch strings.ToLower(strings.TrimSpace(stream.HDRFormat)) {
	case "dolby_vision", "dolbyvision", "dovi", "dv":
		label = "DoVi"
		if stream.DolbyVision != nil && dolbyVisionHDR10Compatible(stream.DolbyVision.BLCompatID) {
			label = "DoVi/HDR10"
		}
	case "hdr10":
		label = "HDR10"
	case "hlg":
		label = "HLG"
	}
	resolution := plexVideoResolution(stream)
	displayTitle := strings.TrimSpace(strings.Join([]string{resolution, label}, " "))
	if displayTitle == "" {
		return "", ""
	}
	codec := plexDisplayCodec(stream.Codec)
	codecAndProfile := strings.TrimSpace(strings.Join([]string{codec, strings.TrimSpace(stream.Profile)}, " "))
	if codecAndProfile == "" {
		return displayTitle, ""
	}
	return displayTitle, displayTitle + " (" + codecAndProfile + ")"
}

func dolbyVisionHDR10Compatible(compatibilityID int) bool {
	// Plex labels the two observed HDR10-compatible signal identifiers as a
	// combined DoVi/HDR10 presentation. Other identifiers may describe SDR or
	// HLG compatibility and must not be promoted to HDR10.
	return compatibilityID == 1 || compatibilityID == 6
}

// plexFrameRate converts ffprobe ratios to the decimal representation emitted
// by Plex Stream metadata while preserving already-decimal values.
func plexFrameRate(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	numerator, denominator, fraction := strings.Cut(value, "/")
	if !fraction {
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil || parsed <= 0 {
			return ""
		}
		return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(parsed, 'f', 3, 64), "0"), ".")
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(numerator), 64)
	if err != nil || n <= 0 {
		return ""
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(denominator), 64)
	if err != nil || d <= 0 {
		return ""
	}
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(n/d, 'f', 3, 64), "0"), ".")
}

// plexMediaFrameRate converts ffprobe's measured frame rate to Plex's nominal
// media classification. Stream metadata retains the precise decimal value so
// clients can distinguish a 23.976 stream from the Media-level 24p label.
func plexMediaFrameRate(value string) string {
	decimal := plexFrameRate(value)
	if decimal == "" {
		return ""
	}
	fps, err := strconv.ParseFloat(decimal, 64)
	if err != nil || fps <= 0 || math.IsNaN(fps) || math.IsInf(fps, 0) {
		return ""
	}
	if math.Abs(fps-25) < 0.01 {
		return "PAL"
	}
	nominal := math.Round(fps)
	if nominal <= 0 || math.IsInf(nominal, 0) {
		return ""
	}
	return strconv.FormatFloat(nominal, 'f', 0, 64) + "p"
}

func plexVideoResolution(stream mediainfo.Stream) string {
	switch {
	case stream.Width >= 3800 || stream.Height >= 2100:
		return "4K"
	case stream.Height >= 1400:
		return "2K"
	case stream.Height >= 1000:
		return "1080p"
	case stream.Height >= 700:
		return "720p"
	case stream.Height > 0:
		return strconv.Itoa(stream.Height) + "p"
	}
	return ""
}

func plexDisplayCodec(value string) string {
	switch plexCodec(value) {
	case "hevc":
		return "HEVC"
	case "h264":
		return "H.264"
	case "mpeg4":
		return "MPEG-4"
	case "av1":
		return "AV1"
	case "vp9":
		return "VP9"
	default:
		return strings.ToUpper(plexCodec(value))
	}
}

func dolbyVisionProjectionAttributes(dolby *mediainfo.DolbyVision) []projectionAttribute {
	if dolby == nil {
		return nil
	}
	attributes := []projectionAttribute{
		integerProjectionAttribute("DOVIBLCompatID", int64(dolby.BLCompatID)),
		booleanProjectionAttribute("DOVIBLPresent", dolby.BLPresent != 0),
		booleanProjectionAttribute("DOVIELPresent", dolby.ELPresent != 0),
		booleanProjectionAttribute("DOVIPresent", true),
		booleanProjectionAttribute("DOVIRPUPresent", dolby.RPUPresent != 0),
	}
	if dolby.Level > 0 {
		attributes = append(attributes, integerProjectionAttribute("DOVILevel", int64(dolby.Level)))
	}
	if dolby.Profile > 0 {
		attributes = append(attributes, integerProjectionAttribute("DOVIProfile", int64(dolby.Profile)))
	}
	if dolby.VersionMajor > 0 || dolby.VersionMinor > 0 {
		attributes = append(attributes, textProjectionAttribute("DOVIVersion", fmt.Sprintf("%d.%d", dolby.VersionMajor, dolby.VersionMinor)))
	}
	return attributes
}

func dispositionProjectionAttributes(disposition mediainfo.Disposition) []projectionAttribute {
	attributes := make([]projectionAttribute, 0, 3)
	if disposition.Forced {
		attributes = append(attributes, booleanProjectionAttribute("forced", true))
	}
	if disposition.HearingImpaired {
		attributes = append(attributes, booleanProjectionAttribute("hearingImpaired", true))
	}
	if disposition.VisualImpaired {
		attributes = append(attributes, booleanProjectionAttribute("visualImpaired", true))
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
	"codec", "profile", "level", "bitrate", "languageCode", "title",
}

var videoStreamProjectionAttributeNames = []string{
	"width", "height", "frameRate", "refFrames", "pixelFormat", "bitDepth",
	"colorSpace", "colorRange", "colorPrimaries", "colorTrc", "chromaLocation",
	"sampleAspectRatio", "displayAspectRatio", "displayTitle", "extendedDisplayTitle",
}

var dolbyVisionProjectionAttributeNames = []string{
	"DOVIBLCompatID", "DOVIBLPresent", "DOVIELPresent", "DOVILevel",
	"DOVIPresent", "DOVIProfile", "DOVIRPUPresent", "DOVIVersion",
}

var audioStreamProjectionAttributeNames = []string{"samplingRate", "channels", "audioChannelLayout"}

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
		if !projectableStream(stream) {
			continue
		}
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

func projectableStream(stream mediainfo.Stream) bool {
	if stream.Disposition.AttachedPicture {
		return false
	}
	switch canonicalStreamType(stream.Type) {
	case "1", "2", "3":
		return true
	default:
		return false
	}
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
	selection, err := xmlProjectionSelectionFromItem(videos[0])
	if err != nil {
		return nil, err
	}
	if selection.value.RatingKey != ratingKey {
		return nil, errors.New("Plex XML metadata ratingKey does not match the requested item")
	}
	return selection, nil
}

func (document *xmlProjectionDocument) selectTargets() ([]projectionSelection, error) {
	roots := xmlProjectionElements(document.nodes)
	if len(roots) != 1 || roots[0].start.Name.Local != "MediaContainer" {
		return nil, errors.New("Plex XML metadata has no unique MediaContainer root")
	}
	videos := directXMLElements(roots[0], "Video")
	selections := make([]projectionSelection, 0, len(videos))
	seen := make(map[string]struct{}, len(videos))
	for _, item := range videos {
		selection, err := xmlProjectionSelectionFromItem(item)
		if err != nil {
			continue
		}
		if _, duplicate := seen[selection.value.RatingKey]; duplicate {
			return nil, errors.New("Plex XML metadata has duplicate ratingKey items")
		}
		seen[selection.value.RatingKey] = struct{}{}
		selections = append(selections, selection)
	}
	return selections, nil
}

func xmlProjectionSelectionFromItem(item *xmlProjectionElement) (*xmlProjectionSelection, error) {
	itemRatingKey, present, err := xmlAttribute(item.start.Attr, "ratingKey")
	if err != nil {
		return nil, err
	}
	if !present || itemRatingKey == "" {
		return nil, errors.New("Plex XML metadata item has no ratingKey")
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
	needs, err := xmlMediaPartNeedsEnrichment(mediaItems[0], parts[0])
	if err != nil {
		return nil, err
	}
	return &xmlProjectionSelection{
		value: Target{RatingKey: itemRatingKey, Part: part, NeedsEnrichment: needs},
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

func (selection *xmlProjectionSelection) streamsNeedEnrichment() (bool, error) {
	return xmlStreamsNeedEnrichment(selection.part)
}

func (selection *xmlProjectionSelection) enrich(media mediainfo.Media, projectStreams bool) (bool, error) {
	changed := addMissingXMLAttributes(selection.media, mediaProjectionAttributes(media))
	changed = addMissingXMLAttributes(selection.part, partProjectionAttributes(media)) || changed
	changed = replaceSTRMPartSizeXML(selection.part, media.Size) || changed
	if !projectStreams {
		return changed, nil
	}
	streamElements := directXMLElements(selection.part, "Stream")
	streamProjections, err := buildStreamProjections(media)
	if err != nil {
		return false, err
	}
	streamIndex, err := indexXMLStreams(streamElements)
	if err != nil {
		return false, err
	}
	if len(streamElements) == 0 {
		for _, projection := range streamProjections {
			selection.part.children = append(selection.part.children, &xmlProjectionNode{element: newXMLStream(projection)})
			changed = true
		}
		return changed, nil
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

func newXMLStream(projection streamProjection) *xmlProjectionElement {
	attributes := make([]xml.Attr, 0, len(projection.attributes)+2)
	attributes = append(attributes,
		xml.Attr{Name: xml.Name{Local: "streamType"}, Value: projection.key.typeName},
		xml.Attr{Name: xml.Name{Local: "index"}, Value: strconv.Itoa(projection.key.index)},
	)
	for _, attribute := range projection.attributes {
		attributes = append(attributes, xml.Attr{Name: xml.Name{Local: attribute.name}, Value: attribute.value})
	}
	return &xmlProjectionElement{start: xml.StartElement{Name: xml.Name{Local: "Stream"}, Attr: attributes}}
}

func replaceSTRMPartSizeXML(part *xmlProjectionElement, mediaSize int64) bool {
	if mediaSize <= strmControlSizeThreshold {
		return false
	}
	file, present, err := xmlAttribute(part.start.Attr, "file")
	if err != nil || !present || !isSTRMPath(file) {
		return false
	}
	for index := range part.start.Attr {
		if part.start.Attr[index].Name.Local != "size" {
			continue
		}
		current, err := strconv.ParseInt(strings.TrimSpace(part.start.Attr[index].Value), 10, 64)
		if err != nil || current < 0 || current > strmControlSizeThreshold || mediaSize <= current {
			return false
		}
		part.start.Attr[index].Value = strconv.FormatInt(mediaSize, 10)
		return true
	}
	return false
}

func isSTRMPath(value string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(value)), ".strm")
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

func xmlMediaPartNeedsEnrichment(media, part *xmlProjectionElement) (bool, error) {
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
	if correction, err := xmlSTRMPartSizeNeedsCorrection(part); err != nil {
		return false, err
	} else if correction {
		needs = true
	}
	return needs, nil
}

func xmlStreamsNeedEnrichment(part *xmlProjectionElement) (bool, error) {
	streams := directXMLElements(part, "Stream")
	if _, err := indexXMLStreams(streams); err != nil {
		return false, err
	}
	if len(streams) == 0 {
		return true, nil
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
			present, err := xmlProjectionAttributePresent(stream, name)
			if err != nil {
				return false, err
			}
			if !present {
				return true, nil
			}
		}
		if key.typeName == "1" {
			incomplete, err := xmlDolbyVisionFieldsIncomplete(stream)
			if err != nil {
				return false, err
			}
			if incomplete {
				return true, nil
			}
		}
	}
	return false, nil
}

func xmlSTRMPartSizeNeedsCorrection(part *xmlProjectionElement) (bool, error) {
	file, present, err := xmlAttribute(part.start.Attr, "file")
	if err != nil || !present || !isSTRMPath(file) {
		return false, err
	}
	size, present, err := xmlAttribute(part.start.Attr, "size")
	if err != nil || !present {
		return false, err
	}
	value, err := strconv.ParseInt(strings.TrimSpace(size), 10, 64)
	return err == nil && value >= 0 && value <= strmControlSizeThreshold, nil
}

func xmlDolbyVisionFieldsIncomplete(stream *xmlProjectionElement) (bool, error) {
	displayTitle, present, err := xmlAttribute(stream.start.Attr, "displayTitle")
	if err != nil {
		return false, err
	}
	dolbyKnown := present && strings.Contains(strings.ToLower(displayTitle), "dovi")
	for _, name := range dolbyVisionProjectionAttributeNames {
		_, fieldPresent, err := xmlAttribute(stream.start.Attr, name)
		if err != nil {
			return false, err
		}
		dolbyKnown = dolbyKnown || fieldPresent
	}
	if !dolbyKnown {
		return false, nil
	}
	for _, name := range dolbyVisionProjectionAttributeNames {
		_, fieldPresent, err := xmlAttribute(stream.start.Attr, name)
		if err != nil || !fieldPresent {
			return !fieldPresent, err
		}
	}
	return false, nil
}

func xmlProjectionAttributePresent(stream *xmlProjectionElement, name string) (bool, error) {
	_, present, err := xmlAttribute(stream.start.Attr, name)
	if err != nil || present {
		return present, err
	}
	for _, alias := range projectionAttributeAliases(name) {
		_, present, err = xmlAttribute(stream.start.Attr, alias)
		if err != nil || present {
			return present, err
		}
	}
	return false, nil
}

func projectionAttributeAliases(name string) []string {
	switch name {
	case "languageCode":
		return []string{"language"}
	case "audioChannelLayout":
		return []string{"channelLayout"}
	default:
		return nil
	}
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
	container, metadata, err := document.directMetadata()
	if err != nil {
		return nil, err
	}
	if len(metadata.items) != 1 {
		return nil, errors.New("Plex JSON metadata must contain one Metadata item")
	}
	selection, err := jsonProjectionSelectionFromItem(document, container, metadata, metadata.items[0])
	if err != nil {
		return nil, err
	}
	if selection.value.RatingKey != ratingKey {
		return nil, errors.New("Plex JSON metadata ratingKey does not match the requested item")
	}
	return selection, nil
}

func (document *jsonProjectionDocument) selectTargets() ([]projectionSelection, error) {
	container, metadata, err := document.directMetadata()
	if err != nil {
		return nil, err
	}
	selections := make([]projectionSelection, 0, len(metadata.items))
	seen := make(map[string]struct{}, len(metadata.items))
	for _, item := range metadata.items {
		selection, err := jsonProjectionSelectionFromItem(document, container, metadata, item)
		if err != nil {
			continue
		}
		if _, duplicate := seen[selection.value.RatingKey]; duplicate {
			return nil, errors.New("Plex JSON metadata has duplicate ratingKey items")
		}
		seen[selection.value.RatingKey] = struct{}{}
		selections = append(selections, selection)
	}
	return selections, nil
}

func (document *jsonProjectionDocument) directMetadata() (jsonObject, jsonProjectionList, error) {
	containerRaw, ok := document.root["MediaContainer"]
	if !ok {
		return nil, jsonProjectionList{}, errors.New("Plex JSON metadata has no MediaContainer")
	}
	container, err := decodeJSONProjectionObject(bytes.TrimSpace(containerRaw))
	if err != nil {
		return nil, jsonProjectionList{}, fmt.Errorf("parse Plex JSON MediaContainer: %w", err)
	}
	metadataRaw, ok := container["Metadata"]
	if !ok {
		return nil, jsonProjectionList{}, errors.New("Plex JSON metadata has no Metadata item")
	}
	metadata, err := decodeJSONProjectionList(metadataRaw, "Metadata")
	if err != nil {
		return nil, jsonProjectionList{}, err
	}
	return container, metadata, nil
}

func jsonProjectionSelectionFromItem(
	document *jsonProjectionDocument,
	container jsonObject,
	metadata jsonProjectionList,
	item *jsonObject,
) (*jsonProjectionSelection, error) {
	itemRatingKey, err := requiredJSONScalarString(*item, "ratingKey")
	if err != nil {
		return nil, err
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
	needs, err := jsonMediaPartNeedsEnrichment(mediaItem, partItem)
	if err != nil {
		return nil, err
	}
	return &jsonProjectionSelection{
		value:        Target{RatingKey: itemRatingKey, Part: part, NeedsEnrichment: needs},
		document:     document,
		container:    container,
		metadata:     metadata,
		metadataItem: item,
		media:        mediaItems,
		parts:        parts,
		mediaItem:    mediaItem,
		part:         partItem,
	}, nil
}

type jsonProjectionSelection struct {
	value        Target
	document     *jsonProjectionDocument
	container    jsonObject
	metadata     jsonProjectionList
	metadataItem *jsonObject
	media        jsonProjectionList
	parts        jsonProjectionList
	mediaItem    *jsonObject
	part         *jsonObject
}

func (selection *jsonProjectionSelection) target() Target {
	return selection.value
}

func (selection *jsonProjectionSelection) streamsNeedEnrichment() (bool, error) {
	return jsonStreamsNeedEnrichment(selection.part)
}

func (selection *jsonProjectionSelection) enrich(media mediainfo.Media, projectStreams bool) (bool, error) {
	changed, err := addMissingJSONAttributes(*selection.mediaItem, mediaProjectionAttributes(media))
	if err != nil {
		return false, err
	}
	partChanged, err := addMissingJSONAttributes(*selection.part, partProjectionAttributes(media))
	if err != nil {
		return false, err
	}
	changed = changed || partChanged
	partSizeChanged, err := replaceSTRMPartSizeJSON(*selection.part, media.Size)
	if err != nil {
		return false, err
	}
	changed = changed || partSizeChanged
	if !projectStreams {
		if changed {
			if err := selection.commit(); err != nil {
				return false, err
			}
		}
		return changed, nil
	}
	streamRaw, exists := (*selection.part)["Stream"]
	streamProjections, err := buildStreamProjections(media)
	if err != nil {
		return false, err
	}
	if !exists {
		if len(streamProjections) > 0 {
			if err := setGeneratedJSONStreams(*selection.part, streamProjections); err != nil {
				return false, err
			}
			changed = true
		}
	} else {
		streams, err := decodeJSONProjectionList(streamRaw, "Stream")
		if err != nil {
			return false, err
		}
		if len(streams.items) == 0 {
			if len(streamProjections) > 0 {
				if err := setGeneratedJSONStreams(*selection.part, streamProjections); err != nil {
					return false, err
				}
				changed = true
			}
		} else {
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
		}
	}
	if changed {
		if err := selection.commit(); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func setGeneratedJSONStreams(part jsonObject, projections []streamProjection) error {
	streams := jsonProjectionList{items: make([]*jsonObject, 0, len(projections)), array: true}
	for _, projection := range projections {
		stream := make(jsonObject)
		if _, err := addMissingJSONAttributes(stream, generatedStreamAttributes(projection)); err != nil {
			return err
		}
		streams.items = append(streams.items, &stream)
	}
	encoded, err := encodeJSONProjectionList(streams)
	if err != nil {
		return err
	}
	part["Stream"] = encoded
	return nil
}

func generatedStreamAttributes(projection streamProjection) []projectionAttribute {
	attributes := make([]projectionAttribute, 0, len(projection.attributes)+2)
	attributes = append(attributes,
		integerProjectionAttribute("streamType", int64(streamTypeNumber(projection.key.typeName))),
		integerProjectionAttribute("index", int64(projection.key.index)),
	)
	return append(attributes, projection.attributes...)
}

func streamTypeNumber(typeName string) int {
	switch typeName {
	case "1":
		return 1
	case "2":
		return 2
	case "3":
		return 3
	default:
		return 0
	}
}

func replaceSTRMPartSizeJSON(part jsonObject, mediaSize int64) (bool, error) {
	if mediaSize <= strmControlSizeThreshold {
		return false, nil
	}
	file, err := requiredJSONString(part, "file")
	if err != nil || !isSTRMPath(file) {
		return false, nil
	}
	raw, present := part["size"]
	if !present {
		return false, nil
	}
	current, ok := jsonRawInt64(raw)
	if !ok || current < 0 || current > strmControlSizeThreshold || mediaSize <= current {
		return false, nil
	}
	part["size"] = json.RawMessage(strconv.FormatInt(mediaSize, 10))
	return true, nil
}

func jsonRawInt64(raw json.RawMessage) (int64, bool) {
	value, ok := jsonRawScalarString(raw)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return parsed, err == nil
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
	(*selection.metadataItem)["Media"] = media
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

func jsonMediaPartNeedsEnrichment(media, part *jsonObject) (bool, error) {
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
	if jsonSTRMPartSizeNeedsCorrection(*part) {
		needs = true
	}
	return needs, nil
}

func jsonStreamsNeedEnrichment(part *jsonObject) (bool, error) {
	streamRaw, exists := (*part)["Stream"]
	if !exists {
		return true, nil
	}
	streams, err := decodeJSONProjectionList(streamRaw, "Stream")
	if err != nil {
		return false, err
	}
	if len(streams.items) == 0 {
		return true, nil
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
			if !jsonProjectionAttributePresent(*stream, name) {
				return true, nil
			}
		}
		if key.typeName == "1" && jsonDolbyVisionFieldsIncomplete(*stream) {
			return true, nil
		}
	}
	return false, nil
}

func jsonSTRMPartSizeNeedsCorrection(part jsonObject) bool {
	file, err := requiredJSONString(part, "file")
	if err != nil || !isSTRMPath(file) {
		return false
	}
	raw, present := part["size"]
	if !present {
		return false
	}
	value, ok := jsonRawInt64(raw)
	return ok && value >= 0 && value <= strmControlSizeThreshold
}

func jsonDolbyVisionFieldsIncomplete(stream jsonObject) bool {
	dolbyKnown := false
	if raw, present := stream["displayTitle"]; present {
		if value, ok := jsonRawScalarString(raw); ok {
			dolbyKnown = strings.Contains(strings.ToLower(value), "dovi")
		}
	}
	for _, name := range dolbyVisionProjectionAttributeNames {
		_, present := stream[name]
		dolbyKnown = dolbyKnown || present
	}
	if !dolbyKnown {
		return false
	}
	for _, name := range dolbyVisionProjectionAttributeNames {
		if _, present := stream[name]; !present {
			return true
		}
	}
	return false
}

func jsonProjectionAttributePresent(stream jsonObject, name string) bool {
	if _, present := stream[name]; present {
		return true
	}
	for _, alias := range projectionAttributeAliases(name) {
		if _, present := stream[alias]; present {
			return true
		}
	}
	return false
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
		if attribute.boolean {
			if attribute.value == "1" {
				raw = []byte("true")
			} else {
				raw = []byte("false")
			}
		} else if attribute.number {
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
