package readiness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// AnchorKey is the cluster-config key holding the stream bindings. It is
	// additive: older readers address their own keys and never list the bucket.
	AnchorKey = "ochre-readiness-binding"
	// AnchorVersion is the only anchor schema this build reads.
	AnchorVersion = 1
	// MarkerPrefix is reserved in every bound bucket for marker keys.
	MarkerPrefix = "_readiness.marker."
)

var (
	// ErrBindingPending is an initialization whose outcome is not yet proved;
	// it blocks activation and is never retried or adopted automatically.
	ErrBindingPending = errors.New("readiness binding initialization is pending")
	// ErrPriorBinding is evidence, or ambiguity, that a binding existed before.
	ErrPriorBinding = errors.New("readiness binding may already have existed")
	// ErrNotFirstRollout is an initialization without explicit first-rollout evidence.
	ErrNotFirstRollout = errors.New("readiness binding initialization requires explicit first-rollout evidence")
)

// AnchorPhase is where an initialization stands.
type AnchorPhase string

const (
	AnchorPending AnchorPhase = "pending"
	AnchorBound   AnchorPhase = "bound"
)

// StreamIntent pins one stream's observed creation and its chosen marker key
// before any marker is written.
type StreamIntent struct {
	Bucket    string    `json:"bucket"`
	Created   time.Time `json:"created"`
	MarkerKey string    `json:"marker_key"`
}

// Anchor is the versioned cluster-config record of one initialization. Streams
// is the endpoint stream then the control stream; Bound is set only when bound.
type Anchor struct {
	Version int            `json:"version"`
	Phase   AnchorPhase    `json:"phase"`
	Intent  string         `json:"intent"`
	Streams []StreamIntent `json:"streams"`
	Bound   []Binding      `json:"bound,omitempty"`
}

// InitConfig names the buckets an initializer works on. The anchor lives in
// ConfigBucket, which must be neither bound stream.
type InitConfig struct {
	ConfigBucket   string
	EndpointBucket string
}

// FirstRollout is the caller's assertion that no earlier binding existed. It
// authenticates nothing; the initializer still refuses when evidence disagrees.
type FirstRollout struct {
	Confirmed bool
}

// Initializer writes and reads the anchor. Every write is a single attempt.
type Initializer struct {
	js   jetstream.JetStream
	cfg  InitConfig
	hook func(step string)
}

// NewInitializer returns an initializer over js.
func NewInitializer(js jetstream.JetStream, cfg InitConfig) *Initializer {
	return &Initializer{js: js, cfg: cfg}
}

// Bound is a verified binding with both streams attached.
type Bound struct {
	Anchor      Anchor
	Endpoints   *Stream
	EndpointsKV jetstream.KeyValue
	Control     *Stream
	ControlKV   jetstream.KeyValue
}

