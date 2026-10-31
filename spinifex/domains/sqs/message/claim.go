package message

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"
)

// StateVersion is the schema version of the state records a claim writes.
const StateVersion = 1

// MaxVisibilityTimeout is the longest visibility a receive may set, 12 hours.
const MaxVisibilityTimeout = 12 * time.Hour

// MetaRc is the receive count a DLQ move carries in (Sqs-Rc); absent otherwise.
const MetaRc = "Rc"

// State record values of s.
const (
	StatusInflight = "inflight"
	StatusMoving   = "moving"
	StatusDeleted  = "deleted"
	StatusExpired  = "expired"
	StatusMoved    = "moved"
)

var (
	// ErrNotVisible reports a message that is delayed, in flight or being
	// moved: it exists but cannot be received now.
	ErrNotVisible = errors.New("message: not visible")
	// ErrGone reports a message that is deleted, expired, moved or past
	// retention, or whose body is no longer stored.
	ErrGone = errors.New("message: gone")
	// ErrClaimLost reports that another receiver's claim won the race.
	ErrClaimLost = errors.New("message: claim lost to another receiver")
	// ErrLeaseExpired reports a claim acknowledged only after its visibility
	// deadline: it counted as a receive, but the message is visible again.
	ErrLeaseExpired = errors.New("message: claim acknowledged after its deadline")
	// ErrNotOurs reports a record or body of a newer schema, or a state this
	// release does not know; it is never received, swept or expired here.
	ErrNotOurs = errors.New("message: record is not this release's to act on")
)

// Claimer performs the receive claim (lifecycle contract M3) on one stored
// message. It uses only the store interface.
type Claimer struct {
	Store Store
	// Now defaults to the wall clock.
	Now func() time.Time
}

// ClaimInput names one stored message and the receive's parameters.
type ClaimInput struct {
	Namespace Namespace
	Position  Position
	// Visibility is the receive's visibility timeout, 0 to 12 hours.
	Visibility time.Duration
	// Retention is the queue's MessageRetentionPeriod, counted from the
	// original enqueue time.
	Retention time.Duration
}

// Claim is a message taken in flight by an acknowledged claim. Generation is
// the receipt generation the receipt handle must be issued for.
type Claim struct {
	// Message.Body is the message body alone, without stored attributes.
	Message      Message
	Attributes   Attributes
	MessageID    string
	ReceiveCount uint64
	Generation   uint64
	Deadline     time.Time
	ReceivedAt   time.Time
	FirstReceive time.Time
	Revision     Revision
}

