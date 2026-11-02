package awsapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

const (
	actionUntagQueue    = "UntagQueue"
	actionListQueueTags = "ListQueueTags"
)

type tagQueueInput struct {
	QueueURL *string           `json:"QueueUrl"`
	Tags     map[string]string `json:"Tags"`
}

type untagQueueInput struct {
	QueueURL *string  `json:"QueueUrl"`
	TagKeys  []string `json:"TagKeys"`
}

// tagQueue adds or overwrites tags on a queue in the caller's account. Tag
// condition keys are not passed: the IAM evaluator fails closed on them. An
// empty tag set is refused as missing, which is provisional.
func tagQueue(d Deps, w http.ResponseWriter, inv dispatch.Invocation, body []byte) error {
	var in tagQueueInput
	if err := decode(body, &in); err != nil {
		return err
	}
	name, err := tagTarget(d, inv, in.QueueURL, len(in.Tags), "Tags")
	if err != nil {
		return err
	}
	if err := queue.ValidateTags(in.Tags, false); err != nil {
		return ownerError(err)
	}
	return mutateTags(d, w, inv, actionTagQueue, name, func() error {
		_, err := d.Queues.Tag(inv.Request.Context(), inv.AccountID, inv.Region, name, in.Tags)
		return err
	})
}

// untagQueue removes tag keys from a queue in the caller's account; keys the
// queue does not carry are ignored.
func untagQueue(d Deps, w http.ResponseWriter, inv dispatch.Invocation, body []byte) error {
	var in untagQueueInput
	if err := decode(body, &in); err != nil {
		return err
	}
	name, err := tagTarget(d, inv, in.QueueURL, len(in.TagKeys), "TagKeys")
	if err != nil {
		return err
	}
	if err := queue.ValidateTagKeys(in.TagKeys); err != nil {
		return ownerError(err)
	}
	return mutateTags(d, w, inv, actionUntagQueue, name, func() error {
		_, err := d.Queues.Untag(inv.Request.Context(), inv.AccountID, inv.Region, name, in.TagKeys)
		return err
	})
}

func tagTarget(d Deps, inv dispatch.Invocation, rawURL *string, n int, member string) (string, error) {
	if rawURL == nil || *rawURL == "" {
		return "", missing("QueueUrl")
	}
	if n == 0 {
		return "", missing(member)
	}
	return parseQueueURL(d, inv, *rawURL)
}

func mutateTags(d Deps, w http.ResponseWriter, inv dispatch.Invocation, action, name string, run func() error) error {
	if err := authorize(inv, action, queueARN(inv, name)); err != nil {
		return err
	}
	if d.Queues == nil || d.Endpoint == "" {
		return errors.New(awserrors.ErrorServiceUnavailableException)
	}
	if err := run(); err != nil {
		slog.DebugContext(inv.Request.Context(), "SQS "+action+" failed", "sqs.queue_name", name, "cloud.account.id", inv.AccountID, "err", err)
		return ownerError(err)
	}
	return writeJSON(w, struct{}{})
}

type listQueueTagsInput struct {
	QueueURL *string `json:"QueueUrl"`
}

type listQueueTagsOutput struct {
	Tags map[string]string `json:"Tags,omitempty"`
}

// listQueueTags returns the tags of a queue in the caller's account. A queue
// with no tags answers with no Tags member, which is provisional.
func listQueueTags(d Deps, w http.ResponseWriter, inv dispatch.Invocation, body []byte) error {
	var in listQueueTagsInput
	if err := decode(body, &in); err != nil {
		return err
	}
	if in.QueueURL == nil || *in.QueueURL == "" {
		return missing("QueueUrl")
	}
	name, err := parseQueueURL(d, inv, *in.QueueURL)
	if err != nil {
		return err
	}
	if err := authorize(inv, actionListQueueTags, queueARN(inv, name)); err != nil {
		return err
	}
	if d.Queues == nil || d.Endpoint == "" {
		return errors.New(awserrors.ErrorServiceUnavailableException)
	}
	ctx := inv.Request.Context()
	tags, err := d.Queues.Tags(ctx, inv.AccountID, inv.Region, name)
	if err != nil {
		slog.DebugContext(ctx, "SQS ListQueueTags failed", "sqs.queue_name", name, "cloud.account.id", inv.AccountID, "err", err)
		return ownerError(err)
	}
	return writeJSON(w, listQueueTagsOutput{Tags: tags})
}
