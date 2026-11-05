// Package execution owns Lambda attempt grants: who may run an attempt and
// report its result. This is a provisional single-node owner; it is not the
// ADR-0009 M1 mechanism, which is still open.
package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// KVBucket holds one record per node: its grant sequence and current attempt.
const KVBucket = "lambda-execution"

const formatVersion = 1

// Attempt states. The attempt lives in its node's record, and every change is
// a conditional write on that one key, so a start, a withdrawal and the next
// grant can never all succeed.
const (
	// StateGranted: recorded; nothing may have started.
	StateGranted = "granted"
	// StateRunning: the daemon may have launched the attempt.
	StateRunning = "running"
	// StateAccepted: a result was accepted under the grant; stop not yet proven.
	StateAccepted = "accepted"
	// StateWithdrawn: the attempt never started, and now never can.
	StateWithdrawn = "withdrawn"
	// StateReleased: the VM stop is proven and its resources released, but the
	// daemon's own record of it is not yet gone; the node stays held.
	StateReleased = "released"
	// StateEnded: the stop is proven and nothing of the attempt remains.
	StateEnded = "ended"
	// StateAmbiguous: the end is unproven; the attempt keeps the node's slot.
	StateAmbiguous = "ambiguous"
)

var (
	// ErrNodeBusy: the node holds an attempt that has not ended.
	ErrNodeBusy = errors.New("execution: node holds an unended attempt")
	// ErrNotCurrent: the grant is not the node's current grant.
	ErrNotCurrent = errors.New("execution: grant is not current")
	// ErrWrongState: the attempt is not in a state that allows the change.
	ErrWrongState = errors.New("execution: attempt is in the wrong state")
	// ErrExpired: the grant's launch deadline has passed.
	ErrExpired = errors.New("execution: grant expired before launch")
	// ErrUnknownRecord: a record this owner cannot read or did not write.
	ErrUnknownRecord = errors.New("execution: unrecognised record")
	// ErrMismatch: the request or code differs from what the grant authorises.
	ErrMismatch = errors.New("execution: request does not match the grant")
	// ErrOutcomeUnknown: a write's outcome is unknown and may have landed.
	ErrOutcomeUnknown = errors.New("execution: write outcome unknown")
)

var segment = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// Grant is the identity that authorises one attempt on one environment
// generation. Seq orders grants on a node and never changes for the attempt.
type Grant struct {
	GrantID       string `json:"grantId"`
	Seq           uint64 `json:"seq"`
	AttemptID     string `json:"attemptId"`
	EnvironmentID string `json:"environmentId"`
	Generation    uint64 `json:"generation"`
	Node          string `json:"node"`
}

// Identity is the exact request and code a grant authorises.
type Identity struct {
	RequestID  string `json:"requestId"`
	AccountID  string `json:"accountId"`
	Region     string `json:"region"`
	FunctionID string `json:"functionId"`
	RevisionID string `json:"revisionId"`
	CodeSHA256 string `json:"codeSha256"`
}

// Attempt is the owner's record of one attempt and its grant.
type Attempt struct {
	Grant     Grant     `json:"grant"`
	Identity  Identity  `json:"identity"`
	State     string    `json:"state"`
	LaunchBy  time.Time `json:"launchBy"`
	CreatedAt time.Time `json:"createdAt"`
	Launch    *Launch   `json:"launch,omitempty"`
}

// Launch identifies an attempt's VM process, its launcher and what it holds,
// so a restarted daemon can prove the stop. The pidfs inodes are comparable
// only within BootID on MachineID; they do not identify the host.
type Launch struct {
	MachineID     string `json:"machineId"`
	BootID        string `json:"bootId"`
	PIDNamespace  uint64 `json:"pidNamespace"`
	LauncherPID   int    `json:"launcherPid"`
	LauncherInode uint64 `json:"launcherInode"`
	PID           int    `json:"pid"`
	Inode         uint64 `json:"inode"`
	TapName       string `json:"tapName"`
	TapIndex      int    `json:"tapIndex"`
	TapOwner      string `json:"tapOwner"`
}

func (l Launch) valid() bool {
	return l.MachineID != "" && l.BootID != "" && l.PIDNamespace != 0 && l.LauncherPID > 0 &&
		l.LauncherInode != 0 && l.PID > 0 && l.Inode != 0
}

// nodeRecord is a node's one slot. Seq survives every attempt so each later
// grant orders after each earlier one.
type nodeRecord struct {
	FormatVersion int      `json:"formatVersion"`
	Seq           uint64   `json:"seq"`
	Attempt       *Attempt `json:"attempt,omitempty"`
}

