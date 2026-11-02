package awsapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

type changeVisibilityBatchInput struct {
	QueueURL *string                       `json:"QueueUrl"`
	Entries  *[]changeVisibilityBatchEntry `json:"Entries"`
}

type changeVisibilityBatchEntry struct {
	ID                *string `json:"Id"`
	ReceiptHandle     *string `json:"ReceiptHandle"`
	VisibilityTimeout *int    `json:"VisibilityTimeout"`
}

// changeMessageVisibilityBatch moves each entry's deadline under the
// ChangeMessageVisibility permission. The batch is validated before any
// change; each entry's outcome, uncertain ones included, is reported under
// its own Id. Handles bind their queue, so a replaced queue is never changed.
func changeMessageVisibilityBatch(d Deps, w http.ResponseWriter, inv dispatch.Invocation, body []byte) error {
	var in changeVisibilityBatchInput
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
	for i, e := range entries {
		if e.ReceiptHandle == nil || *e.ReceiptHandle == "" {
			return missing(fmt.Sprintf("Entries.%d.ReceiptHandle", i+1))
		}
		if e.VisibilityTimeout == nil {
			return missing(fmt.Sprintf("Entries.%d.VisibilityTimeout", i+1))
		}
	}
	name, err := parseQueueURL(d, inv, *in.QueueURL)
	if err != nil {
		return err
	}
	if err := authorize(inv, actionChangeMessageVisibility, queueARN(inv, name)); err != nil {
		return err
	}
	if d.Visibility == nil || d.Queues == nil {
		return errors.New(awserrors.ErrorServiceUnavailableException)
	}
	if _, err := resolveBatchQueue(d, inv, name); err != nil {
		return err
	}
	ctx := inv.Request.Context()
	out := deleteBatchOutput{Successful: []batchSuccess{}, Failed: []batchFailure{}}
	for _, e := range entries {
		if v := *e.VisibilityTimeout; v < 0 || v > message.MaxVisibilitySeconds {
			out.Failed = append(out.Failed, batchFailure{ID: *e.ID, SenderFault: true, Code: queryErrorCodes[awserrors.ErrorInvalidParameterValueException],
				Message: fmt.Sprintf("VisibilityTimeout must be between 0 and %d.", message.MaxVisibilitySeconds)})
			continue
		}
		err := d.Visibility.ChangeVisibility(ctx, message.ChangeVisibilityInput{
			Account: inv.AccountID, Region: inv.Region, Queue: name,
			ReceiptHandle: *e.ReceiptHandle, VisibilityTimeout: *e.VisibilityTimeout,
		})
		if err == nil {
			out.Successful = append(out.Successful, batchSuccess{ID: *e.ID})
			continue
		}
		slog.DebugContext(ctx, "SQS ChangeMessageVisibilityBatch entry failed", "sqs.queue_name", name, "cloud.account.id", inv.AccountID, "sqs.batch_entry_id", *e.ID, "err", err)
		out.Failed = append(out.Failed, visibilityEntryFailure(*e.ID, err))
	}
	return writeJSON(w, out)
}

// visibilityEntryFailure reports one entry's error under its query code, an
// uncertain write before any client fault so it stays retryable; the
// message never carries the receipt handle.
func visibilityEntryFailure(id string, err error) batchFailure {
	switch {
	case errors.Is(err, message.ErrQueueDoesNotExist):
		return batchFailure{ID: id, SenderFault: true, Code: queryErrorCodes[awserrors.ErrorSQSQueueDoesNotExist], Message: "The specified queue doesn't exist."}
	case errors.Is(err, message.ErrUnavailable):
		return batchFailure{ID: id, Code: queryErrorCodes[awserrors.ErrorServiceUnavailableException], Message: "The visibility change could not be confirmed; retry this entry."}
	case errors.Is(err, message.ErrReceiptInvalid):
		return batchFailure{ID: id, SenderFault: true, Code: awserrors.ErrorSQSReceiptHandleIsInvalid, Message: "The specified receipt handle isn't valid."}
	case errors.Is(err, message.ErrNotInflight):
		return batchFailure{ID: id, SenderFault: true, Code: queryErrorCodes[awserrors.ErrorSQSMessageNotInflight], Message: "The message referred to isn't in flight."}
	case errors.Is(err, message.ErrVisibilityCap):
		return batchFailure{ID: id, SenderFault: true, Code: queryErrorCodes[awserrors.ErrorInvalidParameterValueException], Message: "VisibilityTimeout would extend past 12 hours from this receive."}
	case errors.Is(err, message.ErrInvalidParameter):
		return batchFailure{ID: id, SenderFault: true, Code: queryErrorCodes[awserrors.ErrorInvalidParameterValueException], Message: err.Error()}
	}
	return batchFailure{ID: id, Code: awserrors.ErrorInternalFailure, Message: "The visibility change failed."}
}
