package awsapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
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

const (
	rollbackDeadline = 10 * time.Second
	lastModifiedFmt  = "2006-01-02T15:04:05.000-0700"
)

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func serviceError(msg string) error {
	return awserrors.Errorf(awserrors.ErrorServiceException, "%s", msg)
}

// createFunction implements CreateFunction. The name is claimed first, the code
// copy written second, and the claim completed last, so a failure at any point
// leaves either a free name or a claim that expires on its own.
func (d Deps) createFunction(ctx context.Context, c *call) (reply, error) {
	p, err := validateCreate(c.Create)
	if err != nil {
		return reply{}, err
	}
	functionID, err := randomID()
	if err != nil {
		return reply{}, serviceError("could not generate a function identifier")
	}
	revisionID, err := randomID()
	if err != nil {
		return reply{}, serviceError("could not generate a revision identifier")
	}
	var envCipher string
	if len(p.Env) > 0 {
		if d.AEAD == nil {
			return reply{}, serviceError("environment encryption is unavailable")
		}
		if envCipher, err = function.SealEnv(d.AEAD, c.AccountID, functionID, p.Env); err != nil {
			slog.ErrorContext(ctx, "Lambda: seal environment failed", "function_id", functionID, "err", err)
			return reply{}, serviceError("could not protect the function environment")
		}
	}

	rec := function.Record{
		FormatVersion: function.FormatVersion,
		FunctionID:    functionID,
		AccountID:     c.AccountID,
		Region:        c.Region,
		FunctionName:  p.Name,
		Role:          p.Role,
		Runtime:       supportedRuntime,
		Handler:       p.Handler,
		Description:   p.Description,
		Timeout:       p.TimeoutSec,
		MemorySize:    p.MemorySize,
		EnvCipher:     envCipher,
		State:         function.StatePending,
		Code:          function.Code{RevisionID: revisionID, State: function.CodeStoring},
	}
	createRev, err := d.Meta.Create(ctx, c.AccountID, c.Region, rec)
	if err != nil {
		return reply{}, err
	}

	ref := codestore.Ref{AccountID: c.AccountID, FunctionID: functionID, RevisionID: revisionID}
	store, err := d.Codes(ctx)
	if err != nil {
		d.rollback(ctx, c, p.Name, createRev, ref, "code store unavailable", err)
		return reply{}, serviceError("the function code could not be stored")
	}
	sha, err := store.Put(ctx, ref, p.Code)
	if err != nil {
		d.rollback(ctx, c, p.Name, createRev, ref, "code write failed", err)
		return reply{}, serviceError("the function code could not be stored")
	}
	stored, storedRev, err := d.Meta.CodeStored(ctx, c.AccountID, c.Region, p.Name, createRev, sha, int64(len(p.Code)))
	if err != nil {
		d.rollback(ctx, c, p.Name, createRev, ref, "code-stored was not recorded", err)
		return reply{}, serviceError("the function could not be completed")
	}

	cfg, err := configuration(stored, storedRev, p.Env)
	if err != nil {
		return reply{}, serviceError("could not build the function configuration")
	}
	return reply{Status: http.StatusCreated, Body: cfg}, nil
}

// rollback makes one compare-and-swap attempt to free the name. It cannot undo
// a create that completed, because the revision it holds has moved on. The code
// object, if one landed, is left in place and recorded as a cleanup obligation.
func (d Deps) rollback(ctx context.Context, c *call, name string, createRev uint64, ref codestore.Ref, why string, cause error) {
	slog.ErrorContext(ctx, "Lambda: create did not complete", "reason", why, "function_name", name,
		"function_id", ref.FunctionID, "err", cause)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackDeadline)
	defer cancel()
	if err := d.Meta.RollbackCreate(rctx, c.AccountID, c.Region, name, createRev); err != nil {
		slog.WarnContext(ctx, "Lambda: rollback of the name claim failed; it expires on its own",
			"function_name", name, "function_id", ref.FunctionID, "err", err)
	}
	slog.WarnContext(ctx, "Lambda: code object cleanup is owed",
		"account_id", ref.AccountID, "function_id", ref.FunctionID, "revision_id", ref.RevisionID)
}

// configuration renders the record as a FunctionConfiguration.
func configuration(rec function.Record, revision uint64, env map[string]string) (*lambda.FunctionConfiguration, error) {
	cfg := &lambda.FunctionConfiguration{
		FunctionName:     aws.String(rec.FunctionName),
		FunctionArn:      aws.String(arn.FormatLambdaFunction(rec.Region, rec.AccountID, rec.FunctionName)),
		Runtime:          aws.String(rec.Runtime),
		Role:             aws.String(rec.Role),
		Handler:          aws.String(rec.Handler),
		Description:      aws.String(rec.Description),
		Timeout:          aws.Int64(rec.Timeout),
		MemorySize:       aws.Int64(rec.MemorySize),
		LastModified:     aws.String(rec.LastModified.UTC().Format(lastModifiedFmt)),
		Version:          aws.String("$LATEST"),
		State:            aws.String(rec.State),
		PackageType:      aws.String(lambda.PackageTypeZip),
		Architectures:    aws.StringSlice([]string{lambda.ArchitectureX8664}),
		RevisionId:       aws.String(strconv.FormatUint(revision, 10)),
		LastUpdateStatus: aws.String(lambda.LastUpdateStatusSuccessful),
		EphemeralStorage: &lambda.EphemeralStorage{Size: aws.Int64(512)},
		LoggingConfig:    &lambda.LoggingConfig{LogFormat: aws.String(lambda.LogFormatText), LogGroup: aws.String(logGroup(rec.FunctionName))},
	}
	if rec.StateReason != "" {
		cfg.StateReason = aws.String(rec.StateReason)
		cfg.StateReasonCode = aws.String(rec.StateReasonCode)
	}
	if rec.Code.State == function.CodeStored {
		raw, err := hex.DecodeString(rec.Code.SHA256)
		if err != nil {
			return nil, fmt.Errorf("stored digest is not hex: %w", err)
		}
		cfg.CodeSha256 = aws.String(base64.StdEncoding.EncodeToString(raw))
		cfg.CodeSize = aws.Int64(rec.Code.Size)
	}
	if len(env) > 0 {
		vars := make(map[string]*string, len(env))
		for k, v := range env {
			vars[k] = aws.String(v)
		}
		cfg.Environment = &lambda.EnvironmentResponse{Variables: vars}
	}
	return cfg, nil
}
