package admin

import (
	"context"
	"errors"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

type ctxKey string

const ctxRequestID ctxKey = "admin.requestID"

// WithRequestID carries the /admin request ID to handlers that log under it.
// It correlates log records only and authorises nothing.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxRequestID, id)
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

// actorName is the name the caller goes by: the session name for a role
// session and the user name for every other principal.
func actorName(a dispatch.Actor) string {
	if a.PrincipalType == "assumed-role" {
		return a.SessionName
	}
	return a.UserName
}

// RunMethod runs one /admin method against the request body and returns the
// value to marshal as the JSON response. caller is the verified caller.
func (d Deps) RunMethod(ctx context.Context, method string, caller dispatch.Actor, body []byte) (any, error) {
	switch method {
	case "CreateAccount":
		return d.createAccount(ctx, body)
	case "DeleteAccount":
		return d.deleteAccount(ctx, body)
	case "DescribeAccountDeletion":
		return d.describeAccountDeletion(ctx, body)
	case "ListAccounts":
		return d.listAccounts(ctx, body)
	case "GetAccountQuota":
		return d.getAccountQuota(ctx, body)
	case "PutAccountQuota":
		return d.putAccountQuota(ctx, caller, body)
	}
	// The caller's method set is gated by the contract list, so this is
	// unreachable; failing closed keeps a name added there without a case from
	// silently returning 200.
	return nil, errors.New(awserrors.ErrorInvalidAction)
}
