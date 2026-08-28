package plexmeta

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"reflect"
	"strconv"
	"testing"

	"github.com/InfinityPacer/plex-gateway/internal/mediainfo"
)

func TestSelectEnrichmentTargetStrictlySelectsXMLItem(t *testing.T) {
	body := []byte(`<MediaContainer rootExtra="preserve"><Video ratingKey="42" itemExtra="preserve"><Media mediaExtra="preserve"><Part id="7" key="/library/parts/7/1/file" file="/cloud/movie.strm" partExtra="preserve"/></Media></Video></MediaContainer>`)
	want := Part{ID: "7", Key: "/library/parts/7/1/file", File: "/cloud/movie.strm"}

	target, err := SelectEnrichmentTarget(body, "application/xml", "42")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(target.Part, want) || !target.NeedsEnrichment {
		t.Fatalf("target = %#v, want part %#v and enrichment", target, want)
	}
	part, err := SelectEnrichmentPart(body, "application/xml", "42")
	if err != nil || !reflect.DeepEqual(part, want) {
		t.Fatalf("part = %#v, err = %v", part, err)
	}
}

func TestSelectEnrichmentTargetStrictlySelectsJSONItem(t *testing.T) {
	body := []byte(`{"MediaContainer":{"rootExtra":"preserve","Metadata":[{"ratingKey":42,"itemExtra":"preserve","Media":[{"mediaExtra":"preserve","Part":[{"id":7,"key":"/library/parts/7/1/file","file":"/cloud/movie.strm","partExtra":"preserve"}]}]}]}}`)
	target, err := SelectEnrichmentTarget(body, "application/json", "42")
	if err != nil {
		t.Fatal(err)
	}
	want := Part{ID: "7", Key: "/library/parts/7/1/file", File: "/cloud/movie.strm"}
	if !reflect.DeepEqual(target.Part, want) || !target.NeedsEnrichment {
		t.Fatalf("target = %#v, want part %#v and enrichment", target, want)
	}
}

func TestEnrichMetadataXMLFillsOnlyMissingSafeFields(t *testing.T) {
	body := []byte(`<MediaContainer rootExtra="preserve"><Video ratingKey="42" itemExtra="preserve"><Guid id="com.example/movie"/><Media mediaExtra="preserve" container="plex-container" videoCodec="plex-codec"><Part id="7" key="/library/parts/7/1/file" file="/cloud/movie.strm" partExtra="preserve" duration="999"><Stream id="plex-stream-id" streamType="1" index="0" codec="plex-stream-codec" selected="1" default="1" decision="directplay" streamExtra="preserve"/><Stream streamType="2" index="1" streamExtra="audio"/></Part><UnknownNode unknownAttr="keep"><UnknownChild/></UnknownNode></Media></Video></MediaContainer>`)
	original := append([]byte(nil), body...)

	result, changed, err := EnrichMetadata(body, "application/xml", "42", projectionTestMedia())
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("EnrichMetadata reported no change")
	}
	if !bytes.Equal(body, original) {
		t.Fatal("EnrichMetadata modified its input body")
	}

	attrs := xmlProjectionAttributeSets(t, result)
	media := attrs["Media"]
	part := attrs["Part"]
	videoStream := attrs["Stream#1"]
	audioStream := attrs["Stream#2"]
	if media["container"] != "plex-container" || media["videoCodec"] != "plex-codec" || media["videoResolution"] != "4k" || media["duration"] != "123000" {
		t.Fatalf("Media attributes = %#v", media)
	}
	if part["id"] != "7" || part["file"] != "/cloud/movie.strm" || part["duration"] != "999" || part["size"] != "987654321" || part["container"] != "mkv" {
		t.Fatalf("Part attributes = %#v", part)
	}
	if videoStream["id"] != "plex-stream-id" || videoStream["codec"] != "plex-stream-codec" || videoStream["profile"] != "Main 10" || videoStream["width"] != "3840" || videoStream["bitDepth"] != "10" {
		t.Fatalf("video Stream attributes = %#v", videoStream)
	}
	if audioStream["codec"] != "aac" || audioStream["channels"] != "6" || audioStream["samplingRate"] != "48000" {
		t.Fatalf("audio Stream attributes = %#v", audioStream)
	}
	for _, name := range []string{"selected", "default", "decision"} {
		if videoStream[name] != "1" && name != "decision" {
			t.Fatalf("forbidden Stream attribute %q changed: %#v", name, videoStream)
		}
	}
	if videoStream["decision"] != "directplay" || videoStream["streamExtra"] != "preserve" || audioStream["streamExtra"] != "audio" {
		t.Fatalf("existing or unknown Stream attributes were not preserved: video=%#v audio=%#v", videoStream, audioStream)
	}
	if !bytes.Contains(result, []byte(`rootExtra="preserve"`)) || !bytes.Contains(result, []byte(`UnknownNode unknownAttr="keep"`)) || !bytes.Contains(result, []byte(`UnknownChild`)) {
		t.Fatalf("unknown XML attributes/nodes were not preserved: %s", result)
	}
}

