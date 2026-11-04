package awsapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
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

// maxListItems is the largest MaxItems a list action accepts.
const maxListItems = 10000

func notFound(region, accountID, name string) error {
	return awserrors.Errorf(awserrors.ErrorResourceNotFoundException, "Function not found: %s",
		arn.FormatLambdaFunction(region, accountID, name))
}

// lookup resolves the requested $LATEST function's record; query is the
// Qualifier query parameter, empty for an action that has none.
func (d Deps) lookup(ctx context.Context, c *call, query string) (function.Record, uint64, error) {
	name, q, err := resolveFunctionName(c.Region, c.AccountID, c.Params[0])
	if err != nil {
		return function.Record{}, 0, err
	}
	if query != "" {
		if q != "" && q != query {
			return function.Record{}, 0, invalid("The derived qualifier from the function name does not match the specified qualifier.")
		}
		q = query
	}
	return d.lookupTarget(ctx, c, name, q)
}

// lookupTarget reads the record for an already resolved name and qualifier;
// only $LATEST exists.
func (d Deps) lookupTarget(ctx context.Context, c *call, name, q string) (function.Record, uint64, error) {
	if q != "" && q != "$LATEST" {
		return function.Record{}, 0, notFound(c.Region, c.AccountID, name+":"+q)
	}
	rec, revision, err := d.Meta.Get(ctx, c.AccountID, c.Region, name)
	if errors.Is(err, function.ErrNotFound) {
		return function.Record{}, 0, notFound(c.Region, c.AccountID, name)
	}
	return rec, revision, err
}

// loadConfiguration resolves the requested $LATEST function and renders its
// configuration; it is a metadata read and never touches the code store.
func (d Deps) loadConfiguration(ctx context.Context, c *call, query string) (function.Record, *lambda.FunctionConfiguration, error) {
	rec, revision, err := d.lookup(ctx, c, query)
	if err != nil {
		return function.Record{}, nil, err
	}

	var env map[string]string
	if rec.EnvCipher != "" {
		if d.AEAD == nil {
			return function.Record{}, nil, serviceError("environment decryption is unavailable")
		}
		if env, err = function.OpenEnv(d.AEAD, rec.AccountID, rec.FunctionID, rec.EnvCipher); err != nil {
			slog.ErrorContext(ctx, "Lambda: open environment failed", "function_id", rec.FunctionID, "err", err)
			return function.Record{}, nil, serviceError("could not read the function environment")
		}
	}
	cfg, err := configuration(rec, revision, env)
	if err != nil {
		return function.Record{}, nil, serviceError("could not build the function configuration")
	}
	return rec, cfg, nil
}

// getFunctionConfiguration implements GetFunctionConfiguration for $LATEST.
func (d Deps) getFunctionConfiguration(ctx context.Context, c *call) (reply, error) {
	_, cfg, err := d.loadConfiguration(ctx, c, c.Query.Get("Qualifier"))
	if err != nil {
		return reply{}, err
	}
	return reply{Status: http.StatusOK, Body: cfg}, nil
}

// noCodeSigning is AWS's body for a function without a code signing config:
// both members present and null, which the SDK shapes cannot encode.
var noCodeSigning = rawJSON(`{"CodeSigningConfigArn":null,"FunctionName":null}`)

// getFunctionCodeSigningConfig implements GetFunctionCodeSigningConfig; code
// signing is unsupported, so it only proves the function exists.
func (d Deps) getFunctionCodeSigningConfig(ctx context.Context, c *call) (reply, error) {
	if _, _, err := d.lookup(ctx, c, ""); err != nil {
		return reply{}, err
	}
	return reply{Status: http.StatusOK, Body: noCodeSigning}, nil
}

// listVersionsByFunction implements ListVersionsByFunction while $LATEST is
// the only version. Paging is refused rather than guessed at.
func (d Deps) listVersionsByFunction(ctx context.Context, c *call) (reply, error) {
	if raw, ok := c.Query["MaxItems"]; ok {
		n, err := strconv.Atoi(raw[0])
		if err != nil || n < 1 || n > maxListItems {
			return reply{}, invalid("MaxItems must be between 1 and %d", maxListItems)
		}
	}
	if c.Query.Has("Marker") {
		return reply{}, unsupported("Marker")
	}
	rec, cfg, err := d.loadConfiguration(ctx, c, "")
	if err != nil {
		return reply{}, err
	}
	cfg.FunctionArn = aws.String(arn.FormatLambdaFunction(rec.Region, rec.AccountID, rec.FunctionName) + ":$LATEST")
	return reply{Status: http.StatusOK, Body: &lambda.ListVersionsByFunctionOutput{Versions: []*lambda.FunctionConfiguration{cfg}}}, nil
}

// getFunction implements GetFunction for the $LATEST version.
func (d Deps) getFunction(ctx context.Context, c *call) (reply, error) {
	rec, cfg, err := d.loadConfiguration(ctx, c, c.Query.Get("Qualifier"))
	if err != nil {
		return reply{}, err
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
