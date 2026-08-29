package plexmeta

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// SplitMetadataBatch splits one Plex metadata collection into one response
// per requested rating key. The collection must contain each requested item
// exactly once. Unknown container fields are retained, while every output
// contains only its selected item and reports size one.
//
// Plex's JSON shape stores items directly in MediaContainer.Metadata. Its XML
// shape stores items as direct MediaContainer children. XML children without a
// ratingKey are treated as container-level extension elements and copied to
// every output; a child with a ratingKey is always an item candidate.
func SplitMetadataBatch(body []byte, contentType string, ratingKeys []string) (map[string][]byte, error) {
	wanted, err := normalizeBatchRatingKeys(ratingKeys)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("Plex metadata batch response is empty")
	}
	if hasUTF8BOM(trimmed) {
		trimmed = bytes.TrimSpace(trimmed[len([]byte{0xef, 0xbb, 0xbf}):])
	}
	if len(trimmed) == 0 {
		return nil, errors.New("Plex metadata batch response is empty")
	}

	if strings.Contains(strings.ToLower(contentType), "json") || trimmed[0] == '{' || trimmed[0] == '[' {
		return splitJSONMetadataBatch(body, wanted)
	}
	return splitXMLMetadataBatch(body, wanted)
}

func normalizeBatchRatingKeys(values []string) (map[string]struct{}, error) {
	if len(values) == 0 {
		return nil, errors.New("Plex metadata batch has no requested rating keys")
	}
	wanted := make(map[string]struct{}, len(values))
	for _, value := range values {
		normalized, err := normalizeBatchRatingKey(value)
		if err != nil {
			return nil, err
		}
		if _, exists := wanted[normalized]; exists {
			return nil, fmt.Errorf("Plex metadata batch has duplicate requested rating key %q", normalized)
		}
		wanted[normalized] = struct{}{}
	}
	return wanted, nil
}

func normalizeBatchRatingKey(value string) (string, error) {
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, ",/?#\x00\r\n") {
		return "", fmt.Errorf("invalid Plex rating key %q", value)
	}
	return value, nil
}

func splitJSONMetadataBatch(body []byte, wanted map[string]struct{}) (map[string][]byte, error) {
	hasBOM, leading, document, trailing := splitJSONEnvelope(body)
	if len(document) == 0 {
		return nil, errors.New("Plex JSON metadata batch response is empty")
	}

	root, err := decodeJSONProjectionObject(document)
	if err != nil {
		return nil, fmt.Errorf("parse Plex JSON metadata batch: %w", err)
	}

	containerRaw, ok := root["MediaContainer"]
	if !ok {
		return nil, errors.New("Plex JSON metadata batch has no MediaContainer")
	}
	container, err := decodeJSONProjectionObject(bytes.TrimSpace(containerRaw))
	if err != nil {
		return nil, fmt.Errorf("Plex JSON metadata batch has invalid MediaContainer: %w", err)
	}

	metadataRaw, ok := container["Metadata"]
	if !ok {
		return nil, errors.New("Plex JSON metadata batch has no Metadata")
	}
	items, metadataAsObject, err := decodeJSONBatchItems(metadataRaw)
	if err != nil {
		return nil, err
	}
	itemByKey := make(map[string]json.RawMessage, len(items))
	for _, item := range items {
		ratingKey, err := jsonBatchRatingKey(item)
		if err != nil {
			return nil, err
		}
		if _, exists := itemByKey[ratingKey]; exists {
			return nil, fmt.Errorf("Plex JSON metadata batch has duplicate rating key %q", ratingKey)
		}
		itemByKey[ratingKey] = item
	}
	if err := ensureBatchKeysPresent(itemByKey, wanted); err != nil {
		return nil, err
	}

	result := make(map[string][]byte, len(wanted))
	for ratingKey := range wanted {
		output, err := encodeJSONBatchItem(root, container, metadataAsObject, itemByKey[ratingKey], hasBOM, leading, trailing)
		if err != nil {
			return nil, err
		}
		result[ratingKey] = output
	}
	return result, nil
}

