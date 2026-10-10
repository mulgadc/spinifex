// Package state owns the persisted EC2 instance record and its evolution. It
// wraps the legacy vm.InstanceRecord until the spec and status types move here.
package state

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mulgadc/spinifex/spinifex/foundation/lifecycle/resource"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// Record formats carried in the top-level "format" member. An absent member
// is RecordFormatLegacy, the shape every record has been written in so far.
const (
	RecordFormatLegacy     = 1
	RecordFormatTransition = 2
)

// recordFormatKey is the top-level member that names a record's format.
const recordFormatKey = "format"

// Decode and encode failures. Each class is a distinct sentinel so a caller
// can tell a corrupt record from one written by a newer release.
var (
	ErrRecordMalformed          = errors.New("instance record is not valid JSON for its schema")
	ErrRecordNotObject          = errors.New("instance record is not a JSON object")
	ErrRecordTrailingData       = errors.New("instance record has data after its JSON value")
	ErrRecordDuplicateKey       = errors.New("instance record has a duplicate key")
	ErrRecordCaseAliasKey       = errors.New("instance record key differs only in case from a known field")
	ErrRecordFormatNotInteger   = errors.New("instance record format is not an integer")
	ErrRecordFormatOutOfRange   = errors.New("instance record format is below 1")
	ErrRecordFormatAhead        = errors.New("instance record format is newer than this binary can write")
	ErrRecordSectionsNeedFormat = errors.New("assignment and observation need record format 2")
	ErrRecordExtensionConflict  = errors.New("instance record change would drop an unknown member inside an array")
)

// AssignmentReason says why an assignment changed.
type AssignmentReason string

const (
	AssignmentLaunch   AssignmentReason = "launch"
	AssignmentStart    AssignmentReason = "start"
	AssignmentRecovery AssignmentReason = "recovery"
	AssignmentRelease  AssignmentReason = "release"
	AssignmentOperator AssignmentReason = "operator"
	AssignmentMigrated AssignmentReason = "migrated"
)

// InstanceAssignment is the single execution assignment of a format-2
// record. A node holds (instance, Epoch, Generation, Token) as its fence.
type InstanceAssignment struct {
	Node                 string           `json:"node,omitempty"`
	Generation           int64            `json:"generation,omitempty"`
	Epoch                int64            `json:"epoch,omitempty"`
	Token                string           `json:"token,omitempty"`
	ForDesiredGeneration int64            `json:"for_desired_generation,omitempty"`
	Reason               AssignmentReason `json:"reason,omitempty"`
	StorageFence         string           `json:"storage_fence,omitempty"`
}

// ObservedState is the canonical state the assigned node reports. It takes
// the lifecycle values plus the two below, which have no lifecycle state.
type ObservedState string

const (
	ObservedUnconfirmed ObservedState = "unconfirmed"
	ObservedDegraded    ObservedState = "degraded"
)

// EvidenceKind names what an observation's evidence was taken from.
type EvidenceKind string

const (
	EvidenceQMP      EvidenceKind = "qmp"
	EvidenceProcess  EvidenceKind = "process"
	EvidenceJournal  EvidenceKind = "journal"
	EvidenceTeardown EvidenceKind = "teardown"
)

// ObservationEvidence is the proof behind an observation.
type ObservationEvidence struct {
	Kind        EvidenceKind `json:"kind,omitempty"`
	Incarnation string       `json:"incarnation,omitempty"`
	TakenAt     time.Time    `json:"taken_at,omitzero"`
}

// InstanceObservation is the assigned node's report in a format-2 record,
// tagged with the assignment it was made under.
type InstanceObservation struct {
	Node                 string               `json:"node,omitempty"`
	AssignmentEpoch      int64                `json:"assignment_epoch,omitempty"`
	AssignmentGeneration int64                `json:"assignment_generation,omitempty"`
	AssignmentToken      string               `json:"assignment_token,omitempty"`
	DesiredGeneration    int64                `json:"desired_generation,omitempty"`
	State                ObservedState        `json:"state,omitempty"`
	Evidence             *ObservationEvidence `json:"evidence,omitempty"`
	ReportedAt           time.Time            `json:"reported_at,omitzero"`
}

