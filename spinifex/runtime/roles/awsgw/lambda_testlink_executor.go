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
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
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

// testLinkExecutor sends each invocation to the test-link daemon on Node.
type testLinkExecutor struct {
	NATS  natsRequester
	Node  string
	Codes func(context.Context) (codePresigner, error)
}

var _ awsapi.Executor = (*testLinkExecutor)(nil)

// InvokeSync refuses anything it cannot send whole. Once the request is on the
// wire, every failure is an unknown outcome, never a refusal.
func (e *testLinkExecutor) InvokeSync(ctx context.Context, in awsapi.SyncInvocation) (awsapi.SyncResult, error) {
	rec := in.Function
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
	body, err := json.Marshal(testlink.Request{
		AttemptID:   attemptID,
		RequestID:   requestID,
		FunctionARN: arn.FormatLambdaFunction(rec.Region, rec.AccountID, rec.FunctionName),
		Region:      rec.Region,
		AccountID:   rec.AccountID,
		FunctionID:  rec.FunctionID,
		TimeoutMs:   rec.Timeout * 1000,
		MemoryMiB:   rec.MemorySize,
		CodeURL:     url,
		CodeSize:    rec.Code.Size,
		CodeSHA256:  rec.Code.SHA256,
		Payload:     in.Payload,
	})
	if err != nil {
		return awsapi.SyncResult{}, &awsapi.RefusalError{Err: errors.New("encode test-link request")}
	}
	// The encoded request, base64 payload included, must fit one NATS message.
	if limit := e.NATS.MaxPayload(); int64(len(body)) > limit {
		return awsapi.SyncResult{}, &awsapi.RefusalError{Err: fmt.Errorf("encoded request of %d bytes exceeds the %d-byte message limit", len(body), limit)}
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
