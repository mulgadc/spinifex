package awsapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

const (
	actionReceiveMessage = "ReceiveMessage"
	actionDeleteMessage  = "DeleteMessage"
)

// System attribute names a message can carry. Only the first four are ever
// present: send refuses trace headers and FIFO, and redrive is unsupported.
const (
	sysAll                  = "All"
	sysSenderID             = "SenderId"
	sysSentTimestamp        = "SentTimestamp"
	sysReceiveCount         = "ApproximateReceiveCount"
	sysFirstReceive         = "ApproximateFirstReceiveTimestamp"
	sysSequenceNumber       = "SequenceNumber"
	sysDeduplicationID      = "MessageDeduplicationId"
	sysGroupID              = "MessageGroupId"
	sysTraceHeader          = "AWSTraceHeader"
	sysDeadLetterSourceQARN = "DeadLetterQueueSourceArn"
)

var systemAttributeNames = map[string]bool{
	sysAll: true, sysSenderID: true, sysSentTimestamp: true, sysReceiveCount: true,
	sysFirstReceive: true, sysSequenceNumber: true, sysDeduplicationID: true,
	sysGroupID: true, sysTraceHeader: true, sysDeadLetterSourceQARN: true,
}

type receiveMessageInput struct {
	QueueURL *string `json:"QueueUrl"`
	// AttributeNames is the discontinued spelling of
	// MessageSystemAttributeNames; both are honoured.
	AttributeNames              []string `json:"AttributeNames"`
	MessageSystemAttributeNames []string `json:"MessageSystemAttributeNames"`
	MessageAttributeNames       []string `json:"MessageAttributeNames"`
	MaxNumberOfMessages         *int     `json:"MaxNumberOfMessages"`
	VisibilityTimeout           *int     `json:"VisibilityTimeout"`
	WaitTimeSeconds             *int     `json:"WaitTimeSeconds"`
	ReceiveRequestAttemptID     *string  `json:"ReceiveRequestAttemptId"`
}

type receivedMessage struct {
	MessageID              string                      `json:"MessageId"`
	ReceiptHandle          string                      `json:"ReceiptHandle"`
	MD5OfBody              string                      `json:"MD5OfBody"`
	Body                   string                      `json:"Body"`
	Attributes             map[string]string           `json:"Attributes,omitempty"`
	MessageAttributes      map[string]wireAttributeOut `json:"MessageAttributes,omitempty"`
	MD5OfMessageAttributes string                      `json:"MD5OfMessageAttributes,omitempty"`
}

// wireAttributeOut is one returned message attribute; exactly one value
// member is set, since send refuses a missing or empty value.
type wireAttributeOut struct {
	DataType    string  `json:"DataType"`
	StringValue *string `json:"StringValue,omitempty"`
	BinaryValue []byte  `json:"BinaryValue,omitempty"`
}

type receiveMessageOutput struct {
	Messages []receivedMessage `json:"Messages,omitempty"`
}

// receiveMessage takes up to MaxNumberOfMessages messages in flight from a
// queue in the caller's account, long polling for the request's wait or the
// queue's default; a cancelled request ends the wait.
func receiveMessage(d Deps, w http.ResponseWriter, inv dispatch.Invocation, body []byte) error {
	var in receiveMessageInput
	if err := decode(body, &in); err != nil {
		return err
	}
	if in.QueueURL == nil || *in.QueueURL == "" {
		return missing("QueueUrl")
	}
	if in.ReceiveRequestAttemptID != nil {
		return awserrors.Errorf(awserrors.ErrorUnsupportedOperation, "ReceiveRequestAttemptId applies only to FIFO queues, which are not supported.")
	}
	want, err := systemAttributes(in.AttributeNames, in.MessageSystemAttributeNames)
	if err != nil {
		return err
	}
	sel, err := message.ParseAttributeSelector(in.MessageAttributeNames)
	if err != nil {
		return messageError(err)
	}
	name, err := parseQueueURL(d, inv, *in.QueueURL)
	if err != nil {
		return err
	}
	req := message.ReceiveInput{
		Account: inv.AccountID, Region: inv.Region, Queue: name,
		MaxNumberOfMessages: in.MaxNumberOfMessages, VisibilityTimeout: in.VisibilityTimeout,
		WaitTimeSeconds: in.WaitTimeSeconds,
	}
	if err := message.ValidateReceive(req); err != nil {
		return messageError(err)
	}
	if err := authorize(inv, actionReceiveMessage, queueARN(inv, name)); err != nil {
		return err
	}
	if d.Receives == nil || d.Endpoint == "" {
		return errors.New(awserrors.ErrorServiceUnavailableException)
	}
	ctx := inv.Request.Context()
	got, err := d.Receives.Receive(ctx, req)
	if err != nil {
		slog.DebugContext(ctx, "SQS ReceiveMessage failed", "sqs.queue_name", name, "cloud.account.id", inv.AccountID, "err", err)
		return messageError(err)
	}
	out := receiveMessageOutput{}
	for _, m := range got {
		attrs := m.Attributes.Select(sel)
		out.Messages = append(out.Messages, receivedMessage{
			MessageID: m.MessageID, ReceiptHandle: m.ReceiptHandle, MD5OfBody: m.MD5OfBody,
			Body: string(m.Body), Attributes: attributesOf(m, want),
			MessageAttributes: messageAttributesOut(attrs), MD5OfMessageAttributes: attrs.MD5(),
		})
	}
	return writeJSON(w, out)
}