// Store is the only writer of node records.
type Store struct {
	kv  jetstream.KeyValue
	now func() time.Time
	// beforeWrite runs between a read and its conditional write; tests use it.
	beforeWrite func()
}

// Open opens (or creates) the execution bucket.
func Open(ctx context.Context, js jetstream.KeyValueManager) (*Store, error) {
	kv, err := kvstore.GetOrCreateBucket(ctx, js, KVBucket, 1)
	if err != nil {
		return nil, fmt.Errorf("execution: open bucket: %w", err)
	}
	return &Store{kv: kv, now: time.Now}, nil
}

func nodeKey(node string) string { return "node." + node }

// Issue records a new attempt and its grant before anything is dispatched.
// It succeeds only if the node's current attempt ended, was withdrawn, or
// provably never started before its launch deadline.
func (s *Store) Issue(ctx context.Context, a Attempt) (Attempt, error) {
	g := a.Grant
	for _, seg := range []string{g.AttemptID, g.EnvironmentID, g.Node, a.Identity.FunctionID} {
		if !segment.MatchString(seg) {
			return Attempt{}, fmt.Errorf("execution: invalid identifier %q", seg)
		}
	}
	id := a.Identity
	if id.RequestID == "" || id.AccountID == "" || id.Region == "" || id.RevisionID == "" || id.CodeSHA256 == "" {
		return Attempt{}, errors.New("execution: request and code identity are required")
	}
	if g.Generation == 0 {
		return Attempt{}, errors.New("execution: environment generation is required")
	}
	grantID, err := randomID()
	if err != nil {
		return Attempt{}, err
	}
	a.Grant.GrantID, a.State, a.CreatedAt = grantID, StateGranted, s.now()

	rec, rev, err := s.read(ctx, g.Node)
	missing := errors.Is(err, jetstream.ErrKeyNotFound)
	if err != nil && !missing {
		return Attempt{}, err
	}
	if !missing && rec.Attempt != nil {
		if rec.Attempt.Grant.AttemptID == g.AttemptID {
			return Attempt{}, fmt.Errorf("%w: attempt %s already granted", ErrWrongState, g.AttemptID)
		}
		if !s.supersedable(*rec.Attempt) {
			return Attempt{}, ErrNodeBusy
		}
	}
	a.Grant.Seq = rec.Seq + 1
	next := nodeRecord{FormatVersion: formatVersion, Seq: a.Grant.Seq, Attempt: &a}
	if missing {
		err = s.create(ctx, g.Node, next)
	} else {
		err = s.write(ctx, g.Node, next, rev)
	}
	if errors.Is(err, ErrNotCurrent) {
		return Attempt{}, ErrNodeBusy
	}
	if err != nil {
		return Attempt{}, err
	}
	return a, nil
}

// supersedable reports whether a new grant may replace cur. A granted attempt
// past its deadline qualifies because Start refuses it then, and both writes
// are conditional on the same record revision.
func (s *Store) supersedable(cur Attempt) bool {
	switch cur.State {
	case StateEnded, StateWithdrawn:
		return true
	case StateGranted:
		return !s.now().Before(cur.LaunchBy)
	}
	return false
}

// Start moves a current, unexpired grant from granted to running, only for
// the exact request and code it authorises. The daemon calls it before
// allocating anything; a repeat, replay or mismatch is refused unchanged.
func (s *Store) Start(ctx context.Context, g Grant, want Identity) error {
	return s.transition(ctx, g, func(a *Attempt) error {
		if a.Identity != want {
			return ErrMismatch
		}
		if a.State != StateGranted {
			return fmt.Errorf("%w: %s", ErrWrongState, a.State)
		}
		if !s.now().Before(a.LaunchBy) {
			return ErrExpired
		}
		a.State = StateRunning
		return nil
	})
}

// Accept records that a result under g is accepted. Only one result is ever
// accepted, and none once the attempt has ended or been held as ambiguous.
func (s *Store) Accept(ctx context.Context, g Grant) error {
	return s.transition(ctx, g, func(a *Attempt) error {
		if a.State != StateRunning {
			return fmt.Errorf("%w: %s", ErrWrongState, a.State)
		}
		a.State = StateAccepted
		return nil
	})
}

// Launched records the running attempt's launch identity, once, before the
// daemon sends the guest anything.
func (s *Store) Launched(ctx context.Context, g Grant, l Launch) error {
	if !l.valid() {
		return errors.New("execution: incomplete launch identity")
	}
	return s.transition(ctx, g, func(a *Attempt) error {
		if a.State != StateRunning || a.Launch != nil {
			return fmt.Errorf("%w: %s", ErrWrongState, a.State)
		}
		a.Launch = &l
		return nil
	})
}

