package message

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Receive limits. MaxVisibilitySeconds is the queue and per-call ceiling.
const (
	MaxReceiveMessages   = 10
	MaxVisibilitySeconds = 43200
	// ReceiveScanPage and ReceiveScanPages bound one receive's scan work:
	// at most ReceiveScanPages pages of ReceiveScanPage positions.
	ReceiveScanPage  = 100
	ReceiveScanPages = 4
	// MaxWaitSeconds is the longest long poll. LongPollRecheck bounds how
	// long a long poll waits between polls without an append to wake it.
	MaxWaitSeconds  = 20
	LongPollRecheck = time.Second
)

// errScanCut marks a poll whose scan did not complete.
var errScanCut = errors.New("scan queue")

// ReceiveTarget is an active queue resolved for a receive.
type ReceiveTarget struct {
	Namespace Namespace
	Account   string
	// VisibilityTimeout, MessageRetentionPeriod and ReceiveWaitTime are the
	// queue's current values in seconds.
	VisibilityTimeout      int
	MessageRetentionPeriod int
	ReceiveWaitTime        int
}

// ReceiveQueues resolves an active queue for a receive; it is the queue
// owner's capability, injected so this package never reads queue records.
type ReceiveQueues interface {
	ReceiveTarget(ctx context.Context, account, region, name string) (ReceiveTarget, error)
}

// ReceiveInput is one ReceiveMessage request after wire decoding.
type ReceiveInput struct {
	Account, Region, Queue string
	// MaxNumberOfMessages is 1 to MaxReceiveMessages, default 1.
	MaxNumberOfMessages *int
	// VisibilityTimeout overrides the queue's for this receive when set.
	VisibilityTimeout *int
	// WaitTimeSeconds overrides the queue's ReceiveMessageWaitTimeSeconds; an
	// effective wait above 0 is a long poll of that many seconds.
	WaitTimeSeconds *int
}

// Received is one message taken in flight by a receive, with its handle.
type Received struct {
	MessageID     string
	ReceiptHandle string
	Body          []byte
	MD5OfBody     string
	// Attributes and MD5OfMessageAttributes are empty for a message sent
	// without attributes.
	Attributes             Attributes
	MD5OfMessageAttributes string
	SenderID               string
	SentAt                 time.Time
	ReceiveCount           uint64
	FirstReceive           time.Time
	Deadline               time.Time
}

// Receiver is the ReceiveMessage owner core, short and long poll.
type Receiver struct {
	Store  Store
	Queues ReceiveQueues
	Keys   *ReceiptKeys
	// Now defaults to the wall clock.
	Now func() time.Time
	// Recheck is the longest a long poll waits between polls without an
	// append to wake it; zero means LongPollRecheck.
	Recheck time.Duration

	mu sync.Mutex
	// passes resumes each queue's scan pass across receives, so repeated
	// polls cover the shard beyond the first page budget. It is a hint
	// only: a restart begins a new pass and every claim is still checked.
	passes map[QueueID]ScanCursor
}

