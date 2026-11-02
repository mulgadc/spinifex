package awsapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// sqs:ListQueues defines no resource type, so it is authorized on "*".
const actionListQueues = "ListQueues"

type listQueuesInput struct {
	QueueNamePrefix *string `json:"QueueNamePrefix"`
	NextToken       *string `json:"NextToken"`
	MaxResults      *int    `json:"MaxResults"`
}

type listQueuesOutput struct {
	QueueURLs []string `json:"QueueUrls,omitempty"`
	NextToken string   `json:"NextToken,omitempty"`
}

// listQueues lists the caller's active queues in its own account and Region.
// Ordering, the token format and listing consistency are provisional.
func listQueues(d Deps, w http.ResponseWriter, inv dispatch.Invocation, body []byte) error {
	var in listQueuesInput
	if err := decode(body, &in); err != nil {
		return err
	}
	req := queue.ListRequest{Account: inv.AccountID, Region: inv.Region, MaxResults: in.MaxResults}
	if in.QueueNamePrefix != nil {
		req.Prefix = *in.QueueNamePrefix
	}
	if in.NextToken != nil {
		req.NextToken = *in.NextToken
	}
	if err := req.Validate(); err != nil {
		return ownerError(err)
	}
	if err := authorize(inv, actionListQueues, "*"); err != nil {
		return err
	}
	if d.Queues == nil || d.Endpoint == "" {
		return errors.New(awserrors.ErrorServiceUnavailableException)
	}
	ctx := inv.Request.Context()
	res, err := d.Queues.List(ctx, req)
	if err != nil {
		slog.DebugContext(ctx, "SQS ListQueues failed", "cloud.account.id", inv.AccountID, "err", err)
		return ownerError(err)
	}
	out := listQueuesOutput{NextToken: res.NextToken}
	for _, r := range res.Queues {
		out.QueueURLs = append(out.QueueURLs, queueURL(d.Endpoint, r.Account, r.Name))
	}
	return writeJSON(w, out)
}
