package launchd

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"sort"
)

type plistEntry struct {
	Key      string
	String   string
	Array    []string
	Dict     map[string]string
	Bool     *bool
	BoolDict map[string]bool
}

type plistDocument struct {
	XMLName xml.Name `xml:"plist"`
	Version string   `xml:"version,attr"`
	Entries []plistEntry
}

func (p plistDocument) MarshalXML(enc *xml.Encoder, start xml.StartElement) error {
	start.Name.Local = "plist"
	start.Attr = []xml.Attr{{Name: xml.Name{Local: "version"}, Value: p.Version}}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	if err := enc.EncodeToken(xml.StartElement{Name: xml.Name{Local: "dict"}}); err != nil {
		return err
	}
	for _, entry := range p.Entries {
		if err := encodeTextElement(enc, "key", entry.Key); err != nil {
			return err
		}
		switch {
		case entry.Array != nil:
			if err := enc.EncodeToken(xml.StartElement{Name: xml.Name{Local: "array"}}); err != nil {
				return err
			}
			for _, value := range entry.Array {
				if err := encodeTextElement(enc, "string", value); err != nil {
					return err
				}
			}
			if err := enc.EncodeToken(xml.EndElement{Name: xml.Name{Local: "array"}}); err != nil {
				return err
			}
		case entry.Dict != nil:
			if err := encodeStringDict(enc, entry.Dict); err != nil {
				return err
			}
		case entry.BoolDict != nil:
			if err := encodeBoolDict(enc, entry.BoolDict); err != nil {
				return err
			}
		case entry.Bool != nil:
			if err := encodeBool(enc, *entry.Bool); err != nil {
				return err
			}
		default:
			if err := encodeTextElement(enc, "string", entry.String); err != nil {
				return err
			}
		}
	}
	if err := enc.EncodeToken(xml.EndElement{Name: xml.Name{Local: "dict"}}); err != nil {
		return err
	}
	return enc.EncodeToken(start.End())
}

func encodeStringDict(enc *xml.Encoder, values map[string]string) error {
	if err := enc.EncodeToken(xml.StartElement{Name: xml.Name{Local: "dict"}}); err != nil {
		return err
	}
	// Environment order is deliberately deterministic for reviewable service files.
	keys := sortedKeys(values)
	for _, key := range keys {
		if err := encodeTextElement(enc, "key", key); err != nil {
			return err
		}
		if err := encodeTextElement(enc, "string", values[key]); err != nil {
			return err
		}
	}
	return enc.EncodeToken(xml.EndElement{Name: xml.Name{Local: "dict"}})
}

func encodeBoolDict(enc *xml.Encoder, values map[string]bool) error {
	if err := enc.EncodeToken(xml.StartElement{Name: xml.Name{Local: "dict"}}); err != nil {
		return err
	}
	for _, key := range sortedKeys(values) {
		if err := encodeTextElement(enc, "key", key); err != nil {
			return err
		}
		if err := encodeBool(enc, values[key]); err != nil {
			return err
		}
	}
	return enc.EncodeToken(xml.EndElement{Name: xml.Name{Local: "dict"}})
}

func encodeBool(enc *xml.Encoder, value bool) error {
	name := "false"
	if value {
		name = "true"
	}
	start := xml.StartElement{Name: xml.Name{Local: name}}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	return enc.EncodeToken(start.End())
}

func encodeTextElement(enc *xml.Encoder, name, value string) error {
	start := xml.StartElement{Name: xml.Name{Local: name}}
	return enc.EncodeElement(value, start)
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func isOwnedPlist(body []byte, label, instanceID string) (bool, error) {
	root, err := parsePlist(body)
	if err != nil {
		return false, err
	}
	environment, ok := root["EnvironmentVariables"].(map[string]any)
	if !ok {
		return false, nil
	}
	return stringValue(root["Label"]) == label &&
		stringValue(environment[ownerEnvironmentKey]) == ownerEnvironmentVal &&
		stringValue(environment[instanceEnvironmentKey]) == instanceID, nil
}

func plistLogPaths(body []byte) (string, string, error) {
	root, err := parsePlist(body)
	if err != nil {
		return "", "", err
	}
	stdout := stringValue(root["StandardOutPath"])
	stderr := stringValue(root["StandardErrorPath"])
	if stdout == "" || stderr == "" {
		return "", "", fmt.Errorf("plist does not contain both log paths")
	}
	return stdout, stderr, nil
}

func parsePlist(body []byte) (map[string]any, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "plist" {
			continue
		}
		for {
			token, err = decoder.Token()
			if err != nil {
				return nil, err
			}
			child, ok := token.(xml.StartElement)
			if ok && child.Name.Local == "dict" {
				value, err := decodeDict(decoder, child)
				if err != nil {
					return nil, err
				}
				return value, nil
			}
		}
	}
}

func decodeDict(decoder *xml.Decoder, start xml.StartElement) (map[string]any, error) {
	values := make(map[string]any)
	var key string
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch token := token.(type) {
		case xml.StartElement:
			if token.Name.Local == "key" {
				if err := decoder.DecodeElement(&key, &token); err != nil {
					return nil, err
				}
				continue
			}
			if key == "" {
				return nil, fmt.Errorf("plist dictionary value has no key")
			}
			value, err := decodeValue(decoder, token)
			if err != nil {
				return nil, err
			}
			values[key] = value
			key = ""
		case xml.EndElement:
			if token.Name == start.Name {
				if key != "" {
					return nil, fmt.Errorf("plist dictionary key has no value")
				}
				return values, nil
			}
		}
	}
}

func decodeValue(decoder *xml.Decoder, start xml.StartElement) (any, error) {
	switch start.Name.Local {
	case "string", "integer":
		var value string
		if err := decoder.DecodeElement(&value, &start); err != nil {
			return nil, err
		}
		return value, nil
	case "true", "false":
		value := start.Name.Local == "true"
		if err := consumeEmpty(decoder, start); err != nil {
			return nil, err
		}
		return value, nil
	case "dict":
		return decodeDict(decoder, start)
	case "array":
		return decodeArray(decoder, start)
	default:
		return nil, fmt.Errorf("unsupported plist element %q", start.Name.Local)
	}
}

func decodeArray(decoder *xml.Decoder, start xml.StartElement) ([]any, error) {
	var values []any
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch token := token.(type) {
		case xml.StartElement:
			value, err := decodeValue(decoder, token)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		case xml.EndElement:
			if token.Name == start.Name {
				return values, nil
			}
		}
	}
}

func consumeEmpty(decoder *xml.Decoder, start xml.StartElement) error {
	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if end, ok := token.(xml.EndElement); ok && end.Name == start.Name {
			return nil
		}
		if chars, ok := token.(xml.CharData); ok && len(bytes.TrimSpace(chars)) == 0 {
			continue
		}
		return fmt.Errorf("plist boolean contains unexpected content")
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
