package awsapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// Actions authorized on the queue ARN. Tagging at creation also needs
// sqs:TagQueue.
const (
	actionCreateQueue = "CreateQueue"
	actionTagQueue    = "TagQueue"
)

type createQueueInput struct {
	QueueName  *string           `json:"QueueName"`
	Attributes map[string]string `json:"Attributes"`
	Tags       map[string]string `json:"tags"`
}

type createQueueOutput struct {
	QueueURL string `json:"QueueUrl"`
}

// createQueue validates the whole request, authorizes it on the queue ARN it
// names, then creates the queue or returns the matching existing one.
func createQueue(d Deps, w http.ResponseWriter, inv dispatch.Invocation, body []byte) error {
	ctx := inv.Request.Context()
	var in createQueueInput
	if err := decode(body, &in); err != nil {
		return err
	}
	if in.QueueName == nil || *in.QueueName == "" {
		return missing("QueueName")
	}
	name := *in.QueueName
	if err := queue.ValidateName(name); err != nil {
		return ownerError(err)
	}
	if _, err := queue.ValidateCreate(in.Attributes); err != nil {
		return ownerError(err)
	}
	if err := queue.ValidateTags(in.Tags, true); err != nil {
		return ownerError(err)
	}

	resource := queueARN(inv, name)
	if err := authorize(inv, actionCreateQueue, resource); err != nil {
		return err
	}
	if len(in.Tags) > 0 {
		if err := authorize(inv, actionTagQueue, resource); err != nil {
			return err
		}
	}

	if d.Queues == nil || d.Endpoint == "" {
		return errors.New(awserrors.ErrorServiceUnavailableException)
	}
	rec, err := d.Queues.Create(ctx, queue.CreateRequest{
		Account:    inv.AccountID,
		Region:     inv.Region,
		Name:       name,
		Attributes: in.Attributes,
		Tags:       in.Tags,
	})
	if err != nil {
		slog.DebugContext(ctx, "SQS CreateQueue failed", "sqs.queue_name", name, "cloud.account.id", inv.AccountID, "err", err)
		return ownerError(err)
	}
	slog.InfoContext(ctx, "SQS queue created or matched", "sqs.queue_name", rec.Name,
		"cloud.account.id", rec.Account, "sqs.queue_generation", rec.Generation)
	return writeJSON(w, createQueueOutput{QueueURL: queueURL(d.Endpoint, rec.Account, rec.Name)})
}
