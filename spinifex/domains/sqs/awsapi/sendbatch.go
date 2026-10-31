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

// MaxBatchPayload is the most the bodies and message attributes of one
// batch may total, in bytes.
const MaxBatchPayload = message.MaxMessageSize

type sendBatchInput struct {
	QueueURL *string           `json:"QueueUrl"`
	Entries  *[]sendBatchEntry `json:"Entries"`
}

type sendBatchEntry struct {
	ID           *string         `json:"Id"`
	MessageBody  json.RawMessage `json:"MessageBody"`
	DelaySeconds *int            `json:"DelaySeconds"`
	// MessageAttributes is decoded exactly; the features after it are
	// refused, never stored inert, and only checked for presence.
	MessageAttributes       json.RawMessage            `json:"MessageAttributes"`
	MessageSystemAttributes map[string]json.RawMessage `json:"MessageSystemAttributes"`
	MessageGroupID          *string                    `json:"MessageGroupId"`
	MessageDeduplicationID  *string                    `json:"MessageDeduplicationId"`
}

type sendBatchSuccess struct {
	ID                     string `json:"Id"`
	MessageID              string `json:"MessageId"`
	MD5OfMessageBody       string `json:"MD5OfMessageBody"`
	MD5OfMessageAttributes string `json:"MD5OfMessageAttributes,omitempty"`
}

type sendBatchOutput struct {
	Successful []sendBatchSuccess `json:"Successful"`
	Failed     []batchFailure     `json:"Failed"`
}

// sendPlan is one entry after validation: a request to send, or the failure
// that entry reports without reaching the owner.
type sendPlan struct {
	id   string
	req  message.SendInput
	fail *batchFailure
}

