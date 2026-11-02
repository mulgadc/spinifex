package queue

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"time"

	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/nats-io/nats.go/jetstream"
)

// Bucket is the control-plane KV bucket holding queue records and name
// bindings.
const Bucket = "spinifex-sqs-queues"

// createAttempts bounds how often CreateQueue retries after losing a race.
const createAttempts = 3

// Errors the queue owner returns for a resolvable request.
var (
	ErrQueueDoesNotExist    = errors.New("queue does not exist")
	ErrQueueNameExists      = errors.New("queue name exists with different attributes")
	ErrQueueDeletedRecently = errors.New("queue name was deleted recently")
	ErrInvalidIdentity      = errors.New("invalid account or region")
	// ErrUnavailable means the outcome could not be decided; nothing was
	// granted and the request may be retried.
	ErrUnavailable = errors.New("queue store unavailable")
)

var (
	accountPattern = regexp.MustCompile(`^[0-9]{12}$`)
	regionPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// kvOps is the bucket access the owner needs. Gets go to the stream leader,
// key listings may lag it, and writes are create-if-absent or revision CAS.
type kvOps interface {
	get(ctx context.Context, key string) ([]byte, uint64, error)
	create(ctx context.Context, key string, value []byte) (uint64, error)
	update(ctx context.Context, key string, value []byte, rev uint64) (uint64, error)
	delete(ctx context.Context, key string, rev uint64) error
	keys(ctx context.Context, filter string) ([]string, error)
}

type jsKV struct {
	js jetstream.JetStream
	kv jetstream.KeyValue
}

func (k jsKV) get(ctx context.Context, key string) ([]byte, uint64, error) {
	return sqsjs.LeaderKVGet(ctx, k.js, Bucket, key)
}

func (k jsKV) create(ctx context.Context, key string, value []byte) (uint64, error) {
	return k.kv.Create(ctx, key, value)
}

func (k jsKV) update(ctx context.Context, key string, value []byte, rev uint64) (uint64, error) {
	return k.kv.Update(ctx, key, value, rev)
}

func (k jsKV) delete(ctx context.Context, key string, rev uint64) error {
	return k.kv.Delete(ctx, key, jetstream.LastRevision(rev))
}

// Store is the queue owner (ADR-0012:S1, S3). Every decision reads the
// stream leader and writes by compare-and-swap at the revision read, so a
// stale read or an ambiguous write grants nothing.
type Store struct {
	kv kvOps
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// MessageStreams and StateBuckets are the shard counts new queues are
	// allocated from.
	MessageStreams int
	StateBuckets   int
}

// Open opens or creates the bucket, refusing one that could lose records.
func Open(ctx context.Context, js jetstream.JetStream, messageStreams, stateBuckets int) (*Store, error) {
	kv, err := sqsjs.OpenDurableKV(ctx, js, Bucket, "SQS queue records and name bindings")
	if err != nil {
		return nil, err
	}
	return &Store{kv: jsKV{js: js, kv: kv}, MessageStreams: messageStreams, StateBuckets: stateBuckets}, nil
}

func recordKey(account, queueID string) string { return "q." + account + "." + queueID }

func bindingKey(account, region, name string) string {
	return "n." + account + "." + region + "." + name
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// CreateRequest is a validated-on-entry CreateQueue.
type CreateRequest struct {
	Account    string
	Region     string
	Name       string
	Attributes map[string]string
	// Tags apply only when the queue is new; a repeat CreateQueue ignores
	// them, provisionally until characterised against AWS.
	Tags map[string]string
}

// Create creates the queue, or returns the existing one when every supplied
// attribute matches its effective value (row A15). The record is written
// before the binding, so a crash between them leaves a queue nothing resolves.
func (s *Store) Create(ctx context.Context, req CreateRequest) (Record, error) {
	if err := validIdentity(req.Account, req.Region); err != nil {
		return Record{}, err
	}
	if err := ValidateName(req.Name); err != nil {
		return Record{}, err
	}
	supplied, err := ValidateCreate(req.Attributes)
	if err != nil {
		return Record{}, err
	}
	if err := ValidateTags(req.Tags, true); err != nil {
		return Record{}, err
	}
	bkey := bindingKey(req.Account, req.Region, req.Name)
	for range createAttempts {
		b, brev, err := s.readBinding(ctx, bkey)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			b, brev = Binding{}, 0
		case err != nil:
			return Record{}, err
		case b.State == BindingBound:
			r, err := s.existing(ctx, req, b, supplied)
			if !errors.Is(err, errReusable) {
				return r, err
			}
			if b.V > RecordVersion {
				return Record{}, fmt.Errorf("%w: binding %s has schema version %d, newer than %d", ErrUnavailable, bkey, b.V, RecordVersion)
			}
		case b.State == BindingReserved:
			until, err := s.reservedUntil(ctx, req.Account, req.Region, req.Name, b)
			if err != nil {
				return Record{}, err
			}
			if s.now().Before(until) {
				return Record{}, ErrQueueDeletedRecently
			}
			if b.V > RecordVersion {
				return Record{}, fmt.Errorf("%w: binding %s has schema version %d, newer than %d", ErrUnavailable, bkey, b.V, RecordVersion)
			}
		default:
			return Record{}, fmt.Errorf("%w: binding %s in state %q", ErrUnavailable, bkey, b.State)
		}

		r, rrev, err := s.writeRecord(ctx, req, supplied)
		if err != nil {
			return Record{}, err
		}
		won, retry, err := s.bind(ctx, bkey, b, brev, r)
		switch {
		case won:
			return r, nil
		case retry:
			s.discardUnbound(ctx, r, rrev)
			continue
		default:
			return Record{}, err
		}
	}
	return Record{}, fmt.Errorf("%w: CreateQueue %s lost %d races", ErrUnavailable, req.Name, createAttempts)
}

// existing answers a CreateQueue whose name is bound. A binding naming no
// readable active record fails closed: a reconciler repairs it, not a create.
func (s *Store) existing(ctx context.Context, req CreateRequest, b Binding, supplied Attributes) (Record, error) {
	r, _, err := s.readRecord(ctx, req.Account, b.QueueID)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return Record{}, fmt.Errorf("%w: %s is bound to missing queue %s", ErrUnavailable, req.Name, b.QueueID)
	case err != nil:
		return Record{}, err
	case !r.names(req.Account, req.Region, req.Name):
		return Record{}, fmt.Errorf("%w: binding %s names queue %s of another name", ErrUnavailable, req.Name, b.QueueID)
	case r.State == StateDeleting:
		until, err := r.reuseAfter()
		switch {
		case err != nil:
			return Record{}, err
		case s.now().Before(until):
			return Record{}, ErrQueueDeletedRecently
		}
		return Record{}, errReusable
	case r.State != StateActive:
		return Record{}, fmt.Errorf("%w: queue %s in state %q", ErrUnavailable, r.QueueID, r.State)
	case !r.Attributes.Matches(supplied):
		return Record{}, ErrQueueNameExists
	}
	return r, nil
}

