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

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	ec2account "github.com/mulgadc/spinifex/spinifex/domains/ec2/account"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSealer is a recording ec2AccountSettingsSealer. Each call beyond
// failFor succeeds with a fresh, strictly increasing revision, unless err is
// set, in which case every call after failFor fails with it.
type fakeSealer struct {
	mu       sync.Mutex
	calls    []string
	failFor  int
	err      error
	revision uint64
}

func (f *fakeSealer) SealAccountSettings(_ context.Context, accountID string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, accountID)
	if len(f.calls) <= f.failFor {
		return 0, errors.New("account settings owner temporarily unavailable")
	}
	if f.err != nil {
		return 0, f.err
	}
	f.revision++
	return f.revision, nil
}

// fakeSealCapability is a recording sealCapabilityChecker returning a fixed report.
type fakeSealCapability struct {
	calls  int
	report sealCapabilityReport
}

func (f *fakeSealCapability) CheckSealCapability(context.Context, string) sealCapabilityReport {
	f.calls++
	return f.report
}

// completeReport builds a sealCapabilityReport where every named node answers
// compatible at version 1.
func completeReport(nodeIDs ...string) sealCapabilityReport {
	var nodes []nodeSealCapability
	for _, node := range nodeIDs {
		nodes = append(nodes, nodeSealCapability{Node: node, Version: 1})
	}
	return sealCapabilityReport{Complete: true, Nodes: nodes}
}

// alwaysTerminatingChecker stands in for IAM in the end-to-end test: the
// account under test is always TERMINATING.
type alwaysTerminatingChecker struct{}

func (alwaysTerminatingChecker) AccountTerminating(context.Context, string) (bool, error) {
	return true, nil
}

// No subscriber on either subject makes the capability probe incomplete, so
// no seal request is ever sent and the account stays TERMINATING with one
// stuck ec2-account-settings item.
func TestSealReaperNoResponderKeepsAccountTerminating(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)

	capability := newSealCapabilityClient(nc, []string{"node-1"})
	capability.wait = 50 * time.Millisecond
	capability.grace = 10 * time.Millisecond
	sealer := newEC2AccountSettingsSealClient(nc)
	sealer.timeout = 50 * time.Millisecond

	reaper := &accountSettingsSealReaper{sealer: sealer, capability: capability}
	accounts := newFakeAccounts()
	engine := quietEngine(accounts, reaper)

	result, err := engine.Teardown(testCtx(t), request())
	require.ErrorIs(t, err, ErrResourcesStuck)
	assert.False(t, result.AccountDeleted)
	assert.Equal(t, AccountStatusTerminating, accounts.account.Status)

	var stuck []Stuck
	for _, stage := range result.Stages {
		stuck = append(stuck, stage.Stuck...)
	}
	require.Len(t, stuck, 1)
	assert.Equal(t, "ec2-account-settings", stuck[0].Resource.Kind)
}

