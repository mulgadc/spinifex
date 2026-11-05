//go:build lambdatestlink

package awsgw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"

	"github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/codestore"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/testlink"
)

// codePresigner makes the download link for one stored code copy.
type codePresigner interface {
	PresignGet(ctx context.Context, ref codestore.Ref, sha256Hex string, ttl time.Duration) (string, error)
}

// controlExchanger sends one request over the node-local control socket.
type controlExchanger interface {
	Exchange(ctx context.Context, req []byte, cred testlink.Credential, replyBy time.Time) (testlink.Reply, error)
}

var _ controlExchanger = (*testlink.ControlClient)(nil)

const (
	// revokeTimeout bounds the session revoke, which runs even after the
	// Invoke context has ended.
	revokeTimeout = 10 * time.Second
	// replyGrace covers the daemon's cleanup after the hard kill.
	replyGrace = 30 * time.Second
)

// grantIssuer records an attempt's grant and withdraws one never sent.
type grantIssuer interface {
	Issue(ctx context.Context, a execution.Attempt) (execution.Attempt, error)
	Withdraw(ctx context.Context, g execution.Grant) error
}

var _ grantIssuer = (*execution.Store)(nil)

// testLinkExecutor sends each invocation to the test-link daemon on Node.
type testLinkExecutor struct {
	Control controlExchanger
	// RequestLimit bounds the encoded request frame, without its credential.
	RequestLimit int64
	Sessions     lambdaEnvironmentRoleIssuer
	Node         string
	// ImageDigest is the runtime image every request requires of the daemon.
	ImageDigest string
	Codes  func(context.Context) (codePresigner, error)
	Grants func(context.Context) (grantIssuer, error)
	now    func() time.Time
}

var _ awsapi.Executor = (*testLinkExecutor)(nil)