// Withdraw ends a grant that never started, for a caller that knows it sent
// nothing. It fails once the daemon has started the attempt.
func (s *Store) Withdraw(ctx context.Context, g Grant) error {
	return s.transition(ctx, g, func(a *Attempt) error {
		if a.State != StateGranted {
			return fmt.Errorf("%w: %s", ErrWrongState, a.State)
		}
		a.State = StateWithdrawn
		return nil
	})
}

// Released records the caller's proof that the attempt's VM has stopped and
// everything it held is released. The node stays held until Stopped.
func (s *Store) Released(ctx context.Context, g Grant) error {
	return s.transition(ctx, g, func(a *Attempt) error {
		switch a.State {
		case StateRunning, StateAccepted, StateAmbiguous:
			a.State = StateReleased
			return nil
		}
		return fmt.Errorf("%w: %s", ErrWrongState, a.State)
	})
}

// Stopped records that nothing of a released attempt remains, which frees the
// node for the next grant.
func (s *Store) Stopped(ctx context.Context, g Grant) error {
	return s.transition(ctx, g, func(a *Attempt) error {
		if a.State != StateReleased {
			return fmt.Errorf("%w: %s", ErrWrongState, a.State)
		}
		a.State = StateEnded
		return nil
	})
}

// Ambiguous records that the attempt's end is unproven. The node stays held
// until Stopped; no amount of time releases it.
func (s *Store) Ambiguous(ctx context.Context, g Grant) error {
	return s.transition(ctx, g, func(a *Attempt) error {
		switch a.State {
		case StateRunning, StateAccepted, StateAmbiguous:
			a.State = StateAmbiguous
			return nil
		}
		return fmt.Errorf("%w: %s", ErrWrongState, a.State)
	})
}

// Current returns the node's current attempt, read-only.
func (s *Store) Current(ctx context.Context, node string) (Attempt, error) {
	rec, _, err := s.read(ctx, node)
	if errors.Is(err, jetstream.ErrKeyNotFound) || err == nil && rec.Attempt == nil {
		return Attempt{}, ErrNotCurrent
	}
	if err != nil {
		return Attempt{}, err
	}
	return *rec.Attempt, nil
}

// transition applies change to the attempt under g, written only at the
// revision it was read, so a concurrent change refuses it.
func (s *Store) transition(ctx context.Context, g Grant, change func(*Attempt) error) error {
	rec, rev, err := s.read(ctx, g.Node)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return ErrNotCurrent
	}
	if err != nil {
		return err
	}
	if rec.Attempt == nil || rec.Attempt.Grant != g {
		return ErrNotCurrent
	}
	a := *rec.Attempt
	if err := change(&a); err != nil {
		return err
	}
	rec.Attempt = &a
	return s.write(ctx, g.Node, rec, rev)
}

func (s *Store) read(ctx context.Context, node string) (nodeRecord, uint64, error) {
	e, err := s.kv.Get(ctx, nodeKey(node))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nodeRecord{}, 0, err
	}
	if err != nil {
		return nodeRecord{}, 0, fmt.Errorf("execution: read node record: %w", err)
	}
	var rec nodeRecord
	if err := json.Unmarshal(e.Value(), &rec); err != nil || rec.FormatVersion != formatVersion || rec.Seq == 0 ||
		(rec.Attempt != nil && (rec.Attempt.Grant.Node != node || rec.Attempt.Grant.Seq != rec.Seq)) {
		return nodeRecord{}, 0, ErrUnknownRecord
	}
	return rec, e.Revision(), nil
}

func (s *Store) create(ctx context.Context, node string, rec nodeRecord) error {
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if s.beforeWrite != nil {
		s.beforeWrite()
	}
	if _, err := s.kv.Create(ctx, nodeKey(node), body); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return ErrNodeBusy
		}
		return fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
	}
	return nil
}

// write replaces the node record only at rev. A revision conflict means
// another change won; any other error may have landed.
func (s *Store) write(ctx context.Context, node string, rec nodeRecord, rev uint64) error {
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if s.beforeWrite != nil {
		s.beforeWrite()
	}
	if _, err := s.kv.Update(ctx, nodeKey(node), body, rev); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return fmt.Errorf("%w: record changed concurrently", ErrNotCurrent)
		}
		return fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
	}
	return nil
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("execution: grant ID: %w", err)
	}
	return hex.EncodeToString(b), nil
}