// Initialize binds both streams for the first time. It returns the bound
// anchor, or an error that leaves any pending intent in place to block
// activation; an unknown write outcome is never retried.
func (in *Initializer) Initialize(ctx context.Context, ev FirstRollout) (Anchor, error) {
	if err := in.cfg.validate(); err != nil {
		return Anchor{}, err
	}
	if !ev.Confirmed {
		return Anchor{}, ErrNotFirstRollout
	}
	switch a, _, err := in.readAnchor(ctx); {
	case errors.Is(err, ErrAbsent):
	case err != nil:
		return Anchor{}, err
	case a.Phase == AnchorPending:
		return Anchor{}, fmt.Errorf("%w: intent %s", ErrBindingPending, a.Intent)
	default:
		return Anchor{}, fmt.Errorf("%w: already bound by intent %s", ErrWrongState, a.Intent)
	}
	if err := in.noPriorBinding(ctx); err != nil {
		return Anchor{}, err
	}
	in.step("evidence")

	if _, err := kvstore.GetOrCreateBucket(ctx, in.js, KVBucket, 1); err != nil {
		return Anchor{}, fmt.Errorf("%w: open control bucket: %w", ErrStreamUnavailable, err)
	}
	if err := in.controlUnused(ctx); err != nil {
		return Anchor{}, err
	}
	pending := Anchor{Version: AnchorVersion, Phase: AnchorPending}
	var err error
	if pending.Intent, err = randomHex(); err != nil {
		return Anchor{}, err
	}
	for _, bucket := range in.buckets() {
		created, err := streamCreated(ctx, in.js, bucket)
		if err != nil {
			return Anchor{}, err
		}
		suffix, err := randomHex()
		if err != nil {
			return Anchor{}, err
		}
		pending.Streams = append(pending.Streams, StreamIntent{Bucket: bucket, Created: created, MarkerKey: MarkerPrefix + suffix})
	}

	in.step("pending")
	pendingRev, err := in.writeAnchor(ctx, pending, 0)
	if errors.Is(err, ErrChanged) {
		return Anchor{}, fmt.Errorf("%w: another initializer wrote the anchor: %w", ErrContended, err)
	}
	if err != nil {
		return Anchor{}, err
	}

	seqs := make([]uint64, len(pending.Streams))
	for i, si := range pending.Streams {
		in.step("publish " + si.Bucket)
		if seqs[i], err = in.publishMarker(ctx, si); err != nil {
			return pending, err
		}
	}

	// Every marker write has definitively finished; re-observe before binding
	// so a successful write on a replacement is never taken as the original.
	in.step("verify")
	bound := pending
	bound.Phase = AnchorBound
	for i, si := range pending.Streams {
		created, err := streamCreated(ctx, in.js, si.Bucket)
		if err != nil {
			return pending, err
		}
		if !created.Equal(si.Created) {
			return pending, fmt.Errorf("%w: %s created %s, pending intent %s", ErrStreamReplaced, si.Bucket,
				created.Format(time.RFC3339Nano), si.Created.Format(time.RFC3339Nano))
		}
		m, err := readMarker(ctx, in.js, si.Bucket, si.MarkerKey)
		if err != nil {
			return pending, err
		}
		if m.Seq != seqs[i] {
			return pending, fmt.Errorf("%w: %s marker at seq %d, acknowledged %d", ErrStreamReplaced, si.Bucket, m.Seq, seqs[i])
		}
		bound.Bound = append(bound.Bound, Binding{Bucket: si.Bucket, Created: created, Marker: m})
	}

	in.step("bind")
	if _, err := in.writeAnchor(ctx, bound, pendingRev); err != nil {
		return pending, err
	}
	return bound, nil
}

// Load attaches both streams under a bound anchor. A pending anchor blocks;
// a bound one is adopted only after both bindings verify now.
func (in *Initializer) Load(ctx context.Context) (*Bound, error) {
	if err := in.cfg.validate(); err != nil {
		return nil, err
	}
	a, _, err := in.readAnchor(ctx)
	if err != nil {
		return nil, err
	}
	if a.Phase == AnchorPending {
		return nil, fmt.Errorf("%w: intent %s", ErrBindingPending, a.Intent)
	}
	b := &Bound{Anchor: a}
	if b.Endpoints, b.EndpointsKV, err = Attach(ctx, in.js, a.Bound[0]); err != nil {
		return nil, err
	}
	if b.Control, b.ControlKV, err = Attach(ctx, in.js, a.Bound[1]); err != nil {
		return nil, err
	}
	return b, nil
}

func (c InitConfig) validate() error {
	if c.ConfigBucket == "" || c.EndpointBucket == "" || c.EndpointBucket == KVBucket ||
		c.ConfigBucket == c.EndpointBucket || c.ConfigBucket == KVBucket {
		return fmt.Errorf("%w: init config %+v", ErrInvalid, c)
	}
	return nil
}

