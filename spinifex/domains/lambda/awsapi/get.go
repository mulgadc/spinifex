package awsapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/codestore"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// codeURLTTL is how long the presigned code location stays valid.
const codeURLTTL = 10 * time.Minute

func notFound(region, accountID, name string) error {
	return awserrors.Errorf(awserrors.ErrorResourceNotFoundException, "Function not found: %s",
		arn.FormatLambdaFunction(region, accountID, name))
}

// getFunction implements GetFunction for the $LATEST version.
func (d Deps) getFunction(ctx context.Context, c *call) (reply, error) {
	name, q, err := resolveFunctionName(c.Region, c.AccountID, c.Params[0])
	if err != nil {
		return reply{}, err
	}
	if query := c.Query.Get("Qualifier"); query != "" {
		if q != "" && q != query {
			return reply{}, invalid("The derived qualifier from the function name does not match the specified qualifier.")
		}
		q = query
	}
	if q != "" && q != "$LATEST" {
		return reply{}, notFound(c.Region, c.AccountID, name+":"+q)
	}
	rec, revision, err := d.Meta.Get(ctx, c.AccountID, c.Region, name)
	if errors.Is(err, function.ErrNotFound) {
		return reply{}, notFound(c.Region, c.AccountID, name)
	}
	if err != nil {
		return reply{}, err
	}

	var env map[string]string
	if rec.EnvCipher != "" {
		if d.AEAD == nil {
			return reply{}, serviceError("environment decryption is unavailable")
		}
		if env, err = function.OpenEnv(d.AEAD, rec.AccountID, rec.FunctionID, rec.EnvCipher); err != nil {
			slog.ErrorContext(ctx, "Lambda: open environment failed", "function_id", rec.FunctionID, "err", err)
			return reply{}, serviceError("could not read the function environment")
		}
	}
	cfg, err := configuration(rec, revision, env)
	if err != nil {
		return reply{}, serviceError("could not build the function configuration")
	}
	out := &lambda.GetFunctionOutput{
		Configuration: cfg,
		Code:          &lambda.FunctionCodeLocation{RepositoryType: aws.String("S3")},
	}
	if rec.Code.State == function.CodeStored {
		store, err := d.Codes(ctx)
		if err != nil {
			slog.ErrorContext(ctx, "Lambda: code store unavailable for GetFunction", "function_id", rec.FunctionID, "err", err)
			return reply{}, serviceError("the function code location is unavailable")
		}
		ref := codestore.Ref{AccountID: rec.AccountID, FunctionID: rec.FunctionID, RevisionID: rec.Code.RevisionID}
		location, err := store.PresignGet(ctx, ref, rec.Code.SHA256, codeURLTTL)
		if err != nil {
			slog.ErrorContext(ctx, "Lambda: presign code location failed", "function_id", rec.FunctionID, "err", err)
			return reply{}, serviceError("the function code location is unavailable")
		}
		out.Code.Location = aws.String(location)
	}
	return reply{Status: http.StatusOK, Body: out}, nil
}