func decodeJSONBatchItems(raw json.RawMessage) ([]json.RawMessage, bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false, errors.New("Plex JSON metadata batch has empty Metadata")
	}
	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, false, fmt.Errorf("Plex JSON metadata batch has invalid Metadata: %w", err)
		}
		for _, item := range items {
			if !isJSONObject(item) {
				return nil, false, errors.New("Plex JSON metadata batch contains a non-object Metadata item")
			}
		}
		return items, false, nil
	}
	if trimmed[0] == '{' {
		if !isJSONObject(trimmed) {
			return nil, false, errors.New("Plex JSON metadata batch contains an invalid Metadata item")
		}
		return []json.RawMessage{append(json.RawMessage(nil), trimmed...)}, true, nil
	}
	return nil, false, errors.New("Plex JSON metadata batch Metadata is neither an array nor an object")
}

func isJSONObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

func jsonBatchRatingKey(item json.RawMessage) (string, error) {
	object, err := decodeJSONProjectionObject(bytes.TrimSpace(item))
	if err != nil {
		return "", fmt.Errorf("parse Plex JSON metadata item: %w", err)
	}
	raw, ok := object["ratingKey"]
	if !ok {
		return "", errors.New("Plex JSON metadata item has no ratingKey")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		var number json.Number
		if numberErr := json.Unmarshal(raw, &number); numberErr != nil {
			return "", errors.New("Plex JSON metadata item has an invalid ratingKey")
		}
		value = number.String()
	}
	normalized, err := normalizeBatchRatingKey(value)
	if err != nil {
		return "", err
	}
	return normalized, nil
}

func ensureBatchKeysPresent(items map[string]json.RawMessage, wanted map[string]struct{}) error {
	if len(items) != len(wanted) {
		return errors.New("Plex metadata batch returned an unexpected item set")
	}
	for ratingKey := range wanted {
		if _, ok := items[ratingKey]; !ok {
			return fmt.Errorf("Plex metadata batch is missing requested rating key %q", ratingKey)
		}
	}
	return nil
}

func encodeJSONBatchItem(
	root map[string]json.RawMessage,
	container map[string]json.RawMessage,
	metadataAsObject bool,
	item json.RawMessage,
	hasBOM bool,
	leading, trailing []byte,
) ([]byte, error) {
	containerCopy := cloneJSONRawMap(container)
	containerCopy["size"] = json.RawMessage("1")
	if metadataAsObject {
		containerCopy["Metadata"] = append(json.RawMessage(nil), item...)
	} else {
		metadata, err := json.Marshal([]json.RawMessage{item})
		if err != nil {
			return nil, fmt.Errorf("encode Plex JSON metadata item: %w", err)
		}
		containerCopy["Metadata"] = metadata
	}
	rootCopy := cloneJSONRawMap(root)
	containerRaw, err := json.Marshal(containerCopy)
	if err != nil {
		return nil, fmt.Errorf("encode Plex JSON MediaContainer: %w", err)
	}
	rootCopy["MediaContainer"] = containerRaw
	encoded, err := json.Marshal(rootCopy)
	if err != nil {
		return nil, fmt.Errorf("encode Plex JSON metadata batch: %w", err)
	}
	result := make([]byte, 0, len(encoded)+len(leading)+len(trailing)+3)
	if hasBOM {
		result = append(result, 0xef, 0xbb, 0xbf)
	}
	result = append(result, leading...)
	result = append(result, encoded...)
	result = append(result, trailing...)
	return result, nil
}

func cloneJSONRawMap(source map[string]json.RawMessage) map[string]json.RawMessage {
	clone := make(map[string]json.RawMessage, len(source))
	for key, value := range source {
		clone[key] = append(json.RawMessage(nil), value...)
	}
	return clone
}

