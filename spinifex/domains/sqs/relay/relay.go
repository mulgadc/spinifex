package relay

import (
	"context"
	"crypto/md5" //nolint:gosec // SQS defines MD5OfBody.
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
)

// Defaults for a Relay whose fields are zero.
const (
	// DefaultCallLimit bounds a call end to end; it exceeds the 20-second
	// long poll with room for the worker's poll and post.
	DefaultCallLimit = 35 * time.Second
	// DefaultPollerStale is how long after the worker's last poll calls are
	// still queued; past it no worker is assumed and calls fail at once.
	DefaultPollerStale = 30 * time.Second
	// DefaultMaxPending bounds work queued or held by the worker.
	DefaultMaxPending = 512
	// DefaultMaxPendingBytes bounds the encoded inputs of that work.
	DefaultMaxPendingBytes = 64 << 20
	// takenRetention keeps a taken item after its caller left, so a late
	// post is answered as undelivered rather than unknown.
	takenRetention = 2 * time.Minute
)

// Taker is the worker identity that took an item, as the ingress authorized
// it on that poll. Only the same identity may post its result.
type Taker struct {
	InstanceID string
	Generation uint64
	ClaimID    string
}

// Outcome is what became of a posted result.
type Outcome int

const (
	// Delivered: the result was handed to the waiting caller.
	Delivered Outcome = iota + 1
	// CallerGone: the caller had stopped waiting before the result arrived,
	// so nothing from this result reached it.
	CallerGone
	// Unknown: the item is not held; whether a result reached the caller
	// cannot be said, so the worker must not act on non-delivery.
	Unknown
)

var (
	// ErrNotTaker is a post from an identity other than the one that took
	// the item.
	ErrNotTaker = errors.New("relay: work was taken by another worker identity")
	// ErrNoWorker is a call made while no worker is polling, or with the
	// queue full; nothing was handed out.
	ErrNoWorker = errors.New("relay: no SQS worker is accepting work")
)

type item struct {
	work  Work
	taken *Taker
	// gone is set, under the lock, once the caller stops waiting.
	gone   bool
	result chan Result
}

// Relay is the ingress end: it queues owner calls and hands them to the
// polling worker. It holds work in memory only, so it serves the worker
// polling this ingress and nothing else.
type Relay struct {
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// CallLimit, PollerStale and MaxPending default when zero.
	CallLimit       time.Duration
	PollerStale     time.Duration
	MaxPending      int
	MaxPendingBytes int

	mu    sync.Mutex
	items map[string]*item
	queue []string
	// bytes is the encoded input size of every held item.
	bytes    int
	lastPoll time.Time
	// polling counts polls waiting now; a waiting poll is a present worker.
	polling int
	wake    chan struct{}
}

func (r *Relay) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Relay) init() {
	if r.items == nil {
		r.items = map[string]*item{}
		r.wake = make(chan struct{})
	}
}

// call queues one owner call and waits for its result, the caller's context
// or the call limit. Every failure to obtain a result is unavailable.
func (r *Relay) call(ctx context.Context, op string, in any, unavailable error) (json.RawMessage, error) {
	input, err := marshal(in)
	if err != nil {
		return nil, unavailable
	}
	limit := r.CallLimit
	if limit <= 0 {
		limit = DefaultCallLimit
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	deadline, _ := ctx.Deadline()
	id, err := newID()
	if err != nil {
		return nil, unavailable
	}
	it := &item{work: Work{ID: id, Op: op, Deadline: deadline, Input: input}, result: make(chan Result, 1)}
	if err := r.enqueue(it); err != nil {
		return nil, errors.Join(unavailable, err)
	}
	select {
	case res := <-it.result:
		if res.Error != nil {
			return nil, decodeError(res.Error)
		}
		return res.Output, nil
	case <-ctx.Done():
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case res := <-it.result:
		// Posted while the caller was leaving: still delivered.
		if res.Error != nil {
			return nil, decodeError(res.Error)
		}
		return res.Output, nil
	default:
	}
	it.gone = true
	if it.taken == nil {
		r.drop(id)
		r.removeQueued(id)
	}
	return nil, unavailable
}

func (r *Relay) enqueue(it *item) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	r.expire()
	stale := r.PollerStale
	if stale <= 0 {
		stale = DefaultPollerStale
	}
	limit := r.MaxPending
	if limit <= 0 {
		limit = DefaultMaxPending
	}
	maxBytes := r.MaxPendingBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxPendingBytes
	}
	present := r.polling > 0 || !r.lastPoll.IsZero() && r.now().Sub(r.lastPoll) <= stale
	if !present || len(r.items) >= limit || r.bytes+len(it.work.Input) > maxBytes {
		return ErrNoWorker
	}
	r.bytes += len(it.work.Input)
	r.items[it.work.ID] = it
	r.queue = append(r.queue, it.work.ID)
	close(r.wake)
	r.wake = make(chan struct{})
	return nil
}

