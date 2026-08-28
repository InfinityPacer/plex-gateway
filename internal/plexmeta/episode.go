package plexmeta

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Episode identifies the Plex hierarchy used for best-effort next-item
// discovery. Unknown season or episode indices use -1. Parts preserves Plex
// order; Part is the first usable Part, while UniquePart reports the stricter
// single-Media/single-Part shape used by metadata enrichment.
type Episode struct {
	RatingKey            string
	ParentRatingKey      string
	GrandparentRatingKey string
	SeasonIndex          int
	EpisodeIndex         int
	PlayQueueItemID      string
	Part                 Part
	Parts                []Part
	UniquePart           bool
}

// Season identifies one Plex season. Unknown indices use -1 and returned order
// remains available to discovery as a fallback.
type Season struct {
	RatingKey string
	Index     int
}

// PlayQueueItem preserves queue order and type so discovery never skips over
// an intervening movie, clip, or item from another show.
type PlayQueueItem struct {
	Type                 string
	RatingKey            string
	GrandparentRatingKey string
	PlayQueueItemID      string
}

// ParseEpisodes extracts episode hierarchy without rewriting the Plex body.
func ParseEpisodes(body []byte, contentType string) ([]Episode, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("Plex episode response is empty")
	}
	if strings.Contains(strings.ToLower(contentType), "json") || trimmed[0] == '{' || trimmed[0] == '[' {
		return parseJSONEpisodes(trimmed)
	}
	return parseXMLEpisodes(trimmed)
}

// ParseSeasons extracts season identities used only for bounded cross-season
// discovery.
func ParseSeasons(body []byte, contentType string) ([]Season, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("Plex season response is empty")
	}
	if strings.Contains(strings.ToLower(contentType), "json") || trimmed[0] == '{' || trimmed[0] == '[' {
		return parseJSONSeasons(trimmed)
	}
	return parseXMLSeasons(trimmed)
}

// ParsePlayQueue extracts the ordered identity fields needed to prove that an
// item is the immediate successor of the active queue entry.
func ParsePlayQueue(body []byte, contentType string) ([]PlayQueueItem, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("Plex play queue response is empty")
	}
	if strings.Contains(strings.ToLower(contentType), "json") || trimmed[0] == '{' || trimmed[0] == '[' {
		root, err := parseJSONMediaContainer(trimmed)
		if err != nil {
			return nil, err
		}
		items := objectList(root["Metadata"])
		result := make([]PlayQueueItem, 0, len(items))
		for _, item := range items {
			typeName, _ := stringValue(item["type"])
			ratingKey, _ := stringValue(item["ratingKey"])
			grandparentRatingKey, _ := stringValue(item["grandparentRatingKey"])
			playQueueItemID, _ := stringValue(item["playQueueItemID"])
			result = append(result, PlayQueueItem{
				Type: typeName, RatingKey: ratingKey,
				GrandparentRatingKey: grandparentRatingKey,
				PlayQueueItemID:      playQueueItemID,
			})
		}
		return result, nil
	}
	type item struct {
		XMLName              xml.Name
		Type                 string `xml:"type,attr"`
		RatingKey            string `xml:"ratingKey,attr"`
		GrandparentRatingKey string `xml:"grandparentRatingKey,attr"`
		PlayQueueItemID      string `xml:"playQueueItemID,attr"`
	}
	type container struct {
		Items []item `xml:",any"`
	}
	var value container
	if err := xml.Unmarshal(trimmed, &value); err != nil {
		return nil, fmt.Errorf("parse Plex XML play queue: %w", err)
	}
	result := make([]PlayQueueItem, 0, len(value.Items))
	for _, candidate := range value.Items {
		if candidate.XMLName.Local == "MediaContainer" {
			continue
		}
		result = append(result, PlayQueueItem{
			Type: candidate.Type, RatingKey: candidate.RatingKey,
			GrandparentRatingKey: candidate.GrandparentRatingKey,
			PlayQueueItemID:      candidate.PlayQueueItemID,
		})
	}
	return result, nil
}

func parseXMLEpisodes(body []byte) ([]Episode, error) {
	type media struct {
		Parts []Part `xml:"Part"`
	}
	type video struct {
		Type                 string  `xml:"type,attr"`
		RatingKey            string  `xml:"ratingKey,attr"`
		ParentRatingKey      string  `xml:"parentRatingKey,attr"`
		GrandparentRatingKey string  `xml:"grandparentRatingKey,attr"`
		ParentIndex          *int    `xml:"parentIndex,attr"`
		Index                *int    `xml:"index,attr"`
		PlayQueueItemID      string  `xml:"playQueueItemID,attr"`
		Media                []media `xml:"Media"`
	}
	type container struct {
		Videos []video `xml:"Video"`
	}
	var value container
	if err := xml.Unmarshal(body, &value); err != nil {
		return nil, fmt.Errorf("parse Plex XML episodes: %w", err)
	}
	episodes := make([]Episode, 0, len(value.Videos))
	for _, item := range value.Videos {
		if item.Type != "episode" {
			continue
		}
		if item.RatingKey == "" {
			continue
		}
		seasonIndex := -1
		if item.ParentIndex != nil && *item.ParentIndex >= 0 {
			seasonIndex = *item.ParentIndex
		}
		episodeIndex := -1
		if item.Index != nil && *item.Index >= 0 {
			episodeIndex = *item.Index
		}
		episode := Episode{
			RatingKey: item.RatingKey, ParentRatingKey: item.ParentRatingKey,
			GrandparentRatingKey: item.GrandparentRatingKey,
			SeasonIndex:          seasonIndex, EpisodeIndex: episodeIndex,
			PlayQueueItemID: item.PlayQueueItemID,
		}
		for _, mediaItem := range item.Media {
			episode.Parts = append(episode.Parts, mediaItem.Parts...)
		}
		for _, part := range episode.Parts {
			if part.ID != "" && part.File != "" {
				episode.Part = part
				break
			}
		}
		if len(item.Media) == 1 && len(item.Media[0].Parts) == 1 {
			episode.UniquePart = item.Media[0].Parts[0].ID != "" && item.Media[0].Parts[0].File != ""
		}
		episodes = append(episodes, episode)
	}
	return episodes, nil
}

