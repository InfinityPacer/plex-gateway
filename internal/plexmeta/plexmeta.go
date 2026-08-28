// Package plexmeta contains small, lossless readers for Plex metadata
// responses. It deliberately does not model or rewrite the response envelope.
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
)

// Part captures the stable identifiers exposed by Plex for one media part.
// Part IDs are the preferred identity key; key and file are observed paths
// that may change when Plex refreshes its metadata.
type Part struct {
	ID   string `json:"id" xml:"id,attr"`
	Key  string `json:"key" xml:"key,attr"`
	File string `json:"file" xml:"file,attr"`
}

// SelectPart returns the Part at the exact Plex Media/Part indices for the
// first metadata item. Playback-decision indices are hierarchical; flattening
// every Part can select the wrong file when an item has multiple versions.
func SelectPart(body []byte, contentType string, mediaIndex, partIndex int) (Part, error) {
	if mediaIndex < 0 || partIndex < 0 {
		return Part{}, errors.New("Plex media and part indices must be non-negative")
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return Part{}, errors.New("Plex metadata response is empty")
	}
	if strings.Contains(strings.ToLower(contentType), "json") || trimmed[0] == '{' || trimmed[0] == '[' {
		return selectJSONPart(trimmed, mediaIndex, partIndex)
	}
	return selectXMLPart(trimmed, mediaIndex, partIndex)
}

// SelectUniquePart returns the only Part only when the metadata response has
// exactly one Media and that Media has exactly one Part. This permits a protocol
// adapter to resolve an omitted index without guessing across media versions.
func SelectUniquePart(body []byte, contentType string) (Part, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return Part{}, errors.New("Plex metadata response is empty")
	}
	if strings.Contains(strings.ToLower(contentType), "json") || trimmed[0] == '{' || trimmed[0] == '[' {
		return selectUniqueJSONPart(trimmed)
	}
	return selectUniqueXMLPart(trimmed)
}

// ParseParts extracts Part identifiers from Plex XML or JSON without rewriting
// or retaining the original response body.
func ParseParts(body []byte, contentType string) ([]Part, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("Plex metadata response is empty")
	}
	if strings.Contains(strings.ToLower(contentType), "json") || trimmed[0] == '{' || trimmed[0] == '[' {
		return parseJSONParts(trimmed)
	}
	return parseXMLParts(trimmed)
}

func parseXMLParts(body []byte) ([]Part, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	var parts []Part
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse Plex XML metadata: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "Part" {
			continue
		}
		part := Part{}
		for _, attribute := range start.Attr {
			switch attribute.Name.Local {
			case "id":
				part.ID = attribute.Value
			case "key":
				part.Key = attribute.Value
			case "file":
				part.File = attribute.Value
			}
		}
		if part.ID != "" || part.Key != "" || part.File != "" {
			parts = append(parts, part)
		}
	}
	return parts, nil
}

func parseJSONParts(body []byte) ([]Part, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("parse Plex JSON metadata: %w", err)
	}
	var parts []Part
	walkJSON(value, &parts)
	return parts, nil
}

func selectXMLPart(body []byte, mediaIndex, partIndex int) (Part, error) {
	type media struct {
		Parts []Part `xml:"Part"`
	}
	type video struct {
		Media []media `xml:"Media"`
	}
	type container struct {
		Videos []video `xml:"Video"`
	}

	var value container
	if err := xml.Unmarshal(body, &value); err != nil {
		return Part{}, fmt.Errorf("parse Plex XML metadata: %w", err)
	}
	if len(value.Videos) == 0 || mediaIndex >= len(value.Videos[0].Media) {
		return Part{}, errors.New("Plex media index is out of range")
	}
	parts := value.Videos[0].Media[mediaIndex].Parts
	if partIndex >= len(parts) {
		return Part{}, errors.New("Plex part index is out of range")
	}
	part := parts[partIndex]
	if part.ID == "" || part.File == "" {
		return Part{}, errors.New("selected Plex Part has no id or file")
	}
	return part, nil
}

