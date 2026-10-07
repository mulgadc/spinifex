package gateway_rds

import (
	"context"

	"github.com/aws/aws-sdk-go/service/rds"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/nats-io/nats.go"
)

// AddTagsToResource tags the resource named by the request ARN. The daemon validates the ARN
// against the caller's account; the account is never taken from the ARN.
func AddTagsToResource(ctx context.Context, input *rds.AddTagsToResourceInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).AddTagsToResource(ctx, input, caller.AccountID)
}

// RemoveTagsFromResource implements the RDS RemoveTagsFromResource action; the daemon checks the
// ARN against the caller's account.
func RemoveTagsFromResource(ctx context.Context, input *rds.RemoveTagsFromResourceInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).RemoveTagsFromResource(ctx, input, caller.AccountID)
}

// ListTagsForResource implements the RDS ListTagsForResource action; the daemon checks the ARN
// against the caller's account.
func ListTagsForResource(ctx context.Context, input *rds.ListTagsForResourceInput, nc *nats.Conn, caller Caller) (any, error) {
	return handlers_rds.NewNATSService(nc).ListTagsForResource(ctx, input, caller.AccountID)
}