func TestEnrichMetadataJSONFillsOnlyMissingSafeFields(t *testing.T) {
	body := []byte(`{"MediaContainer":{"rootExtra":"preserve","Metadata":[{"ratingKey":"42","itemExtra":"preserve","Media":[{"mediaExtra":"preserve","container":"plex-container","videoCodec":"plex-codec","Part":[{"id":7,"key":"/library/parts/7/1/file","file":"/cloud/movie.strm","partExtra":"preserve","duration":999,"Stream":[{"id":"plex-stream-id","streamType":1,"index":0,"codec":"plex-stream-codec","selected":1,"default":1,"decision":"directplay","streamExtra":"preserve"},{"streamType":2,"index":1,"streamExtra":"audio"}]}]}]}]}}`)
	original := append([]byte(nil), body...)

	result, changed, err := EnrichMetadata(body, "application/json", "42", projectionTestMedia())
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("EnrichMetadata reported no change")
	}
	if !bytes.Equal(body, original) {
		t.Fatal("EnrichMetadata modified its input body")
	}

	var document map[string]any
	if err := json.Unmarshal(result, &document); err != nil {
		t.Fatal(err)
	}
	container := document["MediaContainer"].(map[string]any)
	item := container["Metadata"].([]any)[0].(map[string]any)
	media := item["Media"].([]any)[0].(map[string]any)
	part := media["Part"].([]any)[0].(map[string]any)
	streams := part["Stream"].([]any)
	videoStream := streams[0].(map[string]any)
	audioStream := streams[1].(map[string]any)
	if media["container"] != "plex-container" || media["videoCodec"] != "plex-codec" || media["videoResolution"] != "4k" || media["duration"] != float64(123000) {
		t.Fatalf("Media fields = %#v", media)
	}
	if part["id"] != float64(7) || part["file"] != "/cloud/movie.strm" || part["duration"] != float64(999) || part["size"] != float64(987654321) || part["container"] != "mkv" {
		t.Fatalf("Part fields = %#v", part)
	}
	if videoStream["id"] != "plex-stream-id" || videoStream["codec"] != "plex-stream-codec" || videoStream["profile"] != "Main 10" || videoStream["width"] != float64(3840) || videoStream["bitDepth"] != float64(10) {
		t.Fatalf("video Stream fields = %#v", videoStream)
	}
	if audioStream["codec"] != "aac" || audioStream["channels"] != float64(6) || audioStream["samplingRate"] != float64(48000) {
		t.Fatalf("audio Stream fields = %#v", audioStream)
	}
	if videoStream["selected"] != float64(1) || videoStream["default"] != float64(1) || videoStream["decision"] != "directplay" || videoStream["streamExtra"] != "preserve" || audioStream["streamExtra"] != "audio" {
		t.Fatalf("existing or forbidden Stream fields changed: video=%#v audio=%#v", videoStream, audioStream)
	}
	if container["rootExtra"] != "preserve" || item["itemExtra"] != "preserve" || media["mediaExtra"] != "preserve" || part["partExtra"] != "preserve" {
		t.Fatalf("unknown JSON fields were not preserved: %#v", document)
	}
}

