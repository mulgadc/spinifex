package jetstream

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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

// stateShardHeader records a message's StateShard, so a feed reader can
// rebuild the complete namespace. It is outside metaHeaderPrefix and is
// never returned as metadata.
const stateShardHeader = "Sqs-State-Shard"

// feedInactiveThreshold bounds how long the server keeps a feed's
// ephemeral consumer after its reader vanishes without Close.
const feedInactiveThreshold = time.Minute

// feedPullExpiry replaces nats.go's 30s default pull expiry, under which an
// ordered feed was seen to idle for a whole expiry with stored changes pending.
const feedPullExpiry = 2 * time.Second

// feedCloseTimeout bounds Close's consumer delete.
const feedCloseTimeout = 5 * time.Second

// enumerateIdle is how long the headers-only pass Namespaces makes over a
// message stream waits for a body before checking whether any remain.
const enumerateIdle = time.Second

var (
	// ErrUnsupportedTopology reports a cluster size the store does not run on.
	ErrUnsupportedTopology = errors.New("sqs message store: unsupported topology")
	// ErrCorruptRecord reports a stored message whose subject or internal
	// header cannot be decoded into a namespace.
	ErrCorruptRecord = errors.New("sqs message store: corrupt record")
)

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
	msg := &nats.Msg{Subject: Subject(ns), Data: in.Body, Header: nats.Header{
		stateShardHeader: []string{strconv.FormatUint(uint64(ns.StateShard), 10)},
	}}
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
// The stream leader answers, so a completed write or delete is always seen;
// a follower may not have applied it yet.
func (s *Store) GetState(ctx context.Context, ns message.Namespace, pos message.Position) (message.State, error) {
	if _, err := s.bucket(ctx, ns); err != nil {
		return message.State{}, err
	}
	name := BucketName(ns.StateShard)
	m, err := leaderLastMsg(ctx, s.js, "KV_"+name, "$KV."+name+"."+StateKey(ns, pos))
	switch {
	case errors.Is(err, jsapi.ErrMsgNotFound):
		return message.State{}, message.ErrNotFound
	case err != nil:
		return message.State{}, fmt.Errorf("get state %s/%d: %w", ns.Queue, pos, err)
	case kvRemoved(m.Header):
		return message.State{}, message.ErrNotFound
	}
	return message.State{Value: m.Data, Revision: message.Revision(m.Sequence)}, nil
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
	return message.Message{Position: pos, Body: raw.Data, Meta: domainMeta(raw.Header)}, nil
}

// domainMeta returns only the domain's metadata headers, never the store's
// internal ones; nil when there are none.
func domainMeta(h nats.Header) map[string]string {
	var meta map[string]string
	for k, v := range h {
		if name, ok := strings.CutPrefix(k, metaHeaderPrefix); ok && len(v) > 0 {
			if meta == nil {
				meta = map[string]string{}
			}
			meta[name] = v[0]
		}
	}
	return meta
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
	keys, err := s.liveStateKeys(ctx, ns.StateShard, statePrefix(ns)+".")
	if err != nil {
		return message.Usage{}, fmt.Errorf("count state %s: %w", ns.Queue, err)
	}
	return message.Usage{Messages: info.State.Subjects[Subject(ns)], States: uint64(len(keys))}, nil
}

// DeleteNamespace purges the namespace's subject from its message stream
// and its key prefix from its state bucket, markers included. Both purges
// are subject-scoped, so neighbours are untouched and a repeat is a no-op.
// Both resources are resolved before either purge, so a wrong allocation
// fails without deleting anything.
func (s *Store) DeleteNamespace(ctx context.Context, ns message.Namespace) error {
	stream, err := s.stream(ctx, ns)
	if err != nil {
		return err
	}
	bucket := BucketName(ns.StateShard)
	state, err := s.js.Stream(ctx, "KV_"+bucket)
	if err != nil {
		return fmt.Errorf("open state bucket %s: %w", bucket, err)
	}
	if err := stream.Purge(ctx, jsapi.WithPurgeSubject(Subject(ns))); err != nil {
		return fmt.Errorf("purge messages %s: %w", ns.Queue, err)
	}
	if err := state.Purge(ctx, jsapi.WithPurgeSubject("$KV."+bucket+"."+statePrefix(ns)+".>")); err != nil {
		return fmt.Errorf("purge state %s: %w", ns.Queue, err)
	}
	return nil
}

