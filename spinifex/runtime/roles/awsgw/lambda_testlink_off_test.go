//go:build !lambdatestlink

package awsgw

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	lambdaawsapi "github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	"github.com/stretchr/testify/require"
)

// spyFunctions counts every lookup and serves one Active function with stored code.
type spyFunctions struct{ gets atomic.Int32 }

var _ function.Client = (*spyFunctions)(nil)

func (s *spyFunctions) Get(_ context.Context, account, region, name string) (function.Record, uint64, error) {
	s.gets.Add(1)
	return function.Record{
		FunctionID: "f1", AccountID: account, Region: region, FunctionName: name, Runtime: "provided.al2023",
		Timeout: 3, MemorySize: 128, State: function.StateActive,
		Code: function.Code{RevisionID: "r1", State: function.CodeStored},
	}, 1, nil
}

func (*spyFunctions) Create(context.Context, string, string, function.Record) (uint64, error) {
	return 0, errors.New("unused")
}

func (*spyFunctions) CodeStored(context.Context, string, string, string, uint64, string, int64) (function.Record, uint64, error) {
	return function.Record{}, 0, errors.New("unused")
}

func (*spyFunctions) RollbackCreate(context.Context, string, string, string, uint64) error {
	return errors.New("unused")
}

// spyExec counts invocations that would start an attempt.
type spyExec struct{ calls atomic.Int32 }

func (e *spyExec) InvokeSync(context.Context, lambdaawsapi.SyncInvocation) (lambdaawsapi.SyncResult, error) {
	e.calls.Add(1)
	return lambdaawsapi.SyncResult{Payload: []byte(`{}`), ExecutedVersion: "$LATEST"}, nil
}

// A default build composes the gateway with Lambda unrouted and no executor: a
// signed Invoke is refused before any Lambda handler runs and starts no attempt.
func TestDefaultBuild_InvokeNeverReachesLambda(t *testing.T) {
	require.Nil(t, lambdaTestExecutor(nil, "node1", nil, nil, nil), "a default build has no Lambda executor")

	meta, exec := &spyFunctions{}, &spyExec{}
	lambda := lambdaawsapi.NewRegistration(lambdaawsapi.Deps{Meta: meta, Exec: exec})

	resp := signedLambdaInvoke(t, defaultGateway(t, lambdaTestRoutes(lambda)...))
	require.NotEqual(t, http.StatusOK, resp.StatusCode, "a default build answered Invoke")
	require.Zero(t, meta.gets.Load(), "a default build ran a Lambda handler")
	require.Zero(t, exec.calls.Load(), "a default build started an attempt")

	// The same signed request reaches both once Lambda is routed, so the
	// refusal above comes from the missing route.
	resp = signedLambdaInvoke(t, defaultGateway(t, lambda))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, int32(1), meta.gets.Load())
	require.Equal(t, int32(1), exec.calls.Load())
}
