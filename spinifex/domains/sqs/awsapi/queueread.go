package awsapi

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

const (
	actionGetQueueURL        = "GetQueueUrl"
	actionGetQueueAttributes = "GetQueueAttributes"
)

type getQueueURLInput struct {
	QueueName              *string `json:"QueueName"`
	QueueOwnerAWSAccountID *string `json:"QueueOwnerAWSAccountId"`
}

type getQueueURLOutput struct {
	QueueURL string `json:"QueueUrl"`
}

// getQueueURL returns the URL of an active queue in the caller's account.
// Queue policies are not supported, so naming another owner is refused before
// any read, with the code AWS returns for another account's queue URL.
func getQueueURL(d Deps, w http.ResponseWriter, inv dispatch.Invocation, body []byte) error {
	var in getQueueURLInput
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
	if owner := in.QueueOwnerAWSAccountID; owner != nil && *owner != "" && *owner != inv.AccountID {
		return invalidAddress(d)
	}
	if err := authorize(inv, actionGetQueueURL, queueARN(inv, name)); err != nil {
		return err
	}
	rec, err := resolve(d, inv, name)
	if err != nil {
		return err
	}
	return writeJSON(w, getQueueURLOutput{QueueURL: queueURL(d.Endpoint, rec.Account, rec.Name)})
}

type getQueueAttributesInput struct {
	QueueURL       *string  `json:"QueueUrl"`
	AttributeNames []string `json:"AttributeNames"`
}

type getQueueAttributesOutput struct {
	Attributes map[string]string `json:"Attributes,omitempty"`
}

// getQueueAttributes returns the named attributes of a queue in the caller's
// account. Message counts, and All which includes them, are refused until the
// counts can be read.
func getQueueAttributes(d Deps, w http.ResponseWriter, inv dispatch.Invocation, body []byte) error {
	var in getQueueAttributesInput
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
	if err := queue.ValidateAttributeNames(in.AttributeNames); err != nil {
		return ownerError(err)
	}
	if err := authorize(inv, actionGetQueueAttributes, queueARN(inv, name)); err != nil {
		return err
	}
	rec, err := resolve(d, inv, name)
	if err != nil {
		return err
	}
	attrs, err := rec.AttributesFor(in.AttributeNames)
	if err != nil {
		return ownerError(err)
	}
	return writeJSON(w, getQueueAttributesOutput{Attributes: attrs})
}

func resolve(d Deps, inv dispatch.Invocation, name string) (queue.Record, error) {
	if d.Queues == nil || d.Endpoint == "" {
		return queue.Record{}, errors.New(awserrors.ErrorServiceUnavailableException)
	}
	ctx := inv.Request.Context()
	rec, err := d.Queues.Resolve(ctx, inv.AccountID, inv.Region, name)
	if err != nil {
		slog.DebugContext(ctx, "SQS queue lookup failed", "sqs.queue_name", name, "cloud.account.id", inv.AccountID, "err", err)
		return queue.Record{}, ownerError(err)
	}
	return rec, nil
}

// parseQueueURL returns the queue name a URL of the form
// <endpoint>/<account>/<name> names in the caller's account. Only the
// configured endpoint's scheme and host are accepted; no alias host is.
func parseQueueURL(d Deps, inv dispatch.Invocation, raw string) (string, error) {
	ep, err := url.Parse(d.Endpoint)
	if err != nil || ep.Host == "" {
		return "", errors.New(awserrors.ErrorServiceUnavailableException)
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, ep.Scheme) || !strings.EqualFold(u.Host, ep.Host) ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", invalidAddress(d)
	}
	rest, ok := strings.CutPrefix(u.Path, ep.Path+"/")
	if !ok {
		return "", invalidAddress(d)
	}
	account, name, ok := strings.Cut(rest, "/")
	if !ok || strings.Contains(name, "/") || account != inv.AccountID {
		return "", invalidAddress(d)
	}
	if queue.ValidateName(name) != nil {
		return "", errors.New(awserrors.ErrorSQSQueueDoesNotExist)
	}
	return name, nil
}

func invalidAddress(d Deps) error {
	return awserrors.Errorf(awserrors.ErrorSQSInvalidAddress, "The address %s/ is not valid for this endpoint.", d.Endpoint)
}
