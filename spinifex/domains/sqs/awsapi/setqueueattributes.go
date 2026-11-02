package awsapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

const actionSetQueueAttributes = "SetQueueAttributes"

type setQueueAttributesInput struct {
	QueueURL   *string           `json:"QueueUrl"`
	Attributes map[string]string `json:"Attributes"`
}

// setQueueAttributes updates a queue in the caller's account. The whole
// attribute set is validated before authorizing; an empty set is refused as
// missing, which is provisional until the AWS baseline.
func setQueueAttributes(d Deps, w http.ResponseWriter, inv dispatch.Invocation, body []byte) error {
	var in setQueueAttributesInput
	if err := decode(body, &in); err != nil {
		return err
	}
	if in.QueueURL == nil || *in.QueueURL == "" {
		return missing("QueueUrl")
	}
	if len(in.Attributes) == 0 {
		return missing("Attributes")
	}
	name, err := parseQueueURL(d, inv, *in.QueueURL)
	if err != nil {
		return err
	}
	if _, err := queue.ValidateSet(in.Attributes); err != nil {
		return ownerError(err)
	}
	if err := authorize(inv, actionSetQueueAttributes, queueARN(inv, name)); err != nil {
		return err
	}
	if d.Queues == nil || d.Endpoint == "" {
		return errors.New(awserrors.ErrorServiceUnavailableException)
	}
	ctx := inv.Request.Context()
	if _, err := d.Queues.SetAttributes(ctx, inv.AccountID, inv.Region, name, in.Attributes); err != nil {
		slog.DebugContext(ctx, "SQS SetQueueAttributes failed", "sqs.queue_name", name, "cloud.account.id", inv.AccountID, "err", err)
		return ownerError(err)
	}
	return writeJSON(w, struct{}{})
}
