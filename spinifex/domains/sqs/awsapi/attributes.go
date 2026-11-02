package awsapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// errMalformedText marks JSON text encoding/json would silently repair:
// malformed UTF-8 or an unpaired surrogate escape.
var errMalformedText = errors.New("malformed UTF-8 or UTF-16")

// decodeMessageAttributes reads a MessageAttributes object exactly: names
// and members keep their order and spelling, and an unknown or repeated
// name or member, which encoding/json would resolve silently, is refused. A JSON
// structure error is a serialization fault; a bad value is the owner's
// ErrInvalidParameter, which a batch reports per entry.
func decodeMessageAttributes(raw json.RawMessage) ([]message.Attribute, error) {
	if raw == nil || string(raw) == "null" {
		return nil, nil
	}
	if err := checkJSONText(raw); err != nil {
		if errors.Is(err, errMalformedText) {
			return nil, fmt.Errorf("%w: message attribute names and strings must be well-formed UTF-8 or UTF-16", message.ErrInvalidParameter)
		}
		return nil, err
	}
	members, err := objectMembers(raw)
	if err != nil {
		return nil, err
	}
	out := make([]message.Attribute, 0, len(members))
	for _, m := range members {
		v, err := decodeAttributeValue(m.name, m.value)
		if err != nil {
			return nil, err
		}
		out = append(out, message.Attribute{Name: m.name, Value: v})
	}
	return out, nil
}

func decodeAttributeValue(name string, raw json.RawMessage) (message.AttributeValue, error) {
	members, err := objectMembers(raw)
	if err != nil {
		return message.AttributeValue{}, err
	}
	var v message.AttributeValue
	for _, m := range members {
		if string(m.value) == "null" {
			continue
		}
		var err error
		switch m.name {
		case "DataType":
			err = json.Unmarshal(m.value, &v.DataType)
		case "StringValue":
			var s string
			if err = json.Unmarshal(m.value, &s); err == nil {
				v.StringValue = &s
			}
		case "BinaryValue":
			v.BinaryValue, err = decodeBlob(m.value)
		case "StringListValues":
			v.StringListValues = []string{}
			err = json.Unmarshal(m.value, &v.StringListValues)
		case "BinaryListValues":
			v.BinaryListValues = [][]byte{}
			err = json.Unmarshal(m.value, &v.BinaryListValues)
		default:
			return message.AttributeValue{}, fmt.Errorf("%w: message attribute %q has an unknown member", message.ErrInvalidParameter, name)
		}
		if err != nil {
			return message.AttributeValue{}, errors.New(awserrors.ErrorSerializationException)
		}
	}
	return v, nil
}

// decodeBlob decodes a JSON base64 blob strictly; an empty string is a
// present, empty value.
func decodeBlob(raw json.RawMessage) ([]byte, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, err
	}
	if b == nil {
		b = []byte{}
	}
	return b, nil
}

type member struct {
	name  string
	value json.RawMessage
}

// objectMembers lists a JSON object's members in order, refusing a repeated
// name, which encoding/json would resolve silently to the last.
func objectMembers(raw json.RawMessage) ([]member, error) {
	serr := errors.New(awserrors.ErrorSerializationException)
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, serr
	}
	var out []member
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, serr
		}
		name, ok := t.(string)
		if !ok {
			return nil, serr
		}
		if seen[name] {
			return nil, fmt.Errorf("%w: message attribute JSON repeats the name %q", message.ErrInvalidParameter, name)
		}
		seen[name] = true
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, serr
		}
		out = append(out, member{name: name, value: v})
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, serr
	}
	return out, nil
}

// checkJSONText refuses JSON whose strings encoding/json would repair to
// U+FFFD: malformed UTF-8, or a \u escape of an unpaired surrogate. A
// literal or escaped U+FFFD and a valid escaped pair are kept.
func checkJSONText(raw []byte) error {
	if !utf8.Valid(raw) {
		return errMalformedText
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) || raw[i] != 'u' {
			continue
		}
		r, ok := hex4(raw, i+1)
		if !ok {
			return errors.New(awserrors.ErrorSerializationException)
		}
		i += 4
		switch {
		case utf16.IsSurrogate(r) && r < 0xDC00:
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return errMalformedText
			}
			lo, ok := hex4(raw, i+3)
			if !ok || lo < 0xDC00 || lo > 0xDFFF {
				return errMalformedText
			}
			i += 6
		case utf16.IsSurrogate(r):
			return errMalformedText
		}
	}
	return nil
}