func (in *Initializer) buckets() []string { return []string{in.cfg.EndpointBucket, KVBucket} }

func (in *Initializer) step(name string) {
	if in.hook != nil {
		in.hook(name)
	}
}

// noPriorBinding refuses when a surviving control stream or endpoint marker
// shows an earlier initialization, and blocks when either cannot be read.
func (in *Initializer) noPriorBinding(ctx context.Context) error {
	_, err := in.js.Stream(ctx, streamName(KVBucket))
	switch {
	case err == nil:
		return fmt.Errorf("%w: control stream %s exists", ErrPriorBinding, streamName(KVBucket))
	case !errors.Is(err, jetstream.ErrStreamNotFound):
		return fmt.Errorf("%w: control stream: %w", ErrStreamUnavailable, err)
	}
	bucket := in.cfg.EndpointBucket
	if _, err := streamCreated(ctx, in.js, bucket); err != nil {
		return err
	}
	msg, err := kvstore.LeaderMsgGet(ctx, in.js, streamName(bucket), kvstore.MsgGetRequest{LastFor: "$KV." + bucket + "." + MarkerPrefix + ">"})
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s holds marker %s", ErrPriorBinding, bucket, msg.Subject)
	case !errors.Is(err, jetstream.ErrMsgNotFound):
		return fmt.Errorf("%w: %s marker discovery: %w", ErrStreamUnavailable, bucket, err)
	}
	return nil
}

// controlUnused refuses a control stream this attempt cannot own: one that
// already held a message or was configured differently. An empty stream is
// owned only when this attempt's marker lands as its first message.
func (in *Initializer) controlUnused(ctx context.Context) error {
	st, err := in.js.Stream(ctx, streamName(KVBucket))
	if err != nil {
		return fmt.Errorf("%w: control stream: %w", ErrStreamUnavailable, err)
	}
	info := st.CachedInfo()
	c := info.Config
	if info.State.LastSeq != 0 || info.State.Msgs != 0 {
		return fmt.Errorf("%w: control stream %s already at seq %d", ErrPriorBinding, c.Name, info.State.LastSeq)
	}
	if c.MaxMsgsPerSubject != 1 || c.MaxAge != 0 || c.Mirror != nil || len(c.Sources) != 0 || c.Sealed {
		return fmt.Errorf("%w: control stream %s has a foreign config", ErrPriorBinding, c.Name)
	}
	return nil
}

// readAnchor reads the anchor from the cluster-config stream leader. A delete
// or purge marker in its place is evidence of an earlier anchor.
func (in *Initializer) readAnchor(ctx context.Context) (Anchor, uint64, error) {
	bucket := in.cfg.ConfigBucket
	msg, err := kvstore.LeaderMsgGet(ctx, in.js, streamName(bucket), kvstore.MsgGetRequest{LastFor: "$KV." + bucket + "." + AnchorKey})
	switch {
	case errors.Is(err, jetstream.ErrMsgNotFound):
		return Anchor{}, 0, ErrAbsent
	case err != nil:
		return Anchor{}, 0, fmt.Errorf("%w: read binding anchor: %w", ErrStreamUnavailable, err)
	}
	if op := msg.Header.Get("KV-Operation"); op == "DEL" || op == "PURGE" {
		return Anchor{}, msg.Sequence, fmt.Errorf("%w: anchor removed at revision %d", ErrPriorBinding, msg.Sequence)
	}
	a, err := in.decodeAnchor(msg.Data)
	if err != nil {
		return Anchor{}, msg.Sequence, fmt.Errorf("anchor revision %d: %w", msg.Sequence, err)
	}
	return a, msg.Sequence, nil
}