// InvokeSync records the attempt's grant and issues its environment's session
// before sending, and refuses anything it cannot send whole. Once any request
// byte may be on the socket, every failure is unknown, never a refusal; the
// session is revoked on every path after it is issued.
func (e *testLinkExecutor) InvokeSync(ctx context.Context, in awsapi.SyncInvocation) (awsapi.SyncResult, error) {
	rec := in.Function
	if !testlink.ValidDigest(e.ImageDigest) {
		return awsapi.SyncResult{}, &awsapi.RefusalError{Err: awserrors.Errorf(awserrors.ErrorServiceException,
			"the test execution path has no valid runtime image pin")}
	}
	// The test link cannot deliver environment variables, so it refuses a
	// function that has any before presigning, granting or sending.
	if rec.EnvCipher != "" {
		return awsapi.SyncResult{}, &awsapi.RefusalError{Err: awserrors.Errorf(awserrors.ErrorServiceException,
			"the test execution path does not support function environment variables")}
	}
	if rec.Code.SHA256 == "" || rec.Code.Size <= 0 || rec.Code.Size > testlink.MaxCodeBytes {
		return awsapi.SyncResult{}, &awsapi.RefusalError{Err: fmt.Errorf("code copy of %d bytes is outside the test bound", rec.Code.Size)}
	}
	store, err := e.Codes(ctx)
	if err != nil {
		return awsapi.SyncResult{}, &awsapi.RefusalError{Err: fmt.Errorf("code store: %w", err)}
	}
	ref := codestore.Ref{AccountID: rec.AccountID, FunctionID: rec.FunctionID, RevisionID: rec.Code.RevisionID}
	url, err := store.PresignGet(ctx, ref, rec.Code.SHA256, testlink.CodeURLTTL)
	if err != nil {
		return awsapi.SyncResult{}, &awsapi.RefusalError{Err: errors.New("code download link unavailable")}
	}
	attemptID, err := testLinkID()
	if err != nil {
		return awsapi.SyncResult{}, &awsapi.RefusalError{Err: err}
	}
	requestID, err := testLinkID()
	if err != nil {
		return awsapi.SyncResult{}, &awsapi.RefusalError{Err: err}
	}
	grants, err := e.Grants(ctx)
	if err != nil {
		return awsapi.SyncResult{}, &awsapi.RefusalError{Err: fmt.Errorf("execution owner: %w", err)}
	}
	now := time.Now
	if e.now != nil {
		now = e.now
	}
	// Nothing has been sent yet, so even a grant write of unknown outcome is
	// a refusal; a landed grant expires unstarted at its launch deadline.
	att, err := grants.Issue(ctx, execution.Attempt{
		Grant: execution.Grant{AttemptID: attemptID, EnvironmentID: "env-" + attemptID, Generation: 1, Node: e.Node},
		Identity: execution.Identity{RequestID: requestID, AccountID: rec.AccountID, Region: rec.Region,
			FunctionID: rec.FunctionID, RevisionID: rec.Code.RevisionID, CodeSHA256: rec.Code.SHA256},
		LaunchBy: now().Add(testlink.LaunchWindow),
	})
	if err != nil {
		return awsapi.SyncResult{}, &awsapi.RefusalError{Err: fmt.Errorf("attempt grant: %w", err)}
	}
	refuse := func(err error) (awsapi.SyncResult, error) {
		if werr := grants.Withdraw(ctx, att.Grant); werr != nil {
			err = fmt.Errorf("%w; grant not withdrawn: %w", err, werr)
		}
		return awsapi.SyncResult{}, &awsapi.RefusalError{Err: err}
	}

	lifetime := testlink.AttemptLifetime(rec.Timeout * 1000)
	scope := stsdomain.LambdaEnvironmentSessionScope{
		AccountID:     rec.AccountID,
		FunctionID:    rec.FunctionID,
		FunctionARN:   arn.FormatLambdaFunction(rec.Region, rec.AccountID, rec.FunctionName),
		EnvironmentID: att.Grant.EnvironmentID,
	}
	session, err := e.Sessions.AssumeRoleForLambdaEnvironment(ctx, stsdomain.LambdaEnvironmentSessionRequest{
		AccountID:          scope.AccountID,
		RoleARN:            rec.Role,
		FunctionARN:        scope.FunctionARN,
		FunctionID:         scope.FunctionID,
		EnvironmentID:      scope.EnvironmentID,
		MaxLifetimeSeconds: int64((lifetime + time.Second - 1) / time.Second),
	})
	if err != nil {
		return refuse(fmt.Errorf("execution role session: %w", err))
	}
	cred := testlink.Credential{
		AccessKeyID:     aws.StringValue(session.Credentials.AccessKeyId),
		SecretAccessKey: aws.StringValue(session.Credentials.SecretAccessKey),
		SessionToken:    aws.StringValue(session.Credentials.SessionToken),
	}
	revoke := func() error {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
		defer cancel()
		err := e.Sessions.RevokeLambdaEnvironmentSession(rctx, cred.AccessKeyID, scope)
		if err != nil {
			slog.Warn("Lambda test link: execution session not revoked; it may remain valid until expiry",
				"attempt_id", attemptID, "environment_id", scope.EnvironmentID, "err", err)
		}
		return err
	}

	// The raw payload is checked before encoding allocates its base64 copy.
	if len(in.Payload) > testlink.MaxPayloadBytes {
		_ = revoke()
		return refuse(fmt.Errorf("payload of %d bytes exceeds %d", len(in.Payload), testlink.MaxPayloadBytes))
	}
	body, err := json.Marshal(testlink.Request{
		Grant:       att.Grant,
		AttemptID:   attemptID,
		RequestID:   requestID,
		FunctionARN: invokedARN(rec, in.Qualifier),
		Handler:     rec.Handler,
		Region:      rec.Region,
		AccountID:   rec.AccountID,
		FunctionID:  rec.FunctionID,
		RevisionID:  rec.Code.RevisionID,
		TimeoutMs:   rec.Timeout * 1000,
		MemoryMiB:   rec.MemorySize,
		CodeURL:     url,
		CodeSize:    rec.Code.Size,
		CodeSHA256:  rec.Code.SHA256,
		ImageDigest: e.ImageDigest,
		Payload:     in.Payload,
	})
	if err != nil {
		_ = revoke()
		return refuse(errors.New("encode test-link request"))
	}
	// The encoded frame, base64 payload included, is checked after encoding.
	if int64(len(body)) > e.RequestLimit {
		_ = revoke()
		return refuse(fmt.Errorf("encoded request of %d bytes exceeds the %d-byte message limit", len(body), e.RequestLimit))
	}
	rep, err := e.Control.Exchange(ctx, body, cred, now().Add(lifetime+replyGrace))
	revokeErr := revoke()
	if err != nil {
		if errors.Is(err, testlink.ErrNotSent) {
			return refuse(err)
		}
		return awsapi.SyncResult{}, fmt.Errorf("test-link attempt %s: no reply: %w", attemptID, err)
	}
	switch rep.Outcome {
	case testlink.OutcomeDone:
		if revokeErr != nil {
			return awsapi.SyncResult{}, fmt.Errorf("test-link attempt %s: execution session not revoked: %w", attemptID, revokeErr)
		}
		return awsapi.SyncResult{Payload: rep.Payload, FunctionError: rep.FunctionError, ExecutedVersion: "$LATEST"}, nil
	case testlink.OutcomeRefused:
		// Only the authenticated daemon sends refused, and only before it starts
		// QEMU. A grant it started it has ended itself, so withdrawing then
		// is refused and changes nothing.
		_ = grants.Withdraw(ctx, att.Grant)
		if revokeErr != nil {
			return awsapi.SyncResult{}, fmt.Errorf("test-link attempt %s: refused, execution session not revoked: %w", attemptID, revokeErr)
		}
		return awsapi.SyncResult{}, &awsapi.RefusalError{Err: fmt.Errorf("test-link daemon refused: %s", rep.Error)}
	default:
		return awsapi.SyncResult{}, fmt.Errorf("test-link attempt %s: outcome %q: %s", attemptID, rep.Outcome, rep.Error)
	}
}

func testLinkID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("attempt ID: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// invokedARN is the function ARN the caller invoked, with its qualifier.
func invokedARN(rec function.Record, qualifier string) string {
	a := arn.FormatLambdaFunction(rec.Region, rec.AccountID, rec.FunctionName)
	if qualifier != "" {
		a += ":" + qualifier
	}
	return a
}
