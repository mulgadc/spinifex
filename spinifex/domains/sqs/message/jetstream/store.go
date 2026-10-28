package jetstream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
// ephemeral consumer without a reader: after a vanished reader, or a create
// that timed out yet landed after every delete of its name.
const feedInactiveThreshold = time.Minute

// feedIdleCheck is how long Next waits on the consumer before checking the
// stream; an idle feed costs one leader message get per interval on top of
// nats.go's ordered-consumer heartbeat and pull renewal.
const feedIdleCheck = 2 * time.Second

// feedOpenTimeout bounds a consumer create, which the caller's cancellation
// does not cut short: a cancelled create can still land server-side.
const feedOpenTimeout = 5 * time.Second

// feedCloseTimeout bounds Close's consumer delete.
const feedCloseTimeout = 5 * time.Second

// feedCloseWait bounds Close's wait for consumer work in progress: JetStream's
// default API timeout for an open plus the delete that follows it.
const feedCloseWait = 2 * feedCloseTimeout

// errCleanupPending reports a Close whose deadline passed with consumer work
// still in progress; a later Close waits for it again.
var errCleanupPending = errors.New("consumer cleanup is still in progress")

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
// It uses a leader-answered stream message get, avoiding the direct get a
// lagging follower can serve before it applies a write or delete.
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

// Feed reads the shard's stream, never creating one, through an ephemeral
// in-memory headers-only ordered consumer that Close deletes. If Feed fails, a
// consumer an ambiguous create lands later is left to feedInactiveThreshold.
func (s *Store) Feed(ctx context.Context, messageShard message.Shard, after message.Cursor) (message.Feed, error) {
	// Any valid queue ID resolves the stream; only the shard selects it.
	stream, err := s.stream(ctx, message.Namespace{MessageShard: messageShard, Queue: "feed"})
	if err != nil {
		return nil, err
	}
	f := &feed{shard: messageShard, stream: stream, cursor: uint64(after), idleCheck: feedIdleCheck, closeWait: feedCloseWait}
	f.closing, f.closeNow = context.WithCancel(context.Background())
	if after == math.MaxUint64 {
		// No position follows the maximum cursor; only later appends can.
		info, err := stream.Info(ctx)
		if err != nil {
			return nil, fmt.Errorf("open feed on %s: %w", StreamName(messageShard), err)
		}
		f.cursor = info.State.LastSeq
	}
	if err := f.start(ctx); err != nil {
		f.closeNow()
		return nil, err
	}
	return f, nil
}

// start opens the first consumer. A create already started finishes even if
// ctx ends meanwhile; that consumer is then released and ctx's error returned.
func (f *feed) start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	consumer, it, err := f.open(ctx)
	if err != nil {
		return errors.Join(ctx.Err(), err)
	}
	if err := ctx.Err(); err != nil {
		f.mu.Lock()
		f.begin()
		f.mu.Unlock()
		return errors.Join(err, f.discard(consumer, it))
	}
	f.consumer, f.it = consumer, it
	return nil
}

// open starts a headers-only ordered consumer just after the cursor. Its name
// is chosen here, so a create that times out after landing server-side can
// still be deleted.
func (f *feed) open(ctx context.Context) (jsapi.Consumer, jsapi.MessagesContext, error) {
	prefix, err := consumerPrefix()
	if err != nil {
		return nil, nil, fmt.Errorf("open feed on %s: %w", StreamName(f.shard), err)
	}
	cfg := jsapi.OrderedConsumerConfig{
		NamePrefix:        prefix,
		DeliverPolicy:     jsapi.DeliverAllPolicy,
		HeadersOnly:       true,
		InactiveThreshold: feedInactiveThreshold,
	}
	if f.cursor > 0 {
		cfg.DeliverPolicy = jsapi.DeliverByStartSequencePolicy
		cfg.OptStartSeq = f.cursor + 1
	}
	createCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), feedOpenTimeout)
	defer cancel()
	consumer, err := f.stream.OrderedConsumer(createCtx, cfg)
	if err != nil {
		// nats.go names an ordered consumer's first server-side consumer _1.
		return nil, nil, f.abandon(prefix+"_1", fmt.Errorf("open feed on %s: %w", StreamName(f.shard), err))
	}
	it, err := consumer.Messages()
	if err != nil {
		return nil, nil, f.abandon(consumerName(consumer), fmt.Errorf("open feed on %s: %w", StreamName(f.shard), err))
	}
	return consumer, it, nil
}

