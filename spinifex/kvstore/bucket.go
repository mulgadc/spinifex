// Package kvstore provides a typed store over a single JetStream KV bucket:
// one memoised get-or-create accessor and a JSON codec for the record type,
// replacing the accessor each caller used to hand-roll.
package kvstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

// Config describes the bucket that a Store or Bucket sits over.
type Config struct {
	Name    string
	History int
	// Description is set on the bucket at creation, including a recovery
	// recreate, so a recreated bucket is not left anonymous next to its peers.
	Description string
	Replicas    int
	TTL         time.Duration
	Missing     string
	Attempts    int
	Exhausted   func(key string, attempts int) error

	// RecreateIfMissing allows recovery to create the bucket again when the
	// stream is not merely unreachable but gone. It governs recovery only —
	// the first open is always get-or-create, because bootstrapping an absent
	// bucket is the intent there.
	//
	// Default false, and deliberately so: a bucket that existed and now does
	// not has lost its records, and recreating it turns that into a success
	// the caller cannot see. Set it only where the data is reconstructible,
	// as instance state is by the owning node's next write.
	RecreateIfMissing bool

	// OnOpen runs after every successful open, including a recovery reopen, so
	// a recreated bucket is re-stamped rather than left unversioned. Owners
	// use it to run their schema migrations.
	OnOpen func(ctx context.Context, kv jetstream.KeyValue) error
}

// Bucket memoises a lazily created KV bucket. Construct it with NewBucket.
type Bucket struct {
	js  jetstream.JetStream
	cfg Config

	mu sync.Mutex
	kv jetstream.KeyValue
}

// NewBucket returns a Bucket over js. A nil js is permitted: every call to KV then
// fails with cfg.Missing rather than panicking.
func NewBucket(js jetstream.JetStream, cfg Config) *Bucket {
	return &Bucket{js: js, cfg: cfg}
}

// NewOpenBucket returns a Bucket over an already-open handle, for callers that
// resolve their bucket at construction so a bad connection fails at startup
// rather than on first use. js is what recovery reopens against; pass nil to
// keep the handle fixed for a caller not ready to recover.
func NewOpenBucket(js jetstream.JetStream, kv jetstream.KeyValue, cfg Config) *Bucket {
	return &Bucket{js: js, kv: kv, cfg: cfg}
}

// Name returns the configured bucket name, for callers holding several buckets
// that need to tell them apart in a map or a log line.
func (b *Bucket) Name() string { return b.cfg.Name }

// Configured reports whether the bucket has anything to open against, for the
// callers whose absent-KV path is a legitimate fallback rather than an error.
func (b *Bucket) Configured() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.js != nil || b.kv != nil
}