func splitJSONEnvelope(body []byte) (hasBOM bool, leading, document, trailing []byte) {
	value := body
	if bytes.HasPrefix(value, []byte{0xef, 0xbb, 0xbf}) {
		hasBOM = true
		value = value[3:]
	}
	leadingLength := len(value) - len(bytes.TrimLeft(value, " \t\r\n"))
	leading = append([]byte(nil), value[:leadingLength]...)
	value = value[leadingLength:]
	trailingLength := len(value) - len(bytes.TrimRight(value, " \t\r\n"))
	document = append([]byte(nil), value[:len(value)-trailingLength]...)
	trailing = append([]byte(nil), value[len(value)-trailingLength:]...)
	return hasBOM, leading, document, trailing
}

type batchXMLContentKind uint8

const (
	batchXMLText batchXMLContentKind = iota
	batchXMLElement
	batchXMLComment
	batchXMLDirective
	batchXMLProcInst
)

type batchXMLContent struct {
	kind      batchXMLContentKind
	text      []byte
	element   *batchXMLNode
	comment   []byte
	directive []byte
	procInst  xml.ProcInst
}

type batchXMLNode struct {
	start    xml.StartElement
	children []batchXMLContent
}

type batchXMLDocument struct {
	leading  []batchXMLContent
	root     *batchXMLNode
	trailing []batchXMLContent
	hasBOM   bool
}

func splitXMLMetadataBatch(body []byte, wanted map[string]struct{}) (map[string][]byte, error) {
	document, err := parseBatchXMLDocument(body)
	if err != nil {
		return nil, err
	}
	if document.root == nil || document.root.start.Name.Local != "MediaContainer" {
		return nil, errors.New("Plex XML metadata batch has no MediaContainer root")
	}

	items := make(map[string]*batchXMLNode, len(document.root.children))
	for _, child := range document.root.children {
		if child.kind != batchXMLElement {
			if child.kind == batchXMLText && len(bytes.TrimSpace(child.text)) != 0 {
				return nil, errors.New("Plex XML metadata batch has unexpected container text")
			}
			continue
		}
		ratingKey, present, err := xmlBatchRatingKey(child.element.start.Attr)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		if _, exists := items[ratingKey]; exists {
			return nil, fmt.Errorf("Plex XML metadata batch has duplicate rating key %q", ratingKey)
		}
		items[ratingKey] = child.element
	}
	if err := ensureXMLBatchKeysPresent(items, wanted); err != nil {
		return nil, err
	}

	result := make(map[string][]byte, len(wanted))
	for ratingKey := range wanted {
		outputDocument := document.cloneFor(ratingKey, items[ratingKey])
		encoded, err := encodeBatchXMLDocument(outputDocument)
		if err != nil {
			return nil, err
		}
		result[ratingKey] = encoded
	}
	return result, nil
}

func ensureXMLBatchKeysPresent(items map[string]*batchXMLNode, wanted map[string]struct{}) error {
	if len(items) != len(wanted) {
		return errors.New("Plex XML metadata batch returned an unexpected item set")
	}
	for ratingKey := range wanted {
		if _, ok := items[ratingKey]; !ok {
			return fmt.Errorf("Plex XML metadata batch is missing requested rating key %q", ratingKey)
		}
	}
	return nil
}

func xmlBatchRatingKey(attributes []xml.Attr) (string, bool, error) {
	found := false
	value := ""
	for _, attribute := range attributes {
		if attribute.Name.Local != "ratingKey" || attribute.Name.Space != "" {
			continue
		}
		if found {
			return "", true, errors.New("Plex XML metadata item has duplicate ratingKey attributes")
		}
		found = true
		value = attribute.Value
	}
	if !found {
		return "", false, nil
	}
	if value == "" {
		return "", true, errors.New("Plex XML metadata item has an empty ratingKey")
	}
	ratingKey, err := normalizeBatchRatingKey(value)
	if err != nil {
		return "", true, err
	}
	return ratingKey, true, nil
}

