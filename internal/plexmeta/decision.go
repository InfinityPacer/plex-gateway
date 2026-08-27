package plexmeta

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"strings"
)

// IsDirectPlayDecision reports whether Plex's complete decision response
// explicitly marks the expected Part for Direct Play. Missing or conflicting
// identifiers and every unknown decision shape are rejected.
func IsDirectPlayDecision(body []byte, contentType string, expected Part) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || (expected.ID == "" && expected.Key == "") {
		return false
	}
	if strings.Contains(strings.ToLower(contentType), "json") || trimmed[0] == '{' || trimmed[0] == '[' {
		return isDirectPlayJSON(trimmed, expected)
	}
	return isDirectPlayXML(trimmed, expected)
}

func isDirectPlayXML(body []byte, expected Part) bool {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	found := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return found
		}
		if err != nil {
			return false
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "Part" {
			continue
		}
		attributes := make(map[string]string, len(start.Attr))
		for _, attribute := range start.Attr {
			attributes[attribute.Name.Local] = attribute.Value
		}
		if strings.EqualFold(attributes["decision"], "directplay") && decisionPartMatches(attributes["id"], attributes["key"], expected) {
			found = true
		}
	}
}

func isDirectPlayJSON(body []byte, expected Part) bool {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	var trailing any
	if !errors.Is(decoder.Decode(&trailing), io.EOF) {
		return false
	}
	return walkDecisionJSON(value, expected)
}

func walkDecisionJSON(value any, expected Part) bool {
	switch typed := value.(type) {
	case map[string]any:
		decision, _ := stringValue(typed["decision"])
		id, _ := stringValue(typed["id"])
		key, _ := stringValue(typed["key"])
		if strings.EqualFold(decision, "directplay") && decisionPartMatches(id, key, expected) {
			return true
		}
		for _, child := range typed {
			if walkDecisionJSON(child, expected) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if walkDecisionJSON(child, expected) {
				return true
			}
		}
	}
	return false
}

func decisionPartMatches(id, key string, expected Part) bool {
	matched := false
	if id != "" {
		if expected.ID == "" || id != expected.ID {
			return false
		}
		matched = true
	}
	if key != "" {
		if expected.Key == "" || key != expected.Key {
			return false
		}
		matched = true
	}
	return matched
}