// Limits reports the configured duplicate window and the stream backstop.
func (s *Store) Limits() message.Limits { return s.limits }

// Feed reads the message shard's stream after the cursor through an
// ephemeral, in-memory, AckNone, headers-only ordered consumer that Close
// deletes. It opens a retained historical stream but never creates one.
func (s *Store) Feed(ctx context.Context, messageShard message.Shard, after message.Cursor) (message.Feed, error) {
	// Any valid queue ID resolves the stream; only the shard selects it.
	stream, err := s.stream(ctx, message.Namespace{MessageShard: messageShard, Queue: "feed"})
	if err != nil {
		return nil, err
	}
	cfg := jsapi.OrderedConsumerConfig{
		DeliverPolicy:     jsapi.DeliverAllPolicy,
		HeadersOnly:       true,
		InactiveThreshold: feedInactiveThreshold,
	}
	switch {
	case after == math.MaxUint64:
		// No position follows the maximum cursor; only new appends can.
		cfg.DeliverPolicy = jsapi.DeliverNewPolicy
	case after > 0:
		cfg.DeliverPolicy = jsapi.DeliverByStartSequencePolicy
		cfg.OptStartSeq = uint64(after) + 1
	}
	consumer, err := stream.OrderedConsumer(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open feed on %s: %w", StreamName(messageShard), err)
	}
	it, err := consumer.Messages(jsapi.PullExpiry(feedPullExpiry))
	if err != nil {
		// The consumer already exists server-side and no Feed owns it.
		_ = deleteFeedConsumer(stream, consumer)
		return nil, fmt.Errorf("open feed on %s: %w", StreamName(messageShard), err)
	}
	return &feed{shard: messageShard, stream: stream, consumer: consumer, it: it}, nil
}

// deleteFeedConsumer removes a feed's server-side consumer, if it was
// created, within feedCloseTimeout.
func deleteFeedConsumer(stream jsapi.Stream, consumer jsapi.Consumer) error {
	info := consumer.CachedInfo()
	if info == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), feedCloseTimeout)
	defer cancel()
	if err := stream.DeleteConsumer(ctx, info.Name); err != nil && !errors.Is(err, jsapi.ErrConsumerNotFound) {
		return err
	}
	return nil
}

type feed struct {
	shard    message.Shard
	stream   jsapi.Stream
	consumer jsapi.Consumer
	it       jsapi.MessagesContext
}

var _ message.Feed = (*feed)(nil)

var _ message.Store = (*Store)(nil)

// Next returns the next append on the shard. A record that cannot be
// decoded is returned as ErrCorruptRecord, and the feed moves past it.
func (f *feed) Next(ctx context.Context) (message.Change, error) {
	msg, err := f.it.Next(jsapi.NextContext(ctx))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return message.Change{}, ctxErr
		}
		return message.Change{}, fmt.Errorf("feed on %s: %w", StreamName(f.shard), err)
	}
	md, err := msg.Metadata()
	if err != nil {
		return message.Change{}, fmt.Errorf("feed on %s: %w", StreamName(f.shard), err)
	}
	ns, err := decodeNamespace(f.shard, msg.Subject(), msg.Headers())
	if err != nil {
		return message.Change{}, fmt.Errorf("%w: %s position %d: %w", ErrCorruptRecord, StreamName(f.shard), md.Sequence.Stream, err)
	}
	return message.Change{
		Namespace: ns,
		Position:  message.Position(md.Sequence.Stream),
		Meta:      domainMeta(msg.Headers()),
		Cursor:    message.Cursor(md.Sequence.Stream),
	}, nil
}

// decodeNamespace rebuilds a namespace from a stored subject and header,
// refusing anything it would have to guess.
func decodeNamespace(shard message.Shard, subject string, h nats.Header) (message.Namespace, error) {
	prefix := "sqs.m." + strconv.FormatUint(uint64(shard), 10) + "."
	queue, ok := strings.CutPrefix(subject, prefix)
	if !ok {
		return message.Namespace{}, fmt.Errorf("subject %q is not on message shard %d", subject, shard)
	}
	values := h.Values(stateShardHeader)
	if len(values) != 1 {
		return message.Namespace{}, fmt.Errorf("%d %s headers", len(values), stateShardHeader)
	}
	state, err := strconv.ParseUint(values[0], 10, 32)
	if err != nil {
		return message.Namespace{}, fmt.Errorf("%s %q: %w", stateShardHeader, values[0], err)
	}
	ns := message.Namespace{MessageShard: shard, StateShard: message.Shard(state), Queue: message.QueueID(queue)}
	if err := ns.Validate(); err != nil {
		return message.Namespace{}, err
	}
	return ns, nil
}