func parseBatchXMLDocument(body []byte) (batchXMLDocument, error) {
	var document batchXMLDocument
	value := body
	if bytes.HasPrefix(value, []byte{0xef, 0xbb, 0xbf}) {
		document.hasBOM = true
		value = value[3:]
	}
	decoder := xml.NewDecoder(bytes.NewReader(value))
	seenRoot := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			if !seenRoot {
				return batchXMLDocument{}, errors.New("Plex XML metadata batch response is empty")
			}
			return document, nil
		}
		if err != nil {
			return batchXMLDocument{}, fmt.Errorf("parse Plex XML metadata batch: %w", err)
		}
		if !seenRoot {
			switch typed := token.(type) {
			case xml.StartElement:
				node := &batchXMLNode{}
				if err := decoder.DecodeElement(node, &typed); err != nil {
					return batchXMLDocument{}, fmt.Errorf("parse Plex XML metadata batch: %w", err)
				}
				document.root = node
				seenRoot = true
			default:
				content, err := batchXMLContentFromToken(token)
				if err != nil {
					return batchXMLDocument{}, err
				}
				if content.kind == batchXMLText && len(bytes.TrimSpace(content.text)) != 0 {
					return batchXMLDocument{}, errors.New("Plex XML metadata batch has leading non-whitespace text")
				}
				document.leading = append(document.leading, content)
			}
			continue
		}
		content, err := batchXMLContentFromToken(token)
		if err != nil {
			return batchXMLDocument{}, err
		}
		if content.kind == batchXMLText && len(bytes.TrimSpace(content.text)) != 0 {
			return batchXMLDocument{}, errors.New("Plex XML metadata batch has trailing non-whitespace text")
		}
		document.trailing = append(document.trailing, content)
	}
}

func (node *batchXMLNode) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	node.start = cloneXMLStartElement(start)
	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch typed := token.(type) {
		case xml.StartElement:
			child := &batchXMLNode{}
			if err := decoder.DecodeElement(child, &typed); err != nil {
				return err
			}
			node.children = append(node.children, batchXMLContent{kind: batchXMLElement, element: child})
		case xml.EndElement:
			if typed.Name != start.Name {
				return errors.New("Plex XML metadata batch has mismatched element")
			}
			return nil
		default:
			content, err := batchXMLContentFromToken(token)
			if err != nil {
				return err
			}
			node.children = append(node.children, content)
		}
	}
}

func batchXMLContentFromToken(token xml.Token) (batchXMLContent, error) {
	switch typed := token.(type) {
	case xml.CharData:
		return batchXMLContent{kind: batchXMLText, text: append([]byte(nil), typed...)}, nil
	case xml.Comment:
		return batchXMLContent{kind: batchXMLComment, comment: append([]byte(nil), typed...)}, nil
	case xml.Directive:
		return batchXMLContent{kind: batchXMLDirective, directive: append([]byte(nil), typed...)}, nil
	case xml.ProcInst:
		return batchXMLContent{kind: batchXMLProcInst, procInst: xml.ProcInst{Target: typed.Target, Inst: append([]byte(nil), typed.Inst...)}}, nil
	default:
		return batchXMLContent{}, fmt.Errorf("Plex XML metadata batch has unsupported token %T", token)
	}
}

func (document batchXMLDocument) cloneFor(ratingKey string, item *batchXMLNode) batchXMLDocument {
	clone := batchXMLDocument{hasBOM: document.hasBOM}
	clone.leading = cloneXMLContents(document.leading)
	clone.trailing = cloneXMLContents(document.trailing)
	clone.root = cloneXMLNode(document.root)
	clone.root.start.Attr = setXMLSizeAttribute(clone.root.start.Attr)
	children := make([]batchXMLContent, 0, len(clone.root.children))
	for _, child := range clone.root.children {
		if child.kind != batchXMLElement {
			children = append(children, child)
			continue
		}
		childRatingKey, present, _ := xmlBatchRatingKey(child.element.start.Attr)
		if present && childRatingKey != ratingKey {
			continue
		}
		children = append(children, child)
	}
	clone.root.children = children
	return clone
}

