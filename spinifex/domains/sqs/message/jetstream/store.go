package jetstream

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// Store is the JetStream message store over one dedicated server connection.
type Store struct {
	js      jsapi.JetStream
	cfg     Config
	buckets []jsapi.KeyValue
}

// New provisions the fixed shared layout and returns a store over it. Only
// the configured streams and buckets are created; a queue creates nothing.
// Provisioning is idempotent and raises an existing stream to cfg.
func New(ctx context.Context, js jsapi.JetStream, cfg Config) (*Store, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	replicas, err := clustersize.Replicas()
	if err != nil {
		return nil, fmt.Errorf("sqs message store replicas: %w", err)
	}
	for i := range cfg.MessageStreams {
		sc := cfg.MessageStreamConfig(message.Shard(safecast.IntToUint32(i)), replicas)
		if _, err := js.CreateOrUpdateStream(ctx, sc); err != nil {
			return nil, fmt.Errorf("provision stream %s: %w", sc.Name, err)
		}
	}
	s := &Store{js: js, cfg: cfg, buckets: make([]jsapi.KeyValue, cfg.StateBuckets)}
	for i := range cfg.StateBuckets {
		kv, err := kvutil.GetOrCreateBucketWithOptions(ctx, js, kvutil.BucketOptions{
			Name:        BucketName(i),
			Description: "SQS per-message state, shared by every queue whose shard maps here",
			History:     1,
		})
		if err != nil {
			return nil, fmt.Errorf("provision state bucket %s: %w", BucketName(i), err)
		}
		s.buckets[i] = kv
	}
	return s, nil
}

func (s *Store) check(ns message.Namespace) error {
	if err := ns.Validate(); err != nil {
		return err
	}
	if int(ns.Shard) >= s.cfg.MessageStreams {
		return fmt.Errorf("%w: shard %d outside %d message streams", message.ErrInvalidArgument, ns.Shard, s.cfg.MessageStreams)
	}
	return nil
}

// Append publishes to the namespace's subject and returns the stream
// sequence as the position once the server acknowledges the write. A repeat
// IdempotencyKey inside the duplicate window returns the first sequence.
func (s *Store) Append(ctx context.Context, ns message.Namespace, in message.AppendInput) (message.AppendResult, error) {
	if err := s.check(ns); err != nil {
		return message.AppendResult{}, err
	}
	msg := &nats.Msg{Subject: Subject(ns), Data: in.Body, Header: nats.Header{}}
	for k, v := range in.Meta {
		if err := validMeta(k, v); err != nil {
			return message.AppendResult{}, err
		}
		msg.Header[metaHeaderPrefix+k] = []string{v}
	}
	opts := []jsapi.PublishOpt{jsapi.WithExpectStream(StreamName(ns.Shard))}
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

func (s *Store) bucket(ns message.Namespace) jsapi.KeyValue {
	return s.buckets[s.cfg.BucketIndex(ns.Shard)]
}

// GetState reads a message's state record from its shard's bucket.
func (s *Store) GetState(ctx context.Context, ns message.Namespace, pos message.Position) (message.State, error) {
	if err := s.check(ns); err != nil {
		return message.State{}, err
	}
	e, err := s.bucket(ns).Get(ctx, StateKey(ns, pos))
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
	if err := s.check(ns); err != nil {
		return 0, err
	}
	kv, key := s.bucket(ns), StateKey(ns, pos)
	var rev uint64
	var err error
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
	if err := s.bucket(ns).Delete(ctx, StateKey(ns, pos), jsapi.LastRevision(uint64(expected))); err != nil {
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
