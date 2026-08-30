package plexmeta

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestEnrichDecisionProjectsMatchingDirectPlayPart(t *testing.T) {
	media := projectionTestMedia()
	tests := []struct {
		name        string
		contentType string
		body        []byte
		assert      func(*testing.T, []byte)
	}{
		{
			name:        "XML",
			contentType: "application/xml",
			body:        []byte(`<MediaContainer decisionExtra="preserve"><Video videoExtra="preserve"><Media id="117278" decision="directplay" mediaExtra="preserve"><Part id="118649" key="/library/parts/118649/1/file" size="301" decision="directplay" partExtra="preserve"/></Media></Video></MediaContainer>`),
			assert: func(t *testing.T, result []byte) {
				attributes := xmlProjectionAttributeSets(t, result)
				if attributes["Media"]["decision"] != "directplay" || attributes["Media"]["container"] != "mkv" || attributes["Media"]["bitrate"] != "12000" {
					t.Fatalf("Media attributes = %#v", attributes["Media"])
				}
				if attributes["Part"]["decision"] != "directplay" || attributes["Part"]["size"] != "987654321" || attributes["Part"]["container"] != "mkv" {
					t.Fatalf("Part attributes = %#v", attributes["Part"])
				}
				if attributes["Stream#1"]["codec"] != "hevc" || attributes["Stream#1"]["bitDepth"] != "10" || attributes["Stream#2"]["codec"] != "aac" {
					t.Fatalf("Stream attributes = %#v", attributes)
				}
				for _, fragment := range [][]byte{[]byte(`decisionExtra="preserve"`), []byte(`videoExtra="preserve"`), []byte(`mediaExtra="preserve"`), []byte(`partExtra="preserve"`)} {
					if !bytes.Contains(result, fragment) {
						t.Fatalf("decision field %q was not preserved: %s", fragment, result)
					}
				}
			},
		},
		{
			name:        "JSON",
			contentType: "application/json",
			body:        []byte(`{"MediaContainer":{"decisionExtra":"preserve","Metadata":[{"videoExtra":"preserve","Media":[{"id":117278,"decision":"directplay","mediaExtra":"preserve","Part":[{"id":118649,"key":"/library/parts/118649/1/file","size":301,"decision":"directplay","partExtra":"preserve"}]}]}]}}`),
			assert: func(t *testing.T, result []byte) {
				var document map[string]any
				if err := json.Unmarshal(result, &document); err != nil {
					t.Fatal(err)
				}
				container := document["MediaContainer"].(map[string]any)
				item := container["Metadata"].([]any)[0].(map[string]any)
				mediaObject := item["Media"].([]any)[0].(map[string]any)
				part := mediaObject["Part"].([]any)[0].(map[string]any)
				streams := part["Stream"].([]any)
				if mediaObject["decision"] != "directplay" || mediaObject["container"] != "mkv" || mediaObject["bitrate"] != float64(12000) {
					t.Fatalf("Media fields = %#v", mediaObject)
				}
				if part["decision"] != "directplay" || part["size"] != float64(987654321) || part["container"] != "mkv" {
					t.Fatalf("Part fields = %#v", part)
				}
				if len(streams) != 2 || streams[0].(map[string]any)["codec"] != "hevc" || streams[1].(map[string]any)["codec"] != "aac" {
					t.Fatalf("Stream fields = %#v", streams)
				}
				if container["decisionExtra"] != "preserve" || item["videoExtra"] != "preserve" || mediaObject["mediaExtra"] != "preserve" || part["partExtra"] != "preserve" {
					t.Fatalf("unknown decision fields were not preserved: %#v", document)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := append([]byte(nil), test.body...)
			result, changed, err := EnrichDecision(test.body, test.contentType, Part{
				ID: "118649", Key: "/library/parts/118649/1/file", File: "/cloud/movie.strm",
			}, media)
			if err != nil || !changed {
				t.Fatalf("EnrichDecision changed=%v err=%v", changed, err)
			}
			if !bytes.Equal(test.body, original) {
				t.Fatal("EnrichDecision modified its input body")
			}
			test.assert(t, result)
		})
	}
}

func TestEnrichDecisionRejectsNonMatchingOrAmbiguousPart(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        []byte
	}{
		{
			name:        "XML transcode",
			contentType: "application/xml",
			body:        []byte(`<MediaContainer><Video><Media><Part id="7" key="/library/parts/7/1/file" decision="transcode"/></Media></Video></MediaContainer>`),
		},
		{
			name:        "XML ambiguous",
			contentType: "application/xml",
			body:        []byte(`<MediaContainer><Video><Media><Part id="7" key="/library/parts/7/1/file" decision="directplay"/><Part id="7" key="/library/parts/7/1/file" decision="directplay"/></Media></Video></MediaContainer>`),
		},
		{
			name:        "JSON wrong Part",
			contentType: "application/json",
			body:        []byte(`{"MediaContainer":{"Metadata":[{"Media":[{"Part":[{"id":8,"key":"/library/parts/8/1/file","decision":"directplay"}]}]}]}}`),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := append([]byte(nil), test.body...)
			result, changed, err := EnrichDecision(test.body, test.contentType, Part{ID: "7", Key: "/library/parts/7/1/file"}, projectionTestMedia())
			if err == nil || result != nil || changed {
				t.Fatalf("result=%q changed=%v err=%v", result, changed, err)
			}
			if !bytes.Equal(test.body, original) {
				t.Fatal("failed decision projection modified its input")
			}
		})
	}
}

