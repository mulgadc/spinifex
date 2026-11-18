package awsapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// updateFunctionConfiguration implements UpdateFunctionConfiguration. Omitted
// members keep the function's values; the merged settings commit by
// compare-and-swap at the revision read, so a competing change is refused.
func (d Deps) updateFunctionConfiguration(ctx context.Context, c *call) (reply, error) {
	name, q, err := resolveFunctionName(c.Region, c.AccountID, c.Params[0])
	if err != nil {
		return reply{}, err
	}
	if q != "" {
		return reply{}, invalidFunctionName()
	}
	u, err := validateUpdateConfiguration(c.UpdateConfig, name)
	if err != nil {
		return reply{}, err
	}
	rec, revision, err := d.lookupTarget(ctx, c, name, "")
	if err != nil {
		return reply{}, err
	}
	if rec.State != function.StateActive || rec.Code.State != function.CodeStored {
		return reply{}, awserrors.Errorf(awserrors.ErrorResourceConflictException,
			"The operation cannot be performed at this time. The function is currently in the following state: %s", rec.State)
	}
	if in := c.UpdateConfig; in.RevisionId != nil && aws.StringValue(in.RevisionId) != strconv.FormatUint(revision, 10) {
		return reply{}, awserrors.Errorf(awserrors.ErrorPreconditionFailedException,
			"The Revision Id provided does not match the latest Revision Id. Call the GetFunction/GetAlias API to retrieve the latest Revision Id")
	}

	cfg := function.Configuration{Role: rec.Role, Handler: rec.Handler, Description: rec.Description,
		Timeout: rec.Timeout, MemorySize: rec.MemorySize, EnvCipher: rec.EnvCipher}
	if u.Role != nil {
		cfg.Role = *u.Role
	}
	if u.Handler != nil {
		cfg.Handler = *u.Handler
	}
	if u.Description != nil {
		cfg.Description = *u.Description
	}
	if u.TimeoutSec != nil {
		cfg.Timeout = *u.TimeoutSec
	}
	if u.MemorySize != nil {
		cfg.MemorySize = *u.MemorySize
	}
	// The response environment is settled before the commit: a new one is
	// sealed to this function, a kept one opened, so no write precedes a failure.
	var env map[string]string
	switch {
	case u.SetEnv:
		env = u.Env
		cfg.EnvCipher = ""
		if len(env) > 0 {
			if d.AEAD == nil {
				return reply{}, serviceError("environment encryption is unavailable")
			}
			if cfg.EnvCipher, err = function.SealEnv(d.AEAD, rec.AccountID, rec.FunctionID, env); err != nil {
				slog.ErrorContext(ctx, "Lambda: seal environment failed", "function_id", rec.FunctionID, "err", err)
				return reply{}, serviceError("could not protect the function environment")
			}
		}
	case rec.EnvCipher != "":
		if d.AEAD == nil {
			return reply{}, serviceError("environment decryption is unavailable")
		}
		if env, err = function.OpenEnv(d.AEAD, rec.AccountID, rec.FunctionID, rec.EnvCipher); err != nil {
			slog.ErrorContext(ctx, "Lambda: open environment failed", "function_id", rec.FunctionID, "err", err)
			return reply{}, serviceError("could not read the function environment")
		}
	}

	updated, updatedRev, err := d.Meta.UpdateConfiguration(ctx, c.AccountID, c.Region, rec, revision, cfg)
	if err != nil {
		// A lost reply may follow a commit; the caller reads the function to learn which.
		if _, coded := awserrors.ResolveErrorCode(err); coded {
			return reply{}, err
		}
		slog.ErrorContext(ctx, "Lambda: configuration update owner request failed", "function_id", rec.FunctionID, "err", err)
		return reply{}, serviceError("the function configuration could not be updated; retry the request")
	}
	out, err := configuration(updated, updatedRev, env)
	if err != nil {
		return reply{}, serviceError("could not build the function configuration")
	}
	return reply{Status: http.StatusOK, Body: out}, nil
}
