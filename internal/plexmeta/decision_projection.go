package plexmeta

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/InfinityPacer/plex-gateway/internal/mediainfo"
)

// EnrichDecision fills descriptive Media, Part, and Stream fields for the one
// Direct Play Part already selected by Plex. It preserves every Plex-owned
// decision, identifier, session, and unknown field. A missing, ambiguous, or
// non-Direct-Play Part is rejected without modifying the input bytes.
func EnrichDecision(body []byte, contentType string, expected Part, media mediainfo.Media) ([]byte, bool, error) {
	if expected.ID == "" && expected.Key == "" {
		return nil, false, errors.New("expected Plex Part identity is empty")
	}
	document, err := parseProjectionDocument(body, contentType)
	if err != nil {
		return nil, false, err
	}

	var selection projectionSelection
	var sizeChanged bool
	switch typed := document.(type) {
	case *xmlProjectionDocument:
		xmlSelection, err := typed.selectDecisionPart(expected)
		if err != nil {
			return nil, false, err
		}
		selection = xmlSelection
		sizeChanged, err = replaceDecisionPartSizeXML(xmlSelection.part, media.Size)
		if err != nil {
			return nil, false, err
		}
	case *jsonProjectionDocument:
		jsonSelection, err := typed.selectDecisionPart(expected)
		if err != nil {
			return nil, false, err
		}
		selection = jsonSelection
		sizeChanged, err = replaceDecisionPartSizeJSON(*jsonSelection.part, media.Size)
		if err != nil {
			return nil, false, err
		}
	default:
		return nil, false, errors.New("unsupported Plex decision document")
	}

	changed, err := selection.enrich(media, true)
	if err != nil {
		return nil, false, err
	}
	if sizeChanged && !changed {
		if jsonSelection, ok := selection.(*jsonProjectionSelection); ok {
			if err := jsonSelection.commit(); err != nil {
				return nil, false, err
			}
		}
	}
	if !changed && !sizeChanged {
		return append([]byte(nil), body...), false, nil
	}
	result, err := document.encode()
	if err != nil {
		return nil, false, err
	}
	return result, true, nil
}

func (document *xmlProjectionDocument) selectDecisionPart(expected Part) (*xmlProjectionSelection, error) {
	roots := xmlProjectionElements(document.nodes)
	if len(roots) != 1 || roots[0].start.Name.Local != "MediaContainer" {
		return nil, errors.New("Plex XML decision has no unique MediaContainer root")
	}
	var selected *xmlProjectionSelection
	for _, video := range directXMLElements(roots[0], "Video") {
		for _, media := range directXMLElements(video, "Media") {
			for _, part := range directXMLElements(media, "Part") {
				decision, _, err := xmlAttribute(part.start.Attr, "decision")
				if err != nil {
					return nil, err
				}
				id, _, err := xmlAttribute(part.start.Attr, "id")
				if err != nil {
					return nil, err
				}
				key, _, err := xmlAttribute(part.start.Attr, "key")
				if err != nil {
					return nil, err
				}
				if !strings.EqualFold(decision, "directplay") || !decisionPartMatches(id, key, expected) {
					continue
				}
				if selected != nil {
					return nil, errors.New("Plex XML decision has an ambiguous Direct Play Part")
				}
				selected = &xmlProjectionSelection{
					value: Target{Part: expected, NeedsEnrichment: true},
					media: media,
					part:  part,
				}
			}
		}
	}
	if selected == nil {
		return nil, errors.New("Plex XML decision has no matching Direct Play Part")
	}
	return selected, nil
}

func (document *jsonProjectionDocument) selectDecisionPart(expected Part) (*jsonProjectionSelection, error) {
	container, metadata, err := document.directMetadata()
	if err != nil {
		return nil, err
	}
	var selected *jsonProjectionSelection
	for _, item := range metadata.items {
		mediaRaw, ok := (*item)["Media"]
		if !ok {
			continue
		}
		mediaItems, err := decodeJSONProjectionList(mediaRaw, "Media")
		if err != nil {
			return nil, err
		}
		for _, mediaItem := range mediaItems.items {
			partRaw, ok := (*mediaItem)["Part"]
			if !ok {
				continue
			}
			parts, err := decodeJSONProjectionList(partRaw, "Part")
			if err != nil {
				return nil, err
			}
			for _, part := range parts.items {
				decision, err := optionalJSONDecisionString(*part, "decision")
				if err != nil {
					return nil, err
				}
				id, err := optionalJSONDecisionScalar(*part, "id")
				if err != nil {
					return nil, err
				}
				key, err := optionalJSONDecisionString(*part, "key")
				if err != nil {
					return nil, err
				}
				if !strings.EqualFold(decision, "directplay") || !decisionPartMatches(id, key, expected) {
					continue
				}
				if selected != nil {
					return nil, errors.New("Plex JSON decision has an ambiguous Direct Play Part")
				}
				selected = &jsonProjectionSelection{
					value:        Target{Part: expected, NeedsEnrichment: true},
					document:     document,
					container:    container,
					metadata:     metadata,
					metadataItem: item,
					media:        mediaItems,
					parts:        parts,
					mediaItem:    mediaItem,
					part:         part,
				}
			}
		}
	}
	if selected == nil {
		return nil, errors.New("Plex JSON decision has no matching Direct Play Part")
	}
	return selected, nil
}

func optionalJSONDecisionString(object jsonObject, name string) (string, error) {
	raw, ok := object[name]
	if !ok {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("Plex JSON decision %s is not a string", name)
	}
	return value, nil
}

func optionalJSONDecisionScalar(object jsonObject, name string) (string, error) {
	raw, ok := object[name]
	if !ok {
		return "", nil
	}
	value, ok := jsonRawScalarString(raw)
	if !ok {
		return "", fmt.Errorf("Plex JSON decision %s is not a scalar", name)
	}
	return value, nil
}

func replaceDecisionPartSizeXML(part *xmlProjectionElement, mediaSize int64) (bool, error) {
	if mediaSize <= strmControlSizeThreshold {
		return false, nil
	}
	for index := range part.start.Attr {
		if part.start.Attr[index].Name.Local != "size" {
			continue
		}
		current, err := strconv.ParseInt(strings.TrimSpace(part.start.Attr[index].Value), 10, 64)
		if err != nil {
			return false, errors.New("Plex XML decision Part has an invalid size")
		}
		if current < 0 || current > strmControlSizeThreshold || mediaSize <= current {
			return false, nil
		}
		part.start.Attr[index].Value = strconv.FormatInt(mediaSize, 10)
		return true, nil
	}
	return false, nil
}

func replaceDecisionPartSizeJSON(part jsonObject, mediaSize int64) (bool, error) {
	if mediaSize <= strmControlSizeThreshold {
		return false, nil
	}
	raw, present := part["size"]
	if !present {
		return false, nil
	}
	current, ok := jsonRawInt64(raw)
	if !ok {
		return false, errors.New("Plex JSON decision Part has an invalid size")
	}
	if current < 0 || current > strmControlSizeThreshold || mediaSize <= current {
		return false, nil
	}
	part["size"] = json.RawMessage(strconv.FormatInt(mediaSize, 10))
	return true, nil
}
