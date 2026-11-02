package queue

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
)

// RecordVersion is the schema version this release writes.
const RecordVersion = 1

// MaxNameLen is AWS's queue-name limit (ADR-0012 row A15).
const MaxNameLen = 80

// Queue and binding states.
const (
	StateActive   = "active"
	StateDeleting = "deleting"

	BindingBound    = "bound"
	BindingReserved = "reserved"

	TypeStandard = "standard"
)

// Record is the authoritative queue record, keyed q/<account>/<queueId>.
// Fields a newer release adds are kept on decode and written back unchanged.
type Record struct {
	V                    int               `json:"v"`
	QueueID              string            `json:"queueId"`
	MessageShard         message.Shard     `json:"messageShard"`
	StateShard           message.Shard     `json:"stateShard"`
	Account              string            `json:"account"`
	Region               string            `json:"region"`
	Name                 string            `json:"name"`
	ARN                  string            `json:"arn"`
	CreatedAt            time.Time         `json:"createdAt"`
	Type                 string            `json:"type"`
	State                string            `json:"state"`
	Generation           uint64            `json:"generation"`
	Attributes           Attributes        `json:"attributes"`
	AttributesModifiedAt time.Time         `json:"attributesModifiedAt"`
	Tags                 map[string]string `json:"tags,omitempty"`
	Deletion             *Deletion         `json:"deletion,omitempty"`

	unknown map[string]json.RawMessage
}

// Deletion is the durable deletion intent of a deleting queue.
type Deletion struct {
	RequestedAt    time.Time `json:"requestedAt"`
	Generation     uint64    `json:"generation"`
	RetainedReason string    `json:"retainedReason,omitempty"`
	LastCheck      time.Time `json:"lastCheck,omitzero"`

	unknown map[string]json.RawMessage
}

type deletionFields Deletion

// MarshalJSON writes the known fields and every unknown field read earlier.
func (d *Deletion) MarshalJSON() ([]byte, error) {
	return marshalPreserving(deletionFields(*d), d.unknown)
}

// UnmarshalJSON reads the known fields and keeps the rest.
func (d *Deletion) UnmarshalJSON(b []byte) error {
	var f deletionFields
	unknown, err := unmarshalPreserving(b, &f)
	if err != nil {
		return err
	}
	*d = Deletion(f)
	d.unknown = unknown
	return nil
}

// Binding is a name's resolution, keyed n/<account>/<region>/<name>.
type Binding struct {
	V          int       `json:"v"`
	QueueID    string    `json:"queueId"`
	State      string    `json:"state"`
	ReuseAfter time.Time `json:"reuseAfter,omitzero"`

	unknown map[string]json.RawMessage
}

// Namespace is the queue's store namespace; it needs no store call to exist.
func (r *Record) Namespace() message.Namespace {
	return message.Namespace{MessageShard: r.MessageShard, StateShard: r.StateShard, Queue: message.QueueID(r.QueueID)}
}

type recordFields Record
type bindingFields Binding

// Encode writes the known fields and every unknown field read by Decode.
func (r *Record) Encode() ([]byte, error) {
	return marshalPreserving(recordFields(*r), r.unknown)
}

// DecodeRecord reads the known fields and keeps the rest for Encode.
func DecodeRecord(b []byte) (Record, error) {
	var f recordFields
	unknown, err := unmarshalPreserving(b, &f)
	if err != nil {
		return Record{}, err
	}
	r := Record(f)
	r.unknown = unknown
	return r, nil
}

// Encode writes the known fields and every unknown field read by Decode.
func (b *Binding) Encode() ([]byte, error) {
	return marshalPreserving(bindingFields(*b), b.unknown)
}

// DecodeBinding reads the known fields and keeps the rest for Encode.
func DecodeBinding(data []byte) (Binding, error) {
	var f bindingFields
	unknown, err := unmarshalPreserving(data, &f)
	if err != nil {
		return Binding{}, err
	}
	b := Binding(f)
	b.unknown = unknown
	return b, nil
}

func marshalPreserving(known any, unknown map[string]json.RawMessage) ([]byte, error) {
	b, err := json.Marshal(known)
	if err != nil || len(unknown) == 0 {
		return b, err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(b, &merged); err != nil {
		return nil, err
	}
	for k, v := range unknown {
		if _, ok := merged[k]; !ok {
			merged[k] = v
		}
	}
	return json.Marshal(merged)
}

func unmarshalPreserving(b []byte, known any) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(b, known); err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	names := jsonNames(reflect.TypeOf(known).Elem())
	for k := range all {
		if names[k] {
			delete(all, k)
		}
	}
	if len(all) == 0 {
		return nil, nil
	}
	return all, nil
}

func jsonNames(t reflect.Type) map[string]bool {
	names := make(map[string]bool, t.NumField())
	for f := range t.Fields() {
		if tag, ok := f.Tag.Lookup("json"); ok {
			names[strings.Split(tag, ",")[0]] = true
		}
	}
	return names
}

var queueIDEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewQueueID mints an unguessable per-create identifier: 128 random bits,
// base32, never derived from the name or account (ADR-0011:S9).
func NewQueueID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint queue ID: %w", err)
	}
	return queueIDEncoding.EncodeToString(b[:]), nil
}

// Allocate picks a new queue's message and state shards from the counts
// configured now (ADR-0011:S10). The result is recorded and never recomputed.
func Allocate(queueID string, messageStreams, stateBuckets int) (message.Shard, message.Shard, error) {
	if messageStreams < 1 || stateBuckets < 1 {
		return 0, 0, fmt.Errorf("allocate shards: counts %d and %d must be positive", messageStreams, stateBuckets)
	}
	if messageStreams > math.MaxUint32 || stateBuckets > math.MaxUint32 {
		return 0, 0, fmt.Errorf("allocate shards: counts %d and %d exceed the shard range", messageStreams, stateBuckets)
	}
	sum := sha256.Sum256([]byte(queueID))
	m := binary.BigEndian.Uint32(sum[0:4]) % uint32(messageStreams)
	s := binary.BigEndian.Uint32(sum[4:8]) % uint32(stateBuckets)
	return message.Shard(m), message.Shard(s), nil
}

// ValidateName enforces AWS's standard-queue name rule: 1 to 80 characters
// of letters, digits, hyphen and underscore, case-sensitive (row A15).
func ValidateName(name string) error {
	if name == "" || len(name) > MaxNameLen {
		return fmt.Errorf("%w: length %d outside 1..%d", ErrInvalidQueueName, len(name), MaxNameLen)
	}
	for _, r := range name {
		if !isNameRune(r) {
			return fmt.Errorf("%w: only letters, digits, hyphens and underscores are allowed", ErrInvalidQueueName)
		}
	}
	return nil
}

func isNameRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
}