// sendMessageBatch appends each entry under the SendMessage permission. The
// batch shape, Ids and total size are checked before any append; each entry's
// outcome, uncertain ones included, is reported under its own Id.
func sendMessageBatch(d Deps, w http.ResponseWriter, inv dispatch.Invocation, body []byte) error {
	var in sendBatchInput
	if err := decode(body, &in); err != nil {
		return err
	}
	if in.QueueURL == nil || *in.QueueURL == "" {
		return missing("QueueUrl")
	}
	if in.Entries == nil {
		return missing("Entries")
	}
	entries := *in.Entries
	ids := make([]*string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	if err := validateBatchIDs(ids); err != nil {
		return err
	}
	plans, err := planSends(entries)
	if err != nil {
		return err
	}
	name, err := parseQueueURL(d, inv, *in.QueueURL)
	if err != nil {
		return err
	}
	if err := authorize(inv, actionSendMessage, queueARN(inv, name)); err != nil {
		return err
	}
	if d.Messages == nil || d.Queues == nil || d.Endpoint == "" {
		return errors.New(awserrors.ErrorServiceUnavailableException)
	}
	ctx := inv.Request.Context()
	// Every entry is pinned to the queue the name resolves to now, so a
	// queue replaced under the same name mid-batch is never written to.
	pin, err := resolveBatchQueue(d, inv, name)
	if err != nil {
		return err
	}
	if inv.Actor == nil {
		slog.ErrorContext(ctx, "SQS SendMessageBatch: no caller identity in the invocation")
		return errors.New(awserrors.ErrorInternalFailure)
	}
	actor, err := inv.Actor()
	if err != nil || actor.UserID == "" {
		slog.ErrorContext(ctx, "SQS SendMessageBatch: caller identity unresolved", "cloud.account.id", inv.AccountID, "err", err)
		return errors.New(awserrors.ErrorInternalFailure)
	}
	out := sendBatchOutput{Successful: []sendBatchSuccess{}, Failed: []batchFailure{}}
	for _, p := range plans {
		if p.fail != nil {
			out.Failed = append(out.Failed, *p.fail)
			continue
		}
		p.req.Account, p.req.Region, p.req.Queue, p.req.Sender = inv.AccountID, inv.Region, name, actor.UserID
		p.req.QueueID = pin
		res, err := d.Messages.Send(ctx, p.req)
		if err == nil {
			out.Successful = append(out.Successful, sendBatchSuccess{
				ID: p.id, MessageID: res.MessageID, MD5OfMessageBody: res.MD5OfBody, MD5OfMessageAttributes: res.MD5OfMessageAttributes,
			})
			continue
		}
		slog.DebugContext(ctx, "SQS SendMessageBatch entry failed", "sqs.queue_name", name, "cloud.account.id", inv.AccountID, "sqs.batch_entry_id", p.id, "err", err)
		out.Failed = append(out.Failed, sendEntryFailure(p.id, err))
	}
	return writeJSON(w, out)
}

// planSends decodes and validates every entry. A malformed body, malformed
// attribute JSON or a batch over MaxBatchPayload fails the request; an entry
// the owner would refuse becomes that entry's failure.
func planSends(entries []sendBatchEntry) ([]sendPlan, error) {
	plans := make([]sendPlan, len(entries))
	total := 0
	for i, e := range entries {
		p := sendPlan{id: *e.ID}
		plans[i] = p
		if e.MessageBody == nil || string(e.MessageBody) == "null" {
			f := sendEntryFailure(p.id, message.ErrBodyRequired)
			plans[i].fail = &f
			continue
		}
		body, err := decodeBody(e.MessageBody)
		if err != nil {
			if code, _ := awserrors.ResolveErrorCode(err); code != awserrors.ErrorSQSInvalidMessageContents {
				return nil, err
			}
			plans[i].fail = &batchFailure{ID: p.id, SenderFault: true, Code: awserrors.ErrorSQSInvalidMessageContents, Message: "The message body is not well-formed UTF-8 or UTF-16."}
			continue
		}
		total += len(body)
		req := message.SendInput{Body: body, DelaySeconds: e.DelaySeconds, MessageGroupID: e.MessageGroupID, MessageDeduplicationID: e.MessageDeduplicationID}
		attrs, err := decodeMessageAttributes(e.MessageAttributes)
		if err != nil {
			if !errors.Is(err, message.ErrInvalidParameter) && !errors.Is(err, message.ErrUnsupported) {
				return nil, err
			}
			f := sendEntryFailure(p.id, err)
			plans[i].fail = &f
			continue
		}
		req.MessageAttributes = attrs
		// Decoded attributes count toward the batch total with the body even
		// when the owner will refuse them below.
		total += message.SuppliedSize(attrs)
		if len(e.MessageSystemAttributes) > 0 {
			req.MessageSystemAttributes = map[string]message.AttributeValue{}
			for k := range e.MessageSystemAttributes {
				req.MessageSystemAttributes[k] = message.AttributeValue{}
			}
		}
		if err := message.ValidateMessage(req); err != nil {
			f := sendEntryFailure(p.id, err)
			plans[i].fail = &f
			continue
		}
		plans[i].req = req
	}
	if total > MaxBatchPayload {
		return nil, awserrors.Errorf(awserrors.ErrorSQSBatchRequestTooLong, "Batch requests cannot be longer than %d bytes. You have sent %d bytes.", MaxBatchPayload, total)
	}
	return plans, nil
}

// sendEntryFailure reports one entry's error under its query code, an
// uncertain append before any client fault so it stays retryable. Owner
// messages name offsets and limits, never the body.
func sendEntryFailure(id string, err error) batchFailure {
	switch {
	case errors.Is(err, message.ErrQueueDoesNotExist):
		return batchFailure{ID: id, SenderFault: true, Code: queryErrorCodes[awserrors.ErrorSQSQueueDoesNotExist], Message: "The specified queue doesn't exist."}
	case errors.Is(err, message.ErrUnavailable):
		return batchFailure{ID: id, Code: queryErrorCodes[awserrors.ErrorServiceUnavailableException], Message: "The send could not be confirmed; a retry may store this message twice."}
	case errors.Is(err, message.ErrBodyRequired):
		return batchFailure{ID: id, SenderFault: true, Code: queryErrorCodes[awserrors.ErrorMissingRequiredParameterException], Message: "The request must contain the parameter MessageBody."}
	case errors.Is(err, message.ErrInvalidContents):
		return batchFailure{ID: id, SenderFault: true, Code: awserrors.ErrorSQSInvalidMessageContents, Message: err.Error()}
	case errors.Is(err, message.ErrInvalidParameter):
		return batchFailure{ID: id, SenderFault: true, Code: queryErrorCodes[awserrors.ErrorInvalidParameterValueException], Message: err.Error()}
	case errors.Is(err, message.ErrUnsupported):
		return batchFailure{ID: id, SenderFault: true, Code: queryErrorCodes[awserrors.ErrorUnsupportedOperation], Message: err.Error()}
	}
	return batchFailure{ID: id, Code: awserrors.ErrorInternalFailure, Message: "The send failed."}
}

// resolveBatchQueue resolves a batch's queue once, before any entry runs,
// and fails closed unless the record is exactly the requested queue.
func resolveBatchQueue(d Deps, inv dispatch.Invocation, name string) (message.QueueID, error) {
	ctx := inv.Request.Context()
	rec, err := d.Queues.Resolve(ctx, inv.AccountID, inv.Region, name)
	if err != nil {
		return "", ownerError(err)
	}
	ns := rec.Namespace()
	if err := ns.Validate(); err != nil || rec.Account != inv.AccountID || rec.Region != inv.Region || rec.Name != name {
		slog.ErrorContext(ctx, "SQS batch: resolved queue record is not the requested queue", "sqs.queue_name", name, "cloud.account.id", inv.AccountID, "err", err)
		return "", errors.New(awserrors.ErrorServiceUnavailableException)
	}
	return ns.Queue, nil
}