func TestEnrichMetadataDoesNotCreateStreams(t *testing.T) {
	body := []byte(`<MediaContainer><Video ratingKey="42"><Media><Part id="7" file="/cloud/movie.strm"/></Media></Video></MediaContainer>`)
	result, changed, err := EnrichMetadata(body, "application/xml", "42", projectionTestMedia())
	if err != nil {
		t.Fatal(err)
	}
	if !changed || bytes.Contains(result, []byte("<Stream")) {
		t.Fatalf("result = %s, want changed metadata without a new Stream", result)
	}
}

func TestEnrichMetadataNoOpReturnsOriginalBytes(t *testing.T) {
	for _, test := range []struct {
		name        string
		contentType string
		body        []byte
	}{
		{name: "xml", contentType: "application/xml", body: []byte(completeProjectionXML())},
		{name: "json", contentType: "application/json", body: []byte(completeProjectionJSON())},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := append([]byte(nil), test.body...)
			target, err := SelectEnrichmentTarget(test.body, test.contentType, "42")
			if err != nil {
				t.Fatal(err)
			}
			if target.NeedsEnrichment {
				t.Fatal("complete target still requests enrichment")
			}
			result, changed, err := EnrichMetadata(test.body, test.contentType, "42", projectionTestMedia())
			if err != nil {
				t.Fatal(err)
			}
			if changed || !bytes.Equal(result, original) || !bytes.Equal(test.body, original) {
				t.Fatalf("changed=%v result=%q original=%q input=%q", changed, result, original, test.body)
			}
		})
	}
}

func TestEnrichMetadataRejectsAmbiguousStructureWithoutInputMutation(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "XML multiple items", contentType: "application/xml", body: `<MediaContainer><Video ratingKey="42"><Media><Part id="7" file="/a.strm"/></Media></Video><Video ratingKey="43"><Media><Part id="8" file="/b.strm"/></Media></Video></MediaContainer>`},
		{name: "XML rating mismatch", contentType: "application/xml", body: `<MediaContainer><Video ratingKey="43"><Media><Part id="7" file="/a.strm"/></Media></Video></MediaContainer>`},
		{name: "XML multiple media", contentType: "application/xml", body: `<MediaContainer><Video ratingKey="42"><Media><Part id="7" file="/a.strm"/></Media><Media><Part id="8" file="/b.strm"/></Media></Video></MediaContainer>`},
		{name: "XML multiple parts", contentType: "application/xml", body: `<MediaContainer><Video ratingKey="42"><Media><Part id="7" file="/a.strm"/><Part id="8" file="/b.strm"/></Media></Video></MediaContainer>`},
		{name: "JSON multiple items", contentType: "application/json", body: `{"MediaContainer":{"Metadata":[{"ratingKey":"42","Media":[{"Part":[{"id":7,"file":"/a.strm"}]}]},{"ratingKey":"43","Media":[{"Part":[{"id":8,"file":"/b.strm"}]}]}]}}`},
		{name: "JSON multiple media", contentType: "application/json", body: `{"MediaContainer":{"Metadata":[{"ratingKey":"42","Media":[{"Part":[{"id":7,"file":"/a.strm"}]},{"Part":[{"id":8,"file":"/b.strm"}]}]}]}}`},
		{name: "JSON multiple parts", contentType: "application/json", body: `{"MediaContainer":{"Metadata":[{"ratingKey":"42","Media":[{"Part":[{"id":7,"file":"/a.strm"},{"id":8,"file":"/b.strm"}]}]}]}}`},
		{name: "JSON duplicate path field", contentType: "application/json", body: `{"MediaContainer":{"Metadata":[{"ratingKey":"42","Media":[{"Part":[{"id":7,"id":8,"file":"/a.strm"}]}]}]}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(test.body)
			original := append([]byte(nil), body...)
			if _, err := SelectEnrichmentPart(body, test.contentType, "42"); err == nil {
				t.Fatal("SelectEnrichmentPart unexpectedly succeeded")
			}
			if !bytes.Equal(body, original) {
				t.Fatal("SelectEnrichmentPart modified its input body")
			}
			if result, changed, err := EnrichMetadata(body, test.contentType, "42", projectionTestMedia()); err == nil || result != nil || changed {
				t.Fatalf("EnrichMetadata result=%q changed=%v err=%v", result, changed, err)
			}
			if !bytes.Equal(body, original) {
				t.Fatal("EnrichMetadata modified its input body after an error")
			}
		})
	}
}

func TestEnrichMetadataRejectsDuplicateStreamIdentity(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "XML", contentType: "application/xml", body: `<MediaContainer><Video ratingKey="42"><Media><Part id="7" file="/a.strm"><Stream streamType="1" index="0"/><Stream streamType="video" index="0"/></Part></Media></Video></MediaContainer>`},
		{name: "JSON", contentType: "application/json", body: `{"MediaContainer":{"Metadata":[{"ratingKey":"42","Media":[{"Part":[{"id":7,"file":"/a.strm","Stream":[{"streamType":1,"index":0},{"streamType":"video","index":0}]}]}]}]}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(test.body)
			original := append([]byte(nil), body...)
			if result, changed, err := EnrichMetadata(body, test.contentType, "42", projectionTestMedia()); err == nil || result != nil || changed {
				t.Fatalf("EnrichMetadata result=%q changed=%v err=%v", result, changed, err)
			}
			if !bytes.Equal(body, original) {
				t.Fatal("EnrichMetadata modified its input body after an error")
			}
		})
	}
}