func cloneXMLContents(contents []batchXMLContent) []batchXMLContent {
	clone := make([]batchXMLContent, 0, len(contents))
	for _, content := range contents {
		copyContent := content
		copyContent.text = append([]byte(nil), content.text...)
		copyContent.comment = append([]byte(nil), content.comment...)
		copyContent.directive = append([]byte(nil), content.directive...)
		copyContent.procInst.Inst = append([]byte(nil), content.procInst.Inst...)
		if content.element != nil {
			copyContent.element = cloneXMLNode(content.element)
		}
		clone = append(clone, copyContent)
	}
	return clone
}

func cloneXMLNode(node *batchXMLNode) *batchXMLNode {
	if node == nil {
		return nil
	}
	return &batchXMLNode{start: cloneXMLStartElement(node.start), children: cloneXMLContents(node.children)}
}

func cloneXMLStartElement(start xml.StartElement) xml.StartElement {
	clone := start
	clone.Attr = append([]xml.Attr(nil), start.Attr...)
	return clone
}

func setXMLSizeAttribute(attributes []xml.Attr) []xml.Attr {
	for index := range attributes {
		if attributes[index].Name.Local == "size" {
			attributes[index].Value = "1"
			return attributes
		}
	}
	return append(attributes, xml.Attr{Name: xml.Name{Local: "size"}, Value: "1"})
}

func encodeBatchXMLDocument(document batchXMLDocument) ([]byte, error) {
	var buffer bytes.Buffer
	if document.hasBOM {
		buffer.Write([]byte{0xef, 0xbb, 0xbf})
	}
	encoder := xml.NewEncoder(&buffer)
	declaration := -1
	for index, content := range document.leading {
		if content.kind == batchXMLProcInst && strings.EqualFold(content.procInst.Target, "xml") {
			declaration = index
			break
		}
	}
	if declaration >= 0 {
		if err := encodeBatchXMLContent(encoder, document.leading[declaration]); err != nil {
			return nil, fmt.Errorf("encode Plex XML metadata batch: %w", err)
		}
	}
	for index, content := range document.leading {
		if index == declaration {
			continue
		}
		if err := encodeBatchXMLContent(encoder, content); err != nil {
			return nil, fmt.Errorf("encode Plex XML metadata batch: %w", err)
		}
	}
	if document.root == nil {
		return nil, errors.New("Plex XML metadata batch has no root")
	}
	if err := encodeBatchXMLNode(encoder, document.root); err != nil {
		return nil, fmt.Errorf("encode Plex XML metadata batch: %w", err)
	}
	for _, content := range document.trailing {
		if err := encodeBatchXMLContent(encoder, content); err != nil {
			return nil, fmt.Errorf("encode Plex XML metadata batch: %w", err)
		}
	}
	if err := encoder.Flush(); err != nil {
		return nil, fmt.Errorf("encode Plex XML metadata batch: %w", err)
	}
	return buffer.Bytes(), nil
}

func encodeBatchXMLNode(encoder *xml.Encoder, node *batchXMLNode) error {
	if err := encoder.EncodeToken(node.start); err != nil {
		return err
	}
	for _, content := range node.children {
		if err := encodeBatchXMLContent(encoder, content); err != nil {
			return err
		}
	}
	return encoder.EncodeToken(nodeEndElement(node.start))
}

func encodeBatchXMLContent(encoder *xml.Encoder, content batchXMLContent) error {
	switch content.kind {
	case batchXMLText:
		return encoder.EncodeToken(xml.CharData(content.text))
	case batchXMLElement:
		return encodeBatchXMLNode(encoder, content.element)
	case batchXMLComment:
		return encoder.EncodeToken(xml.Comment(content.comment))
	case batchXMLDirective:
		return encoder.EncodeToken(xml.Directive(content.directive))
	case batchXMLProcInst:
		return encoder.EncodeToken(content.procInst)
	default:
		return fmt.Errorf("unsupported XML content kind %d", content.kind)
	}
}

func nodeEndElement(start xml.StartElement) xml.EndElement {
	return xml.EndElement{Name: start.Name}
}

func hasUTF8BOM(value []byte) bool {
	return bytes.HasPrefix(value, []byte{0xef, 0xbb, 0xbf})
}