// drop forgets a held item and its bytes.
func (r *Relay) drop(id string) {
	if it, ok := r.items[id]; ok {
		r.bytes -= len(it.work.Input)
		delete(r.items, id)
	}
}

func (r *Relay) removeQueued(id string) {
	for i, q := range r.queue {
		if q == id {
			r.queue = append(r.queue[:i], r.queue[i+1:]...)
			return
		}
	}
}

// expire drops taken items whose caller left long enough ago that no post
// for them is still expected.
func (r *Relay) expire() {
	now := r.now()
	for id, it := range r.items {
		if it.taken != nil && now.After(it.work.Deadline.Add(takenRetention)) {
			r.drop(id)
		}
	}
}

// Take hands taker up to limit queued items whose inputs total at most
// maxBytes, but always at least one, waiting until one is queued or ctx
// ends. The poll itself marks a worker present.
func (r *Relay) Take(ctx context.Context, taker Taker, limit, maxBytes int) []Work {
	r.mu.Lock()
	r.polling++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.polling--
		r.lastPoll = r.now()
		r.mu.Unlock()
	}()
	for {
		r.mu.Lock()
		r.init()
		r.lastPoll = r.now()
		var out []Work
		size := 0
		for len(r.queue) > 0 && len(out) < limit {
			id := r.queue[0]
			it, ok := r.items[id]
			if !ok || it.gone {
				r.queue = r.queue[1:]
				continue
			}
			if len(out) > 0 && size+len(it.work.Input) > maxBytes {
				break
			}
			r.queue = r.queue[1:]
			size += len(it.work.Input)
			t := taker
			it.taken = &t
			out = append(out, it.work)
		}
		wake := r.wake
		r.mu.Unlock()
		if len(out) > 0 {
			return out
		}
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
		}
	}
}

