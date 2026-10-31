// Package workerserve runs the SQS queue and message owners inside the
// worker guest against the dedicated store, taking owner calls from the
// gateway by pull and posting each result back.
package workerserve

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/relay"
)

const (
	// markerBucket holds worker facts the store must outlive a worker disk for.
	markerBucket = "sqs_worker"
	receiptKeyID = 1
	markerKey    = "receipt-key.1"
)

var (
	// ErrReceiptKeyChanged is a disk key whose fingerprint differs from the
	// one the store recorded: handles already issued would stop verifying.
	ErrReceiptKeyChanged = errors.New("worker receipt key does not match the store's recorded fingerprint")
)

// fingerprint identifies key without revealing it.
func fingerprint(key []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("spinifex-sqs receipt-key fingerprint v1"))
	return hex.EncodeToString(m.Sum(nil))
}

// CheckReceiptKey binds key to the store: the first worker to serve records
// its fingerprint, and every later start must present the same key.
func CheckReceiptKey(ctx context.Context, js jetstream.JetStream, key []byte) error {
	kv, err := sqsjs.OpenDurableKV(ctx, js, markerBucket, "SQS worker facts the store outlives a worker disk for")
	if err != nil {
		return err
	}
	fp := fingerprint(key)
	entry, err := kv.Get(ctx, markerKey)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		if _, err = kv.Create(ctx, markerKey, []byte(fp)); err == nil {
			return nil
		}
		if !errors.Is(err, jetstream.ErrKeyExists) {
			return fmt.Errorf("record receipt key fingerprint: %w", err)
		}
		entry, err = kv.Get(ctx, markerKey)
	}
	if err != nil {
		return fmt.Errorf("read receipt key fingerprint: %w", err)
	}
	if !hmac.Equal(entry.Value(), []byte(fp)) {
		return ErrReceiptKeyChanged
	}
	return nil
}

// OpenOwners opens the queue and message owners on the dedicated store with
// the single-node layout, after binding the receipt key to that store.
func OpenOwners(ctx context.Context, js jetstream.JetStream, receiptKey []byte) (relay.Owners, error) {
	if err := CheckReceiptKey(ctx, js, receiptKey); err != nil {
		return relay.Owners{}, err
	}
	keys, err := message.NewReceiptKeys(receiptKeyID, map[uint32][]byte{receiptKeyID: receiptKey})
	if err != nil {
		return relay.Owners{}, err
	}
	cfg := sqsjs.DefaultConfig(1)
	qs, err := queue.Open(ctx, js, cfg.MessageStreams, cfg.StateBuckets)
	if err != nil {
		return relay.Owners{}, fmt.Errorf("open queue owner: %w", err)
	}
	ms, err := sqsjs.New(ctx, js, cfg)
	if err != nil {
		return relay.Owners{}, fmt.Errorf("open message store: %w", err)
	}
	return relay.Owners{
		Queues:     qs,
		Messages:   &message.Sender{Store: ms, Queues: qs},
		Receives:   &message.Receiver{Store: ms, Queues: qs, Keys: keys},
		Deletes:    &message.Deleter{Store: ms, Queues: qs, Keys: keys},
		Visibility: &message.VisibilityChanger{Store: ms, Queues: qs, Keys: keys},
	}, nil
}

// Gateway is the worker's instance-role channel to its node's gateway.
type Gateway interface {
	PollWork(ctx context.Context) ([]relay.Work, error)
	CompleteWork(ctx context.Context, id string, result []byte) (string, error)
}

// Executor runs owner calls; relay.Owners is the real one.
type Executor interface {
	Execute(ctx context.Context, w relay.Work) ([]byte, []relay.Claim)
	Release(ctx context.Context, claims []relay.Claim) error
}

var _ Executor = relay.Owners{}

// Outcome names CompleteWork answers with; only OutcomeCallerGone proves
// that nothing from a result reached the caller.
const (
	OutcomeDelivered  = "delivered"
	OutcomeCallerGone = "caller-gone"
)

// Run opens the owners on the store and serves until ctx ends.
func Run(ctx context.Context, gw Gateway, js jetstream.JetStream, receiptKey []byte, logf func(string)) error {
	owners, err := OpenOwners(ctx, js, receiptKey)
	if err != nil {
		return err
	}
	return Serve(ctx, Config{Gateway: gw, Executor: owners, Log: logf})
}

// Config sizes the serve loop.
type Config struct {
	Gateway  Gateway
	Executor Executor
	// Concurrency bounds calls run at once; zero is 32.
	Concurrency int
	// PostLimit bounds one CompleteWork; zero is 30 seconds.
	PostLimit time.Duration
	// Backoff is the wait after a failed poll; zero is one second.
	Backoff time.Duration
	Log     func(string)
}

// Serve polls and runs owner calls until ctx ends.
func Serve(ctx context.Context, cfg Config) error {
	if cfg.Gateway == nil || cfg.Executor == nil {
		return errors.New("workerserve: no gateway or executor is configured")
	}
	conc := cfg.Concurrency
	if conc <= 0 {
		conc = 32
	}
	backoff := cfg.Backoff
	if backoff <= 0 {
		backoff = time.Second
	}
	logf := cfg.Log
	if logf == nil {
		logf = func(string) {}
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	defer wg.Wait()
	last := ""
	for ctx.Err() == nil {
		works, err := cfg.Gateway.PollWork(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			// Only a change is logged, so a long outage does not flood the console.
			if state := "poll failed: " + err.Error(); state != last {
				logf(state)
				last = state
			}
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			continue
		}
		if last != "" {
			logf("polling the gateway for SQS work")
			last = ""
		}
		for _, w := range works {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return ctx.Err()
			}
			wg.Go(func() {
				defer func() { <-sem }()
				handle(ctx, cfg, w, logf)
			})
		}
	}
	return ctx.Err()
}

// handle runs one call and posts its result. Claims are released only when
// the result provably never reached the caller; any other failure leaves
// them in flight until their visibility timeout expires.
func handle(ctx context.Context, cfg Config, w relay.Work, logf func(string)) {
	callCtx, cancel := context.WithDeadline(ctx, w.Deadline)
	body, claims := cfg.Executor.Execute(callCtx, w)
	cancel()
	limit := cfg.PostLimit
	if limit <= 0 {
		limit = 30 * time.Second
	}
	postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), limit)
	defer cancel()
	outcome, err := cfg.Gateway.CompleteWork(postCtx, w.ID, body)
	if err == nil && outcome == OutcomeCallerGone {
		release(ctx, cfg, claims, logf)
		return
	}
	if err != nil {
		logf(fmt.Sprintf("%s result not confirmed; any received messages reappear after their visibility timeout: %v", w.Op, err))
	}
}

func release(ctx context.Context, cfg Config, claims []relay.Claim, logf func(string)) {
	if len(claims) == 0 {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := cfg.Executor.Release(rctx, claims); err != nil {
		logf(fmt.Sprintf("release of %d undelivered messages failed; they reappear after their visibility timeout: %v", len(claims), err))
	}
}