// abandon deletes a consumer no Feed owns after a failed open. Unless the
// delete removed it, the name is kept for Close to retry: a create that timed
// out may land after this delete.
func (f *feed) abandon(name string, err error) error {
	ctx, cancel := context.WithTimeout(context.Background(), feedCloseTimeout)
	defer cancel()
	derr := f.stream.DeleteConsumer(ctx, name)
	if derr == nil {
		return err
	}
	f.mu.Lock()
	f.retained = append(f.retained, name)
	f.mu.Unlock()
	if errors.Is(derr, jsapi.ErrConsumerNotFound) {
		return err
	}
	return errors.Join(err, derr)
}

// consumerPrefix returns a random consumer name prefix.
func consumerPrefix() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// current returns the live iterator, nil while none is open.
func (f *feed) current() (jsapi.MessagesContext, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.it, f.closed
}

// isClosed reports whether Close has begun.
func (f *feed) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// begin records consumer work Close must wait for; f.mu must be held.
func (f *feed) begin() {
	if f.busy == 0 {
		f.idle = make(chan struct{})
	}
	f.busy++
}

// end finishes work recorded by begin; f.mu must be held.
func (f *feed) end() {
	f.busy--
	if f.busy == 0 {
		close(f.idle)
		f.idle = nil
	}
}

// reopen opens a consumer from the cursor as tracked work. One that finishes
// after Close began, or after ctx ended, is released rather than kept; Close
// wins over ctx when both apply.
func (f *feed) reopen(ctx context.Context) (jsapi.MessagesContext, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, message.ErrFeedClosed
	}
	if err := ctx.Err(); err != nil {
		f.mu.Unlock()
		return nil, err
	}
	f.begin()
	f.mu.Unlock()
	consumer, it, err := f.open(ctx)
	f.mu.Lock()
	if err == nil && !f.closed && ctx.Err() == nil {
		f.consumer, f.it = consumer, it
		f.end()
		f.mu.Unlock()
		return it, nil
	}
	if err != nil {
		f.end()
		f.mu.Unlock()
		return nil, f.interrupted(ctx, err)
	}
	closed := f.closed
	f.mu.Unlock()
	derr := f.discard(consumer, it)
	if closed {
		return nil, message.ErrFeedClosed
	}
	return nil, errors.Join(ctx.Err(), derr)
}

// detach takes the live consumer as tracked work, leaving the feed with none.
func (f *feed) detach() (jsapi.Consumer, jsapi.MessagesContext) {
	f.mu.Lock()
	defer f.mu.Unlock()
	consumer, it := f.consumer, f.it
	f.consumer, f.it = nil, nil
	if it != nil {
		f.begin()
	}
	return consumer, it
}

// discard releases a consumer taken as tracked work and ends that work,
// keeping the name of one it could not delete for Close to retry.
func (f *feed) discard(consumer jsapi.Consumer, it jsapi.MessagesContext) error {
	err := release(f.stream, consumer, it)
	f.mu.Lock()
	if err != nil {
		f.retained = append(f.retained, consumerName(consumer))
	}
	f.end()
	f.mu.Unlock()
	return err
}

// release stops an iterator and deletes its consumer.
func release(stream jsapi.Stream, consumer jsapi.Consumer, it jsapi.MessagesContext) error {
	if it == nil {
		return nil
	}
	it.Stop()
	return deleteConsumerNamed(stream, consumerName(consumer))
}

func consumerName(consumer jsapi.Consumer) string {
	if info := consumer.CachedInfo(); info != nil {
		return info.Name
	}
	return ""
}

// deleteConsumerNamed removes a consumer, if named and present, within
// feedCloseTimeout.
func deleteConsumerNamed(stream jsapi.Stream, name string) error {
	if name == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), feedCloseTimeout)
	defer cancel()
	if err := stream.DeleteConsumer(ctx, name); err != nil && !errors.Is(err, jsapi.ErrConsumerNotFound) {
		return err
	}
	return nil
}