// Claim takes one message in flight. It answers only after the store
// acknowledges the conditional write; a write whose outcome is unknown is
// ErrUnavailable and is never resent, so it can count as a receive that
// returned nothing, and the message returns at its deadline.
func (c *Claimer) Claim(ctx context.Context, in ClaimInput) (Claim, error) {
	if c == nil || c.Store == nil {
		return Claim{}, fmt.Errorf("%w: claimer is not configured", ErrUnavailable)
	}
	if err := in.Namespace.Validate(); err != nil {
		return Claim{}, err
	}
	if in.Position == 0 {
		return Claim{}, fmt.Errorf("%w: position 0", ErrInvalidArgument)
	}
	if in.Visibility < 0 || in.Visibility > MaxVisibilityTimeout || in.Visibility%time.Second != 0 {
		return Claim{}, fmt.Errorf("%w: visibility timeout must be whole seconds from 0 to 43200", ErrInvalidParameter)
	}
	if in.Retention <= 0 {
		return Claim{}, fmt.Errorf("%w: queue retention %s is not positive", ErrUnavailable, in.Retention)
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}

	msg, err := c.Store.Get(ctx, in.Namespace, in.Position)
	if errors.Is(err, ErrNotFound) {
		return Claim{}, ErrGone
	}
	if err != nil {
		return Claim{}, fmt.Errorf("%w: read message: %w", ErrUnavailable, err)
	}
	h, err := readHeaders(msg.Meta)
	if err != nil {
		return Claim{}, err
	}
	// The stored payload is split before any write, so a corrupt one is
	// never taken in flight.
	var attrs Attributes
	if h.v > MetaVersionPlain {
		if msg.Body, attrs, err = decodePayload(msg.Body, h.md5a); err != nil {
			return Claim{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
	}
	cur, stateErr := c.Store.GetState(ctx, in.Namespace, in.Position)
	// The claim's time is taken after both reads, so a slow read cannot spend
	// the visibility the record is about to grant.
	t := now().UnixMilli()
	if t >= h.enq+in.Retention.Milliseconds() {
		return Claim{}, ErrGone
	}
	var rec stateRecord
	switch {
	case errors.Is(stateErr, ErrNotFound):
		if t < h.vis {
			return Claim{}, ErrNotVisible
		}
		rec = stateRecord{c: h.rc}
		cur = State{}
	case stateErr != nil:
		return Claim{}, fmt.Errorf("%w: read state: %w", ErrUnavailable, stateErr)
	default:
		if rec, err = decodeState(cur.Value); err != nil {
			return Claim{}, err
		}
		switch {
		case rec.s == StatusInflight && rec.d > t, rec.s == StatusMoving:
			return Claim{}, ErrNotVisible
		case rec.s != StatusInflight:
			return Claim{}, ErrGone
		}
	}
	if rec.c == math.MaxUint64 || rec.g == math.MaxUint64 {
		return Claim{}, fmt.Errorf("%w: receive count or generation exhausted", ErrUnavailable)
	}

	rec.v = StateVersion
	rec.s = StatusInflight
	rec.c++
	rec.g++
	rec.d = t + in.Visibility.Milliseconds()
	rec.r = t
	if rec.f == 0 {
		rec.f = t
	}
	value, err := rec.encode()
	if err != nil {
		return Claim{}, fmt.Errorf("%w: encode state: %w", ErrUnavailable, err)
	}
	rev, err := c.Store.PutState(ctx, in.Namespace, in.Position, value, cur.Revision)
	if errors.Is(err, ErrRevisionMismatch) {
		return Claim{}, ErrClaimLost
	}
	if err != nil {
		return Claim{}, fmt.Errorf("%w: claim outcome unknown: %w", ErrUnavailable, err)
	}
	if err := c.recheckBody(ctx, in, rev); err != nil {
		return Claim{}, err
	}
	if in.Visibility > 0 && now().UnixMilli() >= rec.d {
		return Claim{}, ErrLeaseExpired
	}
	return Claim{
		Message: msg, Attributes: attrs, MessageID: h.id, ReceiveCount: rec.c, Generation: rec.g,
		Deadline: time.UnixMilli(rec.d).UTC(), ReceivedAt: time.UnixMilli(rec.r).UTC(), FirstReceive: time.UnixMilli(rec.f).UTC(),
		Revision: rev,
	}, nil
}

// recheckBody confirms the body still exists after every acknowledged claim:
// purge and removal delete bodies without first moving the state record, so
// a raced deletion is seen here and the claim just written is withdrawn.
func (c *Claimer) recheckBody(ctx context.Context, in ClaimInput, rev Revision) error {
	_, err := c.Store.Get(ctx, in.Namespace, in.Position)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound):
		if derr := c.Store.DeleteState(ctx, in.Namespace, in.Position, rev); derr != nil {
			return fmt.Errorf("%w: withdraw claim on removed message: %w", ErrGone, derr)
		}
		return ErrGone
	default:
		return fmt.Errorf("%w: confirm message after claim: %w", ErrUnavailable, err)
	}
}

var md5Hex = regexp.MustCompile(`^[0-9a-f]{32}$`)

type headers struct {
	v        int
	id       string
	enq, vis int64
	rc       uint64
	md5a     string
}

