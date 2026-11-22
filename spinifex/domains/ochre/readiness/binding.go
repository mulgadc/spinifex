package readiness

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

var (
	// ErrStreamMissing is a bound stream, or its marker, that no longer exists.
	ErrStreamMissing = errors.New("bound readiness stream is missing")
	// ErrStreamReplaced is a stream whose incarnation differs from its binding.
	// Its bytes and revisions are never authoritative.
	ErrStreamReplaced = errors.New("bound readiness stream was replaced")
	// ErrStreamUnavailable is a stream whose identity cannot be read now.
	ErrStreamUnavailable = errors.New("bound readiness stream is unavailable")
)

// Binding is independently recorded evidence of one bucket's stream
// incarnation: its creation time and the stream position of a marker key.
type Binding struct {
	Bucket  string    `json:"bucket"`
	Created time.Time `json:"created"`
	Marker  Marker    `json:"marker"`
}

// Marker is the last message on a never-rewritten key, as the stream leader
// reported it. Comparing Seq and Time detects a rewrite or a recreated stream;
// a restore or replay that preserves both is left to recovery.
type Marker struct {
	Key  string    `json:"key"`
	Seq  uint64    `json:"seq"`
	Time time.Time `json:"time"`
}

func (m Marker) complete() bool { return m.Key != "" && m.Seq != 0 && !m.Time.IsZero() }

func streamName(bucket string) string { return "KV_" + bucket }

// Observe reads the binding of an existing bucket's stream and marker without
// creating either. Deciding that the observation may be recorded is the
// caller's job.
func Observe(ctx context.Context, js jetstream.JetStream, bucket, markerKey string) (Binding, error) {
	created, err := streamCreated(ctx, js, bucket)
	if err != nil {
		return Binding{}, err
	}
	marker, err := readMarker(ctx, js, bucket, markerKey)
	if err != nil {
		return Binding{}, err
	}
	return Binding{Bucket: bucket, Created: created, Marker: marker}, nil
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
	if b.Bucket == "" || b.Created.IsZero() || !b.Marker.complete() {
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

// Verify checks the stream's reported identity and durability against the
// binding. Call it before interpreting a read or sending a write and after every
// acknowledgement; it is not atomic against a later write or config change.
func (s *Stream) Verify(ctx context.Context) error {
	s.mu.Lock()
	q := s.quarantine
	s.mu.Unlock()
	if q != nil {
		return q
	}
	b := s.binding
	info, err := streamInfo(ctx, s.js, b.Bucket)
	if err == nil && !info.Created.Equal(b.Created) {
		err = fmt.Errorf("%w: %s created %s, bound %s", ErrStreamReplaced,
			b.Bucket, info.Created.Format(time.RFC3339Nano), b.Created.Format(time.RFC3339Nano))
	}
	if err == nil {
		err = checkDurable(b.Bucket, info.Config)
	}
	if err == nil {
		var m Marker
		if m, err = readMarker(ctx, s.js, b.Bucket, b.Marker.Key); err == nil && (m.Seq != b.Marker.Seq || !m.Time.Equal(b.Marker.Time)) {
			err = fmt.Errorf("%w: %s marker %s at seq %d %s, bound seq %d %s", ErrStreamReplaced, b.Bucket, m.Key,
				m.Seq, m.Time.Format(time.RFC3339Nano), b.Marker.Seq, b.Marker.Time.Format(time.RFC3339Nano))
		}
	}
	switch {
	case errors.Is(err, ErrStreamMissing), errors.Is(err, ErrStreamReplaced), errors.Is(err, ErrNotDurable):
		return s.quarantined(err)
	case err != nil:
		return err
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

// streamCreated reads the stream's creation time.
func streamCreated(ctx context.Context, js jetstream.JetStream, bucket string) (time.Time, error) {
	info, err := streamInfo(ctx, js, bucket)
	if err != nil {
		return time.Time{}, err
	}
	return info.Created, nil
}

// streamInfo reads the stream's information. A leaderless group member may
// also answer, so an answer naming no current leader is refused.
func streamInfo(ctx context.Context, js jetstream.JetStream, bucket string) (*jetstream.StreamInfo, error) {
	st, err := js.Stream(ctx, streamName(bucket))
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrStreamMissing, bucket)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrStreamUnavailable, bucket, err)
	}
	info := st.CachedInfo()
	if info == nil || info.Created.IsZero() {
		return nil, fmt.Errorf("%w: %s reported no creation time", ErrStreamUnavailable, bucket)
	}
	if info.Cluster == nil || info.Cluster.Leader == "" {
		return nil, fmt.Errorf("%w: %s reported no stream leader", ErrStreamUnavailable, bucket)
	}
	return info, nil
}

// checkDurable refuses c on the shared kvstore durability terms. An unknown
// cluster size leaves durability unknown, which is unavailable, not refused.
func checkDurable(bucket string, c jetstream.StreamConfig) error {
	replicas, err := clustersize.Replicas()
	if err != nil {
		return fmt.Errorf("%w: %s replica count: %w", ErrStreamUnavailable, bucket, err)
	}
	if err := kvstore.CheckDurable(c, replicas); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNotDurable, bucket, err)
	}
	return nil
}

// readMarker reads the last message on key through a get only the stream
// leader answers. The message may be a value or a delete marker; only its
// position matters.
func readMarker(ctx context.Context, js jetstream.JetStream, bucket, key string) (Marker, error) {
	if key == "" {
		return Marker{}, fmt.Errorf("%w: %s has no marker key", ErrInvalid, bucket)
	}
	msg, err := kvstore.LeaderMsgGet(ctx, js, streamName(bucket), kvstore.MsgGetRequest{LastFor: "$KV." + bucket + "." + key})
	switch {
	case errors.Is(err, jetstream.ErrMsgNotFound), errors.Is(err, jetstream.ErrStreamNotFound):
		return Marker{}, fmt.Errorf("%w: %s marker %s", ErrStreamMissing, bucket, key)
	case err != nil:
		return Marker{}, fmt.Errorf("%w: %s marker %s: %w", ErrStreamUnavailable, bucket, key, err)
	case msg.Sequence == 0 || msg.Time.IsZero():
		return Marker{}, fmt.Errorf("%w: %s marker %s reported no position", ErrStreamUnavailable, bucket, key)
	}
	return Marker{Key: key, Seq: msg.Sequence, Time: msg.Time}, nil
}
