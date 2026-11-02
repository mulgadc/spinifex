package message

import (
	"context"
	"crypto/md5" //nolint:gosec // MD5OfMessageBody is an AWS wire checksum, not a security control.
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MetaVersion is the newest message schema this release reads. A send
// writes MetaVersionPlain unless the message has attributes, so a reader that
// predates attributes refuses such a message instead of misreading its body.
const (
	MetaVersion      = 2
	MetaVersionPlain = 1
)

// MaxDelaySeconds is the largest per-message or queue DelaySeconds.
const MaxDelaySeconds = 900

// MinMessageSize and MaxMessageSize bound a queue's MaximumMessageSize.
const (
	MinMessageSize = 1024
	MaxMessageSize = 1 << 20
)

// Metadata keys a send writes beside the body. They are the lifecycle
// contract's Sqs-V, Sqs-Id, Sqs-Enq, Sqs-Vis, Sqs-Sender, Sqs-Md5, Sqs-Md5A
// and Sqs-Acct.
const (
	MetaV      = "V"
	MetaID     = "Id"
	MetaEnq    = "Enq"
	MetaVis    = "Vis"
	MetaSender = "Sender"
	MetaMD5    = "Md5"
	MetaMD5A   = "Md5A"
	MetaAcct   = "Acct"
)

var (
	// ErrQueueDoesNotExist reports that the target is not an active queue.
	ErrQueueDoesNotExist = errors.New("message: queue does not exist")
	// ErrBodyRequired reports an absent or empty message body.
	ErrBodyRequired = errors.New("message: body is required")
	// ErrInvalidContents reports a body with characters SQS does not allow.
	ErrInvalidContents = errors.New("message: invalid message contents")
	// ErrInvalidParameter reports an out-of-range size or delay.
	ErrInvalidParameter = errors.New("message: invalid parameter value")
	// ErrUnsupported reports a send feature this release does not implement.
	ErrUnsupported = errors.New("message: unsupported")
	// ErrUnavailable reports that the append was not acknowledged; it may
	// still have been stored, and a retry can duplicate it.
	ErrUnavailable = errors.New("message: unavailable")
)

// Target is an active queue resolved for a send.
type Target struct {
	Namespace Namespace
	Account   string
	// DelaySeconds and MaximumMessageSize are the queue's current values.
	DelaySeconds       int
	MaximumMessageSize int
}

// Queues resolves an active queue for a send; it is the queue owner's
// capability, injected so this package never reads queue records itself.
type Queues interface {
	SendTarget(ctx context.Context, account, region, name string) (Target, error)
}

// SendInput is one SendMessage request after wire decoding.
type SendInput struct {
	Account, Region, Queue string
	// Sender is the SenderId recorded with the message.
	Sender string
	Body   []byte
	// DelaySeconds overrides the queue's delay when set.
	DelaySeconds *int
	// QueueID, when set, pins the queue: if the name now resolves to any
	// other queue the send is ErrQueueDoesNotExist and nothing is appended.
	QueueID QueueID

	// MessageAttributes is a list so a repeated name reaches validation.
	MessageAttributes       []Attribute
	MessageSystemAttributes map[string]AttributeValue
	// Presence alone is refused: an empty string is still a supplied member.
	MessageGroupID         *string
	MessageDeduplicationID *string
}

// SendResult is returned only after the append is acknowledged as durable.
type SendResult struct {
	MessageID string
	MD5OfBody string
	// MD5OfMessageAttributes is empty when the message has no attributes.
	MD5OfMessageAttributes string
	Position               Position
	EnqueuedAt             time.Time
	VisibleAfter           time.Time
}

// Sender is the SendMessage owner core.
type Sender struct {
	Store  Store
	Queues Queues
	// Now and NewID default to the wall clock and a random UUID.
	Now   func() time.Time
	NewID func() (string, error)
}

// Send validates a message, resolves its queue and appends it durably. It
// answers only after the store acknowledges the append; any append failure
// is ErrUnavailable, never success.
func (s *Sender) Send(ctx context.Context, in SendInput) (SendResult, error) {
	if s == nil || s.Store == nil || s.Queues == nil {
		return SendResult{}, fmt.Errorf("%w: sender is not configured", ErrUnavailable)
	}
	if err := validateRequest(in); err != nil {
		return SendResult{}, err
	}
	t, err := s.Queues.SendTarget(ctx, in.Account, in.Region, in.Queue)
	if err != nil {
		return SendResult{}, err
	}
	if t.Account != in.Account {
		return SendResult{}, fmt.Errorf("%w: resolved queue belongs to another account", ErrUnavailable)
	}
	if in.QueueID != "" && t.Namespace.Queue != in.QueueID {
		return SendResult{}, fmt.Errorf("%w: the queue name now resolves to another queue", ErrQueueDoesNotExist)
	}
	if t.MaximumMessageSize < MinMessageSize || t.MaximumMessageSize > MaxMessageSize {
		return SendResult{}, fmt.Errorf("%w: queue size limit %d outside %d..%d", ErrUnavailable, t.MaximumMessageSize, MinMessageSize, MaxMessageSize)
	}
	if t.DelaySeconds < 0 || t.DelaySeconds > MaxDelaySeconds {
		return SendResult{}, fmt.Errorf("%w: queue delay %d outside 0..%d", ErrUnavailable, t.DelaySeconds, MaxDelaySeconds)
	}
	if err := t.Namespace.Validate(); err != nil {
		return SendResult{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	attrs, err := NewAttributes(in.MessageAttributes)
	if err != nil {
		return SendResult{}, err
	}
	if len(in.Body)+attrs.Size() > t.MaximumMessageSize {
		return SendResult{}, fmt.Errorf("%w: message body and attributes must be shorter than %d bytes", ErrInvalidParameter, t.MaximumMessageSize+1)
	}
	delay := t.DelaySeconds
	if in.DelaySeconds != nil {
		delay = *in.DelaySeconds
	}
	id, err := s.newID()
	if err != nil {
		return SendResult{}, fmt.Errorf("%w: message ID: %w", ErrUnavailable, err)
	}
	enq := s.now().UTC().Truncate(time.Millisecond)
	vis := enq.Add(time.Duration(delay) * time.Second)
	sum := md5.Sum(in.Body) //nolint:gosec // AWS wire checksum.
	digest := hex.EncodeToString(sum[:])
	meta := map[string]string{
		MetaV:      strconv.Itoa(MetaVersionPlain),
		MetaID:     id,
		MetaEnq:    strconv.FormatInt(enq.UnixMilli(), 10),
		MetaVis:    strconv.FormatInt(vis.UnixMilli(), 10),
		MetaSender: in.Sender,
		MetaMD5:    digest,
		MetaAcct:   t.Account,
	}
	payload := in.Body
	if attrs.Len() > 0 {
		meta[MetaV] = strconv.Itoa(MetaVersion)
		meta[MetaMD5A] = attrs.MD5()
		payload = encodePayload(attrs, in.Body)
	}
	res, err := s.Store.Append(ctx, t.Namespace, AppendInput{Body: payload, Meta: meta})
	if err != nil {
		return SendResult{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if res.Position == 0 || res.Duplicate {
		return SendResult{}, fmt.Errorf("%w: append returned no new position", ErrUnavailable)
	}
	return SendResult{
		MessageID: id, MD5OfBody: digest, MD5OfMessageAttributes: attrs.MD5(),
		Position: res.Position, EnqueuedAt: enq, VisibleAfter: vis,
	}, nil
}

// validateRequest checks what needs no queue: the request and its sender.
func validateRequest(in SendInput) error {
	if err := ValidateMessage(in); err != nil {
		return err
	}
	if in.Sender == "" || strings.ContainsAny(in.Sender, "\r\n") {
		return fmt.Errorf("%w: sender ID is not recordable", ErrUnavailable)
	}
	return nil
}

// ValidateMessage checks what a send needs neither queue nor sender for:
// supported features, attributes, the body's characters and the delay.
func ValidateMessage(in SendInput) error {
	switch {
	case len(in.MessageSystemAttributes) > 0:
		return fmt.Errorf("%w: message system attributes", ErrUnsupported)
	case in.MessageGroupID != nil:
		return fmt.Errorf("%w: MessageGroupId on a standard queue", ErrUnsupported)
	case in.MessageDeduplicationID != nil:
		return fmt.Errorf("%w: MessageDeduplicationId on a standard queue", ErrUnsupported)
	}
	if len(in.Body) == 0 {
		return ErrBodyRequired
	}
	if err := validateContents(in.Body); err != nil {
		return err
	}
	if d := in.DelaySeconds; d != nil && (*d < 0 || *d > MaxDelaySeconds) {
		return fmt.Errorf("%w: DelaySeconds must be an integer from 0 to %d", ErrInvalidParameter, MaxDelaySeconds)
	}
	attrs, err := NewAttributes(in.MessageAttributes)
	if err != nil {
		return err
	}
	// Whether AWS digests and returns a Number as sent or with its zeros
	// stripped is not established, so Number is refused until it is.
	for _, a := range attrs.All() {
		if base, _, _ := strings.Cut(a.Value.DataType, "."); base == "Number" {
			return fmt.Errorf("%w: message attribute %q of type Number", ErrUnsupported, a.Name)
		}
	}
	return nil
}

// validateContents allows only the characters AWS documents for a body:
// #x9, #xA, #xD, #x20 to #xD7FF, #xE000 to #xFFFD and #x10000 to #x10FFFF.
// The error names the offset, never the body.
func validateContents(body []byte) error {
	for i := 0; i < len(body); {
		r, n := utf8.DecodeRune(body[i:])
		if r == utf8.RuneError && n <= 1 {
			return fmt.Errorf("%w: invalid UTF-8 at byte %d", ErrInvalidContents, i)
		}
		if !allowedRune(r) {
			return fmt.Errorf("%w: disallowed character U+%04X at byte %d", ErrInvalidContents, r, i)
		}
		i += n
	}
	return nil
}

func allowedRune(r rune) bool {
	return r == 0x9 || r == 0xA || r == 0xD || r >= 0x20 && r <= 0xD7FF ||
		r >= 0xE000 && r <= 0xFFFD || r >= 0x10000 && r <= 0x10FFFF
}

func (s *Sender) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Sender) newID() (string, error) {
	if s.NewID != nil {
		return s.NewID()
	}
	return NewMessageID()
}

// NewMessageID mints a random (version 4) UUID.
func NewMessageID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}