func TestEnrichMetadataRejectsMalformedStreamIdentity(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "XML", contentType: "application/xml", body: `<MediaContainer><Video ratingKey="42"><Media><Part id="7" file="/a.strm"><Stream streamType="1" index="not-an-index"/></Part></Media></Video></MediaContainer>`},
		{name: "JSON", contentType: "application/json", body: `{"MediaContainer":{"Metadata":[{"ratingKey":"42","Media":[{"Part":[{"id":7,"file":"/a.strm","Stream":[{"streamType":1,"index":"not-an-index"}]}]}]}]}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(test.body)
			original := append([]byte(nil), body...)
			if _, err := SelectEnrichmentTarget(body, test.contentType, "42"); err == nil {
				t.Fatal("SelectEnrichmentTarget unexpectedly accepted malformed Stream identity")
			}
			if !bytes.Equal(body, original) {
				t.Fatal("SelectEnrichmentTarget modified its input body")
			}
		})
	}
}

func TestEnrichMetadataSkipsUnmatchedStreams(t *testing.T) {
	body := []byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"42","Media":[{"Part":[{"id":7,"file":"/a.strm","Stream":[{"streamType":1,"index":99,"codec":"keep"}]}]}]}]}}`)
	result, changed, err := EnrichMetadata(body, "application/json", "42", projectionTestMedia())
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("Media/Part fields should still have been projected")
	}
	var document map[string]any
	if err := json.Unmarshal(result, &document); err != nil {
		t.Fatal(err)
	}
	stream := document["MediaContainer"].(map[string]any)["Metadata"].([]any)[0].(map[string]any)["Media"].([]any)[0].(map[string]any)["Part"].([]any)[0].(map[string]any)["Stream"].([]any)[0].(map[string]any)
	if len(stream) != 3 || stream["codec"] != "keep" || stream["streamType"] != float64(1) || stream["index"] != float64(99) {
		t.Fatalf("unmatched Stream was changed or created: %#v", stream)
	}
}

