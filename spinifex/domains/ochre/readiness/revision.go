package readiness

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// SubjectRevision is one key's raw last message as its stream leader reported
// it. Seq zero is a key that has never been written; a delete or purge marker
// keeps its own non-zero Seq, which a fixed-revision write must name.
type SubjectRevision struct {
	Seq   uint64
	Op    string
	Value []byte
}

// Key operations a SubjectRevision can report. A value carries no operation
// header and is reported as OpPut.
const (
	OpPut   = "PUT"
	OpDel   = "DEL"
	OpPurge = "PURGE"
)

// Live reports whether the key currently holds a value.
func (r SubjectRevision) Live() bool { return r.Seq != 0 && r.Op == OpPut }

var validKey = regexp.MustCompile(`^[-/_=.a-zA-Z0-9]+$`)

func checkKey(bucket, key string) error {
	if bucket == "" || !validKey.MatchString(key) || strings.HasPrefix(key, ".") ||
		strings.HasSuffix(key, ".") || strings.HasPrefix(key, MarkerPrefix) {
		return fmt.Errorf("%w: key %q in bucket %q", ErrInvalid, key, bucket)
	}
	return nil
}

// ReadRevision reads key's last-subject revision in bucket through a get only
// the stream leader answers, including a delete or purge marker. The caller
// verifies the bucket's binding before interpreting it.
func ReadRevision(ctx context.Context, js jetstream.JetStream, bucket, key string) (SubjectRevision, error) {
	if err := checkKey(bucket, key); err != nil {
		return SubjectRevision{}, err
	}
	msg, err := kvstore.LeaderMsgGet(ctx, js, streamName(bucket), kvstore.MsgGetRequest{LastFor: "$KV." + bucket + "." + key})
	switch {
	case errors.Is(err, jetstream.ErrMsgNotFound):
		return SubjectRevision{}, nil
	case errors.Is(err, jetstream.ErrStreamNotFound):
		return SubjectRevision{}, fmt.Errorf("%w: %s", ErrStreamMissing, bucket)
	case err != nil:
		return SubjectRevision{}, fmt.Errorf("%w: %s revision: %w", ErrStreamUnavailable, bucket, err)
	case msg.Sequence == 0:
		return SubjectRevision{}, fmt.Errorf("%w: %s reported no revision", ErrStreamUnavailable, bucket)
	}
	switch op := msg.Header.Get("KV-Operation"); op {
	case "":
		return SubjectRevision{Seq: msg.Sequence, Op: OpPut, Value: msg.Data}, nil
	case OpDel, OpPurge:
		return SubjectRevision{Seq: msg.Sequence, Op: op}, nil
	default:
		return SubjectRevision{}, fmt.Errorf("%w: %s revision %d has operation %q", ErrUnsupported, bucket, msg.Sequence, op)
	}
}

// WriteAt publishes value to key exactly once, only while key's last-subject
// revision is expected; zero lands only on a key never written. ErrChanged is
// a definite refusal; ErrOutcomeUnknown may have landed and is never retried.
func WriteAt(ctx context.Context, js jetstream.JetStream, bucket, key string, value []byte, expected uint64) (uint64, error) {
	if err := checkKey(bucket, key); err != nil {
		return 0, err
	}
	msg := &nats.Msg{Subject: "$KV." + bucket + "." + key, Data: value}
	return publishOnce(ctx, js, msg, bucket, expected, "write "+bucket)
}