// writeRecord creates a new queue record under a fresh queue ID. A failed or
// unanswered write leaves a record no name resolves to yet; its binding write
// may still land, so age alone never makes it safe to remove.
func (s *Store) writeRecord(ctx context.Context, req CreateRequest, supplied Attributes) (Record, uint64, error) {
	id, err := NewQueueID()
	if err != nil {
		return Record{}, 0, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	mshard, sshard, err := Allocate(id, s.MessageStreams, s.StateBuckets)
	if err != nil {
		return Record{}, 0, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	at := s.now().UTC().Truncate(time.Second)
	r := Record{
		V:                    RecordVersion,
		QueueID:              id,
		MessageShard:         mshard,
		StateShard:           sshard,
		Account:              req.Account,
		Region:               req.Region,
		Name:                 req.Name,
		ARN:                  arn.FormatSQSQueue(req.Region, req.Account, req.Name),
		CreatedAt:            at,
		Type:                 TypeStandard,
		State:                StateActive,
		Generation:           1,
		Attributes:           supplied.WithDefaults(),
		AttributesModifiedAt: at,
	}
	if len(req.Tags) > 0 {
		r.Tags = maps.Clone(req.Tags)
	}
	enc, err := r.Encode()
	if err != nil {
		return Record{}, 0, err
	}
	rev, err := s.kv.create(ctx, recordKey(req.Account, id), enc)
	if err != nil {
		return Record{}, 0, fmt.Errorf("%w: write queue record: %w", ErrUnavailable, err)
	}
	return r, rev, nil
}

// bind points the name at r by compare-and-swap at brev. It reports won when
// the binding names r, and retry when the write provably cannot apply; any
// other outcome fails closed and keeps r, which may yet be bound.
func (s *Store) bind(ctx context.Context, key string, prev Binding, brev uint64, r Record) (won, retry bool, err error) {
	nb := prev
	nb.V, nb.QueueID, nb.State, nb.ReuseAfter = RecordVersion, r.QueueID, BindingBound, time.Time{}
	enc, err := nb.Encode()
	if err != nil {
		return false, false, err
	}
	if brev == 0 {
		_, err = s.kv.create(ctx, key, enc)
	} else {
		_, err = s.kv.update(ctx, key, enc, brev)
	}
	if err == nil {
		return true, false, nil
	}
	if isConflict(err) {
		return false, true, nil
	}

	got, grev, rerr := s.readBinding(ctx, key)
	switch {
	case rerr == nil && got.QueueID == r.QueueID:
		if got.State == BindingBound {
			return true, false, nil
		}
	case rerr == nil && grev != brev:
		return false, true, nil
	case errors.Is(rerr, jetstream.ErrKeyNotFound) && brev != 0:
		return false, true, nil
	}
	return false, false, fmt.Errorf("%w: binding %s outcome unknown: %w", ErrUnavailable, key, err)
}

// discardUnbound removes a record whose binding write cannot apply. It is
// conditional on the record's own revision; a failure leaves it for the
// reconciler, which removes unbound records.
func (s *Store) discardUnbound(ctx context.Context, r Record, rev uint64) {
	_ = s.kv.delete(ctx, recordKey(r.Account, r.QueueID), rev)
}

// Resolve returns the active queue a name resolves to: its binding must be
// bound and name an active record of that name (lifecycle contract 4.2).
func (s *Store) Resolve(ctx context.Context, account, region, name string) (Record, error) {
	r, _, err := s.resolve(ctx, account, region, name)
	if err != nil {
		return Record{}, err
	}
	if r.State != StateActive {
		return Record{}, ErrQueueDoesNotExist
	}
	return r, nil
}

// resolve returns the record a bound name names, in any state, with its
// revision.
func (s *Store) resolve(ctx context.Context, account, region, name string) (Record, uint64, error) {
	if err := validIdentity(account, region); err != nil {
		return Record{}, 0, err
	}
	if ValidateName(name) != nil {
		return Record{}, 0, ErrQueueDoesNotExist
	}
	b, _, err := s.readBinding(ctx, bindingKey(account, region, name))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return Record{}, 0, ErrQueueDoesNotExist
	case err != nil:
		return Record{}, 0, err
	case b.State != BindingBound:
		return Record{}, 0, ErrQueueDoesNotExist
	}
	r, rev, err := s.readRecord(ctx, account, b.QueueID)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return Record{}, 0, ErrQueueDoesNotExist
	case err != nil:
		return Record{}, 0, err
	case !r.names(account, region, name):
		return Record{}, 0, ErrQueueDoesNotExist
	}
	return r, rev, nil
}

