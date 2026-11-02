package awsapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

const actionSendMessage = "SendMessage"

type sendMessageInput struct {
	QueueURL     *string         `json:"QueueUrl"`
	MessageBody  json.RawMessage `json:"MessageBody"`
	DelaySeconds *int            `json:"DelaySeconds"`
	// MessageAttributes is decoded exactly; the features after it are
	// refused, never stored inert, and only checked for presence.
	MessageAttributes       json.RawMessage            `json:"MessageAttributes"`
	MessageSystemAttributes map[string]json.RawMessage `json:"MessageSystemAttributes"`
	MessageGroupID          *string                    `json:"MessageGroupId"`
	MessageDeduplicationID  *string                    `json:"MessageDeduplicationId"`
}

type sendMessageOutput struct {
	MessageID              string `json:"MessageId"`
	MD5OfMessageBody       string `json:"MD5OfMessageBody"`
	MD5OfMessageAttributes string `json:"MD5OfMessageAttributes,omitempty"`
}

// sendMessage appends one message to a queue in the caller's account. The
// request is validated before authorizing; the size limit needs the queue, so
// it is checked after. SenderId is the authenticated caller's aws:userid.
func sendMessage(d Deps, w http.ResponseWriter, inv dispatch.Invocation, body []byte) error {
	var in sendMessageInput
	if err := decode(body, &in); err != nil {
		return err
	}
	if in.QueueURL == nil || *in.QueueURL == "" {
		return missing("QueueUrl")
	}
	if in.MessageBody == nil || string(in.MessageBody) == "null" {
		return missing("MessageBody")
	}
	msgBody, err := decodeBody(in.MessageBody)
	if err != nil {
		return err
	}
	name, err := parseQueueURL(d, inv, *in.QueueURL)
	if err != nil {
		return err
	}
	req := message.SendInput{
		Account: inv.AccountID, Region: inv.Region, Queue: name,
		Body: msgBody, DelaySeconds: in.DelaySeconds,
	}
	if req.MessageAttributes, err = decodeMessageAttributes(in.MessageAttributes); err != nil {
		return attributeError(err)
	}
	if len(in.MessageSystemAttributes) > 0 {
		req.MessageSystemAttributes = map[string]message.AttributeValue{}
	}
	for k := range in.MessageSystemAttributes {
		req.MessageSystemAttributes[k] = message.AttributeValue{}
	}
	req.MessageGroupID = in.MessageGroupID
	req.MessageDeduplicationID = in.MessageDeduplicationID
	if err := message.ValidateMessage(req); err != nil {
		return sendError(err)
	}
	if err := authorize(inv, actionSendMessage, queueARN(inv, name)); err != nil {
		return err
	}
	if d.Messages == nil || d.Endpoint == "" {
		return errors.New(awserrors.ErrorServiceUnavailableException)
	}
	ctx := inv.Request.Context()
	if inv.Actor == nil {
		slog.ErrorContext(ctx, "SQS SendMessage: no caller identity in the invocation")
		return errors.New(awserrors.ErrorInternalFailure)
	}
	actor, err := inv.Actor()
	if err != nil || actor.UserID == "" {
		slog.ErrorContext(ctx, "SQS SendMessage: caller identity unresolved", "cloud.account.id", inv.AccountID, "err", err)
		return errors.New(awserrors.ErrorInternalFailure)
	}
	req.Sender = actor.UserID
	res, err := d.Messages.Send(ctx, req)
	if err != nil {
		slog.DebugContext(ctx, "SQS SendMessage failed", "sqs.queue_name", name, "cloud.account.id", inv.AccountID, "err", err)
		return sendError(err)
	}
	return writeJSON(w, sendMessageOutput{MessageID: res.MessageID, MD5OfMessageBody: res.MD5OfBody, MD5OfMessageAttributes: res.MD5OfMessageAttributes})
}

// attributeError passes a serialization fault through and maps an owner
// validation error to its wire code.
func attributeError(err error) error {
	if errors.Is(err, message.ErrInvalidParameter) || errors.Is(err, message.ErrUnsupported) {
		return sendError(err)
	}
	return err
}

// sendError maps a message owner error to its SQS wire code. The empty-body,
// unsupported-feature and oversize codes are provisional until the AWS baseline.
func sendError(err error) error {
	switch {
	// An uncertain outcome is never reported as a settled refusal.
	case errors.Is(err, message.ErrUnavailable):
		return errors.New(awserrors.ErrorServiceUnavailableException)
	case errors.Is(err, message.ErrQueueDoesNotExist):
		return errors.New(awserrors.ErrorSQSQueueDoesNotExist)
	case errors.Is(err, message.ErrBodyRequired):
		return missing("MessageBody")
	case errors.Is(err, message.ErrInvalidContents):
		return awserrors.Errorf(awserrors.ErrorSQSInvalidMessageContents, "%s", err.Error())
	case errors.Is(err, message.ErrInvalidParameter):
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValueException, "%s", err.Error())
	case errors.Is(err, message.ErrUnsupported):
		return awserrors.Errorf(awserrors.ErrorUnsupportedOperation, "%s", err.Error())
	}
	return errors.New(awserrors.ErrorInternalFailure)
}

// decodeBody decodes the MessageBody JSON string without the replacements
// encoding/json makes: malformed UTF-8 and unpaired \u surrogate escapes are
// refused rather than stored as U+FFFD. The code is provisional until baseline.
func decodeBody(raw json.RawMessage) ([]byte, error) {
	invalid := awserrors.Errorf(awserrors.ErrorSQSInvalidMessageContents, "The message body is not well-formed UTF-8 or UTF-16.")
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil, errors.New(awserrors.ErrorSerializationException)
	}
	if err := checkJSONText(raw); err != nil {
		if errors.Is(err, errMalformedText) {
			return nil, invalid
		}
		return nil, err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, errors.New(awserrors.ErrorSerializationException)
	}
	return []byte(s), nil
}

func hex4(b []byte, at int) (rune, bool) {
	if at+4 > len(b) {
		return 0, false
	}
	var r rune
	for _, c := range b[at : at+4] {
		switch {
		case c >= '0' && c <= '9':
			r = r<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			r = r<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			r = r<<4 | rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	return r, true
}