// transitionWire is the typed shape of a format-2 record. Its first three
// members are vm.InstanceRecord's, so a format-1 record is its prefix.
type transitionWire struct {
	Metadata    resource.Metadata    `json:"metadata"`
	Spec        vm.InstanceSpec      `json:"spec"`
	Status      vm.InstanceStatus    `json:"status"`
	Assignment  *InstanceAssignment  `json:"assignment,omitempty"`
	Observation *InstanceObservation `json:"observation,omitempty"`
}

// TransitionRecord is a decoded instance record plus the JSON it came from,
// so Encode can write back what the caller did not change as it was read. The
// zero value encodes as a new format-1 record built from the typed fields.
type TransitionRecord struct {
	Record      vm.InstanceRecord
	Assignment  *InstanceAssignment
	Observation *InstanceObservation

	format   int
	readOnly bool
	raw      map[string]any // the decoded input, numbers kept as json.Number
	snapshot map[string]any // the typed fields as decoded, re-encoded
}

// Format is the record's format: 1 when the marker is absent.
func (r *TransitionRecord) Format() int {
	if r.format == 0 {
		return RecordFormatLegacy
	}
	return r.format
}

// ReadOnly reports a record in a format newer than this binary writes. Its
// legacy fields are readable; Encode refuses it.
func (r *TransitionRecord) ReadOnly() bool { return r.readOnly }

// DecodeTransitionRecord decodes data, rejecting malformed JSON, duplicate or
// case-aliased keys at any depth, trailing data and an invalid format marker.
// A format above 2 decodes its legacy fields and is marked read-only.
func DecodeTransitionRecord(data []byte) (*TransitionRecord, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%w: invalid UTF-8", ErrRecordMalformed)
	}
	if err := validateRecordTokens(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRecordMalformed, err)
	}

	format, err := recordFormat(raw)
	if err != nil {
		return nil, err
	}
	r := &TransitionRecord{format: format, raw: raw}
	if format > RecordFormatTransition {
		r.readOnly = true
	}

	if format == RecordFormatTransition {
		var w transitionWire
		if err := json.Unmarshal(data, &w); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrRecordMalformed, err)
		}
		r.Record = vm.InstanceRecord{Metadata: w.Metadata, Spec: w.Spec, Status: w.Status}
		r.Assignment, r.Observation = w.Assignment, w.Observation
	} else if err := json.Unmarshal(data, &r.Record); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRecordMalformed, err)
	}

	r.snapshot, err = r.typedTree()
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Encode writes only the typed paths that differ from the decoded snapshot
// over the input, keeping unknown members. A changed array is replaced whole,
// and refused if its read elements carry unknown members. It adds no marker.
func (r *TransitionRecord) Encode() ([]byte, error) {
	if r.readOnly {
		return nil, fmt.Errorf("%w: format %d", ErrRecordFormatAhead, r.format)
	}
	if r.Format() != RecordFormatTransition && (r.Assignment != nil || r.Observation != nil) {
		return nil, fmt.Errorf("%w: record is format %d", ErrRecordSectionsNeedFormat, r.Format())
	}
	current, err := r.typedTree()
	if err != nil {
		return nil, err
	}
	raw, snapshot := r.raw, r.snapshot
	if raw == nil {
		raw, snapshot = map[string]any{}, map[string]any{}
	}
	merged, err := mergeChanged(transitionSchema(), raw, snapshot, current, "$")
	if err != nil {
		return nil, err
	}
	return json.Marshal(merged)
}

