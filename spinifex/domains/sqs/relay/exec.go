package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
)

// A posted result is a SigV4 payload of at most sigv4.MaxPayloadLen. Bodies
// and string values travel as base64, so a message encodes to at most 4/3 of
// the largest SQS message plus messageAllowance.
const (
	messageAllowance = 16 << 10
	resultAllowance  = 64 << 10
)

// ReceiveLimit is the most messages one receive returns, so its result fits
// a post whatever the messages hold. A claimed message is never dropped.
var ReceiveLimit = min(message.MaxReceiveMessages,
	(sigv4.MaxPayloadLen-resultAllowance)/((message.MaxMessageSize*4+2)/3+messageAllowance))

// ErrResultTooLarge is a result that would not fit one post, or could not be
// encoded. ReceiveLimit makes it unreachable for any result the owners return.
var ErrResultTooLarge = errors.New("relay: result exceeds the post limit")

// Owners are the worker's queue and message owners.
type Owners struct {
	Queues     awsapi.Queues
	Messages   awsapi.Messages
	Receives   awsapi.Receives
	Deletes    awsapi.Deletes
	Visibility awsapi.Visibility
}

// Claim is a message a receive took in flight, for release if the result is
// proven undelivered. It holds a receipt handle and is never logged.
type Claim struct {
	Account, Region, Queue string
	ReceiptHandle          string
}

// Execute runs w against the owners and returns its encoded result and the
// claims it took. A result that cannot be posted whole is replaced by an
// unavailable error and its claims are withheld, so they are never released.
func (o Owners) Execute(ctx context.Context, w Work) ([]byte, []Claim) {
	out, claims, err := o.run(ctx, w)
	res := Result{Output: out}
	if err != nil {
		res = Result{Error: encodeError(err)}
	}
	body, mErr := MarshalResult(res)
	if mErr == nil && len(body) <= sigv4.MaxPayloadLen {
		return body, claims
	}
	body, _ = MarshalResult(Result{Error: encodeError(fmt.Errorf("%w: %w", message.ErrUnavailable, ErrResultTooLarge))})
	return body, nil
}

// Release returns claims to visibility at once. Call it only when the
// ingress proved the result was not delivered.
func (o Owners) Release(ctx context.Context, claims []Claim) error {
	var errs []error
	for _, c := range claims {
		errs = append(errs, o.Visibility.ChangeVisibility(ctx, message.ChangeVisibilityInput{
			Account: c.Account, Region: c.Region, Queue: c.Queue, ReceiptHandle: c.ReceiptHandle,
		}))
	}
	return errors.Join(errs...)
}

func decodeIn[T any](raw json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("%w: undecodable work input", errWorkerFault)
	}
	return v, nil
}

func encodeOut(v any, err error) (json.RawMessage, []Claim, error) {
	if err != nil {
		return nil, nil, err
	}
	b, err := marshal(v)
	return b, nil, err
}

func (o Owners) run(ctx context.Context, w Work) (json.RawMessage, []Claim, error) {
	switch w.Op {
	case OpCreateQueue:
		in, err := decodeIn[queue.CreateRequest](w.Input)
		if err != nil {
			return nil, nil, err
		}
		return encodeOut(o.Queues.Create(ctx, in))
	case OpResolveQueue:
		in, err := decodeIn[nameInput](w.Input)
		if err != nil {
			return nil, nil, err
		}
		return encodeOut(o.Queues.Resolve(ctx, in.Account, in.Region, in.Name))
	case OpListQueues:
		in, err := decodeIn[queue.ListRequest](w.Input)
		if err != nil {
			return nil, nil, err
		}
		return encodeOut(o.Queues.List(ctx, in))
	case OpSetQueueAttributes:
		in, err := decodeIn[attrsInput](w.Input)
		if err != nil {
			return nil, nil, err
		}
		return encodeOut(o.Queues.SetAttributes(ctx, in.Account, in.Region, in.Name, in.Attributes))
	case OpTagQueue:
		in, err := decodeIn[tagsInput](w.Input)
		if err != nil {
			return nil, nil, err
		}
		return encodeOut(o.Queues.Tag(ctx, in.Account, in.Region, in.Name, in.Tags))
	case OpUntagQueue:
		in, err := decodeIn[untagInput](w.Input)
		if err != nil {
			return nil, nil, err
		}
		return encodeOut(o.Queues.Untag(ctx, in.Account, in.Region, in.Name, in.Keys))
	case OpListQueueTags:
		in, err := decodeIn[nameInput](w.Input)
		if err != nil {
			return nil, nil, err
		}
		tags, err := o.Queues.Tags(ctx, in.Account, in.Region, in.Name)
		if tags == nil {
			tags = map[string]string{}
		}
		return encodeOut(tags, err)
	case OpSendMessage:
		in, err := decodeIn[message.SendInput](w.Input)
		if err != nil {
			return nil, nil, err
		}
		return encodeOut(o.Messages.Send(ctx, in))
	case OpReceiveMessage:
		return o.receive(ctx, w.Input)
	case OpDeleteMessage:
		in, err := decodeIn[message.DeleteInput](w.Input)
		if err != nil {
			return nil, nil, err
		}
		return encodeOut(struct{}{}, o.Deletes.Delete(ctx, in))
	case OpChangeVisibility:
		in, err := decodeIn[message.ChangeVisibilityInput](w.Input)
		if err != nil {
			return nil, nil, err
		}
		return encodeOut(struct{}{}, o.Visibility.ChangeVisibility(ctx, in))
	}
	return nil, nil, fmt.Errorf("%w: unknown operation", errWorkerFault)
}

// receive lowers a valid request above ReceiveLimit before anything is
// claimed; an out-of-range request reaches the owner and is refused there.
func (o Owners) receive(ctx context.Context, raw json.RawMessage) (json.RawMessage, []Claim, error) {
	in, err := decodeIn[message.ReceiveInput](raw)
	if err != nil {
		return nil, nil, err
	}
	if n := in.MaxNumberOfMessages; n != nil && *n > ReceiveLimit && *n <= message.MaxReceiveMessages {
		limit := ReceiveLimit
		in.MaxNumberOfMessages = &limit
	}
	msgs, err := o.Receives.Receive(ctx, in)
	if err != nil {
		return nil, nil, err
	}
	claims := make([]Claim, len(msgs))
	for i, m := range msgs {
		claims[i] = Claim{Account: in.Account, Region: in.Region, Queue: in.Queue, ReceiptHandle: m.ReceiptHandle}
	}
	b, err := marshal(toWireReceived(msgs))
	return b, claims, err
}
