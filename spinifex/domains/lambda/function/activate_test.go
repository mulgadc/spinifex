package function_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	actAccount = "000000000001"
	actRegion  = "ap-southeast-2"
	actDigest  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func newOwner(t *testing.T) *function.MetaService {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	return function.NewMetaService(testutil.NewJetStream(t, nc))
}

func create(t *testing.T, svc *function.MetaService, name string) uint64 {
	t.Helper()
	resp, err := svc.Create(t.Context(), &function.CreateRequest{Region: actRegion, Record: function.Record{
		FunctionID: "id-" + name, FunctionName: name, Role: "arn:aws:iam::000000000001:role/r",
		Runtime: "provided.al2023", Handler: "bootstrap", Timeout: 3, MemorySize: 128,
		Code: function.Code{RevisionID: "rev-" + name},
	}}, actAccount)
	require.NoError(t, err)
	return resp.Revision
}

func get(t *testing.T, svc *function.MetaService, name string) *function.GetResponse {
	t.Helper()
	resp, err := svc.Get(t.Context(), &function.GetRequest{Region: actRegion, FunctionName: name}, actAccount)
	require.NoError(t, err)
	return resp
}

func storeCode(t *testing.T, svc *function.MetaService, name string, rev uint64) {
	t.Helper()
	_, err := svc.CodeStored(t.Context(), &function.CodeStoredRequest{Region: actRegion, FunctionName: name, Revision: rev, SHA256: actDigest, Size: 9}, actAccount)
	require.NoError(t, err)
}

func TestActivation_WithoutReadinessNothingLeavesPending(t *testing.T) {
	svc := newOwner(t)
	storeCode(t, svc, "hello", create(t, svc, "hello"))

	require.NoError(t, svc.ActivatePending(t.Context()))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	svc.RunActivation(ctx, time.Millisecond)

	got := get(t, svc, "hello").Record
	assert.Equal(t, function.StatePending, got.State)
	assert.Equal(t, function.ReasonCreating, got.StateReason)
}

func TestActivation_UnmetConditionStaysPendingWithItsReason(t *testing.T) {
	svc := newOwner(t)
	storeCode(t, svc, "hello", create(t, svc, "hello"))
	svc.SetReadiness(func(context.Context, function.Record) error { return errors.New("The node is not serving.") })

	require.NoError(t, svc.ActivatePending(t.Context()))
	first := get(t, svc, "hello")
	assert.Equal(t, function.StatePending, first.Record.State)
	assert.Equal(t, function.ReasonCodeCreating, first.Record.StateReasonCode)
	assert.Equal(t, "The node is not serving.", first.Record.StateReason)

	require.NoError(t, svc.ActivatePending(t.Context()))
	assert.Equal(t, first.Revision, get(t, svc, "hello").Revision, "an unchanged reason is not rewritten")
}

func TestActivation_ReadyStoredFunctionBecomesActive(t *testing.T) {
	svc := newOwner(t)
	storeCode(t, svc, "hello", create(t, svc, "hello"))
	storingRev := create(t, svc, "storing")
	var seen []string
	svc.SetReadiness(func(_ context.Context, rec function.Record) error {
		seen = append(seen, rec.FunctionName)
		return nil
	})

	require.NoError(t, svc.ActivatePending(t.Context()))

	got := get(t, svc, "hello").Record
	assert.Equal(t, function.StateActive, got.State)
	assert.Empty(t, got.StateReason)
	assert.Empty(t, got.StateReasonCode)
	assert.Equal(t, function.CodeStored, got.Code.State)
	storing := get(t, svc, "storing")
	assert.Equal(t, function.StatePending, storing.Record.State, "a function still storing its code is never activated")
	assert.Equal(t, storingRev, storing.Revision)
	assert.Equal(t, []string{"hello"}, seen)
}

func TestActivation_CodeStoredWakesTheLoop(t *testing.T) {
	svc := newOwner(t)
	svc.SetReadiness(func(context.Context, function.Record) error { return nil })
	storeCode(t, svc, "first", create(t, svc, "first"))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); svc.RunActivation(ctx, time.Hour) }()
	t.Cleanup(func() { cancel(); <-done })
	require.Eventually(t, func() bool { return get(t, svc, "first").Record.State == function.StateActive },
		10*time.Second, 20*time.Millisecond, "the start pass activates what was already stored")

	storeCode(t, svc, "hello", create(t, svc, "hello"))

	require.Eventually(t, func() bool { return get(t, svc, "hello").Record.State == function.StateActive },
		10*time.Second, 20*time.Millisecond, "after the start pass, only the code-stored wake can activate within the hour")
}
