package jetstream

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/mulgadc/bluebottle/pkg/safecast"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go"
	jsapi "github.com/nats-io/nats.go/jetstream"
)

// metaHeaderPrefix keeps domain metadata clear of the Nats-* headers the
// server acts on.
const metaHeaderPrefix = "Sqs-Meta-"

// ErrUnsupportedTopology reports a cluster size the store does not run on.
var ErrUnsupportedTopology = errors.New("sqs message store: unsupported topology")

// Store is the JetStream message store over one dedicated server connection.
type Store struct {
	js     jsapi.JetStream
	limits message.Limits

	mu      sync.Mutex
	streams map[message.Shard]jsapi.Stream
	buckets map[message.Shard]jsapi.KeyValue
}

// New provisions the configured shared layout and returns a store over it.
// A queue creates nothing, and nothing is deleted: streams and buckets from
// an earlier, larger configuration stay addressable for the queues on them.
func New(ctx context.Context, js jsapi.JetStream, cfg Config) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	replicas, err := supportedReplicas()
	if err != nil {
		return nil, err
	}
	s := &Store{
		js:      js,
		limits:  message.Limits{RetentionBackstop: message.MinRetentionBackstop, IdempotencyWindow: cfg.DuplicateWindow},
		streams: make(map[message.Shard]jsapi.Stream, cfg.MessageStreams),
		buckets: make(map[message.Shard]jsapi.KeyValue, cfg.StateBuckets),
	}
	for i := range cfg.MessageStreams {
		shard := message.Shard(safecast.IntToUint32(i))
		sc := cfg.MessageStreamConfig(shard, replicas)
		stream, err := js.CreateOrUpdateStream(ctx, sc)
		if err != nil {
			return nil, fmt.Errorf("provision stream %s: %w", sc.Name, err)
		}
		s.streams[shard] = stream
	}
	for i := range cfg.StateBuckets {
		shard := message.Shard(safecast.IntToUint32(i))
		kv, err := kvutil.GetOrCreateBucketWithOptions(ctx, js, kvutil.BucketOptions{
			Name:        BucketName(shard),
			Description: "SQS per-message state, shared by every queue allocated this state shard",
			History:     1,
			// Every record is written after its body, so this backstop
			// never expires a record before the body it describes.
			TTL: message.MinRetentionBackstop,
		})
		if err != nil {
			return nil, fmt.Errorf("provision state bucket %s: %w", BucketName(shard), err)
		}
		s.buckets[shard] = kv
	}
	return s, nil
}

// supportedReplicas refuses every cluster size but one node (R1) and three
// (R3) before anything is provisioned, so no other replica count is created.
func supportedReplicas() (int, error) {
	switch n := clustersize.Nodes(); n {
	case 1, 3:
		return clustersize.ReplicasFor(n), nil
	default:
		return 0, fmt.Errorf("%w: %d nodes declared; supported are 1 (R1) and 3 (R3)", ErrUnsupportedTopology, n)
	}
}

// Append publishes to the namespace's subject and returns the stream
// sequence as the position once the server acknowledges the write. A repeat
// IdempotencyKey inside the duplicate window returns the first sequence.
func (s *Store) Append(ctx context.Context, ns message.Namespace, in message.AppendInput) (message.AppendResult, error) {
	if err := ns.Validate(); err != nil {
		return message.AppendResult{}, err
	}
	msg := &nats.Msg{Subject: Subject(ns), Data: in.Body, Header: nats.Header{}}
	for k, v := range in.Meta {
		if err := validMeta(k, v); err != nil {
			return message.AppendResult{}, err
		}
		msg.Header[metaHeaderPrefix+k] = []string{v}
	}
	opts := []jsapi.PublishOpt{jsapi.WithExpectStream(StreamName(ns.MessageShard))}
	if in.IdempotencyKey != "" {
		// The duplicate window is per stream, so the key is scoped to the
		// queue sharing it.
		opts = append(opts, jsapi.WithMsgID(string(ns.Queue)+":"+in.IdempotencyKey))
	}
	ack, err := s.js.PublishMsg(ctx, msg, opts...)
	if err != nil {
		return message.AppendResult{}, fmt.Errorf("append to %s: %w", ns.Queue, err)
	}
	return message.AppendResult{Position: message.Position(ack.Sequence), Duplicate: ack.Duplicate}, nil
}

// validMeta rejects metadata a NATS header line cannot carry.
func validMeta(k, v string) error {
	if k == "" || strings.ContainsAny(k, ": \t\r\n") || strings.ContainsAny(v, "\r\n") {
		return fmt.Errorf("%w: metadata key %q cannot be a header", message.ErrInvalidArgument, k)
	}
	return nil
}

