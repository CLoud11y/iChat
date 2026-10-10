package messaging

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"unicode/utf8"
)

func DecodeRequest(body []byte) (SendRequest, error) {
	var request SendRequest
	if err := decodeObject(body, &request); err != nil {
		return SendRequest{}, err
	}
	return request, nil
}

func DecodeEnvelope(body []byte) (Envelope, error) {
	var envelope Envelope
	if err := decodeObject(body, &envelope); err != nil {
		return Envelope{}, err
	}
	if _, err := ValidateEnvelope(envelope); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

// Both wire objects are flat structs. Exact JSON names, duplicate rejection,
// and null rejection avoid encoding/json's case aliases and last-value wins.
// The HTTP adapter must additionally bound reads before allocating the body.
func decodeObject(body []byte, destination any) error {
	if len(body) > MaxBodyBytes || !utf8.Valid(body) || !json.Valid(body) || !validUnicodeEscapes(body) {
		return invalid("JSON body")
	}
	value := reflect.ValueOf(destination).Elem()
	fields := make(map[string]any, value.NumField())
	for i := 0; i < value.NumField(); i++ {
		name := value.Type().Field(i).Tag.Get("json")
		for j, c := range name {
			if c == ',' {
				name = name[:j]
				break
			}
		}
		fields[name] = value.Field(i).Addr().Interface()
	}
	d := json.NewDecoder(bytes.NewReader(body))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return invalid("JSON object")
	}
	seen := make(map[string]bool, len(fields))
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return invalid("JSON field")
		}
		name, ok := token.(string)
		field, exists := fields[name]
		if !ok || !exists || seen[name] {
			return invalid("JSON field")
		}
		seen[name] = true
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil || bytes.Equal(raw, []byte("null")) {
			return invalid(name)
		}
		if err := json.Unmarshal(raw, field); err != nil {
			return invalid(name)
		}
	}
	return nil // json.Valid already checked the closing brace and EOF.
}

// encoding/json replaces unpaired UTF-16 surrogate escapes with U+FFFD.
// Reject them before decoding so malformed text never silently changes content.
// Syntax has already been checked by json.Valid; legitimate U+FFFD is allowed.
func validUnicodeEscapes(body []byte) bool {
	inString := false
	for i := 0; i < len(body); i++ {
		if body[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || body[i] != '\\' {
			continue
		}
		if body[i+1] != 'u' {
			i++
			continue
		}
		n, _ := strconv.ParseUint(string(body[i+2:i+6]), 16, 16)
		i += 5
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(body) || body[i+1] != '\\' || body[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(body[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}
