// Package relay carries SQS owner calls from the ingress to the SQS worker,
// which pulls work over its instance-role channel and posts each result back;
// no message payload crosses platform NATS.
package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
)

// Operations the relay carries, one per owner method the adapter calls.
const (
	OpCreateQueue        = "CreateQueue"
	OpResolveQueue       = "ResolveQueue"
	OpListQueues         = "ListQueues"
	OpSetQueueAttributes = "SetQueueAttributes"
	OpTagQueue           = "TagQueue"
	OpUntagQueue         = "UntagQueue"
	OpListQueueTags      = "ListQueueTags"
	OpSendMessage        = "SendMessage"
	OpReceiveMessage     = "ReceiveMessage"
	OpDeleteMessage      = "DeleteMessage"
	OpChangeVisibility   = "ChangeMessageVisibility"
)

// Work is one owner call handed to the worker. Deadline is when the caller
// stops waiting; a result posted after it is not delivered.
type Work struct {
	ID       string          `json:"id"`
	Op       string          `json:"op"`
	Deadline time.Time       `json:"deadline"`
	Input    json.RawMessage `json:"input"`
}

// Result is the worker's answer to one Work: an output or an owner error.
type Result struct {
	Output json.RawMessage `json:"output,omitempty"`
	Error  *WireError      `json:"error,omitempty"`
}

// WireError is an owner error by kind alone. Owner and backend text never
// crosses the wire, since a wrapped error can carry a handle, body or secret.
type WireError struct {
	Kind string `json:"kind"`
}

// kinds lists every owner sentinel the adapter distinguishes with the fixed
// text the ingress answers it with. The first match names the error, so
// Unavailable comes first: an uncertain write is never reported as settled.
var kinds = []struct {
	name string
	err  error
	msg  string
}{
	{"queue.Unavailable", queue.ErrUnavailable, "The queue service is temporarily unavailable."},
	{"message.Unavailable", message.ErrUnavailable, "The queue service is temporarily unavailable."},
	{"queue.QueueDoesNotExist", queue.ErrQueueDoesNotExist, "The specified queue does not exist."},
	{"queue.QueueNameExists", queue.ErrQueueNameExists, "A queue already exists with the same name and a different value for attribute(s)."},
	{"queue.QueueDeletedRecently", queue.ErrQueueDeletedRecently, "You must wait 60 seconds after deleting a queue before you can create another with the same name."},
	{"queue.InvalidIdentity", queue.ErrInvalidIdentity, "The request identity is not valid."},
	{"queue.InvalidAttributeName", queue.ErrInvalidAttributeName, "An attribute name is not valid."},
	{"queue.InvalidAttributeValue", queue.ErrInvalidAttributeValue, "An attribute value is not valid."},
	{"queue.UnsupportedAttribute", queue.ErrUnsupportedAttribute, "An attribute is not supported."},
	{"queue.InvalidQueueName", queue.ErrInvalidQueueName, "Can only include alphanumeric characters, hyphens, or underscores. 1 to 80 in length."},
	{"queue.InvalidTag", queue.ErrInvalidTag, "A tag is not valid."},
	{"queue.InvalidListRequest", queue.ErrInvalidListRequest, "The list request is not valid."},
	{"message.QueueDoesNotExist", message.ErrQueueDoesNotExist, "The specified queue does not exist."},
	{"message.BodyRequired", message.ErrBodyRequired, "The request must contain the parameter MessageBody."},
	{"message.InvalidContents", message.ErrInvalidContents, "The message contains characters outside the allowed set."},
	{"message.InvalidParameter", message.ErrInvalidParameter, "A parameter value is not valid."},
	{"message.Unsupported", message.ErrUnsupported, "A parameter or operation is not supported."},
	{"message.ReceiptInvalid", message.ErrReceiptInvalid, "The receipt handle is not valid."},
	{"message.NotInflight", message.ErrNotInflight, "The message is not in flight."},
	{"message.VisibilityCap", message.ErrVisibilityCap, "The total visibility timeout for the message is beyond the 12-hour limit."},
}

// errWorkerFault is a worker error that is not an owner sentinel; the
// adapter answers it as an internal failure.
var errWorkerFault = errors.New("relay: SQS worker failed the request")