// typedTree re-encodes the typed fields that the record's format covers.
func (r *TransitionRecord) typedTree() (map[string]any, error) {
	var (
		out []byte
		err error
	)
	if r.Format() == RecordFormatTransition {
		out, err = json.Marshal(transitionWire{
			Metadata: r.Record.Metadata, Spec: r.Record.Spec, Status: r.Record.Status,
			Assignment: r.Assignment, Observation: r.Observation,
		})
	} else {
		out, err = json.Marshal(&r.Record)
	}
	if err != nil {
		return nil, fmt.Errorf("encode instance record: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	var tree map[string]any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("re-read instance record: %w", err)
	}
	return tree, nil
}

// mergeChanged applies the paths where current differs from snapshot onto
// raw. A struct merges per member and keeps members it does not know; a map
// merges per entry; any other differing value is replaced by current's.
func mergeChanged(s *fieldSchema, raw, snapshot, current any, path string) (any, error) {
	if reflect.DeepEqual(snapshot, current) {
		return raw, nil
	}
	if rawArr, ok := raw.([]any); ok && s != nil {
		if at, found := unknownMember(s, rawArr, path); found {
			return nil, fmt.Errorf("%w: changed array %s would drop unknown member %s", ErrRecordExtensionConflict, path, at)
		}
	}
	rawObj, rawIsObj := raw.(map[string]any)
	snapObj, snapIsObj := snapshot.(map[string]any)
	curObj, curIsObj := current.(map[string]any)
	if s == nil || (s.fields == nil && !s.isMap) || !rawIsObj || !snapIsObj || !curIsObj {
		return current, nil
	}
	out := make(map[string]any, len(rawObj))
	for k, v := range rawObj {
		_, inSnap := snapObj[k]
		_, inCur := curObj[k]
		switch {
		case !s.isMap && (!inSnap || inCur):
			out[k] = v // unknown, or known and still present
		case s.isMap && inSnap && inCur:
			out[k] = v // a map key the typed decode did not normalise
		}
	}
	for k, curV := range curObj {
		snapV, inSnap := snapObj[k]
		rawV, inRaw := out[k]
		child := s.elem
		if !s.isMap {
			child = s.fields[k]
		}
		switch {
		case inSnap && inRaw:
			merged, err := mergeChanged(child, rawV, snapV, curV, path+"."+k)
			if err != nil {
				return nil, err
			}
			out[k] = merged
		case inSnap && !s.isMap && reflect.DeepEqual(snapV, curV):
			// A zero member the input left out stays out.
		default:
			out[k] = curV
		}
	}
	return out, nil
}

// unknownMember finds the first member of v, at any depth, that schema s does
// not know. An opaque value (nil schema) is not inspected.
func unknownMember(s *fieldSchema, v any, path string) (string, bool) {
	if s == nil {
		return "", false
	}
	switch v := v.(type) {
	case []any:
		for i, e := range v {
			if at, found := unknownMember(s.elem, e, path+"["+strconv.Itoa(i)+"]"); found {
				return at, true
			}
		}
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			child := s.elem
			if !s.isMap {
				known, ok := s.fields[k]
				if !ok {
					return path + "." + k, true
				}
				child = known
			}
			if at, found := unknownMember(child, v[k], path+"."+k); found {
				return at, true
			}
		}
	}
	return "", false
}

// recordFormat reads the marker: absent is 1, an integer above 2 is returned
// for read-only handling, anything else is refused.
func recordFormat(raw map[string]any) (int, error) {
	v, ok := raw[recordFormatKey]
	if !ok {
		return RecordFormatLegacy, nil
	}
	n, ok := v.(json.Number)
	if !ok || !isIntegerLiteral(n.String()) {
		return 0, fmt.Errorf("%w: %v", ErrRecordFormatNotInteger, v)
	}
	lit := n.String()
	if strings.HasPrefix(lit, "-") {
		return 0, fmt.Errorf("%w: %s", ErrRecordFormatOutOfRange, lit)
	}
	if len(lit) > 9 {
		return math.MaxInt32, nil // newer than anything this binary can name
	}
	f, err := strconv.Atoi(lit)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrRecordFormatNotInteger, err)
	}
	if f < RecordFormatLegacy {
		return 0, fmt.Errorf("%w: %d", ErrRecordFormatOutOfRange, f)
	}
	return f, nil
}