func projectionTestMedia() mediainfo.Media {
	return mediainfo.Media{
		Complete:        true,
		Container:       "mkv",
		DurationMS:      123000,
		Size:            987654321,
		Bitrate:         12000000,
		VideoCodec:      "hevc",
		VideoProfile:    "Main 10",
		VideoResolution: "4k",
		Width:           3840,
		Height:          2160,
		AspectRatio:     "16:9",
		FrameRate:       "23.976",
		AudioCodec:      "aac",
		AudioProfile:    "LC",
		AudioChannels:   6,
		Streams: []mediainfo.Stream{
			{
				Index: 0, Type: "video", Codec: "hevc", Profile: "Main 10", Level: 153,
				Bitrate: 12000000, Width: 3840, Height: 2160, FrameRate: "23.976",
				ReferenceFrames: 4, PixelFormat: "yuv420p10le", BitDepth: 10,
				ColorSpace: "bt2020nc", ColorRange: "tv", ColorPrimaries: "bt2020",
				ColorTransfer: "smpte2084", ChromaLocation: "left",
				SampleAspectRatio: "1:1", DisplayAspectRatio: "16:9", Language: "eng", Title: "Video",
			},
			{
				Index: 1, Type: "audio", Codec: "aac", Profile: "LC", Level: 1,
				Bitrate: 640000, SampleRate: 48000, Channels: 6, ChannelLayout: "5.1",
				Language: "eng", Title: "English",
			},
		},
	}
}

func xmlProjectionAttributeSets(t *testing.T, body []byte) map[string]map[string]string {
	t.Helper()
	decoder := xml.NewDecoder(bytes.NewReader(body))
	result := make(map[string]map[string]string)
	streamCount := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		name := start.Name.Local
		if name == "Stream" {
			streamCount++
			name += "#" + strconv.Itoa(streamCount)
		}
		attributes := make(map[string]string, len(start.Attr))
		for _, attribute := range start.Attr {
			attributes[attribute.Name.Local] = attribute.Value
		}
		if _, exists := result[name]; !exists {
			result[name] = attributes
		}
	}
	return result
}

func completeProjectionXML() string {
	return `<MediaContainer><Video ratingKey="42"><Media container="mkv" duration="123000" bitrate="12000000" width="3840" height="2160" aspectRatio="16:9" audioChannels="6" audioCodec="aac" videoCodec="hevc" videoResolution="4k" videoFrameRate="23.976" videoProfile="Main 10" audioProfile="LC"><Part id="7" key="/library/parts/7/1/file" file="/cloud/movie.strm" duration="123000" size="987654321" container="mkv" videoProfile="Main 10" audioProfile="LC"><Stream streamType="1" index="0" codec="hevc" profile="Main 10" level="153" bitrate="12000000" width="3840" height="2160" frameRate="23.976" refFrames="4" pixelFormat="yuv420p10le" bitDepth="10" colorSpace="bt2020nc" colorRange="tv" colorPrimaries="bt2020" colorTrc="smpte2084" chromaLocation="left" sampleAspectRatio="1:1" displayAspectRatio="16:9" language="eng" title="Video"/><Stream streamType="2" index="1" codec="aac" profile="LC" level="1" bitrate="640000" language="eng" title="English" samplingRate="48000" channels="6" channelLayout="5.1"/></Part></Media></Video></MediaContainer>`
}

func completeProjectionJSON() string {
	return `{"MediaContainer":{"Metadata":[{"ratingKey":"42","Media":[{"container":"mkv","duration":123000,"bitrate":12000000,"width":3840,"height":2160,"aspectRatio":"16:9","audioChannels":6,"audioCodec":"aac","videoCodec":"hevc","videoResolution":"4k","videoFrameRate":"23.976","videoProfile":"Main 10","audioProfile":"LC","Part":[{"id":7,"key":"/library/parts/7/1/file","file":"/cloud/movie.strm","duration":123000,"size":987654321,"container":"mkv","videoProfile":"Main 10","audioProfile":"LC","Stream":[{"streamType":1,"index":0,"codec":"hevc","profile":"Main 10","level":153,"bitrate":12000000,"width":3840,"height":2160,"frameRate":"23.976","refFrames":4,"pixelFormat":"yuv420p10le","bitDepth":10,"colorSpace":"bt2020nc","colorRange":"tv","colorPrimaries":"bt2020","colorTrc":"smpte2084","chromaLocation":"left","sampleAspectRatio":"1:1","displayAspectRatio":"16:9","language":"eng","title":"Video"},{"streamType":2,"index":1,"codec":"aac","profile":"LC","level":1,"bitrate":640000,"language":"eng","title":"English","samplingRate":48000,"channels":6,"channelLayout":"5.1"}]}]}]}]}}`
}