// bucket opens the state bucket named by the namespace's StateShard,
// including one only an earlier configuration provisioned.
func (s *Store) bucket(ctx context.Context, ns message.Namespace) (jsapi.KeyValue, error) {
	if err := ns.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	kv, ok := s.buckets[ns.StateShard]
	s.mu.Unlock()
	if ok {
		return kv, nil
	}
	kv, err := s.js.KeyValue(ctx, BucketName(ns.StateShard))
	if err != nil {
		return nil, fmt.Errorf("open state bucket %s: %w", BucketName(ns.StateShard), err)
	}
	s.mu.Lock()
	s.buckets[ns.StateShard] = kv
	s.mu.Unlock()
	return kv, nil
}

// GetState reads a message's state record from its state shard's bucket.
func (s *Store) GetState(ctx context.Context, ns message.Namespace, pos message.Position) (message.State, error) {
	kv, err := s.bucket(ctx, ns)
	if err != nil {
		return message.State{}, err
	}
	e, err := kv.Get(ctx, StateKey(ns, pos))
	switch {
	case errors.Is(err, jsapi.ErrKeyNotFound):
		return message.State{}, message.ErrNotFound
	case err != nil:
		return message.State{}, fmt.Errorf("get state %s/%d: %w", ns.Queue, pos, err)
	}
	return message.State{Value: e.Value(), Revision: message.Revision(e.Revision())}, nil
}

// PutState is a revision compare-and-set; expected zero creates the record.
func (s *Store) PutState(ctx context.Context, ns message.Namespace, pos message.Position, value []byte, expected message.Revision) (message.Revision, error) {
	kv, err := s.bucket(ctx, ns)
	if err != nil {
		return 0, err
	}
	key := StateKey(ns, pos)
	var rev uint64
	if expected == 0 {
		rev, err = kv.Create(ctx, key, value)
	} else {
		rev, err = kv.Update(ctx, key, value, uint64(expected))
	}
	if err != nil {
		return 0, casError(err, "put state %s/%d", ns.Queue, pos)
	}
	return message.Revision(rev), nil
}

// DeleteState removes a state record if its revision is still expected.
func (s *Store) DeleteState(ctx context.Context, ns message.Namespace, pos message.Position, expected message.Revision) error {
	if _, err := s.GetState(ctx, ns, pos); err != nil {
		return err
	}
	if expected == 0 {
		// LastRevision(0) would skip the check against a present record.
		return message.ErrRevisionMismatch
	}
	kv, err := s.bucket(ctx, ns)
	if err != nil {
		return err
	}
	if err := kv.Delete(ctx, StateKey(ns, pos), jsapi.LastRevision(uint64(expected))); err != nil {
		return casError(err, "delete state %s/%d", ns.Queue, pos)
	}
	return nil
}

// casError maps a lost compare-and-set to message.ErrRevisionMismatch.
func casError(err error, format string, args ...any) error {
	if errors.Is(err, jsapi.ErrKeyExists) || errors.Is(err, jsapi.ErrKeyRevisionMismatch) {
		return message.ErrRevisionMismatch
	}
	return fmt.Errorf(format+": %w", append(args, err)...)
}

// stream opens the message stream named by the namespace's MessageShard,
// including one only an earlier configuration provisioned. It never
// creates one.
func (s *Store) stream(ctx context.Context, ns message.Namespace) (jsapi.Stream, error) {
	if err := ns.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	stream, ok := s.streams[ns.MessageShard]
	s.mu.Unlock()
	if ok {
		return stream, nil
	}
	stream, err := s.js.Stream(ctx, StreamName(ns.MessageShard))
	if err != nil {
		return nil, fmt.Errorf("open stream %s: %w", StreamName(ns.MessageShard), err)
	}
	s.mu.Lock()
	s.streams[ns.MessageShard] = stream
	s.mu.Unlock()
	return stream, nil
}

// owned reads the message at pos if it belongs to the namespace's subject;
// a position on a neighbour's subject is reported as ErrNotFound.
func (s *Store) owned(ctx context.Context, stream jsapi.Stream, ns message.Namespace, pos message.Position) (*jsapi.RawStreamMsg, error) {
	raw, err := stream.GetMsg(ctx, uint64(pos))
	switch {
	case errors.Is(err, jsapi.ErrMsgNotFound):
		return nil, message.ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("get message %s/%d: %w", ns.Queue, pos, err)
	case raw.Subject != Subject(ns):
		return nil, message.ErrNotFound
	}
	return raw, nil
}