// decodeAnchor accepts only AnchorVersion with known fields and a shape that
// matches this initializer's buckets.
func (in *Initializer) decodeAnchor(data []byte) (Anchor, error) {
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return Anchor{}, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	if head.Version != AnchorVersion {
		return Anchor{}, fmt.Errorf("%w: anchor version %d", ErrUnsupported, head.Version)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var a Anchor
	if err := dec.Decode(&a); err != nil {
		return Anchor{}, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	want := in.buckets()
	bad := !isHexID(a.Intent) || len(a.Streams) != len(want)
	for i := 0; !bad && i < len(want); i++ {
		si := a.Streams[i]
		suffix, ok := strings.CutPrefix(si.MarkerKey, MarkerPrefix)
		bad = si.Bucket != want[i] || si.Created.IsZero() || !ok || !isHexID(suffix)
	}
	switch {
	case bad:
	case a.Phase == AnchorPending:
		bad = len(a.Bound) != 0
	case a.Phase == AnchorBound:
		bad = len(a.Bound) != len(want)
		for i := 0; !bad && i < len(want); i++ {
			b, si := a.Bound[i], a.Streams[i]
			bad = b.Bucket != si.Bucket || !b.Created.Equal(si.Created) || b.Marker.Key != si.MarkerKey || !b.Marker.complete()
		}
	default:
		bad = true
	}
	if bad {
		return Anchor{}, fmt.Errorf("%w: anchor shape", ErrUnsupported)
	}
	return a, nil
}

// writeAnchor publishes a at expected, the anchor subject's last sequence.
func (in *Initializer) writeAnchor(ctx context.Context, a Anchor, expected uint64) (uint64, error) {
	data, err := json.Marshal(a)
	if err != nil {
		return 0, fmt.Errorf("encode binding anchor: %w", err)
	}
	bucket := in.cfg.ConfigBucket
	msg := &nats.Msg{Subject: "$KV." + bucket + "." + AnchorKey, Data: data}
	return in.publishOnce(ctx, msg, bucket, expected, "anchor "+string(a.Phase))
}

// publishMarker writes si's marker as a delete marker that only lands on a
// subject with no earlier message. The control marker must also be its stream's
// first message, which is what makes the control stream this attempt's own.
func (in *Initializer) publishMarker(ctx context.Context, si StreamIntent) (uint64, error) {
	msg := &nats.Msg{Subject: "$KV." + si.Bucket + "." + si.MarkerKey, Header: nats.Header{}}
	msg.Header.Set("KV-Operation", "DEL")
	var extra []jetstream.PublishOpt
	if si.Bucket == KVBucket {
		extra = append(extra, jetstream.WithExpectLastSequence(0))
	}
	return in.publishOnce(ctx, msg, si.Bucket, 0, "marker "+si.Bucket, extra...)
}

// publishOnce sends msg once to bucket's stream, conditional on the subject's
// last sequence. A refused condition is ErrChanged; anything else that is not
// an acknowledgement is ErrOutcomeUnknown.
func (in *Initializer) publishOnce(ctx context.Context, msg *nats.Msg, bucket string, expected uint64, what string, extra ...jetstream.PublishOpt) (uint64, error) {
	opts := append([]jetstream.PublishOpt{
		jetstream.WithExpectStream(streamName(bucket)),
		jetstream.WithExpectLastSequencePerSubject(expected),
		jetstream.WithRetryAttempts(0)}, extra...)
	ack, err := in.js.PublishMsg(ctx, msg, opts...)
	switch {
	case err == nil && ack != nil && ack.Stream == streamName(bucket):
		return ack.Sequence, nil
	case err == nil:
		return 0, fmt.Errorf("%w: %s acknowledged by an unexpected stream", ErrOutcomeUnknown, what)
	case kvstore.IsRevisionConflict(err):
		return 0, fmt.Errorf("%w: %s at subject sequence %d: %w", ErrChanged, what, expected, err)
	default:
		return 0, fmt.Errorf("%w: %s: %w", ErrOutcomeUnknown, what, err)
	}
}

// isHexID reports whether s has exactly randomHex's form.
func isHexID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func randomHex() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("random identifier: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