// isIntegerLiteral accepts only JSON's integer grammar, so 2.0 and 2e0 are
// not integers even though they denote one.
func isIntegerLiteral(s string) bool {
	s = strings.TrimPrefix(s, "-")
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// fieldSchema is the set of JSON member names a Go type decodes. A nil
// schema is a leaf or an opaque value: nothing below it is checked.
type fieldSchema struct {
	fields map[string]*fieldSchema // struct members by exact JSON name
	elem   *fieldSchema            // element of a slice or value of a map
	isMap  bool
}

var (
	recordSchemaOnce sync.Once
	recordSchema     *fieldSchema
)

// transitionSchema covers every format, so a case alias of a format-2
// section name is refused in a format-1 record too.
func transitionSchema() *fieldSchema {
	recordSchemaOnce.Do(func() {
		recordSchema = schemaOf(reflect.TypeFor[transitionWire](), map[reflect.Type]*fieldSchema{})
		recordSchema.fields[recordFormatKey] = nil
	})
	return recordSchema
}

var (
	jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

func schemaOf(t reflect.Type, seen map[reflect.Type]*fieldSchema) *fieldSchema {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	pt := reflect.PointerTo(t)
	if pt.Implements(jsonUnmarshalerType) || pt.Implements(textUnmarshalerType) {
		return nil
	}
	if s, ok := seen[t]; ok {
		return s
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		s := &fieldSchema{isMap: t.Kind() == reflect.Map}
		seen[t] = s
		s.elem = schemaOf(t.Elem(), seen)
		return s
	case reflect.Struct:
		s := &fieldSchema{fields: map[string]*fieldSchema{}}
		seen[t] = s
		addStructFields(s, t, seen)
		return s
	default:
		return nil
	}
}

// addStructFields follows encoding/json's naming: the tag name, else the Go
// name; "-" skips; an untagged embedded struct promotes its members.
func addStructFields(s *fieldSchema, t reflect.Type, seen map[reflect.Type]*fieldSchema) {
	for f := range t.Fields() {
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			et := f.Type
			if et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				addStructFields(s, et, seen)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		s.fields[name] = schemaOf(f.Type, seen)
	}
}

// validateRecordTokens walks data token by token. encoding/json keeps the
// last of two duplicate keys and matches keys case-insensitively, so both
// would otherwise pass silently.
func validateRecordTokens(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRecordMalformed, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("%w: starts with %v", ErrRecordNotObject, tok)
	}
	if err := walkObject(dec, transitionSchema(), "$"); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return ErrRecordTrailingData
	}
	return nil
}

// walkObject consumes an object whose '{' has been read.
func walkObject(dec *json.Decoder, s *fieldSchema, path string) error {
	keys := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("%w: %w", ErrRecordMalformed, err)
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("%w: key %v at %s", ErrRecordMalformed, tok, path)
		}
		if keys[key] {
			return fmt.Errorf("%w: %q at %s", ErrRecordDuplicateKey, key, path)
		}
		keys[key] = true

		var child *fieldSchema
		if s != nil && s.fields != nil {
			known, exact := s.fields[key]
			if !exact {
				for name := range s.fields {
					if strings.EqualFold(name, key) {
						return fmt.Errorf("%w: %q aliases %q at %s", ErrRecordCaseAliasKey, key, name, path)
					}
				}
			}
			child = known
		} else if s != nil {
			child = s.elem
		}
		if err := walkValue(dec, child, path+"."+key); err != nil {
			return err
		}
	}
	return closeDelim(dec, '}', path)
}

func walkValue(dec *json.Decoder, s *fieldSchema, path string) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRecordMalformed, err)
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		return walkObject(dec, s, path)
	case '[':
		var elem *fieldSchema
		if s != nil {
			elem = s.elem
		}
		for i := 0; dec.More(); i++ {
			if err := walkValue(dec, elem, path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
		return closeDelim(dec, ']', path)
	default:
		return fmt.Errorf("%w: unexpected %v at %s", ErrRecordMalformed, d, path)
	}
}

func closeDelim(dec *json.Decoder, want json.Delim, path string) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRecordMalformed, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf("%w: expected %v at %s", ErrRecordMalformed, want, path)
	}
	return nil
}