// readableAttributes are every name GetQueueAttributes accepts.
var readableAttributes = []string{
	"All", "Policy", AttrVisibilityTimeout, AttrMaximumMessageSize, AttrMessageRetentionPeriod,
	"ApproximateNumberOfMessages", "ApproximateNumberOfMessagesNotVisible", "CreatedTimestamp",
	"LastModifiedTimestamp", "QueueArn", "ApproximateNumberOfMessagesDelayed", AttrDelaySeconds,
	AttrReceiveMessageWaitTimeSeconds, "RedrivePolicy", AttrFifoQueue, "ContentBasedDeduplication",
	"KmsMasterKeyId", "KmsDataKeyReusePeriodSeconds", "DeduplicationScope", "FifoThroughputLimit",
	"RedriveAllowPolicy", AttrSqsManagedSseEnabled,
}

// unservedAttributes are valid names whose values are not available yet. A
// request needing one, including All, is refused rather than answered without
// it.
var unservedAttributes = []string{
	"All", "ApproximateNumberOfMessages", "ApproximateNumberOfMessagesNotVisible", "ApproximateNumberOfMessagesDelayed",
}

// AttributesFor returns the named attributes r carries; names valid in AWS
// that r does not carry, such as an unset Policy, are omitted.
func (r *Record) AttributesFor(names []string) (map[string]string, error) {
	if err := ValidateAttributeNames(names); err != nil {
		return nil, err
	}
	all := r.visibleAttributes()
	out := map[string]string{}
	for _, name := range names {
		if v, ok := all[name]; ok {
			out[name] = v
		}
	}
	return out, nil
}

