package plexmeta

import "testing"

func TestParseEpisodesXMLAndJSON(t *testing.T) {
	fixtures := []struct {
		name        string
		contentType string
		body        string
	}{
		{
			name: "XML", contentType: "application/xml",
			body: `<MediaContainer><Video type="episode" ratingKey="42" parentRatingKey="7" grandparentRatingKey="3" parentIndex="1" index="2" playQueueItemID="99"><Media><Part id="9" key="/library/parts/9/file" file="/cloud/e02.strm"/></Media></Video></MediaContainer>`,
		},
		{
			name: "JSON", contentType: "application/json",
			body: `{"MediaContainer":{"Metadata":[{"type":"episode","ratingKey":42,"parentRatingKey":7,"grandparentRatingKey":3,"parentIndex":1,"index":2,"playQueueItemID":99,"Media":[{"Part":[{"id":9,"key":"/library/parts/9/file","file":"/cloud/e02.strm"}]}]}]}}`,
		},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			episodes, err := ParseEpisodes([]byte(fixture.body), fixture.contentType)
			if err != nil {
				t.Fatal(err)
			}
			if len(episodes) != 1 {
				t.Fatalf("episodes = %#v", episodes)
			}
			got := episodes[0]
			if got.RatingKey != "42" || got.ParentRatingKey != "7" || got.GrandparentRatingKey != "3" ||
				got.SeasonIndex != 1 || got.EpisodeIndex != 2 || got.PlayQueueItemID != "99" ||
				!got.UniquePart || got.Part.ID != "9" || got.Part.File != "/cloud/e02.strm" {
				t.Fatalf("episode = %#v", got)
			}
		})
	}
}

func TestParseEpisodesRejectsAmbiguousPart(t *testing.T) {
	episodes, err := ParseEpisodes([]byte(`<MediaContainer><Video type="episode" ratingKey="42" parentIndex="1" index="2"><Media><Part id="9" file="/a.strm"/><Part id="10" file="/b.strm"/></Media></Video></MediaContainer>`), "application/xml")
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 1 || episodes[0].UniquePart {
		t.Fatalf("episodes = %#v", episodes)
	}
}

func TestParseEpisodesPreservesIncompleteHierarchy(t *testing.T) {
	for _, fixture := range []struct {
		contentType string
		body        string
	}{
		{"application/xml", `<MediaContainer><Video type="episode" ratingKey="42" index="2"/></MediaContainer>`},
		{"application/json", `{"MediaContainer":{"Metadata":[{"type":"episode","ratingKey":"42","index":2}]}}`},
	} {
		episodes, err := ParseEpisodes([]byte(fixture.body), fixture.contentType)
		if err != nil {
			t.Fatal(err)
		}
		if len(episodes) != 1 || episodes[0].RatingKey != "42" ||
			episodes[0].SeasonIndex != -1 || episodes[0].EpisodeIndex != 2 {
			t.Fatalf("ParseEpisodes(%s) = %#v", fixture.contentType, episodes)
		}
	}
}

func TestParseSeasonsXMLAndJSON(t *testing.T) {
	for _, fixture := range []struct {
		contentType string
		body        string
	}{
		{"application/xml", `<MediaContainer><Directory type="season" ratingKey="7" index="1"/><Directory type="show" ratingKey="3" index="9"/></MediaContainer>`},
		{"application/json", `{"MediaContainer":{"Metadata":[{"type":"season","ratingKey":7,"index":1},{"type":"show","ratingKey":3,"index":9}]}}`},
	} {
		seasons, err := ParseSeasons([]byte(fixture.body), fixture.contentType)
		if err != nil {
			t.Fatal(err)
		}
		if len(seasons) != 1 || seasons[0] != (Season{RatingKey: "7", Index: 1}) {
			t.Fatalf("seasons = %#v", seasons)
		}
	}
}

func TestParsePlayQueuePreservesInterveningTypes(t *testing.T) {
	for _, fixture := range []struct {
		contentType string
		body        string
	}{
		{"application/xml", `<MediaContainer><Video type="episode" ratingKey="42" grandparentRatingKey="3" playQueueItemID="99"/><Video type="movie" ratingKey="50" playQueueItemID="100"/></MediaContainer>`},
		{"application/json", `{"MediaContainer":{"Metadata":[{"type":"episode","ratingKey":42,"grandparentRatingKey":3,"playQueueItemID":99},{"type":"movie","ratingKey":50,"playQueueItemID":100}]}}`},
	} {
		items, err := ParsePlayQueue([]byte(fixture.body), fixture.contentType)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 2 || items[0].RatingKey != "42" || items[0].PlayQueueItemID != "99" ||
			items[1].Type != "movie" || items[1].RatingKey != "50" {
			t.Fatalf("items = %#v", items)
		}
	}
}
