package function

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func storeCode(t *testing.T, svc *MetaService, name string, rev uint64) {
	t.Helper()
	_, err := svc.CodeStored(t.Context(), &CodeStoredRequest{Region: testRegion, FunctionName: name, Revision: rev, SHA256: testDigest, Size: 9}, testAccount)
	require.NoError(t, err)
}

func TestActivation_WithoutReadinessNothingLeavesPending(t *testing.T) {
	svc, _ := newTestService(t)
	storeCode(t, svc, "hello", mustCreate(t, svc, "hello", "f1"))

	require.NoError(t, svc.ActivatePending(t.Context()))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	svc.RunActivation(ctx, time.Millisecond)

	got := mustGet(t, svc, "hello").Record
	assert.Equal(t, StatePending, got.State)
	assert.Equal(t, ReasonCreating, got.StateReason)
}

func TestActivation_UnmetConditionStaysPendingWithItsReason(t *testing.T) {
	svc, _ := newTestService(t)
	storeCode(t, svc, "hello", mustCreate(t, svc, "hello", "f1"))
	svc.SetReadiness(func(context.Context, Record) error { return errors.New("The node is not serving.") })

	require.NoError(t, svc.ActivatePending(t.Context()))
	first := mustGet(t, svc, "hello")
	assert.Equal(t, StatePending, first.Record.State)
	assert.Equal(t, ReasonCodeCreating, first.Record.StateReasonCode)
	assert.Equal(t, "The node is not serving.", first.Record.StateReason)

	require.NoError(t, svc.ActivatePending(t.Context()))
	assert.Equal(t, first.Revision, mustGet(t, svc, "hello").Revision, "an unchanged reason is not rewritten")
}

func TestActivation_ReadyStoredFunctionBecomesActive(t *testing.T) {
	svc, _ := newTestService(t)
	storeCode(t, svc, "hello", mustCreate(t, svc, "hello", "f1"))
	storingRev := mustCreate(t, svc, "storing", "f2")
	var seen []string
	svc.SetReadiness(func(_ context.Context, rec Record) error {
		seen = append(seen, rec.FunctionName)
		return nil
	})

	require.NoError(t, svc.ActivatePending(t.Context()))

	got := mustGet(t, svc, "hello").Record
	assert.Equal(t, StateActive, got.State)
	assert.Empty(t, got.StateReason)
	assert.Empty(t, got.StateReasonCode)
	assert.Equal(t, CodeStored, got.Code.State)
	storing := mustGet(t, svc, "storing")
	assert.Equal(t, StatePending, storing.Record.State, "a function still storing its code is never activated")
	assert.Equal(t, storingRev, storing.Revision)
	assert.Equal(t, []string{"hello"}, seen)
}

func TestActivation_CodeStoredWakesTheLoop(t *testing.T) {
	svc, _ := newTestService(t)
	svc.SetReadiness(func(context.Context, Record) error { return nil })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); svc.RunActivation(ctx, time.Hour) }()
	t.Cleanup(func() { cancel(); <-done })

	storeCode(t, svc, "hello", mustCreate(t, svc, "hello", "f1"))

	require.Eventually(t, func() bool { return mustGet(t, svc, "hello").Record.State == StateActive },
		10*time.Second, 20*time.Millisecond, "activation must not wait for the hourly tick")
}
