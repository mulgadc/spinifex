package awsapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/codestore"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// ownerError passes an AWS-coded owner refusal through and turns anything
// else, such as an owner with no responder for the subject, into a retryable
// ServiceException, logging the stage that failed.
func ownerError(ctx context.Context, stage string, rec function.Record, codeRevisionID string, err error) error {
	if _, coded := awserrors.ResolveErrorCode(err); coded {
		return err
	}
	slog.ErrorContext(ctx, "Lambda: code update owner request failed", "stage", stage,
		"function_id", rec.FunctionID, "revision_id", codeRevisionID, "err", err)
	return serviceError("the function code could not be updated; retry the request")
}

// updateFunctionCode implements UpdateFunctionCode for an inline ZIP. The new
// copy is written under a fresh code revision while the record still names the
// old one, and the record moves to it only by compare-and-swap.
func (d Deps) updateFunctionCode(ctx context.Context, c *call) (reply, error) {
	in, err := decodeUpdateCode(c.Body)
	if err != nil {
		return reply{}, err
	}
	name, q, err := resolveFunctionName(c.Region, c.AccountID, c.Params[0])
	if err != nil {
		return reply{}, err
	}
	if q != "" {
		return reply{}, invalidFunctionName()
	}
	if err := validateUpdateCode(in); err != nil {
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
	if want := aws.StringValue(in.RevisionId); in.RevisionId != nil && want != strconv.FormatUint(revision, 10) {
		return reply{}, awserrors.Errorf(awserrors.ErrorPreconditionFailedException,
			"The Revision Id provided does not match the latest Revision Id. Call the GetFunction/GetAlias API to retrieve the latest Revision Id")
	}
	// The environment is opened before anything is written, so a commit is
	// never followed by a response that cannot be rendered.
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
	if aws.BoolValue(in.DryRun) {
		cfg, err := configuration(rec, revision, env)
		if err != nil {
			return reply{}, serviceError("could not build the function configuration")
		}
		return reply{Status: http.StatusOK, Body: cfg}, nil
	}

	codeRevisionID, err := randomID()
	if err != nil {
		return reply{}, serviceError("could not generate a revision identifier")
	}
	if err := d.Meta.BeginCodeUpdate(ctx, c.AccountID, c.Region, rec, revision, codeRevisionID); err != nil {
		return reply{}, ownerError(ctx, "begin", rec, codeRevisionID, err)
	}
	ref := codestore.Ref{AccountID: rec.AccountID, FunctionID: rec.FunctionID, RevisionID: codeRevisionID}
	store, err := d.Codes(ctx)
	if err != nil {
		owedCopy(ctx, ref, "code store unavailable", err)
		return reply{}, serviceError("the function code could not be stored")
	}
	sha, err := store.Put(ctx, ref, in.ZipFile)
	if err != nil {
		owedCopy(ctx, ref, "code write failed", err)
		return reply{}, serviceError("the function code could not be stored")
	}
	updated, updatedRev, err := d.Meta.CodeUpdated(ctx, c.AccountID, c.Region, rec, revision, codeRevisionID, sha, int64(len(in.ZipFile)))
	if err != nil {
		// The commit may have landed with its reply lost; nothing is undone.
		owedCopy(ctx, ref, "commit not confirmed", err)
		return reply{}, ownerError(ctx, "commit", rec, codeRevisionID, err)
	}
	cfg, err := configuration(updated, updatedRev, env)
	if err != nil {
		return reply{}, serviceError("could not build the function configuration")
	}
	return reply{Status: http.StatusOK, Body: cfg}, nil
}

// owedCopy logs a code update whose new copy may exist without the function
// naming it; its upload record stays owed for later cleanup.
func owedCopy(ctx context.Context, ref codestore.Ref, why string, cause error) {
	slog.WarnContext(ctx, "Lambda: code update did not complete; code object cleanup may be owed", "reason", why,
		"account_id", ref.AccountID, "function_id", ref.FunctionID, "revision_id", ref.RevisionID, "err", cause)
}