func selectUniqueXMLPart(body []byte) (Part, error) {
	type media struct {
		Parts []Part `xml:"Part"`
	}
	type video struct {
		Media []media `xml:"Media"`
	}
	type container struct {
		Videos []video `xml:"Video"`
	}

	var value container
	if err := xml.Unmarshal(body, &value); err != nil {
		return Part{}, fmt.Errorf("parse Plex XML metadata: %w", err)
	}
	if len(value.Videos) != 1 || len(value.Videos[0].Media) != 1 || len(value.Videos[0].Media[0].Parts) != 1 {
		return Part{}, errors.New("Plex metadata does not contain a unique Media/Part")
	}
	part := value.Videos[0].Media[0].Parts[0]
	if part.ID == "" || part.File == "" {
		return Part{}, errors.New("selected Plex Part has no id or file")
	}
	return part, nil
}

func selectJSONPart(body []byte, mediaIndex, partIndex int) (Part, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return Part{}, fmt.Errorf("parse Plex JSON metadata: %w", err)
	}

	root, ok := value.(map[string]any)
	if !ok {
		return Part{}, errors.New("Plex JSON metadata has no object root")
	}
	mediaContainer, ok := root["MediaContainer"].(map[string]any)
	if !ok {
		return Part{}, errors.New("Plex JSON metadata has no MediaContainer")
	}
	metadata := objectList(mediaContainer["Metadata"])
	if len(metadata) == 0 {
		return Part{}, errors.New("Plex JSON metadata has no Metadata item")
	}
	media := objectList(metadata[0]["Media"])
	if mediaIndex >= len(media) {
		return Part{}, errors.New("Plex media index is out of range")
	}
	parts := objectList(media[mediaIndex]["Part"])
	if partIndex >= len(parts) {
		return Part{}, errors.New("Plex part index is out of range")
	}
	part, ok := partFromMap(parts[partIndex])
	if !ok || part.ID == "" || part.File == "" {
		return Part{}, errors.New("selected Plex Part has no id or file")
	}
	return part, nil
}

func selectUniqueJSONPart(body []byte) (Part, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return Part{}, fmt.Errorf("parse Plex JSON metadata: %w", err)
	}

	root, ok := value.(map[string]any)
	if !ok {
		return Part{}, errors.New("Plex JSON metadata has no object root")
	}
	mediaContainer, ok := root["MediaContainer"].(map[string]any)
	if !ok {
		return Part{}, errors.New("Plex JSON metadata has no MediaContainer")
	}
	metadata := objectList(mediaContainer["Metadata"])
	if len(metadata) != 1 {
		return Part{}, errors.New("Plex JSON metadata does not contain one Metadata item")
	}
	media := objectList(metadata[0]["Media"])
	if len(media) != 1 {
		return Part{}, errors.New("Plex metadata does not contain a unique Media/Part")
	}
	parts := objectList(media[0]["Part"])
	if len(parts) != 1 {
		return Part{}, errors.New("Plex metadata does not contain a unique Media/Part")
	}
	part, ok := partFromMap(parts[0])
	if !ok || part.ID == "" || part.File == "" {
		return Part{}, errors.New("selected Plex Part has no id or file")
	}
	return part, nil
}

func objectList(value any) []map[string]any {
	switch typed := value.(type) {
	case []any:
		result := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			if object, ok := item.(map[string]any); ok {
				result = append(result, object)
			}
		}
		return result
	case map[string]any:
		return []map[string]any{typed}
	default:
		return nil
	}
}

func walkJSON(value any, parts *[]Part) {
	switch typed := value.(type) {
	case map[string]any:
		if candidate, ok := partFromMap(typed); ok {
			*parts = append(*parts, candidate)
		}
		for _, child := range typed {
			walkJSON(child, parts)
		}
	case []any:
		for _, child := range typed {
			walkJSON(child, parts)
		}
	}
}

func partFromMap(value map[string]any) (Part, bool) {
	file, hasFile := stringValue(value["file"])
	key, hasKey := stringValue(value["key"])
	id, hasID := stringValue(value["id"])
	if !hasFile || (!hasID && !hasKey) {
		return Part{}, false
	}
	return Part{ID: id, Key: key, File: file}, true
}

func stringValue(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, typed != ""
	case json.Number:
		return typed.String(), true
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	default:
		return "", false
	}
}