// KV opens the bucket on first use and returns the cached handle thereafter.
func (b *Bucket) KV(ctx context.Context) (jetstream.KeyValue, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.kv != nil {
		return b.kv, nil
	}
	if b.js == nil {
		if b.cfg.Missing == "" {
			return nil, errors.New("kvstore: no JetStream client configured")
		}
		return nil, errors.New(b.cfg.Missing)
	}
	kv, err := b.open(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.onOpen(ctx, kv); err != nil {
		return nil, err
	}
	b.kv = kv
	return kv, nil
}

// jsAPIStreamMsgGet is the non-direct stream read, spelled out rather than
// reached through nats.go. Every convenience wrapper switches to
// $JS.API.DIRECT.GET on a stream with AllowDirect, and KV buckets always
// have it. The default API prefix holds because every caller builds its
// client with plain jetstream.New, so no domain is in play.
const jsAPIStreamMsgGet = "$JS.API.STREAM.MSG.GET.KV_%s"

// jsErrCodeMessageNotFound is returned for a filter matching nothing, which is
// an empty prefix rather than a failure.
const jsErrCodeMessageNotFound = 10037

// LastSequence reports the stream sequence of the newest message matching
// filter, or zero when nothing matches. A tombstone is a message, so the
// sequence follows the stream rather than the surviving key set.
//
// The stream leader answers, which is the entire point of the hand-rolled
// request: nats.go's GetLastMsgForSubject would serve this from any replica,
// so a reader asking "am I behind?" could be answered by a replica that is
// equally behind.
func (b *Bucket) LastSequence(ctx context.Context, filter string) (uint64, error) {
	b.mu.Lock()
	js := b.js
	b.mu.Unlock()
	if js == nil {
		if b.cfg.Missing == "" {
			return 0, errors.New("kvstore: no JetStream client configured")
		}
		return 0, errors.New(b.cfg.Missing)
	}

	req, err := json.Marshal(map[string]string{"last_by_subj": "$KV." + b.cfg.Name + "." + filter})
	if err != nil {
		return 0, fmt.Errorf("kvstore: encode last-sequence request for %s: %w", b.cfg.Name, err)
	}
	msg, err := js.Conn().RequestWithContext(ctx, fmt.Sprintf(jsAPIStreamMsgGet, b.cfg.Name), req)
	if err != nil {
		return 0, fmt.Errorf("kvstore: last sequence %s %s: %w", b.cfg.Name, filter, err)
	}

	var resp struct {
		Message *struct {
			Sequence uint64 `json:"seq"`
		} `json:"message"`
		Error *struct {
			ErrCode     int    `json:"err_code"`
			Description string `json:"description"`
		} `json:"error"`
	}
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return 0, fmt.Errorf("kvstore: decode last sequence %s: %w", b.cfg.Name, err)
	}
	if resp.Error != nil {
		if resp.Error.ErrCode == jsErrCodeMessageNotFound {
			return 0, nil
		}
		return 0, fmt.Errorf("kvstore: last sequence %s %s: %s", b.cfg.Name, filter, resp.Error.Description)
	}
	if resp.Message == nil {
		return 0, fmt.Errorf("kvstore: last sequence %s %s: empty response", b.cfg.Name, filter)
	}
	return resp.Message.Sequence, nil
}

// Reopen discards the memoised handle and resolves the bucket again, for a
// caller that saw the stream go out from under it. It reconnects; whether it
// may also recreate an absent bucket is Config.RecreateIfMissing.
func (b *Bucket) Reopen(ctx context.Context) (jetstream.KeyValue, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.kv = nil
	if b.js == nil {
		if b.cfg.Missing == "" {
			return nil, errors.New("kvstore: cannot reopen without a JetStream client")
		}
		return nil, errors.New(b.cfg.Missing)
	}

	// Another goroutine may already have repaired it, so reconnecting comes
	// first and is the only step that is always safe.
	kv, err := b.js.KeyValue(ctx, b.cfg.Name)
	if err != nil {
		if !b.cfg.RecreateIfMissing {
			return nil, fmt.Errorf("kvstore: reopen %s: %w", b.cfg.Name, err)
		}
		if !errors.Is(err, jetstream.ErrBucketNotFound) && !kvutil.IsStreamUnavailable(err) {
			return nil, fmt.Errorf("kvstore: reopen %s: %w", b.cfg.Name, err)
		}
		slog.WarnContext(ctx, "kvstore: bucket stream lost, recreating", "bucket", b.cfg.Name)
		if kv, err = b.open(ctx); err != nil {
			return nil, fmt.Errorf("kvstore: recreate %s: %w", b.cfg.Name, err)
		}
	}

	if err := b.onOpen(ctx, kv); err != nil {
		return nil, err
	}
	b.kv = kv
	slog.InfoContext(ctx, "kvstore: bucket reopened", "bucket", b.cfg.Name)
	return kv, nil
}

// withKV runs op against the bucket, reopening and running it once more if the
// stream was lost underneath it. A failure to reopen surfaces the original
// error, not the reopen's — the first one is what the caller was doing.
//
// op therefore has to be safe to run twice. Anything carrying a revision the
// caller supplied is not, and must not use this.
func (b *Bucket) withKV(ctx context.Context, op func(jetstream.KeyValue) error) error {
	kv, err := b.KV(ctx)
	if err != nil {
		return err
	}
	err = op(kv)
	if err == nil || !kvutil.IsStreamUnavailable(err) {
		return err
	}

	kv, reopenErr := b.Reopen(ctx)
	if reopenErr != nil {
		slog.WarnContext(ctx, "kvstore: recovery failed", "bucket", b.cfg.Name, "err", reopenErr)
		return err
	}
	return op(kv)
}