func TestDecisionNeedsEnrichmentTrustsMaterializedStreams(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		want        bool
	}{
		{
			name:        "XML materialized",
			contentType: "application/xml",
			body:        `<MediaContainer><Video><Media container="mp4" duration="1414016" bitrate="3730" width="3840" height="2160" aspectRatio="1.78" audioChannels="2" audioCodec="aac" videoCodec="hevc" videoResolution="4k" videoFrameRate="PAL" videoProfile="main" audioProfile="lc" decision="directplay"><Part id="9" key="/library/parts/9/1/file" file="/media/cloud/episode.strm" duration="1414016" size="659294933" container="mp4" videoProfile="main" audioProfile="lc" decision="directplay"><Stream id="101" streamType="1" index="0" codec="hevc"/><Stream id="102" streamType="2" index="1" codec="aac"/></Part></Media></Video></MediaContainer>`,
		},
		{
			name:        "JSON materialized",
			contentType: "application/json",
			body:        `{"MediaContainer":{"Metadata":[{"Media":[{"container":"mp4","duration":1414016,"bitrate":3730,"width":3840,"height":2160,"aspectRatio":1.78,"audioChannels":2,"audioCodec":"aac","videoCodec":"hevc","videoResolution":"4k","videoFrameRate":"PAL","videoProfile":"main","audioProfile":"lc","decision":"directplay","Part":[{"id":9,"key":"/library/parts/9/1/file","file":"/media/cloud/episode.strm","duration":1414016,"size":659294933,"container":"mp4","videoProfile":"main","audioProfile":"lc","decision":"directplay","Stream":[{"id":101,"streamType":1,"index":0,"codec":"hevc"},{"id":102,"streamType":2,"index":1,"codec":"aac"}]}]}]}]}}`,
		},
		{
			name:        "XML projected",
			contentType: "application/xml",
			body:        `<MediaContainer><Video><Media container="mp4" duration="1414016" bitrate="3730" width="3840" height="2160" aspectRatio="1.78" audioChannels="2" audioCodec="aac" videoCodec="hevc" videoResolution="4k" videoFrameRate="PAL" videoProfile="main" audioProfile="lc" decision="directplay"><Part id="9" key="/library/parts/9/1/file" file="/media/cloud/episode.strm" duration="1414016" size="659294933" container="mp4" videoProfile="main" audioProfile="lc" decision="directplay"><Stream streamType="1" index="0" codec="hevc"/></Part></Media></Video></MediaContainer>`,
			want:        true,
		},
		{
			name:        "JSON projected",
			contentType: "application/json",
			body:        `{"MediaContainer":{"Metadata":[{"Media":[{"container":"mp4","duration":1414016,"bitrate":3730,"width":3840,"height":2160,"aspectRatio":1.78,"audioChannels":2,"audioCodec":"aac","videoCodec":"hevc","videoResolution":"4k","videoFrameRate":"PAL","videoProfile":"main","audioProfile":"lc","decision":"directplay","Part":[{"id":9,"key":"/library/parts/9/1/file","file":"/media/cloud/episode.strm","duration":1414016,"size":659294933,"container":"mp4","videoProfile":"main","audioProfile":"lc","decision":"directplay","Stream":[{"streamType":1,"index":0,"codec":"hevc"}]}]}]}]}}`,
			want:        true,
		},
	}
	expected := Part{ID: "9", Key: "/library/parts/9/1/file", File: "/media/cloud/episode.strm"}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := DecisionNeedsEnrichment([]byte(test.body), test.contentType, expected)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("DecisionNeedsEnrichment=%v, want %v", got, test.want)
			}
			result, changed, err := EnrichDecision([]byte(test.body), test.contentType, expected, projectionTestMedia())
			if err != nil {
				t.Fatal(err)
			}
			if !test.want && (changed || !bytes.Equal(result, []byte(test.body))) {
				t.Fatalf("materialized decision changed=%v result=%s", changed, result)
			}
		})
	}
}