// readHeaders validates the body metadata a claim depends on. A newer schema
// is not this release's; a malformed known field fails closed.
func readHeaders(meta map[string]string) (headers, error) {
	v, err := strconv.Atoi(meta[MetaV])
	switch {
	case err != nil || v < 1:
		return headers{}, fmt.Errorf("%w: message schema version %q", ErrUnavailable, meta[MetaV])
	case v > MetaVersion:
		return headers{}, ErrNotOurs
	}
	h := headers{v: v, id: meta[MetaID]}
	if h.id == "" {
		return headers{}, fmt.Errorf("%w: message has no ID", ErrUnavailable)
	}
	if h.enq, err = strconv.ParseInt(meta[MetaEnq], 10, 64); err != nil || h.enq <= 0 {
		return headers{}, fmt.Errorf("%w: message enqueue time %q", ErrUnavailable, meta[MetaEnq])
	}
	if h.vis, err = strconv.ParseInt(meta[MetaVis], 10, 64); err != nil || h.vis < h.enq {
		return headers{}, fmt.Errorf("%w: message delay-until %q", ErrUnavailable, meta[MetaVis])
	}
	if meta[MetaSender] == "" {
		return headers{}, fmt.Errorf("%w: message has no sender", ErrUnavailable)
	}
	if !md5Hex.MatchString(meta[MetaMD5]) {
		return headers{}, fmt.Errorf("%w: message body MD5 %q", ErrUnavailable, meta[MetaMD5])
	}
	md5a, hasA := meta[MetaMD5A]
	switch {
	case v > MetaVersionPlain && !md5Hex.MatchString(md5a):
		return headers{}, fmt.Errorf("%w: message attributes MD5 %q", ErrUnavailable, md5a)
	case v == MetaVersionPlain && hasA:
		return headers{}, fmt.Errorf("%w: schema %d message carries an attributes MD5", ErrUnavailable, v)
	}
	h.md5a = md5a
	if raw, ok := meta[MetaRc]; ok {
		if h.rc, err = strconv.ParseUint(raw, 10, 64); err != nil {
			return headers{}, fmt.Errorf("%w: carried receive count %q", ErrUnavailable, raw)
		}
	}
	return h, nil
}

// stateRecord is the lifecycle contract's state record. raw keeps every
// field, so a rewrite preserves fields a newer release added.
type stateRecord struct {
	v       int
	s       string
	c, g    uint64
	d, r, f int64
	raw     map[string]json.RawMessage
}

var stateKnown = map[string]bool{"v": true, "s": true, "c": true, "g": true, "d": true, "r": true, "f": true}

// decodeState reads a stored state record. Undecodable or inconsistent
// records fail closed; a newer version or unknown s is not ours.
func decodeState(b []byte) (stateRecord, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || raw == nil {
		return stateRecord{}, fmt.Errorf("%w: state record is not a JSON object", ErrUnavailable)
	}
	var known struct {
		V *int    `json:"v"`
		S *string `json:"s"`
		C uint64  `json:"c"`
		G uint64  `json:"g"`
		D int64   `json:"d"`
		R int64   `json:"r"`
		F int64   `json:"f"`
	}
	if err := json.Unmarshal(b, &known); err != nil {
		return stateRecord{}, fmt.Errorf("%w: state record field: %w", ErrUnavailable, err)
	}
	switch {
	case known.V == nil || *known.V < 1:
		return stateRecord{}, fmt.Errorf("%w: state record has no valid version", ErrUnavailable)
	case *known.V > StateVersion:
		return stateRecord{}, ErrNotOurs
	case known.S == nil:
		return stateRecord{}, fmt.Errorf("%w: state record has no state", ErrUnavailable)
	}
	rec := stateRecord{v: *known.V, s: *known.S, c: known.C, g: known.G, d: known.D, r: known.R, f: known.F, raw: raw}
	switch rec.s {
	case StatusInflight:
		if rec.c == 0 || rec.g == 0 || rec.d <= 0 || rec.r <= 0 || rec.f <= 0 || rec.f > rec.r {
			return stateRecord{}, fmt.Errorf("%w: in-flight state record is incomplete", ErrUnavailable)
		}
	case StatusMoving, StatusDeleted, StatusExpired, StatusMoved:
	default:
		return stateRecord{}, ErrNotOurs
	}
	return rec, nil
}

// encode writes the known fields over the preserved raw record.
func (r stateRecord) encode() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(r.raw)+len(stateKnown))
	for k, v := range r.raw {
		if !stateKnown[k] {
			out[k] = v
		}
	}
	set := func(k string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		out[k] = b
		return nil
	}
	for _, f := range []struct {
		k string
		v any
	}{{"v", r.v}, {"s", r.s}, {"c", r.c}, {"g", r.g}, {"d", r.d}, {"r", r.r}, {"f", r.f}} {
		if err := set(f.k, f.v); err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