// systemAttributes is the set of system attributes asked for across both
// spellings. An unknown name is InvalidAttributeName, provisionally.
func systemAttributes(lists ...[]string) (map[string]bool, error) {
	want := map[string]bool{}
	for _, names := range lists {
		for _, n := range names {
			if !systemAttributeNames[n] {
				return nil, awserrors.Errorf(awserrors.ErrorSQSInvalidAttributeName, "Unknown Attribute %s.", n)
			}
			want[n] = true
		}
	}
	return want, nil
}

func attributesOf(m message.Received, want map[string]bool) map[string]string {
	all := want[sysAll]
	out := map[string]string{}
	if all || want[sysSenderID] {
		out[sysSenderID] = m.SenderID
	}
	if all || want[sysSentTimestamp] {
		out[sysSentTimestamp] = strconv.FormatInt(m.SentAt.UnixMilli(), 10)
	}
	if all || want[sysReceiveCount] {
		out[sysReceiveCount] = strconv.FormatUint(m.ReceiveCount, 10)
	}
	if all || want[sysFirstReceive] {
		out[sysFirstReceive] = strconv.FormatInt(m.FirstReceive.UnixMilli(), 10)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// messageAttributesOut renders the selected attributes; the digest covers
// only these, provisionally until compared against AWS.
func messageAttributesOut(a message.Attributes) map[string]wireAttributeOut {
	if a.Len() == 0 {
		return nil
	}
	out := make(map[string]wireAttributeOut, a.Len())
	for _, at := range a.All() {
		out[at.Name] = wireAttributeOut{DataType: at.Value.DataType, StringValue: at.Value.StringValue, BinaryValue: at.Value.BinaryValue}
	}
	return out
}

type deleteMessageInput struct {
	QueueURL      *string `json:"QueueUrl"`
	ReceiptHandle *string `json:"ReceiptHandle"`
}

// deleteMessage tombstones the message a receipt handle names. The handle is
// authenticated against the queue only after authorizing, so an unauthorized
// caller learns nothing about it.
func deleteMessage(d Deps, w http.ResponseWriter, inv dispatch.Invocation, body []byte) error {
	var in deleteMessageInput
	if err := decode(body, &in); err != nil {
		return err
	}
	if in.QueueURL == nil || *in.QueueURL == "" {
		return missing("QueueUrl")
	}
	if in.ReceiptHandle == nil || *in.ReceiptHandle == "" {
		return missing("ReceiptHandle")
	}
	name, err := parseQueueURL(d, inv, *in.QueueURL)
	if err != nil {
		return err
	}
	if err := authorize(inv, actionDeleteMessage, queueARN(inv, name)); err != nil {
		return err
	}
	if d.Deletes == nil {
		return errors.New(awserrors.ErrorServiceUnavailableException)
	}
	ctx := inv.Request.Context()
	err = d.Deletes.Delete(ctx, message.DeleteInput{
		Account: inv.AccountID, Region: inv.Region, Queue: name, ReceiptHandle: *in.ReceiptHandle,
	})
	if err != nil {
		slog.DebugContext(ctx, "SQS DeleteMessage failed", "sqs.queue_name", name, "cloud.account.id", inv.AccountID, "err", err)
		return messageError(err)
	}
	return writeJSON(w, struct{}{})
}

// messageError maps a message owner error to its SQS wire code; the
// parameter, 12-hour limit and unsupported codes are provisional.
func messageError(err error) error {
	switch {
	case errors.Is(err, message.ErrQueueDoesNotExist):
		return errors.New(awserrors.ErrorSQSQueueDoesNotExist)
	case errors.Is(err, message.ErrUnavailable):
		return errors.New(awserrors.ErrorServiceUnavailableException)
	case errors.Is(err, message.ErrReceiptInvalid):
		return errors.New(awserrors.ErrorSQSReceiptHandleIsInvalid)
	case errors.Is(err, message.ErrNotInflight):
		return errors.New(awserrors.ErrorSQSMessageNotInflight)
	case errors.Is(err, message.ErrVisibilityCap):
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValueException, "VisibilityTimeout would extend past 12 hours from this receive.")
	case errors.Is(err, message.ErrInvalidParameter):
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValueException, "%s", err.Error())
	case errors.Is(err, message.ErrUnsupported):
		return awserrors.Errorf(awserrors.ErrorUnsupportedOperation, "%s", err.Error())
	}
	return errors.New(awserrors.ErrorInternalFailure)
}