// onOpen runs the configured open hook, naming the bucket on failure so a
// migration error is not mistaken for a connection one.
func (b *Bucket) onOpen(ctx context.Context, kv jetstream.KeyValue) error {
	if b.cfg.OnOpen == nil {
		return nil
	}
	if err := b.cfg.OnOpen(ctx, kv); err != nil {
		return fmt.Errorf("kvstore: on-open %s: %w", b.cfg.Name, err)
	}
	return nil
}

// Watch returns a watcher over the keys matching filter, which takes NATS
// subject wildcards ("node.*", "lb.*", ">" for everything).
//
// Updates only: the caller reconciles once at startup, so replaying every
// existing key would fire a redundant pass, and replaying them again on every
// re-establish would defeat the debounce. The cost is that a disconnect gap is
// invisible, which is what the caller's periodic resync exists to cover.
// A watcher that dies with the stream is not resurrected by a later reopen;
// only establishing a new one recovers, which is what the caller's periodic
// resync drives.
func (b *Bucket) Watch(ctx context.Context, filter string) (jetstream.KeyWatcher, error) {
	var w jetstream.KeyWatcher
	// Wrapped inside the closure, so a bucket that could not be opened at all
	// reports the configured reason rather than a watch failure on top of it.
	err := b.withKV(ctx, func(kv jetstream.KeyValue) error {
		var err error
		w, err = kv.Watch(ctx, filter, jetstream.UpdatesOnly())
		if err != nil {
			return fmt.Errorf("kvstore: watch %s %s: %w", b.cfg.Name, filter, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return w, nil
}

// WatchFrom returns a watcher over the keys matching filter that resumes
// from revision: it replays every value at or after revision, followed by
// the end-of-replay marker, before turning into an ordinary live feed. It
// takes no UpdatesOnly, unlike Watch, because the whole point is to replay
// what a Snapshot's high-water mark missed — a caller rebuilding a cache
// from a snapshot uses this to close that window without a buffering fence.
func (b *Bucket) WatchFrom(ctx context.Context, filter string, revision uint64) (jetstream.KeyWatcher, error) {
	var w jetstream.KeyWatcher
	// Wrapped inside the closure, so a bucket that could not be opened at all
	// reports the configured reason rather than a watch failure on top of it.
	err := b.withKV(ctx, func(kv jetstream.KeyValue) error {
		var err error
		w, err = kv.Watch(ctx, filter, jetstream.ResumeFromRevision(revision))
		if err != nil {
			return fmt.Errorf("kvstore: watch %s %s: %w", b.cfg.Name, filter, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return w, nil
}

// open picks the kvutil helper matching the configured TTL and replica count.
func (b *Bucket) open(ctx context.Context) (jetstream.KeyValue, error) {
	switch {
	case b.cfg.TTL > 0:
		return kvutil.GetOrCreateBucketWithOptions(ctx, b.js, kvutil.BucketOptions{
			Name:        b.cfg.Name,
			Description: b.cfg.Description,
			History:     b.cfg.History,
			Replicas:    b.cfg.Replicas,
			TTL:         b.cfg.TTL,
		})
	case b.cfg.Replicas > 0 || b.cfg.Description != "":
		return kvutil.GetOrCreateBucketWithOptions(ctx, b.js, kvutil.BucketOptions{
			Name:        b.cfg.Name,
			Description: b.cfg.Description,
			History:     b.cfg.History,
			Replicas:    b.cfg.Replicas,
		})
	default:
		return kvutil.GetOrCreateBucket(ctx, b.js, b.cfg.Name, b.cfg.History)
	}
}
