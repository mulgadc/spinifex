package readiness

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

var (
	// ErrStreamMissing is a bound stream that no longer exists.
	ErrStreamMissing = errors.New("bound readiness stream is missing")
	// ErrStreamReplaced is a stream whose incarnation differs from its binding.
	// Its bytes and revisions are never authoritative.
	ErrStreamReplaced = errors.New("bound readiness stream was replaced")
	// ErrStreamUnavailable is a stream whose identity cannot be read now.
	ErrStreamUnavailable = errors.New("bound readiness stream is unavailable")
)

// Binding is independently recorded evidence of one bucket's stream
// incarnation: the bucket and the creation time reported with a named leader.
type Binding struct {
	Bucket  string    `json:"bucket"`
	Created time.Time `json:"created"`
}

func streamName(bucket string) string { return "KV_" + bucket }

// Observe reads the binding of an existing bucket's stream without creating
// it. Deciding that the observation may be recorded is the caller's job.
func Observe(ctx context.Context, js jetstream.JetStream, bucket string) (Binding, error) {
	created, err := streamCreated(ctx, js, bucket)
	if err != nil {
		return Binding{}, err
	}
	return Binding{Bucket: bucket, Created: created}, nil
}

// Stream is a bucket attached under a binding. Once it is seen missing or
// replaced it stays quarantined for the life of the handle.
type Stream struct {
	js      jetstream.JetStream
	binding Binding

	mu         sync.Mutex
	quarantine error
}

// Attach verifies b and opens its bucket without creating or recreating the
// stream, then verifies again so an open that raced a replacement is refused.
func Attach(ctx context.Context, js jetstream.JetStream, b Binding) (*Stream, jetstream.KeyValue, error) {
	if b.Bucket == "" || b.Created.IsZero() {
		return nil, nil, fmt.Errorf("%w: binding %+v", ErrInvalid, b)
	}
	s := &Stream{js: js, binding: b}
	if err := s.Verify(ctx); err != nil {
		return nil, nil, err
	}
	kv, err := js.KeyValue(ctx, b.Bucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, nil, s.quarantined(fmt.Errorf("%w: %s", ErrStreamMissing, b.Bucket))
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: open %s: %w", ErrStreamUnavailable, b.Bucket, err)
	}
	if err := s.Verify(ctx); err != nil {
		return nil, nil, err
	}
	return s, kv, nil
}

// Verify checks the stream's reported identity against the binding. Call it
// before interpreting a read or sending a write, and again after every
// acknowledgement; it compares identities and is not an atomic write fence.
func (s *Stream) Verify(ctx context.Context) error {
	s.mu.Lock()
	q := s.quarantine
	s.mu.Unlock()
	if q != nil {
		return q
	}
	created, err := streamCreated(ctx, s.js, s.binding.Bucket)
	switch {
	case errors.Is(err, ErrStreamMissing):
		return s.quarantined(err)
	case err != nil:
		return err
	case !created.Equal(s.binding.Created):
		return s.quarantined(fmt.Errorf("%w: %s created %s, bound %s", ErrStreamReplaced,
			s.binding.Bucket, created.Format(time.RFC3339Nano), s.binding.Created.Format(time.RFC3339Nano)))
	}
	// A concurrent check may have latched a replacement while this answer was
	// in flight; the latch wins over an older success.
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quarantine
}

func (s *Stream) quarantined(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quarantine == nil {
		s.quarantine = err
	}
	return s.quarantine
}

// streamCreated reads the stream's creation time. A leaderless group member
// may also answer, so an answer naming no current leader is refused.
func streamCreated(ctx context.Context, js jetstream.JetStream, bucket string) (time.Time, error) {
	st, err := js.Stream(ctx, streamName(bucket))
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return time.Time{}, fmt.Errorf("%w: %s", ErrStreamMissing, bucket)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s: %w", ErrStreamUnavailable, bucket, err)
	}
	info := st.CachedInfo()
	if info == nil || info.Created.IsZero() {
		return time.Time{}, fmt.Errorf("%w: %s reported no creation time", ErrStreamUnavailable, bucket)
	}
	if info.Cluster == nil || info.Cluster.Leader == "" {
		return time.Time{}, fmt.Errorf("%w: %s reported no stream leader", ErrStreamUnavailable, bucket)
	}
	return info.Created, nil
}