// Complete delivers res for id if taker took it and its caller still waits.
// CallerGone is the only outcome that proves nothing reached the caller.
func (r *Relay) Complete(taker Taker, id string, res Result) (Outcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	it, ok := r.items[id]
	if !ok || it.taken == nil {
		return Unknown, nil
	}
	if *it.taken != taker {
		return 0, ErrNotTaker
	}
	r.drop(id)
	if it.gone {
		return CallerGone, nil
	}
	it.result <- res
	return Delivered, nil
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Owner capabilities the public adapter runs against, carried to the worker.
// A success is reported only for a complete result that names the queue the
// caller asked about; anything else is unavailable, never a settled answer.
var (
	_ awsapi.Queues     = (*Relay)(nil)
	_ awsapi.Messages   = (*Relay)(nil)
	_ awsapi.Receives   = (*Relay)(nil)
	_ awsapi.Deletes    = (*Relay)(nil)
	_ awsapi.Visibility = (*Relay)(nil)
)

var md5Hex = regexp.MustCompile(`^[0-9a-f]{32}$`)

func decodeOut[T any](raw json.RawMessage, unavailable error) (T, error) {
	var v T
	if len(raw) == 0 || string(raw) == "null" {
		return v, unavailable
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, unavailable
	}
	return v, nil
}

// validRecord requires an active queue with a usable namespace, since the
// adapter pins sends and batches to it.
func validRecord(r queue.Record, account, region, name string) bool {
	return r.QueueID != "" && r.State == queue.StateActive && r.Namespace().Validate() == nil &&
		r.Account == account && r.Region == region && (name == "" || r.Name == name)
}

func (r *Relay) record(ctx context.Context, op string, in any, account, region, name string) (queue.Record, error) {
	raw, err := r.call(ctx, op, in, queue.ErrUnavailable)
	if err != nil {
		return queue.Record{}, err
	}
	rec, err := decodeOut[queue.Record](raw, queue.ErrUnavailable)
	if err != nil || !validRecord(rec, account, region, name) {
		return queue.Record{}, queue.ErrUnavailable
	}
	return rec, nil
}

func (r *Relay) Create(ctx context.Context, req queue.CreateRequest) (queue.Record, error) {
	return r.record(ctx, OpCreateQueue, req, req.Account, req.Region, req.Name)
}

func (r *Relay) Resolve(ctx context.Context, account, region, name string) (queue.Record, error) {
	return r.record(ctx, OpResolveQueue, nameInput{account, region, name}, account, region, name)
}

func (r *Relay) List(ctx context.Context, req queue.ListRequest) (queue.ListResult, error) {
	raw, err := r.call(ctx, OpListQueues, req, queue.ErrUnavailable)
	if err != nil {
		return queue.ListResult{}, err
	}
	res, err := decodeOut[queue.ListResult](raw, queue.ErrUnavailable)
	if err != nil {
		return queue.ListResult{}, err
	}
	for _, q := range res.Queues {
		if !validRecord(q, req.Account, req.Region, "") || !strings.HasPrefix(q.Name, req.Prefix) {
			return queue.ListResult{}, queue.ErrUnavailable
		}
	}
	return res, nil
}

func (r *Relay) SetAttributes(ctx context.Context, account, region, name string, attrs map[string]string) (queue.Record, error) {
	return r.record(ctx, OpSetQueueAttributes, attrsInput{account, region, name, attrs}, account, region, name)
}

func (r *Relay) Tag(ctx context.Context, account, region, name string, tags map[string]string) (queue.Record, error) {
	return r.record(ctx, OpTagQueue, tagsInput{account, region, name, tags}, account, region, name)
}

func (r *Relay) Untag(ctx context.Context, account, region, name string, keys []string) (queue.Record, error) {
	return r.record(ctx, OpUntagQueue, untagInput{account, region, name, keys}, account, region, name)
}

func (r *Relay) Tags(ctx context.Context, account, region, name string) (map[string]string, error) {
	raw, err := r.call(ctx, OpListQueueTags, nameInput{account, region, name}, queue.ErrUnavailable)
	if err != nil {
		return nil, err
	}
	return decodeOut[map[string]string](raw, queue.ErrUnavailable)
}

func (r *Relay) Send(ctx context.Context, in message.SendInput) (message.SendResult, error) {
	raw, err := r.call(ctx, OpSendMessage, in, message.ErrUnavailable)
	if err != nil {
		return message.SendResult{}, err
	}
	res, err := decodeOut[message.SendResult](raw, message.ErrUnavailable)
	// A position and enqueue time are the evidence the append completed.
	if err != nil || res.MessageID == "" || res.Position == 0 || res.EnqueuedAt.IsZero() || !md5Hex.MatchString(res.MD5OfBody) ||
		res.MD5OfBody != md5Of(in.Body) || res.MD5OfMessageAttributes != sentAttributesMD5(in) {
		return message.SendResult{}, message.ErrUnavailable
	}
	return res, nil
}

func (r *Relay) Receive(ctx context.Context, in message.ReceiveInput) ([]message.Received, error) {
	raw, err := r.call(ctx, OpReceiveMessage, in, message.ErrUnavailable)
	if err != nil {
		return nil, err
	}
	want := 1
	if in.MaxNumberOfMessages != nil {
		want = *in.MaxNumberOfMessages
	}
	wire, err := decodeOut[[]received](raw, message.ErrUnavailable)
	if err != nil || len(wire) > min(want, ReceiveLimit) {
		return nil, message.ErrUnavailable
	}
	out, err := fromWireReceived(wire)
	if err != nil {
		return nil, message.ErrUnavailable
	}
	for _, m := range out {
		if m.MessageID == "" || m.ReceiptHandle == "" || m.MD5OfBody != md5Of(m.Body) || m.SenderID == "" ||
			m.SentAt.IsZero() || m.FirstReceive.IsZero() || m.Deadline.IsZero() || m.ReceiveCount == 0 ||
			m.Attributes.Len() > 0 && m.MD5OfMessageAttributes != m.Attributes.MD5() {
			return nil, message.ErrUnavailable
		}
	}
	return out, nil
}

// ack accepts only the empty object the worker answers a settled call with.
func ack(raw json.RawMessage, err error) error {
	if err != nil {
		return err
	}
	if string(raw) != "{}" {
		return message.ErrUnavailable
	}
	return nil
}

func (r *Relay) Delete(ctx context.Context, in message.DeleteInput) error {
	return ack(r.call(ctx, OpDeleteMessage, in, message.ErrUnavailable))
}

func (r *Relay) ChangeVisibility(ctx context.Context, in message.ChangeVisibilityInput) error {
	return ack(r.call(ctx, OpChangeVisibility, in, message.ErrUnavailable))
}

// sentAttributesMD5 is the digest the worker must return for in's attributes,
// empty when it has none.
func sentAttributesMD5(in message.SendInput) string {
	attrs, err := message.NewAttributes(in.MessageAttributes)
	if err != nil || attrs.Len() == 0 {
		return ""
	}
	return attrs.MD5()
}

func md5Of(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // SQS defines MD5OfBody; this is an integrity check, not security.
	return hex.EncodeToString(sum[:])
}
