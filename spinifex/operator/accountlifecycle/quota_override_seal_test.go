package accountlifecycle

//test:in-package — the reaper, sealer and capability checker are unexported;
// no seal without a complete probe, completion only after a sealed reply and
// idempotent fresh runs are only observable from inside the package.

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	admissionv1 "github.com/mulgadc/spinifex/contracts/admission/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeQuotaSealer is a recording quotaOverrideSealer. Each call beyond failFor
// succeeds with a fresh, strictly increasing revision, unless err is set, in
// which case every call after failFor fails with it.
type fakeQuotaSealer struct {
	mu       sync.Mutex
	calls    []string
	failFor  int
	err      error
	revision uint64
}

func (f *fakeQuotaSealer) SealAccountQuotaOverride(_ context.Context, accountID string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, accountID)
	if len(f.calls) <= f.failFor {
		return 0, errors.New("quota owner temporarily unavailable")
	}
	if f.err != nil {
		return 0, f.err
	}
	f.revision++
	return f.revision, nil
}

func incompleteReport(node string) sealCapabilityReport {
	return sealCapabilityReport{Nodes: []nodeSealCapability{{Node: node, Reason: sealCapabilityReasonNoReply}}}
}

// No subscriber on either subject makes the capability probe incomplete, so no
// seal request is sent and the account stays TERMINATING with one stuck
// quota-override item.
func TestQuotaSealReaperNoResponderKeepsAccountTerminating(t *testing.T) {
	t.Parallel()
	_, nc, _ := testutil.StartTestJetStream(t)

	capability := newQuotaSealCapabilityClient(nc, []string{"gw-1"})
	capability.wait = 50 * time.Millisecond
	capability.grace = 10 * time.Millisecond
	sealer := newQuotaOverrideSealClient(nc)
	sealer.wait = 50 * time.Millisecond

	accounts := newFakeAccounts()
	engine := quietEngine(accounts, &quotaOverrideSealReaper{sealer: sealer, capability: capability})

	result, err := engine.Teardown(testCtx(t), request())
	require.ErrorIs(t, err, ErrResourcesStuck)
	assert.False(t, result.AccountDeleted)
	assert.Equal(t, AccountStatusTerminating, accounts.account.Status)

	var stuck []Stuck
	for _, stage := range result.Stages {
		stuck = append(stuck, stage.Stuck...)
	}
	require.Len(t, stuck, 1)
	assert.Equal(t, "quota-override", stuck[0].Resource.Kind)
}

// An incomplete probe sends no seal at all.
func TestQuotaSealReaperIncompleteCapabilitySendsNoSeal(t *testing.T) {
	sealer := &fakeQuotaSealer{}
	reaper := &quotaOverrideSealReaper{sealer: sealer, capability: &fakeSealCapability{report: incompleteReport("gw-1")}}

	err := reaper.Delete(testCtx(t), testAccountID, Resource{}, true)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServiceUnavailable, awserrors.ValidErrorCodeFromError(err))
	assert.Contains(t, err.Error(), "quota override seal capability incomplete: gw-1="+sealCapabilityReasonNoReply)
	assert.Empty(t, sealer.calls)
}

// Whatever the owner refuses with, the account stays TERMINATING: an owner
// refusal, a timeout, and each reply shape the client rejects.
func TestQuotaSealReaperOwnerFailureKeepsAccountTerminating(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{"incorrect-state", errors.New(awserrors.ErrorIncorrectState)},
		{"internal-error", errors.New(awserrors.ErrorInternalError)},
		{"timeout", context.DeadlineExceeded},
		{"wrong-account", errors.New(`seal quota override: reply named account "999999999999", not "000000000042"`)},
		{"wrong-disposition", errors.New(`seal quota override: unexpected disposition "pending"`)},
		{"zero-revision", errors.New("seal quota override: reply carried no revision")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sealer := &fakeQuotaSealer{err: tc.err}
			reaper := &quotaOverrideSealReaper{sealer: sealer, capability: &fakeSealCapability{report: completeReport("gw-1")}}
			accounts := newFakeAccounts()
			engine := quietEngine(accounts, reaper)

			result, err := engine.Teardown(testCtx(t), request())
			require.ErrorIs(t, err, ErrResourcesStuck)
			assert.False(t, result.AccountDeleted)
			assert.Equal(t, AccountStatusTerminating, accounts.account.Status)
			assert.NotEmpty(t, sealer.calls)
		})
	}
}