// ValidateAttributeNames rejects a name GetQueueAttributes does not accept,
// and one whose value cannot be served yet.
func ValidateAttributeNames(names []string) error {
	for _, name := range names {
		if !slices.Contains(readableAttributes, name) {
			return fmt.Errorf("%w: %s", ErrInvalidAttributeName, name)
		}
		if slices.Contains(unservedAttributes, name) {
			return fmt.Errorf("%w: %s cannot be read yet", ErrUnsupportedAttribute, name)
		}
	}
	return nil
}

func (r *Record) visibleAttributes() map[string]string {
	all := map[string]string{
		"QueueArn":              r.ARN,
		"CreatedTimestamp":      strconv.FormatInt(r.CreatedAt.Unix(), 10),
		"LastModifiedTimestamp": strconv.FormatInt(r.AttributesModifiedAt.Unix(), 10),
	}
	for name, v := range r.Attributes {
		if slices.Contains(readableAttributes, name) {
			all[name] = v
		}
	}
	return all
}

func (r *Record) names(account, region, name string) bool {
	return r.Account == account && r.Region == region && r.Name == name
}

func (s *Store) readBinding(ctx context.Context, key string) (Binding, uint64, error) {
	v, rev, err := s.kv.get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return Binding{}, 0, err
		}
		return Binding{}, 0, fmt.Errorf("%w: read %s: %w", ErrUnavailable, key, err)
	}
	b, err := DecodeBinding(v)
	if err != nil {
		return Binding{}, 0, fmt.Errorf("%w: decode %s: %w", ErrUnavailable, key, err)
	}
	if !b.wellFormed() {
		return Binding{}, 0, fmt.Errorf("%w: binding %s is malformed", ErrUnavailable, key)
	}
	return b, rev, nil
}

// wellFormed refuses a binding that cannot be acted on safely: a reservation
// without a deadline must never authorise reuse of its name.
func (b *Binding) wellFormed() bool {
	switch {
	case b.V < 1 || b.QueueID == "":
		return false
	case b.State == BindingReserved:
		return !b.ReuseAfter.IsZero()
	}
	return true
}

func (s *Store) readRecord(ctx context.Context, account, queueID string) (Record, uint64, error) {
	key := recordKey(account, queueID)
	v, rev, err := s.kv.get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return Record{}, 0, err
		}
		return Record{}, 0, fmt.Errorf("%w: read %s: %w", ErrUnavailable, key, err)
	}
	r, err := DecodeRecord(v)
	if err != nil || r.QueueID != queueID {
		return Record{}, 0, fmt.Errorf("%w: decode %s", ErrUnavailable, key)
	}
	return r, rev, nil
}

func validIdentity(account, region string) error {
	if !accountPattern.MatchString(account) || !regionPattern.MatchString(region) {
		return ErrInvalidIdentity
	}
	return nil
}

// isConflict reports a compare-and-swap the server refused: the write did
// not apply.
func isConflict(err error) bool {
	return errors.Is(err, jetstream.ErrKeyExists) || errors.Is(err, jetstream.ErrKeyRevisionMismatch)
}