func parseJSONEpisodes(body []byte) ([]Episode, error) {
	root, err := parseJSONMediaContainer(body)
	if err != nil {
		return nil, err
	}
	items := objectList(root["Metadata"])
	episodes := make([]Episode, 0, len(items))
	for _, item := range items {
		typeName, _ := stringValue(item["type"])
		ratingKey, _ := stringValue(item["ratingKey"])
		seasonIndex, seasonOK := nonNegativeJSONInt(item["parentIndex"])
		episodeIndex, episodeOK := nonNegativeJSONInt(item["index"])
		if typeName != "episode" {
			continue
		}
		if ratingKey == "" {
			continue
		}
		if !seasonOK {
			seasonIndex = -1
		}
		if !episodeOK {
			episodeIndex = -1
		}
		parentRatingKey, _ := stringValue(item["parentRatingKey"])
		grandparentRatingKey, _ := stringValue(item["grandparentRatingKey"])
		playQueueItemID, _ := stringValue(item["playQueueItemID"])
		episode := Episode{
			RatingKey: ratingKey, ParentRatingKey: parentRatingKey,
			GrandparentRatingKey: grandparentRatingKey,
			SeasonIndex:          seasonIndex, EpisodeIndex: episodeIndex,
			PlayQueueItemID: playQueueItemID,
		}
		media := objectList(item["Media"])
		for _, mediaItem := range media {
			for _, rawPart := range objectList(mediaItem["Part"]) {
				if part, ok := partFromMap(rawPart); ok {
					episode.Parts = append(episode.Parts, part)
				}
			}
		}
		for _, part := range episode.Parts {
			if part.ID != "" && part.File != "" {
				episode.Part = part
				break
			}
		}
		if len(media) == 1 && len(objectList(media[0]["Part"])) == 1 {
			episode.UniquePart = episode.Part.ID != "" && episode.Part.File != ""
		}
		episodes = append(episodes, episode)
	}
	return episodes, nil
}

func parseXMLSeasons(body []byte) ([]Season, error) {
	type directory struct {
		Type      string `xml:"type,attr"`
		RatingKey string `xml:"ratingKey,attr"`
		Index     *int   `xml:"index,attr"`
	}
	type container struct {
		Directories []directory `xml:"Directory"`
	}
	var value container
	if err := xml.Unmarshal(body, &value); err != nil {
		return nil, fmt.Errorf("parse Plex XML seasons: %w", err)
	}
	seasons := make([]Season, 0, len(value.Directories))
	for _, item := range value.Directories {
		if item.Type != "season" {
			continue
		}
		if item.RatingKey == "" {
			continue
		}
		index := -1
		if item.Index != nil && *item.Index >= 0 {
			index = *item.Index
		}
		seasons = append(seasons, Season{RatingKey: item.RatingKey, Index: index})
	}
	return seasons, nil
}

func parseJSONSeasons(body []byte) ([]Season, error) {
	root, err := parseJSONMediaContainer(body)
	if err != nil {
		return nil, err
	}
	items := objectList(root["Metadata"])
	seasons := make([]Season, 0, len(items))
	for _, item := range items {
		typeName, _ := stringValue(item["type"])
		ratingKey, _ := stringValue(item["ratingKey"])
		index, indexOK := nonNegativeJSONInt(item["index"])
		if typeName != "season" {
			continue
		}
		if ratingKey == "" {
			continue
		}
		if !indexOK {
			index = -1
		}
		seasons = append(seasons, Season{RatingKey: ratingKey, Index: index})
	}
	return seasons, nil
}

func parseJSONMediaContainer(body []byte) (map[string]any, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("parse Plex JSON hierarchy: %w", err)
	}
	root, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("Plex JSON hierarchy has no object root")
	}
	container, ok := root["MediaContainer"].(map[string]any)
	if !ok {
		return nil, errors.New("Plex JSON hierarchy has no MediaContainer")
	}
	return container, nil
}

func nonNegativeJSONInt(value any) (int, bool) {
	text, ok := stringValue(value)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.Atoi(text)
	return parsed, err == nil && parsed >= 0
}
