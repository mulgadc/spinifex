package awsapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// singleQuery returns the one value of a query member; present but repeated
// or empty is refused.
func singleQuery(c *call, member, field string) (string, bool, error) {
	v, ok := c.Query[member]
	if !ok {
		return "", false, nil
	}
	if len(v) != 1 || v[0] == "" {
		return "", true, invalid("1 validation error detected: Value at '%s' failed to satisfy constraint: Member must have length greater than or equal to 1", field)
	}
	return v[0], true, nil
}

// listFunctions implements ListFunctions over $LATEST. Pages hold at most
// function.MaxListPage functions, and may be short or empty while a
// NextMarker remains, since each owner request also bounds its scan.
func (d Deps) listFunctions(ctx context.Context, c *call) (reply, error) {
	if c.Query.Has("MasterRegion") {
		return reply{}, unsupported("MasterRegion")
	}
	if v, ok, err := singleQuery(c, "FunctionVersion", "functionVersion"); err != nil {
		return reply{}, err
	} else if ok && v != lambda.FunctionVersionAll {
		return reply{}, invalid("1 validation error detected: Value '%s' at 'functionVersion' failed to satisfy constraint: Member must satisfy enum value set: [ALL]", v)
	}
	maxItems := function.MaxListPage
	if v, ok, err := singleQuery(c, "MaxItems", "maxItems"); err != nil {
		return reply{}, err
	} else if ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListItems {
			return reply{}, invalid("MaxItems must be between 1 and %d", maxListItems)
		}
		maxItems = min(n, function.MaxListPage)
	}
	marker, _, err := singleQuery(c, "Marker", "marker")
	if err != nil {
		return reply{}, err
	}

	listed, next, err := d.Meta.List(ctx, c.AccountID, c.Region, marker, maxItems)
	if err != nil {
		if _, coded := awserrors.ResolveErrorCode(err); coded {
			return reply{}, err
		}
		slog.ErrorContext(ctx, "Lambda: list owner request failed", "err", err)
		return reply{}, serviceError("the functions could not be listed; retry the request")
	}
	out := &lambda.ListFunctionsOutput{Functions: make([]*lambda.FunctionConfiguration, 0, len(listed))}
	for _, l := range listed {
		cfg, err := d.render(ctx, l.Record, l.Revision)
		if err != nil {
			return reply{}, err
		}
		// ListFunctions returns a subset of the configuration: AWS leaves the
		// state and last-update members to GetFunction.
		cfg.State, cfg.StateReason, cfg.StateReasonCode = nil, nil, nil
		cfg.LastUpdateStatus, cfg.LastUpdateStatusReason, cfg.LastUpdateStatusReasonCode = nil, nil, nil
		out.Functions = append(out.Functions, cfg)
	}
	if next != "" {
		out.NextMarker = aws.String(next)
	}
	return reply{Status: http.StatusOK, Body: out}, nil
}

// deleteFunction implements DeleteFunction for a whole function. Any explicit
// qualifier is refused before anything is read, so a request scoped to a
// version can never delete the function.
func (d Deps) deleteFunction(ctx context.Context, c *call) (reply, error) {
	if c.Delete == nil {
		return reply{}, serviceError("delete request was not validated")
	}
	if q := c.Delete.Qualifier; q != "" {
		if q == "$LATEST" {
			return reply{}, invalid("$LATEST version cannot be deleted without deleting the function.")
		}
		return reply{}, notFound(c.Region, c.AccountID, c.Delete.Name+":"+q)
	}
	rec, revision, err := d.lookupTarget(ctx, c, c.Delete.Name, "")
	if err != nil {
		return reply{}, err
	}
	if err := d.Meta.Delete(ctx, c.AccountID, c.Region, rec, revision); err != nil {
		// A lost reply may follow the unbinding; the caller reads the function to learn which.
		if _, coded := awserrors.ResolveErrorCode(err); coded {
			return reply{}, err
		}
		slog.ErrorContext(ctx, "Lambda: delete owner request failed", "function_id", rec.FunctionID, "err", err)
		return reply{}, serviceError("the function could not be deleted; retry the request")
	}
	return reply{Status: http.StatusNoContent}, nil
}