// feed is read by one caller at a time; Close may run concurrently with Next.
type feed struct {
	shard     message.Shard
	stream    jsapi.Stream
	idleCheck time.Duration
	closeWait time.Duration
	// closing is cancelled by Close, interrupting an in-flight Next.
	closing  context.Context
	closeNow context.CancelFunc
	// claimed, if set, runs after Next claims a change; tests use it.
	claimed func()

	mu sync.Mutex
	// cursor is written only by Next, under mu, as it claims a change.
	cursor   uint64
	closed   bool
	consumer jsapi.Consumer
	it       jsapi.MessagesContext
	// busy counts consumer opens and releases in progress; idle is closed
	// when it returns to zero.
	busy int
	idle chan struct{}
	// retained names consumers whose delete failed, for Close to retry.
	retained []string
}

var _ message.Feed = (*feed)(nil)

var _ message.Store = (*Store)(nil)

// Next returns the next append on the shard. A record that cannot be
// decoded is returned as ErrCorruptRecord, and the feed moves past it.
func (f *feed) Next(ctx context.Context) (message.Change, error) {
	for {
		msg, err := f.next(ctx)
		if err != nil {
			return message.Change{}, err
		}
		if msg != nil {
			c, err := f.claim(msg)
			if f.claimed != nil {
				f.claimed()
			}
			return c, err
		}
	}
}

// claim orders a delivery against Close under mu: claimed before Close
// begins, the change is returned and the cursor advances; otherwise withheld.
func (f *feed) claim(msg jsapi.Msg) (message.Change, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return message.Change{}, message.ErrFeedClosed
	}
	return f.change(msg)
}

// next waits up to idleCheck for the consumer. If it delivers nothing while
// the stream holds a change after the cursor, the consumer is replaced from
// the cursor, so a stored change never waits on a wedged pull.
func (f *feed) next(ctx context.Context) (jsapi.Msg, error) {
	opCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(f.closing, cancel)()
	it, closed := f.current()
	if closed {
		return nil, message.ErrFeedClosed
	}
	if it == nil {
		var err error
		if it, err = f.reopen(ctx); err != nil {
			return nil, err
		}
	}
	waitCtx, cancelWait := context.WithTimeout(opCtx, f.idleCheck)
	msg, err := it.Next(jsapi.NextContext(waitCtx))
	cancelWait()
	if err == nil {
		return msg, nil
	}
	if err := f.stopped(ctx); err != nil {
		return nil, err
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("feed on %s: %w", StreamName(f.shard), err)
	}
	_, err = f.stream.GetMsg(opCtx, f.cursor+1, jsapi.WithGetMsgSubject(shardFilter(f.shard)))
	switch {
	case errors.Is(err, jsapi.ErrMsgNotFound):
		return nil, nil
	case err != nil:
		return nil, f.interrupted(ctx, err)
	}
	// The next call reopens from the cursor unless Close has run.
	if consumer, old := f.detach(); old != nil {
		if err := f.discard(consumer, old); err != nil {
			return nil, fmt.Errorf("feed on %s: %w", StreamName(f.shard), err)
		}
	}
	return nil, nil
}

// stopped reports why Next must end early: the feed closed or ctx is done.
func (f *feed) stopped(ctx context.Context) error {
	if f.closing.Err() != nil {
		return message.ErrFeedClosed
	}
	return ctx.Err()
}

// interrupted prefers the reason Next was stopped over err.
func (f *feed) interrupted(ctx context.Context, err error) error {
	if stop := f.stopped(ctx); stop != nil {
		return stop
	}
	return fmt.Errorf("feed on %s: %w", StreamName(f.shard), err)
}

