package awsapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// MaxBatchEntries is the most entries one batch request may carry.
const MaxBatchEntries = 10

// batchEntryID is the documented entry Id syntax: up to 80 alphanumeric,
// hyphen or underscore characters.
var batchEntryID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

type deleteBatchInput struct {
	QueueURL *string             `json:"QueueUrl"`
	Entries  *[]deleteBatchEntry `json:"Entries"`
}

type deleteBatchEntry struct {
	ID            *string `json:"Id"`
	ReceiptHandle *string `json:"ReceiptHandle"`
}

type batchSuccess struct {
	ID string `json:"Id"`
}

type batchFailure struct {
	ID          string `json:"Id"`
	SenderFault bool   `json:"SenderFault"`
	Code        string `json:"Code"`
	Message     string `json:"Message,omitempty"`
}

type deleteBatchOutput struct {
	Successful []batchSuccess `json:"Successful"`
	Failed     []batchFailure `json:"Failed"`
}

// deleteMessageBatch deletes each entry's message under the DeleteMessage
// permission. The whole batch is validated before any delete; each entry's
// outcome, uncertain ones included, is reported under its own Id.
func deleteMessageBatch(d Deps, w http.ResponseWriter, inv dispatch.Invocation, body []byte) error {
	var in deleteBatchInput
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
	if err := validateBatch(entries); err != nil {
		return err
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
	out := deleteBatchOutput{Successful: []batchSuccess{}, Failed: []batchFailure{}}
	for i, e := range entries {
		err := d.Deletes.Delete(ctx, message.DeleteInput{
			Account: inv.AccountID, Region: inv.Region, Queue: name, ReceiptHandle: *e.ReceiptHandle,
		})
		if err == nil {
			out.Successful = append(out.Successful, batchSuccess{ID: *e.ID})
			continue
		}
		if i == 0 && errors.Is(err, message.ErrQueueDoesNotExist) {
			return messageError(err)
		}
		slog.DebugContext(ctx, "SQS DeleteMessageBatch entry failed", "sqs.queue_name", name, "cloud.account.id", inv.AccountID, "sqs.batch_entry_id", *e.ID, "err", err)
		out.Failed = append(out.Failed, batchEntryFailure(*e.ID, err))
	}
	return writeJSON(w, out)
}

// validateBatch rejects the whole request before any side effect: no
// entries, too many, a malformed or repeated Id, or a missing handle.
func validateBatch(entries []deleteBatchEntry) error {
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
	}
	return nil
}

// validateBatchIDs checks a batch's entry count and that every entry Id is
// well formed and distinct.
func validateBatchIDs(ids []*string) error {
	if len(ids) == 0 {
		return errors.New(awserrors.ErrorSQSEmptyBatchRequest)
	}
	if len(ids) > MaxBatchEntries {
		return awserrors.Errorf(awserrors.ErrorSQSTooManyEntriesInBatchRequest, "Maximum number of entries per request are %d. You have sent %d.", MaxBatchEntries, len(ids))
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == nil || !batchEntryID.MatchString(*id) {
			return errors.New(awserrors.ErrorSQSInvalidBatchEntryId)
		}
		if _, dup := seen[*id]; dup {
			return awserrors.Errorf(awserrors.ErrorSQSBatchEntryIdsNotDistinct, "Id %s repeated.", *id)
		}
		seen[*id] = struct{}{}
	}
	return nil
}

// batchEntryFailure reports one entry's error under its query code, in
// messageError's precedence so an uncertain delete stays retryable; the
// message never carries the receipt handle.
func batchEntryFailure(id string, err error) batchFailure {
	switch {
	case errors.Is(err, message.ErrQueueDoesNotExist):
		return batchFailure{ID: id, SenderFault: true, Code: queryErrorCodes[awserrors.ErrorSQSQueueDoesNotExist], Message: "The specified queue doesn't exist."}
	case errors.Is(err, message.ErrUnavailable):
		return batchFailure{ID: id, Code: queryErrorCodes[awserrors.ErrorServiceUnavailableException], Message: "The delete could not be confirmed; retry this entry."}
	case errors.Is(err, message.ErrReceiptInvalid):
		return batchFailure{ID: id, SenderFault: true, Code: awserrors.ErrorSQSReceiptHandleIsInvalid, Message: "The specified receipt handle isn't valid."}
	}
	return batchFailure{ID: id, Code: awserrors.ErrorInternalFailure, Message: "The delete failed."}
}