// encodeError names err by the first owner sentinel it wraps.
func encodeError(err error) *WireError {
	for _, k := range kinds {
		if errors.Is(err, k.err) {
			return &WireError{Kind: k.name}
		}
	}
	return &WireError{Kind: "fault"}
}

// remoteError is a decoded owner error: fixed text that unwraps to the
// owner's sentinel.
type remoteError struct {
	sentinel error
	msg      string
}

func (e *remoteError) Error() string { return e.msg }
func (e *remoteError) Unwrap() error { return e.sentinel }

func decodeError(w *WireError) error {
	for _, k := range kinds {
		if k.name == w.Kind {
			return &remoteError{sentinel: k.err, msg: k.msg}
		}
	}
	return errWorkerFault
}

// marshal encodes without HTML escaping, so a body's worst-case growth is
// the bound the receive budget assumes.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// MarshalResult is the CompleteWork body for r.
func MarshalResult(r Result) ([]byte, error) { return marshal(r) }

// Wire forms of owner inputs and outputs that are not plain JSON values.

type nameInput struct {
	Account, Region, Name string
}

type attrsInput struct {
	Account, Region, Name string
	Attributes            map[string]string
}

type tagsInput struct {
	Account, Region, Name string
	Tags                  map[string]string
}

type untagInput struct {
	Account, Region, Name string
	Keys                  []string
}

// received is a message on the wire. Body and string values travel as
// base64, so a message's encoded size is at most 4/3 of its SQS size plus
// a bounded allowance, whatever characters it holds.
type received struct {
	MessageID              string
	ReceiptHandle          string
	Body                   []byte
	MD5OfBody              string
	Attributes             []wireAttribute
	MD5OfMessageAttributes string
	SenderID               string
	SentAt                 time.Time
	ReceiveCount           uint64
	FirstReceive           time.Time
	Deadline               time.Time
}

type wireAttribute struct {
	Name        string
	DataType    string
	StringValue []byte `json:",omitempty"`
	HasString   bool   `json:",omitempty"`
	BinaryValue []byte `json:",omitempty"`
}

func toWireReceived(in []message.Received) []received {
	out := make([]received, len(in))
	for i, m := range in {
		attrs := make([]wireAttribute, 0, m.Attributes.Len())
		for _, a := range m.Attributes.All() {
			w := wireAttribute{Name: a.Name, DataType: a.Value.DataType, BinaryValue: a.Value.BinaryValue}
			if a.Value.StringValue != nil {
				w.StringValue, w.HasString = []byte(*a.Value.StringValue), true
			}
			attrs = append(attrs, w)
		}
		out[i] = received{
			MessageID: m.MessageID, ReceiptHandle: m.ReceiptHandle, Body: m.Body, MD5OfBody: m.MD5OfBody,
			Attributes: attrs, MD5OfMessageAttributes: m.MD5OfMessageAttributes,
			SenderID: m.SenderID, SentAt: m.SentAt, ReceiveCount: m.ReceiveCount,
			FirstReceive: m.FirstReceive, Deadline: m.Deadline,
		}
	}
	return out
}

func fromWireReceived(in []received) ([]message.Received, error) {
	out := make([]message.Received, len(in))
	for i, m := range in {
		list := make([]message.Attribute, 0, len(m.Attributes))
		for _, w := range m.Attributes {
			v := message.AttributeValue{DataType: w.DataType, BinaryValue: w.BinaryValue}
			if w.HasString {
				s := string(w.StringValue)
				v.StringValue = &s
			}
			list = append(list, message.Attribute{Name: w.Name, Value: v})
		}
		attrs, err := message.NewAttributes(list)
		if err != nil {
			return nil, err
		}
		out[i] = message.Received{
			MessageID: m.MessageID, ReceiptHandle: m.ReceiptHandle, Body: m.Body, MD5OfBody: m.MD5OfBody,
			Attributes: attrs, MD5OfMessageAttributes: m.MD5OfMessageAttributes,
			SenderID: m.SenderID, SentAt: m.SentAt, ReceiveCount: m.ReceiveCount,
			FirstReceive: m.FirstReceive, Deadline: m.Deadline,
		}
	}
	return out, nil
}
