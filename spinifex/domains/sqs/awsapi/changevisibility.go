package awsapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

const actionChangeMessageVisibility = "ChangeMessageVisibility"

type changeVisibilityInput struct {
	QueueURL          *string `json:"QueueUrl"`
	ReceiptHandle     *string `json:"ReceiptHandle"`
	VisibilityTimeout *int    `json:"VisibilityTimeout"`
}

// changeMessageVisibility moves the deadline of the message a receipt handle
// names. The handle is authenticated only after authorizing, as for delete.
func changeMessageVisibility(d Deps, w http.ResponseWriter, inv dispatch.Invocation, body []byte) error {
	var in changeVisibilityInput
	if err := decode(body, &in); err != nil {
		return err
	}
	if in.QueueURL == nil || *in.QueueURL == "" {
		return missing("QueueUrl")
	}
	if in.ReceiptHandle == nil || *in.ReceiptHandle == "" {
		return missing("ReceiptHandle")
	}
	if in.VisibilityTimeout == nil {
		return missing("VisibilityTimeout")
	}
	if v := *in.VisibilityTimeout; v < 0 || v > message.MaxVisibilitySeconds {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValueException, "VisibilityTimeout must be between 0 and %d.", message.MaxVisibilitySeconds)
	}
	name, err := parseQueueURL(d, inv, *in.QueueURL)
	if err != nil {
		return err
	}
	if err := authorize(inv, actionChangeMessageVisibility, queueARN(inv, name)); err != nil {
		return err
	}
	if d.Visibility == nil {
		return errors.New(awserrors.ErrorServiceUnavailableException)
	}
	ctx := inv.Request.Context()
	err = d.Visibility.ChangeVisibility(ctx, message.ChangeVisibilityInput{
		Account: inv.AccountID, Region: inv.Region, Queue: name,
		ReceiptHandle: *in.ReceiptHandle, VisibilityTimeout: *in.VisibilityTimeout,
	})
	if err != nil {
		slog.DebugContext(ctx, "SQS ChangeMessageVisibility failed", "sqs.queue_name", name, "cloud.account.id", inv.AccountID, "err", err)
		return messageError(err)
	}
	return writeJSON(w, struct{}{})
}