// The client rejects every reply that is not a seal of this account.
func TestQuotaSealClientRejectsMalformedReplies(t *testing.T) {
	cases := []struct {
		name  string
		reply admissionv1.SealAccountQuotaOverrideReply
		want  string
	}{
		{"wrong account", admissionv1.SealAccountQuotaOverrideReply{AccountID: "999999999999", Disposition: admissionv1.AccountQuotaOverrideDispositionSealed, Revision: 3}, "reply named account"},
		{"wrong disposition", admissionv1.SealAccountQuotaOverrideReply{AccountID: testAccountID, Disposition: "pending", Revision: 3}, "unexpected disposition"},
		{"zero revision", admissionv1.SealAccountQuotaOverrideReply{AccountID: testAccountID, Disposition: admissionv1.AccountQuotaOverrideDispositionSealed}, "no revision"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, nc, _ := testutil.StartTestJetStream(t)
			sub, err := nc.Subscribe(admissionv1.SealAccountQuotaOverrideSubject, func(msg *nats.Msg) {
				data, _ := json.Marshal(tc.reply)
				_ = msg.Respond(data)
			})
			require.NoError(t, err)
			t.Cleanup(func() { _ = sub.Unsubscribe() })
			require.NoError(t, nc.Flush())

			_, err = newQuotaOverrideSealClient(nc).SealAccountQuotaOverride(testCtx(t), testAccountID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// A transient owner failure is retried by the engine's next pass and then seals.
func TestQuotaSealReaperRetriesThenSeals(t *testing.T) {
	sealer := &fakeQuotaSealer{failFor: 1}
	capability := &fakeSealCapability{report: completeReport("gw-1")}
	reaper := &quotaOverrideSealReaper{sealer: sealer, capability: capability}

	require.Error(t, reaper.Delete(testCtx(t), testAccountID, Resource{}, false))
	listed, err := reaper.List(testCtx(t), testAccountID)
	require.NoError(t, err)
	assert.Len(t, listed, 1, "a failed seal leaves the item pending")

	require.NoError(t, reaper.Delete(testCtx(t), testAccountID, Resource{}, false))
	listed, err = reaper.List(testCtx(t), testAccountID)
	require.NoError(t, err)
	assert.Empty(t, listed)
}

// The probe runs immediately before every seal attempt and is never reused.
func TestQuotaSealReaperProbedBeforeEverySeal(t *testing.T) {
	sealer := &fakeQuotaSealer{failFor: 2}
	capability := &fakeSealCapability{report: completeReport("gw-1")}
	reaper := &quotaOverrideSealReaper{sealer: sealer, capability: capability}

	for range 3 {
		_ = reaper.Delete(testCtx(t), testAccountID, Resource{}, false)
	}
	assert.Equal(t, 3, capability.calls)
	assert.Len(t, sealer.calls, 3)
}

// Inventory and DryRun list the item without sealing or probing anything.
func TestQuotaSealReaperListDoesNoIO(t *testing.T) {
	sealer := &fakeQuotaSealer{}
	capability := &fakeSealCapability{report: completeReport("gw-1")}
	reaper := &quotaOverrideSealReaper{sealer: sealer, capability: capability}

	listed, err := reaper.List(testCtx(t), testAccountID)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, "quota-override", listed[0].Kind)
	assert.Equal(t, testAccountID, listed[0].ID)
	assert.Empty(t, sealer.calls)
	assert.Zero(t, capability.calls)
}

// State is per engine: a fresh reaper lists and seals again.
func TestQuotaSealReaperFreshRunSealsAgain(t *testing.T) {
	sealer := &fakeQuotaSealer{}
	capability := &fakeSealCapability{report: completeReport("gw-1")}

	first := &quotaOverrideSealReaper{sealer: sealer, capability: capability}
	require.NoError(t, first.Delete(testCtx(t), testAccountID, Resource{}, false))

	second := &quotaOverrideSealReaper{sealer: sealer, capability: capability}
	listed, err := second.List(testCtx(t), testAccountID)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.NoError(t, second.Delete(testCtx(t), testAccountID, Resource{}, false))
	assert.Equal(t, []string{testAccountID, testAccountID}, sealer.calls)
}