// Receive returns up to MaxNumberOfMessages messages taken in flight, waiting
// up to the effective wait for one. It claims only once queue and signer are
// known; an unsettled scan or claim with nothing claimed is ErrUnavailable.
func (r *Receiver) Receive(ctx context.Context, in ReceiveInput) ([]Received, error) {
	if r == nil || r.Store == nil || r.Queues == nil {
		return nil, fmt.Errorf("%w: receiver is not configured", ErrUnavailable)
	}
	if err := r.Keys.Check(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	want, visOverride, err := validateReceive(in)
	if err != nil {
		return nil, err
	}
	t, err := r.Queues.ReceiveTarget(ctx, in.Account, in.Region, in.Queue)
	if err != nil {
		return nil, err
	}
	vis, retention, wait, err := receiveTiming(in.Account, t, visOverride, in.WaitTimeSeconds)
	if err != nil {
		return nil, err
	}
	if wait == 0 {
		out, _, err := r.poll(ctx, t.Namespace, want, vis, retention)
		return out, err
	}
	return r.longPoll(ctx, t.Namespace, want, vis, retention, wait)
}

// longPoll repeats the bounded poll until a message is claimed, the wait
// ends or ctx is done. The wake feed is open before the first poll, so an
// append landing between a poll and the wait still wakes it. All waiting
// work is bounded by the wait; empty is returned only after a full poll.
func (r *Receiver) longPoll(ctx context.Context, ns Namespace, want int, vis, retention, wait time.Duration) ([]Received, error) {
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	feed, err := r.Store.Feed(wctx, ns.MessageShard, math.MaxUint64)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, fmt.Errorf("%w: open wake feed: %w", ErrUnavailable, err)
	}
	var (
		wake    = make(chan struct{}, 1)
		done    = make(chan struct{})
		closing atomic.Bool
		feedErr error
	)
	go func() {
		defer close(done)
		for {
			ch, err := feed.Next(wctx)
			if err != nil {
				if wctx.Err() == nil && !closing.Load() {
					feedErr = err
				}
				return
			}
			if ch.Namespace == ns {
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		}
	}()
	var once sync.Once
	// stop releases the feed and reports a failure it saw before release.
	stop := func() error {
		once.Do(func() {
			closing.Store(true)
			if err := feed.Close(); err != nil {
				slog.WarnContext(ctx, "SQS receive wake feed close failed", "err", err)
			}
			cancel()
			<-done
		})
		if feedErr != nil {
			return fmt.Errorf("%w: wake feed: %w", ErrUnavailable, feedErr)
		}
		return nil
	}
	defer func() { _ = stop() }()
	checked := false
	for {
		out, next, err := r.poll(wctx, ns, want, vis, retention)
		if len(out) > 0 {
			return out, nil
		}
		if err != nil {
			return nil, r.endErr(ctx, wctx, err, checked, stop)
		}
		checked = true
		nap := r.recheck()
		// A delayed or in-flight message becomes visible without an append,
		// so the wait is cut to the next known visibility or the recheck.
		if !next.IsZero() {
			nap = min(nap, max(next.Sub(r.now()), 0))
		}
		timer := time.NewTimer(nap)
		select {
		case <-wctx.Done():
		case <-done:
		case <-wake:
			timer.Stop()
			continue
		case <-timer.C:
			continue
		}
		timer.Stop()
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, stop()
	}
}

// endErr settles a poll that failed with nothing claimed. Caller
// cancellation wins; only a scan ended by the wait's deadline, after a full
// poll and with a healthy feed, is an empty result; anything else is
// unavailable.
func (r *Receiver) endErr(ctx, wctx context.Context, err error, checked bool, stop func() error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	if wctx.Err() != nil && checked && errors.Is(err, errScanCut) && errors.Is(err, context.DeadlineExceeded) {
		return stop()
	}
	return err
}

// poll is one bounded scan-and-claim pass. It returns every message it
// claimed and the earliest delay end it saw; a scan or claim it could not
// settle is ErrUnavailable only when it claimed nothing.
func (r *Receiver) poll(ctx context.Context, ns Namespace, want int, vis, retention time.Duration) ([]Received, time.Time, error) {
	claimer := &Claimer{Store: r.Store, Now: r.Now}
	var (
		out       []Received
		unsettled error
		next      time.Time
	)
	cur := r.resume(ns.Queue)
	// fresh is true once a pass begun in this poll is under way; a second
	// completed pass would only repeat it, so the poll stops there.
	fresh := cur == ScanCursor{}
	for pages := 0; pages < ReceiveScanPages && len(out) < want; pages++ {
		page, err := r.Store.Scan(ctx, ns.MessageShard, cur, ReceiveScanPage)
		if err != nil {
			if unsettled == nil {
				unsettled = fmt.Errorf("%w: %w: %w", ErrUnavailable, errScanCut, err)
			}
			break
		}
		full := false
		for _, ch := range page.Changes {
			if len(out) == want {
				// Resume the pass at the first change not yet tried.
				cur, full = ScanCursor{After: ch.Cursor - 1, Bound: page.Next.Bound}, true
				break
			}
			if ch.Namespace != ns {
				continue
			}
			if at, ok := r.delayedUntil(ch); ok {
				if next.IsZero() || at.Before(next) {
					next = at
				}
				continue
			}
			got, err := claimer.Claim(ctx, ClaimInput{Namespace: ns, Position: ch.Position, Visibility: vis, Retention: retention})
			switch {
			case err == nil:
				rc, err := r.received(ns.Queue, got)
				if err != nil {
					return nil, time.Time{}, err
				}
				out = append(out, rc)
			case errors.Is(err, ErrNotVisible), errors.Is(err, ErrGone), errors.Is(err, ErrClaimLost), errors.Is(err, ErrLeaseExpired):
			default:
				if unsettled == nil {
					unsettled = err
				}
			}
		}
		if full {
			break
		}
		cur = page.Next
		if page.Done {
			cur = ScanCursor{}
			if fresh {
				break
			}
			fresh = true
		}
	}
	r.save(ns.Queue, cur)
	if len(out) == 0 && unsettled != nil {
		return nil, time.Time{}, unsettled
	}
	return out, next, nil
}

// ValidateReceive checks the request alone, so an adapter can refuse it
// before authorizing; the queue's wait default is checked at Receive.
func ValidateReceive(in ReceiveInput) error {
	_, _, err := validateReceive(in)
	return err
}

func validateReceive(in ReceiveInput) (want int, vis *int, err error) {
	want = 1
	if in.MaxNumberOfMessages != nil {
		want = *in.MaxNumberOfMessages
		if want < 1 || want > MaxReceiveMessages {
			return 0, nil, fmt.Errorf("%w: MaxNumberOfMessages must be between 1 and %d", ErrInvalidParameter, MaxReceiveMessages)
		}
	}
	if in.VisibilityTimeout != nil && (*in.VisibilityTimeout < 0 || *in.VisibilityTimeout > MaxVisibilitySeconds) {
		return 0, nil, fmt.Errorf("%w: VisibilityTimeout must be between 0 and %d", ErrInvalidParameter, MaxVisibilitySeconds)
	}
	if in.WaitTimeSeconds != nil {
		if w := *in.WaitTimeSeconds; w < 0 || w > MaxWaitSeconds {
			return 0, nil, fmt.Errorf("%w: WaitTimeSeconds must be between 0 and %d", ErrInvalidParameter, MaxWaitSeconds)
		}
	}
	return want, in.VisibilityTimeout, nil
}

// receiveTiming checks the resolved queue before any claim: a target for
// another account or with unreadable timing is unavailable, not defaulted.
// The wait is the request's WaitTimeSeconds, else the queue's default.
func receiveTiming(account string, t ReceiveTarget, override, wait *int) (vis, retention, waitFor time.Duration, err error) {
	switch {
	case t.Account != account:
		return 0, 0, 0, fmt.Errorf("%w: resolved queue belongs to another account", ErrUnavailable)
	case t.VisibilityTimeout < 0 || t.VisibilityTimeout > MaxVisibilitySeconds:
		return 0, 0, 0, fmt.Errorf("%w: queue visibility timeout %d outside 0..%d", ErrUnavailable, t.VisibilityTimeout, MaxVisibilitySeconds)
	case t.MessageRetentionPeriod < 60 || t.MessageRetentionPeriod > 1209600:
		return 0, 0, 0, fmt.Errorf("%w: queue retention %d outside 60..1209600", ErrUnavailable, t.MessageRetentionPeriod)
	case t.ReceiveWaitTime < 0 || t.ReceiveWaitTime > MaxWaitSeconds:
		return 0, 0, 0, fmt.Errorf("%w: queue receive wait %d outside 0..%d", ErrUnavailable, t.ReceiveWaitTime, MaxWaitSeconds)
	}
	if err := t.Namespace.Validate(); err != nil {
		return 0, 0, 0, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	secs, w := t.VisibilityTimeout, t.ReceiveWaitTime
	if override != nil {
		secs = *override
	}
	if wait != nil {
		w = *wait
	}
	return time.Duration(secs) * time.Second, time.Duration(t.MessageRetentionPeriod) * time.Second, time.Duration(w) * time.Second, nil
}

// delayedUntil reports a still-delayed message's visibility, skipped
// without a state read, only when all its metadata is valid; anything else
// goes to the claim, which fails closed.
func (r *Receiver) delayedUntil(ch Change) (time.Time, bool) {
	h, err := readHeaders(ch.Meta)
	if err != nil || h.vis <= r.now().UnixMilli() {
		return time.Time{}, false
	}
	return time.UnixMilli(h.vis), true
}

func (r *Receiver) received(q QueueID, c Claim) (Received, error) {
	h, err := r.Keys.Encode(Receipt{Queue: q, Position: c.Message.Position, Generation: c.Generation})
	if err != nil {
		return Received{}, fmt.Errorf("%w: sign receipt: %w", ErrUnavailable, err)
	}
	enq, _ := strconv.ParseInt(c.Message.Meta[MetaEnq], 10, 64)
	return Received{
		MessageID: c.MessageID, ReceiptHandle: h, Body: c.Message.Body,
		MD5OfBody: c.Message.Meta[MetaMD5], SenderID: c.Message.Meta[MetaSender],
		Attributes: c.Attributes, MD5OfMessageAttributes: c.Attributes.MD5(),
		SentAt: time.UnixMilli(enq).UTC(), ReceiveCount: c.ReceiveCount,
		FirstReceive: c.FirstReceive, Deadline: c.Deadline,
	}, nil
}

func (r *Receiver) recheck() time.Duration {
	if r.Recheck > 0 {
		return r.Recheck
	}
	return LongPollRecheck
}

func (r *Receiver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Receiver) resume(q QueueID) ScanCursor {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.passes[q]
}

func (r *Receiver) save(q QueueID, c ScanCursor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c == (ScanCursor{}) {
		delete(r.passes, q)
		return
	}
	if r.passes == nil {
		r.passes = make(map[QueueID]ScanCursor)
	}
	r.passes[q] = c
}