// Get reads a message body and its domain metadata.
func (s *Store) Get(ctx context.Context, ns message.Namespace, pos message.Position) (message.Message, error) {
	stream, err := s.stream(ctx, ns)
	if err != nil {
		return message.Message{}, err
	}
	raw, err := s.owned(ctx, stream, ns, pos)
	if err != nil {
		return message.Message{}, err
	}
	var meta map[string]string
	for k, v := range raw.Header {
		if name, ok := strings.CutPrefix(k, metaHeaderPrefix); ok && len(v) > 0 {
			if meta == nil {
				meta = map[string]string{}
			}
			meta[name] = v[0]
		}
	}
	return message.Message{Position: pos, Body: raw.Data, Meta: meta}, nil
}

// Delete removes one message of the namespace. A position that is absent
// or on another subject is left alone and reported as success.
func (s *Store) Delete(ctx context.Context, ns message.Namespace, pos message.Position) error {
	stream, err := s.stream(ctx, ns)
	if err != nil {
		return err
	}
	if _, err := s.owned(ctx, stream, ns, pos); err != nil {
		if errors.Is(err, message.ErrNotFound) {
			return nil
		}
		return err
	}
	if err := stream.DeleteMsg(ctx, uint64(pos)); err != nil && !errors.Is(err, jsapi.ErrMsgNotFound) {
		return fmt.Errorf("delete message %s/%d: %w", ns.Queue, pos, err)
	}
	return nil
}

// DeleteThrough purges the namespace's subject below pos+1. A subject-filtered
// purge with a sequence removes only that subject's messages before the
// sequence; neighbours on the stream and every state record stay.
func (s *Store) DeleteThrough(ctx context.Context, ns message.Namespace, pos message.Position) error {
	stream, err := s.stream(ctx, ns)
	if err != nil {
		return err
	}
	opts := []jsapi.StreamPurgeOpt{jsapi.WithPurgeSubject(Subject(ns))}
	if pos < math.MaxUint64 {
		opts = append(opts, jsapi.WithPurgeSequence(uint64(pos)+1))
	}
	if err := stream.Purge(ctx, opts...); err != nil {
		return fmt.Errorf("delete %s through %d: %w", ns.Queue, pos, err)
	}
	return nil
}

// Count reports the namespace's stored messages, from the stream's
// per-subject state, and its live state records, from its state bucket.
func (s *Store) Count(ctx context.Context, ns message.Namespace) (message.Usage, error) {
	stream, err := s.stream(ctx, ns)
	if err != nil {
		return message.Usage{}, err
	}
	info, err := stream.Info(ctx, jsapi.WithSubjectFilter(Subject(ns)))
	if err != nil {
		return message.Usage{}, fmt.Errorf("count messages %s: %w", ns.Queue, err)
	}
	kv, err := s.bucket(ctx, ns)
	if err != nil {
		return message.Usage{}, err
	}
	// Listing keys skips delete markers, which a subject count would include.
	lister, err := kv.ListKeysFiltered(ctx, statePrefix(ns)+".>")
	if err != nil {
		return message.Usage{}, fmt.Errorf("count state %s: %w", ns.Queue, err)
	}
	var states uint64
	for range lister.Keys() {
		states++
	}
	// The lister closes early on cancellation without an error, which
	// would otherwise read as a short count.
	if err := ctx.Err(); err != nil {
		return message.Usage{}, fmt.Errorf("count state %s: %w", ns.Queue, err)
	}
	return message.Usage{Messages: info.State.Subjects[Subject(ns)], States: states}, nil
}

// DeleteNamespace purges the namespace's subject from its message stream
// and its key prefix from its state bucket, markers included. Both purges
// are subject-scoped, so neighbours are untouched and a repeat is a no-op.
func (s *Store) DeleteNamespace(ctx context.Context, ns message.Namespace) error {
	stream, err := s.stream(ctx, ns)
	if err != nil {
		return err
	}
	if err := stream.Purge(ctx, jsapi.WithPurgeSubject(Subject(ns))); err != nil {
		return fmt.Errorf("purge messages %s: %w", ns.Queue, err)
	}
	bucket := BucketName(ns.StateShard)
	state, err := s.js.Stream(ctx, "KV_"+bucket)
	if err != nil {
		return fmt.Errorf("open state bucket %s: %w", bucket, err)
	}
	if err := state.Purge(ctx, jsapi.WithPurgeSubject("$KV."+bucket+"."+statePrefix(ns)+".>")); err != nil {
		return fmt.Errorf("purge state %s: %w", ns.Queue, err)
	}
	return nil
}

// Limits reports the configured duplicate window and the stream backstop.
func (s *Store) Limits() message.Limits { return s.limits }
