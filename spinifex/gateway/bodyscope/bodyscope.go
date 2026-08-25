// Package bodyscope reads the handful of identifier fields a policy scope
// resolver needs out of a JSON request body, without deserialising the typed
// input the handler will build from the same bytes.
//
// Two properties matter here. A type mismatch on an unrelated field cannot
// poison the parse and silently widen the request to "*", because every field
// is decoded on demand. And AWS JSON 1.1 spells fields lower-camel while the
// SDK structs are upper-camel, so lookups are case-insensitive by design
// rather than by relying on encoding/json's own fallback.
package bodyscope

import (
	"encoding/json"
	"log/slog"
	"strings"
)

// Scope is a parsed request body. The zero value is usable and reports every
// field as absent, which is what a body the gate cannot parse resolves to.
type Scope struct {
	fields map[string]json.RawMessage
}

// Parse reads body as a JSON object. A body that is empty or does not parse
// yields an empty Scope rather than an error: it is the handler that rejects a
// malformed request, so the caller sees a validation fault rather than a
// denial. action names the caller for the debug line only.
func Parse(action string, body []byte) Scope {
	if len(body) == 0 {
		return Scope{}
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		slog.Debug("bodyscope: body does not parse, authorizing account-wide", "action", action, "err", err)
		return Scope{}
	}
	fields := make(map[string]json.RawMessage, len(raw))
	for name, value := range raw {
		fields[strings.ToLower(name)] = value
	}
	return Scope{fields: fields}
}

// String returns the first field in names that holds a JSON string, or "" when
// none does. Several names are accepted because one action's identifier is
// spelled differently across the surfaces that carry it.
func (s Scope) String(names ...string) string {
	for _, name := range names {
		raw, ok := s.fields[strings.ToLower(name)]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			continue
		}
		if value != "" {
			return value
		}
	}
	return ""
}

// Strings returns the first field in names that holds a JSON array of strings,
// with empty elements dropped. A field of any other shape is skipped.
func (s Scope) Strings(names ...string) []string {
	for _, name := range names {
		raw, ok := s.fields[strings.ToLower(name)]
		if !ok {
			continue
		}
		var values []string
		if err := json.Unmarshal(raw, &values); err != nil {
			continue
		}
		out := make([]string, 0, len(values))
		for _, value := range values {
			if value != "" {
				out = append(out, value)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// Object returns the nested object held by the first field in names that holds
// one, or an empty Scope when none does. Some identifiers sit below the top
// level of the request.
func (s Scope) Object(names ...string) Scope {
	for _, name := range names {
		raw, ok := s.fields[strings.ToLower(name)]
		if !ok {
			continue
		}
		nested := Parse(name, raw)
		if len(nested.fields) > 0 {
			return nested
		}
	}
	return Scope{}
}

// Has reports whether the body carried any of names, whatever its shape. Used
// where the presence of an optional field decides whether a scope applies at
// all, rather than its value.
func (s Scope) Has(names ...string) bool {
	for _, name := range names {
		if _, ok := s.fields[strings.ToLower(name)]; ok {
			return true
		}
	}
	return false
}
