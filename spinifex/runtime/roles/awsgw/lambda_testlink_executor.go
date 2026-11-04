//go:build lambdatestlink

package awsgw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/codestore"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/testlink"
	"github.com/nats-io/nats.go"
)

// codePresigner makes the download link for one stored code copy.
type codePresigner interface {
	PresignGet(ctx context.Context, ref codestore.Ref, sha256Hex string, ttl time.Duration) (string, error)
}

// natsRequester sends one NATS request; *nats.Conn satisfies it.
type natsRequester interface {
	RequestWithContext(ctx context.Context, subj string, data []byte) (*nats.Msg, error)
	MaxPayload() int64
}

var _ natsRequester = (*nats.Conn)(nil)

// grantIssuer records an attempt's grant and withdraws one never sent.
type grantIssuer interface {
	Issue(ctx context.Context, a execution.Attempt) (execution.Attempt, error)
	Withdraw(ctx context.Context, g execution.Grant) error
}

var _ grantIssuer = (*execution.Store)(nil)

// testLinkExecutor sends each invocation to the test-link daemon on Node.
type testLinkExecutor struct {
	NATS   natsRequester
	Node   string
	Codes  func(context.Context) (codePresigner, error)
	Grants func(context.Context) (grantIssuer, error)
	now    func() time.Time
}

var _ awsapi.Executor = (*testLinkExecutor)(nil)

// InvokeSync records the attempt's grant before sending and refuses anything
// it cannot send whole. Once the request is on the wire, every failure is an
// unknown outcome, never a refusal.
func (e *testLinkExecutor) InvokeSync(ctx context.Context, in awsapi.SyncInvocation) (awsapi.SyncResult, error) {
	rec := in.Function
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
		Payload:     in.Payload,
	})
	if err != nil {
		return refuse(errors.New("encode test-link request"))
	}
	// The encoded request, base64 payload included, must fit one NATS message.
	if limit := e.NATS.MaxPayload(); int64(len(body)) > limit {
		return refuse(fmt.Errorf("encoded request of %d bytes exceeds the %d-byte message limit", len(body), limit))
	}
	msg, err := e.NATS.RequestWithContext(ctx, testlink.Subject(e.Node), body)
	if err != nil {
		return awsapi.SyncResult{}, fmt.Errorf("test-link attempt %s: no reply: %w", attemptID, err)
	}
	var rep testlink.Reply
	if err := json.Unmarshal(msg.Data, &rep); err != nil {
		return awsapi.SyncResult{}, fmt.Errorf("test-link attempt %s: unreadable reply", attemptID)
	}
	switch rep.Outcome {
	case testlink.OutcomeDone:
		return awsapi.SyncResult{Payload: rep.Payload, FunctionError: rep.FunctionError, ExecutedVersion: "$LATEST"}, nil
	case testlink.OutcomeRefused:
		// The daemon launched nothing. A grant it started it has ended itself,
		// so withdrawing is refused then and changes nothing.
		_ = grants.Withdraw(ctx, att.Grant)
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