// Whatever the owner refuses with, the account stays TERMINATING: an owner
// refusal, a timeout, and each reply shape the client rejects (wrong account,
// wrong disposition, no revision).
func TestSealReaperOwnerFailureKeepsAccountTerminating(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"incorrect-state", errors.New(awserrors.ErrorIncorrectState)},
		{"internal-error", errors.New(awserrors.ErrorInternalError)},
		{"timeout", context.DeadlineExceeded},
		{"wrong-account", errors.New(`seal account settings: reply named account "999999999999", not "000000000042"`)},
		{"wrong-disposition", errors.New(`seal account settings: unexpected disposition "pending"`)},
		{"zero-revision", errors.New("seal account settings: reply carried no revision")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sealer := &fakeSealer{err: tc.err}
			reaper := &accountSettingsSealReaper{sealer: sealer, capability: &fakeSealCapability{report: completeReport("node-1")}}
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

// The reaper retries through the engine's own drain mechanics: it fails
// twice, then succeeds, and only the success lets the account be deleted.
// List is non-empty before the success and empty only after it.
func TestSealReaperCompletesOnlyAfterSealed(t *testing.T) {
	sealer := &fakeSealer{failFor: 2}
	reaper := &accountSettingsSealReaper{sealer: sealer, capability: &fakeSealCapability{report: completeReport("node-1")}}
	accounts := newFakeAccounts()
	engine := quietEngine(accounts, reaper)

	before, err := reaper.List(testCtx(t), testAccountID)
	require.NoError(t, err)
	require.Len(t, before, 1)

	result, err := engine.Teardown(testCtx(t), request())
	require.NoError(t, err)
	assert.True(t, result.AccountDeleted)
	assert.GreaterOrEqual(t, len(sealer.calls), 3)

	after, err := reaper.List(testCtx(t), testAccountID)
	require.NoError(t, err)
	assert.Empty(t, after)
}

// Each teardown builds a fresh engine and a fresh reaper, so a retried or
// taken-over run seals again: two engines against the same owner both
// succeed, each with a higher revision than the last.
func TestSealReaperFreshRunSealsAgain(t *testing.T) {
	sealer := &fakeSealer{}
	capability := &fakeSealCapability{report: completeReport("node-1")}

	firstEngine := quietEngine(newFakeAccounts(), &accountSettingsSealReaper{sealer: sealer, capability: capability})
	firstResult, err := firstEngine.Teardown(testCtx(t), request())
	require.NoError(t, err)
	assert.True(t, firstResult.AccountDeleted)

	secondEngine := quietEngine(newFakeAccounts(), &accountSettingsSealReaper{sealer: sealer, capability: capability})
	secondResult, err := secondEngine.Teardown(testCtx(t), request())
	require.NoError(t, err)
	assert.True(t, secondResult.AccountDeleted)

	require.Len(t, sealer.calls, 2)
	assert.Equal(t, uint64(2), sealer.revision)
}

// Inventory and the explicit dry run, and a Teardown request marked dry run,
// all list the pending seal and send no seal request: only a real teardown
// may seal.
func TestSealReaperInventoryDoesNotSeal(t *testing.T) {
	sealer := &fakeSealer{}
	capability := &fakeSealCapability{report: completeReport("node-1")}
	reaper := &accountSettingsSealReaper{sealer: sealer, capability: capability}
	accounts := newFakeAccounts()
	engine := quietEngine(accounts, reaper)

	inventory, err := engine.Inventory(testCtx(t), testAccountID)
	require.NoError(t, err)
	assert.Empty(t, sealer.calls)
	assert.Zero(t, capability.calls)
	found := false
	for _, stage := range inventory.Stages {
		for _, resource := range stage.Deleted {
			if resource.Kind == "ec2-account-settings" {
				found = true
			}
		}
	}
	assert.True(t, found, "inventory must still list the pending seal")

	dryRun := request()
	dryRun.DryRun = true
	_, err = engine.Teardown(testCtx(t), dryRun)
	require.NoError(t, err)
	assert.Empty(t, sealer.calls)
	assert.Zero(t, capability.calls)
}

// A real settings service subscribed on the real seal subject, behind a real
// engine: teardown seals the record, replacing the characterisation test that
// showed it used to survive.
func TestTeardownSealsAccountSettings(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	ctx := testCtx(t)

	settings, err := ec2account.NewAccountSettingsServiceImplWithNATS(ctx, nil, nc)
	require.NoError(t, err)
	settings.SetAccountTerminationChecker(alwaysTerminatingChecker{})
	_, err = settings.EnableEbsEncryptionByDefault(ctx, &ec2.EnableEbsEncryptionByDefaultInput{}, testAccountID)
	require.NoError(t, err)

	sealSub, err := nc.QueueSubscribe(ec2v1.SealAccountSettingsSubject, "spinifex-workers", func(msg *nats.Msg) {
		natsmsg.ServeNATSRequestCtx(msg, func(ctx context.Context, req *ec2v1.SealAccountSettingsRequest) (*ec2v1.SealAccountSettingsReply, error) {
			seal, err := settings.SealAccount(ctx, req.AccountID)
			if err != nil {
				return nil, err
			}
			return &ec2v1.SealAccountSettingsReply{
				AccountID: req.AccountID, Disposition: ec2v1.AccountSettingsDispositionSealed, Revision: seal.Revision,
			}, nil
		})
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sealSub.Unsubscribe() })

	capSub, err := nc.Subscribe(ec2v1.AccountSettingsSealCapabilitySubject("node-1"), func(msg *nats.Msg) {
		reply, err := json.Marshal(ec2v1.AccountSettingsSealCapabilityReply{Node: "node-1", CapabilityVersion: 1})
		require.NoError(t, err)
		out := nats.NewMsg("")
		out.Data = reply
		out.Header.Set(natsmsg.NodeIDHeader, "node-1")
		require.NoError(t, msg.RespondMsg(out))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = capSub.Unsubscribe() })
	require.NoError(t, nc.Flush())

	accounts := newFakeAccounts()
	engine := quietEngine(accounts, EC2AccountSettingsReapers(nc, []string{"node-1"})...)
	// The real capability probe holds CollectUntilDeadline's full ResponderGrace
	// (150 ms) even when the one expected node answers instantly, so the quiet
	// engine's default 200 ms StageDrain leaves no room for the sweep after it.
	engine.Timeouts = Timeouts{StageDrain: 2 * time.Second, DrainPoll: 10 * time.Millisecond}

	result, err := engine.Teardown(ctx, request())
	require.NoError(t, err)
	assert.True(t, result.AccountDeleted)

	bucket, err := js.KeyValue(ctx, ec2account.KVBucketAccountSettings)
	require.NoError(t, err)
	entry, err := bucket.Get(ctx, testAccountID)
	require.NoError(t, err)
	var rec ec2account.AccountSettingsRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &rec))
	assert.Equal(t, ec2account.AccountSettingsStateSealed, rec.State)
}