func (f *feed) change(msg jsapi.Msg) (message.Change, error) {
	md, err := msg.Metadata()
	if err != nil {
		return message.Change{}, fmt.Errorf("feed on %s: %w", StreamName(f.shard), err)
	}
	f.cursor = md.Sequence.Stream
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

// Close deletes every consumer the feed knows of, waiting within closeWait for
// opens and releases in progress and retrying failed deletes. A timed-out
// create landing after its last delete is left to feedInactiveThreshold.
func (f *feed) Close() error {
	deadline := time.Now().Add(f.closeWait)
	f.mu.Lock()
	f.closed = true
	consumer, it := f.consumer, f.it
	f.consumer, f.it = nil, nil
	if it != nil {
		f.begin()
	}
	f.mu.Unlock()
	f.closeNow()
	if it != nil {
		_ = f.discard(consumer, it)
	}
	for {
		f.mu.Lock()
		if idle := f.idle; idle != nil {
			f.mu.Unlock()
			select {
			case <-idle:
				continue
			case <-time.After(time.Until(deadline)):
				return fmt.Errorf("close feed on %s: %w", StreamName(f.shard), errCleanupPending)
			}
		}
		names := f.retained
		f.retained = nil
		if len(names) == 0 {
			f.mu.Unlock()
			return nil
		}
		f.begin()
		f.mu.Unlock()
		var failed []string
		var errs []error
		for _, name := range names {
			if err := deleteConsumerNamed(f.stream, name); err != nil {
				failed, errs = append(failed, name), append(errs, err)
			}
		}
		f.mu.Lock()
		f.retained = append(f.retained, failed...)
		f.end()
		f.mu.Unlock()
		if len(errs) > 0 {
			return fmt.Errorf("close feed on %s: %w", StreamName(f.shard), errors.Join(errs...))
		}
	}
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
	_, _, err = readRange(ctx, s.js, info.Config.Name, shardFilter(messageShard), 0, info.State.LastSeq, func(msg *jsapi.RawStreamMsg) (bool, error) {
		seq := msg.Sequence
		ns, err := decodeNamespace(messageShard, msg.Subject, msg.Header)
		if err != nil {
			return false, fmt.Errorf("%w: %s position %d: %w", ErrCorruptRecord, StreamName(messageShard), seq, err)
		}
		a, seen := out[ns.Queue]
		if seen && a.stateShard != ns.StateShard {
			return false, fmt.Errorf("%w: %s bodies name state shards %d and %d (position %d)", ErrCorruptRecord, msg.Subject, a.stateShard, ns.StateShard, seq)
		}
		out[ns.Queue] = bodyAllocation{stateShard: ns.StateShard, messages: a.messages + 1}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("enumerate %s: %w", StreamName(messageShard), err)
	}
	return out, nil
}

// liveStateKeys reads the live state keys under keyPrefix in the state
// shard's bucket, through its last sequence at the call, from the stream
// leader: a follower's direct get or consumer may lag applied deletes.
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
	return leaderStateKeys(ctx, s.js, bucket, keyPrefix, info.State.LastSeq)
}

// shardFilter matches every message subject on a message shard.
func shardFilter(shard message.Shard) string {
	return "sqs.m." + strconv.FormatUint(uint64(shard), 10) + ".>"
}

// readRange calls fn, in order, for each stored match of filter in
// (after, bound] until fn returns false. Each match is a leader message get,
// which returns the whole body: a follower's consumer may lag applied purges.
func readRange(ctx context.Context, js jsapi.JetStream, stream, filter string, after, bound uint64, fn func(*jsapi.RawStreamMsg) (bool, error)) (uint64, bool, error) {
	req := msgGetRequest{NextFor: filter}
	for after < bound {
		req.Seq = after + 1
		msg, err := leaderMsgGet(ctx, js, stream, req)
		switch {
		case errors.Is(err, jsapi.ErrMsgNotFound):
			return bound, true, nil
		case err != nil:
			return after, false, err
		case msg.Sequence <= after:
			return after, false, jsapi.ErrInvalidJetStreamResponse
		case msg.Sequence > bound:
			return bound, true, nil
		}
		cont, err := fn(msg)
		if err != nil {
			return after, false, err
		}
		after = msg.Sequence
		if !cont && after < bound {
			return after, false, nil
		}
	}
	return bound, true, nil
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
// returning metadata only. The first page bounds the pass at the stream's
// last sequence; deleted positions are skipped by the stream itself.
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
	last, done, err := readRange(ctx, s.js, stream.CachedInfo().Config.Name, shardFilter(messageShard), uint64(from.After), uint64(from.Bound), func(msg *jsapi.RawStreamMsg) (bool, error) {
		seq := msg.Sequence
		ns, err := decodeNamespace(messageShard, msg.Subject, msg.Header)
		if err != nil {
			return false, fmt.Errorf("%w: %s position %d: %w", ErrCorruptRecord, StreamName(messageShard), seq, err)
		}
		page.Changes = append(page.Changes, message.Change{
			Namespace: ns, Position: message.Position(seq), Meta: domainMeta(msg.Header), Cursor: message.Cursor(seq),
		})
		return len(page.Changes) < limit, nil
	})
	if err != nil {
		return message.ScanPage{}, fmt.Errorf("scan %s: %w", StreamName(messageShard), err)
	}
	page.Next.After, page.Done = message.Cursor(last), done
	return page, nil
}