// Close stops the feed and deletes its server-side consumer, which would
// otherwise linger until its inactive threshold.
func (f *feed) Close() error {
	f.it.Stop()
	if err := deleteFeedConsumer(f.stream, f.consumer); err != nil {
		return fmt.Errorf("close feed on %s: %w", StreamName(f.shard), err)
	}
	return nil
}

// Namespaces lists every namespace on the message shard holding a body or a
// live state record. Bodies give their state shard through the internal
// header; state is found in every existing state bucket, historical included.
func (s *Store) Namespaces(ctx context.Context, messageShard message.Shard) ([]message.NamespaceUsage, error) {
	bodies, err := s.bodyAllocations(ctx, messageShard)
	if err != nil {
		return nil, err
	}
	states, err := s.stateAllocations(ctx, messageShard)
	if err != nil {
		return nil, err
	}
	var conflicts []error
	rows := map[message.QueueID]*message.NamespaceUsage{}
	for queue, b := range bodies {
		rows[queue] = &message.NamespaceUsage{
			Namespace: message.Namespace{MessageShard: messageShard, StateShard: b.stateShard, Queue: queue},
			Usage:     message.Usage{Messages: b.messages},
		}
	}
	for queue, byShard := range states {
		if len(byShard) > 1 {
			conflicts = append(conflicts, fmt.Errorf("%w: queue %s on message shard %d has state in %d state shards", ErrCorruptRecord, queue, messageShard, len(byShard)))
			continue
		}
		for shard, n := range byShard {
			row, ok := rows[queue]
			switch {
			case !ok:
				rows[queue] = &message.NamespaceUsage{
					Namespace: message.Namespace{MessageShard: messageShard, StateShard: shard, Queue: queue},
					Usage:     message.Usage{States: n},
				}
			case row.Namespace.StateShard != shard:
				conflicts = append(conflicts, fmt.Errorf("%w: queue %s on message shard %d: bodies name state shard %d, state is in %d", ErrCorruptRecord, queue, messageShard, row.Namespace.StateShard, shard))
			default:
				row.Usage.States = n
			}
		}
	}
	if len(conflicts) > 0 {
		return nil, errors.Join(conflicts...)
	}
	out := make([]message.NamespaceUsage, 0, len(rows))
	for _, row := range rows {
		out = append(out, *row)
	}
	slices.SortFunc(out, func(a, b message.NamespaceUsage) int {
		return strings.Compare(string(a.Namespace.Queue), string(b.Namespace.Queue))
	})
	return out, nil
}

type bodyAllocation struct {
	stateShard message.Shard
	messages   uint64
}

// bodyAllocations reads the header of every stored body on the message
// shard in one bounded pass, counting each queue's bodies and requiring
// every body of a queue to name the same state shard.
func (s *Store) bodyAllocations(ctx context.Context, messageShard message.Shard) (map[message.QueueID]bodyAllocation, error) {
	stream, err := s.stream(ctx, message.Namespace{MessageShard: messageShard, Queue: "enumerate"})
	if err != nil {
		return nil, err
	}
	info, err := stream.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("enumerate %s: %w", StreamName(messageShard), err)
	}
	out := map[message.QueueID]bodyAllocation{}
	_, _, err = readRange(ctx, stream, shardFilter(messageShard), 0, info.State.LastSeq, func(msg jsapi.Msg, seq uint64) (bool, error) {
		ns, err := decodeNamespace(messageShard, msg.Subject(), msg.Headers())
		if err != nil {
			return false, fmt.Errorf("%w: %s position %d: %w", ErrCorruptRecord, StreamName(messageShard), seq, err)
		}
		a, seen := out[ns.Queue]
		if seen && a.stateShard != ns.StateShard {
			return false, fmt.Errorf("%w: %s bodies name state shards %d and %d (position %d)", ErrCorruptRecord, msg.Subject(), a.stateShard, ns.StateShard, seq)
		}
		out[ns.Queue] = bodyAllocation{stateShard: ns.StateShard, messages: a.messages + 1}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("enumerate %s: %w", StreamName(messageShard), err)
	}
	return out, nil
}

// liveStateKeys reads every state key under keyPrefix in the state shard's
// bucket in one bounded pass over its stream, skipping delete markers. It
// replaces the client's key lister, which ends on consumer state.
func (s *Store) liveStateKeys(ctx context.Context, shard message.Shard, keyPrefix string) (map[string]struct{}, error) {
	bucket := BucketName(shard)
	stream, err := s.js.Stream(ctx, "KV_"+bucket)
	if err != nil {
		return nil, fmt.Errorf("open state bucket %s: %w", bucket, err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("read state bucket %s: %w", bucket, err)
	}
	subjectPrefix := "$KV." + bucket + "."
	keys := map[string]struct{}{}
	_, _, err = readRange(ctx, stream, subjectPrefix+keyPrefix+">", 0, info.State.LastSeq, func(msg jsapi.Msg, _ uint64) (bool, error) {
		key := strings.TrimPrefix(msg.Subject(), subjectPrefix)
		switch msg.Headers().Get("KV-Operation") {
		case "DEL", "PURGE":
			delete(keys, key)
		default:
			keys[key] = struct{}{}
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// shardFilter matches every message subject on a message shard.
func shardFilter(shard message.Shard) string {
	return "sqs.m." + strconv.FormatUint(uint64(shard), 10) + ".>"
}

// readRange calls fn, in order and headers only, for each stored match of
// filter in (after, bound] until fn returns false. Completion rests on a
// direct get of the next match, never on consumer state.
func readRange(ctx context.Context, stream jsapi.Stream, filter string, after, bound uint64, fn func(jsapi.Msg, uint64) (bool, error)) (uint64, bool, error) {
	for {
		if after >= bound {
			return bound, true, nil
		}
		next, err := stream.GetMsg(ctx, after+1, jsapi.WithGetMsgSubject(filter))
		switch {
		case errors.Is(err, jsapi.ErrMsgNotFound):
			return bound, true, nil
		case err != nil:
			return after, false, err
		case next.Sequence > bound:
			return bound, true, nil
		}
		last, done, more, err := readFrom(ctx, stream, filter, next.Sequence, bound, fn)
		if last > after {
			after = last
		}
		if err != nil || done || !more {
			return after, done, err
		}
	}
}

// readFrom drains one headers-only ordered consumer from start. more reports
// the consumer went idle before the bound, so readRange must probe again.
func readFrom(ctx context.Context, stream jsapi.Stream, filter string, start, bound uint64, fn func(jsapi.Msg, uint64) (bool, error)) (last uint64, done, more bool, err error) {
	consumer, err := stream.OrderedConsumer(ctx, jsapi.OrderedConsumerConfig{
		FilterSubjects:    []string{filter},
		DeliverPolicy:     jsapi.DeliverByStartSequencePolicy,
		OptStartSeq:       start,
		HeadersOnly:       true,
		InactiveThreshold: feedInactiveThreshold,
	})
	if err != nil {
		return 0, false, false, err
	}
	defer func() { _ = deleteFeedConsumer(stream, consumer) }()
	it, err := consumer.Messages()
	if err != nil {
		return 0, false, false, err
	}
	defer it.Stop()
	for {
		msg, err := nextOrIdle(ctx, it)
		if err != nil {
			return last, false, false, err
		}
		if msg == nil {
			return last, false, true, nil
		}
		md, err := msg.Metadata()
		if err != nil {
			return last, false, false, err
		}
		seq := md.Sequence.Stream
		if seq > bound {
			return bound, true, false, nil
		}
		cont, err := fn(msg, seq)
		if err != nil {
			return last, false, false, err
		}
		last = seq
		if seq == bound {
			return bound, true, false, nil
		}
		if !cont {
			return last, false, false, nil
		}
		// A hint only: readRange's probe decides whether anything remains.
		if md.NumPending == 0 {
			return last, false, true, nil
		}
	}
}

// nextOrIdle returns the next message, or nil when none arrives within
// enumerateIdle while ctx is still live.
func nextOrIdle(ctx context.Context, it jsapi.MessagesContext) (jsapi.Msg, error) {
	waitCtx, cancel := context.WithTimeout(ctx, enumerateIdle)
	defer cancel()
	msg, err := it.Next(jsapi.NextContext(waitCtx))
	switch {
	case err == nil:
		return msg, nil
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case waitCtx.Err() != nil:
		return nil, nil
	default:
		return nil, err
	}
}

// stateAllocations counts the message shard's live state keys per queue and
// state shard across every state bucket that exists, found by listing and
// never created, so a bucket only an earlier configuration made is included.
func (s *Store) stateAllocations(ctx context.Context, messageShard message.Shard) (map[message.QueueID]map[message.Shard]uint64, error) {
	shards, err := s.stateShards(ctx)
	if err != nil {
		return nil, err
	}
	prefix := "q." + strconv.FormatUint(uint64(messageShard), 10) + "."
	out := map[message.QueueID]map[message.Shard]uint64{}
	for _, shard := range shards {
		keys, err := s.liveStateKeys(ctx, shard, prefix)
		if err != nil {
			return nil, fmt.Errorf("enumerate %s: %w", BucketName(shard), err)
		}
		for key := range keys {
			queue, err := decodeStateKey(prefix, key)
			if err != nil {
				return nil, fmt.Errorf("%w: %s key %q: %w", ErrCorruptRecord, BucketName(shard), key, err)
			}
			if out[queue] == nil {
				out[queue] = map[message.Shard]uint64{}
			}
			out[queue][shard]++
		}
	}
	return out, nil
}

// stateShards lists the state shards that have a bucket. A bucket whose
// name is not exactly a canonical sqs_state_<n> is not the store's.
func (s *Store) stateShards(ctx context.Context) ([]message.Shard, error) {
	lister := s.js.KeyValueStoreNames(ctx)
	var shards []message.Shard
	for name := range lister.Name() {
		digits, ok := strings.CutPrefix(name, "sqs_state_")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(digits, 10, 32)
		if err != nil || strconv.FormatUint(n, 10) != digits {
			continue
		}
		shards = append(shards, message.Shard(n))
	}
	if err := lister.Error(); err != nil {
		return nil, fmt.Errorf("list state buckets: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("list state buckets: %w", err)
	}
	slices.Sort(shards)
	return shards, nil
}

// decodeStateKey parses q.<message shard>.<queue>.<position> under prefix.
func decodeStateKey(prefix, key string) (message.QueueID, error) {
	rest, ok := strings.CutPrefix(key, prefix)
	if !ok {
		return "", errors.New("not on this message shard")
	}
	queue, pos, ok := strings.Cut(rest, ".")
	if !ok {
		return "", errors.New("no position")
	}
	n, err := strconv.ParseUint(pos, 10, 64)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != pos {
		return "", fmt.Errorf("position %q", pos)
	}
	ns := message.Namespace{Queue: message.QueueID(queue)}
	if err := ns.Validate(); err != nil {
		return "", err
	}
	return ns.Queue, nil
}

// Scan reads one page of a bounded pass over the message shard's stream,
// headers only. The first page bounds the pass at the stream's last
// sequence; deleted positions are skipped by the stream itself.
func (s *Store) Scan(ctx context.Context, messageShard message.Shard, from message.ScanCursor, limit int) (message.ScanPage, error) {
	if err := message.ValidateScan(from, limit); err != nil {
		return message.ScanPage{}, err
	}
	stream, err := s.stream(ctx, message.Namespace{MessageShard: messageShard, Queue: "scan"})
	if err != nil {
		return message.ScanPage{}, err
	}
	if from.Bound == 0 {
		info, err := stream.Info(ctx)
		if err != nil {
			return message.ScanPage{}, fmt.Errorf("scan %s: %w", StreamName(messageShard), err)
		}
		from.Bound = message.Cursor(info.State.LastSeq)
	}
	page := message.ScanPage{Next: from}
	last, done, err := readRange(ctx, stream, shardFilter(messageShard), uint64(from.After), uint64(from.Bound), func(msg jsapi.Msg, seq uint64) (bool, error) {
		ns, err := decodeNamespace(messageShard, msg.Subject(), msg.Headers())
		if err != nil {
			return false, fmt.Errorf("%w: %s position %d: %w", ErrCorruptRecord, StreamName(messageShard), seq, err)
		}
		page.Changes = append(page.Changes, message.Change{
			Namespace: ns, Position: message.Position(seq), Meta: domainMeta(msg.Headers()), Cursor: message.Cursor(seq),
		})
		return len(page.Changes) < limit, nil
	})
	if err != nil {
		return message.ScanPage{}, fmt.Errorf("scan %s: %w", StreamName(messageShard), err)
	}
	page.Next.After, page.Done = message.Cursor(last), done
	return page, nil
}
