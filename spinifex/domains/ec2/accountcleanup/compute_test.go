package accountcleanup

//test:in-package — the compute owners are fakes so each test controls the
// read or terminate that fails and can assert no terminate happened.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	ec2spotinstance "github.com/mulgadc/spinifex/spinifex/domains/ec2/spotinstance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	runningID  = "i-0aaaaaaaaaaaaaaa1"
	stoppedID  = "i-0aaaaaaaaaaaaaaa2"
	managedID  = "i-0aaaaaaaaaaaaaaa3"
	stoppingID = "i-0aaaaaaaaaaaaaaa4"
)

type fakeSpot struct {
	scans      []*ec2spotinstance.AccountScan
	scanErr    error
	cancelErr  error
	scanCalls  int
	cancelled  []string
	cancelCall int
}

func (f *fakeSpot) ScanAccount(context.Context, string) (*ec2spotinstance.AccountScan, error) {
	f.scanCalls++
	if f.scanErr != nil {
		return nil, f.scanErr
	}
	i := min(f.scanCalls-1, len(f.scans)-1)
	return f.scans[i], nil
}

func (f *fakeSpot) CancelSpotInstanceRequests(_ context.Context, in *ec2.CancelSpotInstanceRequestsInput, _ string) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	f.cancelCall++
	if f.cancelErr != nil {
		return nil, f.cancelErr
	}
	for _, id := range in.SpotInstanceRequestIds {
		f.cancelled = append(f.cancelled, aws.StringValue(id))
	}
	return &ec2.CancelSpotInstanceRequestsOutput{}, nil
}

type fakeInventory struct {
	out *ec2.DescribeInstancesOutput
	err error
}

func (f *fakeInventory) AccountInstances(context.Context, string) (*ec2.DescribeInstancesOutput, error) {
	return f.out, f.err
}

type fakeTerminator struct {
	failFor    map[string]error
	terminated []string
}

func (f *fakeTerminator) TerminateInstance(_ context.Context, _, id string) error {
	if err := f.failFor[id]; err != nil {
		return err
	}
	f.terminated = append(f.terminated, id)
	return nil
}

type fakeTeardown struct {
	outstanding []ec2instance.OutstandingInstance
	err         error
	reads       int
}

func (f *fakeTeardown) AccountTeardownOutstanding(string) ([]ec2instance.OutstandingInstance, error) {
	f.reads++
	return f.outstanding, f.err
}

type fakeManaged struct {
	ids map[string]bool
	err error
}

func (f *fakeManaged) AccountManagedInstances(context.Context, string) (map[string]bool, error) {
	return f.ids, f.err
}

func listing(states map[string]string) *ec2.DescribeInstancesOutput {
	res := &ec2.Reservation{}
	for id, state := range states {
		res.Instances = append(res.Instances, &ec2.Instance{InstanceId: aws.String(id), State: &ec2.InstanceState{Name: aws.String(state)}})
	}
	return &ec2.DescribeInstancesOutput{Reservations: []*ec2.Reservation{res}}
}

type computeRig struct {
	spot       *fakeSpot
	inventory  *fakeInventory
	terminator *fakeTerminator
	teardown   *fakeTeardown
	managed    *fakeManaged
}

func newComputeRig() *computeRig {
	return &computeRig{
		spot:       &fakeSpot{scans: []*ec2spotinstance.AccountScan{{}}},
		inventory:  &fakeInventory{out: listing(map[string]string{runningID: ec2.InstanceStateNameRunning, stoppedID: ec2.InstanceStateNameStopped})},
		terminator: &fakeTerminator{},
		teardown:   &fakeTeardown{},
		managed:    &fakeManaged{ids: map[string]bool{}},
	}
}

func (r *computeRig) cleanup() *ComputeCleanup {
	return NewCompute(r.spot, r.inventory, r.terminator, r.teardown, r.managed)
}

func (r *computeRig) run(t *testing.T, force bool) *ComputeResult {
	t.Helper()
	res, err := r.cleanup().CleanupCompute(t.Context(), &ComputeRequest{AccountID: testAccount, Force: force})
	require.NoError(t, err)
	return res
}

func itemFor(res *ComputeResult, id string) *Item {
	for i := range res.Items {
		if res.Items[i].ResourceID == id {
			return &res.Items[i]
		}
	}
	return nil
}

func TestCleanupCompute_TerminatesEveryLiveInstance(t *testing.T) {
	rig := newComputeRig()
	res := rig.run(t, false)

	assert.ElementsMatch(t, []string{runningID, stoppedID}, rig.terminator.terminated)
	assert.Equal(t, StatusIncomplete, res.Status, "an absent teardown record is not proof of completion")
	assert.Equal(t, DispositionPending, itemFor(res, runningID).Disposition)
	assert.Equal(t, ReasonTeardownUnconfirmed, itemFor(res, runningID).Reason)
}

func TestCleanupCompute_SkipsInstanceAlreadyTerminating(t *testing.T) {
	rig := newComputeRig()
	rig.inventory.out = listing(map[string]string{stoppingID: ec2.InstanceStateNameShuttingDown})
	rig.teardown.outstanding = []ec2instance.OutstandingInstance{{InstanceID: stoppingID, State: "shutting-down"}}

	res := rig.run(t, false)

	assert.Empty(t, rig.terminator.terminated, "an instance already terminating is not terminated again")
	item := itemFor(res, stoppingID)
	require.NotNil(t, item)
	assert.Equal(t, DispositionBlocked, item.Disposition)
	assert.Equal(t, ReasonTerminating, item.Reason)
	assert.Equal(t, StatusIncomplete, res.Status)
}

func TestCleanupCompute_UnacknowledgedTerminateIsUnproven(t *testing.T) {
	rig := newComputeRig()
	rig.terminator.failFor = map[string]error{runningID: errors.New("no responders")}

	res := rig.run(t, false)

	item := itemFor(res, runningID)
	require.NotNil(t, item)
	assert.Equal(t, DispositionBlocked, item.Disposition)
	assert.Equal(t, ReasonTerminationUnproven, item.Reason)
	assert.Equal(t, DispositionPending, itemFor(res, stoppedID).Disposition, "one unproven instance does not stop the others")
	assert.Equal(t, StatusIncomplete, res.Status)
}

func TestCleanupCompute_TeardownOutstandingIsBlockedTerminating(t *testing.T) {
	rig := newComputeRig()
	rig.teardown.outstanding = []ec2instance.OutstandingInstance{
		{InstanceID: runningID, State: "shutting-down", Teardown: map[string]string{"qemu": "pending"}},
	}

	res := rig.run(t, false)

	item := itemFor(res, runningID)
	assert.Equal(t, DispositionBlocked, item.Disposition)
	assert.Equal(t, ReasonTerminating, item.Reason)
	assert.Equal(t, DispositionPending, itemFor(res, stoppedID).Disposition)
}

func TestCleanupCompute_UnreadableTeardownLeavesTerminatesUnproven(t *testing.T) {
	rig := newComputeRig()
	rig.teardown.err = errors.New("store down")

	res := rig.run(t, false)

	assert.Equal(t, StatusIncomplete, res.Status)
	assert.Contains(t, res.IncompleteReasons, ReasonTeardownUnread)
	for _, id := range []string{runningID, stoppedID} {
		assert.Equal(t, ReasonTerminationUnproven, itemFor(res, id).Reason)
	}
}

func TestCleanupCompute_CancelsOpenAndActiveSpotRequests(t *testing.T) {
	rig := newComputeRig()
	rig.spot.scans = []*ec2spotinstance.AccountScan{
		{Active: []ec2spotinstance.AccountSpotRequest{{RequestID: "sir-open", State: ec2.SpotInstanceStateOpen}, {RequestID: "sir-active", State: ec2.SpotInstanceStateActive}}},
		{Terminal: []ec2spotinstance.AccountSpotRequest{{RequestID: "sir-open", State: ec2.SpotInstanceStateCancelled}, {RequestID: "sir-active", State: ec2.SpotInstanceStateCancelled}, {RequestID: "sir-old", State: ec2.SpotInstanceStateClosed}}},
	}

	res := rig.run(t, false)

	assert.ElementsMatch(t, []string{"sir-open", "sir-active"}, rig.spot.cancelled)
	assert.Equal(t, DispositionCancelled, itemFor(res, "sir-open").Disposition)
	assert.Equal(t, DispositionTerminal, itemFor(res, "sir-old").Disposition)
	assert.NotContains(t, res.IncompleteReasons, ReasonSpotInventoryIncomplete)
}

// A failed request is terminal, like closed and cancelled: it is neither
// cancelled again nor does it keep the run incomplete.
func TestCleanupCompute_FailedSpotRequestIsTerminal(t *testing.T) {
	rig := newComputeRig()
	rig.spot.scans = []*ec2spotinstance.AccountScan{{
		Active:   []ec2spotinstance.AccountSpotRequest{{RequestID: "sir-failed-active", State: ec2.SpotInstanceStateFailed}},
		Terminal: []ec2spotinstance.AccountSpotRequest{{RequestID: "sir-failed", State: ec2.SpotInstanceStateFailed}},
	}}

	res := rig.run(t, false)

	assert.Zero(t, rig.spot.cancelCall, "a failed request is not cancelled")
	assert.NotContains(t, res.IncompleteReasons, ReasonSpotInventoryIncomplete)
	assert.Equal(t, DispositionTerminal, itemFor(res, "sir-failed").Disposition)
	assert.ElementsMatch(t, []string{runningID, stoppedID}, rig.terminator.terminated, "a failed spot request does not hold up the terminate")
}

func TestCleanupCompute_SpotStillOpenAfterCancelIsBlocked(t *testing.T) {
	rig := newComputeRig()
	open := &ec2spotinstance.AccountScan{Active: []ec2spotinstance.AccountSpotRequest{{RequestID: "sir-open", State: ec2.SpotInstanceStateOpen}}}
	rig.spot.scans = []*ec2spotinstance.AccountScan{open, open}

	res := rig.run(t, false)

	assert.Equal(t, ReasonSpotCancelFailed, itemFor(res, "sir-open").Reason)
	assert.Empty(t, rig.terminator.terminated, "an instance could be relaunched while a request is open")
}

// ADR-0003:S7. A read the run cannot complete terminates nothing.
func TestS7_CleanupComputeUnprovenReadTerminatesNothing(t *testing.T) {
	cases := map[string]func(*computeRig){
		"instance inventory error": func(r *computeRig) { r.inventory.err = errors.New("incomplete listing") },
		"spot scan error":          func(r *computeRig) { r.spot.scanErr = errors.New("scan failed") },
		"spot cancel error": func(r *computeRig) {
			r.spot.scans = []*ec2spotinstance.AccountScan{{Active: []ec2spotinstance.AccountSpotRequest{{RequestID: "sir-open", State: ec2.SpotInstanceStateOpen}}}}
			r.spot.cancelErr = errors.New("cancel failed")
		},
		"managed reader error": func(r *computeRig) { r.managed.err = errors.New("nodegroup store down") },
	}
	for name, arrange := range cases {
		for _, force := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				rig := newComputeRig()
				arrange(rig)

				res := rig.run(t, force)

				assert.Empty(t, rig.terminator.terminated, "ADR-0003:S7: an unproven read must terminate nothing (force=%v)", force)
				assert.Equal(t, StatusIncomplete, res.Status)
				assert.NotEmpty(t, res.IncompleteReasons)
			})
		}
	}
}

// ADR-0003:S7. An instance a managed service owns is never terminated.
func TestS7_CleanupComputeManagedInstanceIsNeverTerminated(t *testing.T) {
	for _, force := range []bool{false, true} {
		rig := newComputeRig()
		rig.inventory.out = listing(map[string]string{managedID: ec2.InstanceStateNameRunning, runningID: ec2.InstanceStateNameRunning})
		rig.managed.ids = map[string]bool{managedID: true}

		res := rig.run(t, force)

		assert.Equal(t, []string{runningID}, rig.terminator.terminated, "ADR-0003:S7: a managed-owned instance must not be terminated (force=%v)", force)
		item := itemFor(res, managedID)
		assert.Equal(t, DispositionBlocked, item.Disposition)
		assert.Equal(t, ReasonManagedServiceOwner, item.Reason)
		assert.Equal(t, StatusIncomplete, res.Status)
	}
}

// ADR-0003:S7. A missing managed-instance reader is a wiring error and not
// the same as "none owned".
func TestS7_CleanupComputeNilManagedReaderTerminatesNothing(t *testing.T) {
	rig := newComputeRig()

	_, err := NewCompute(rig.spot, rig.inventory, rig.terminator, rig.teardown, nil).
		CleanupCompute(t.Context(), &ComputeRequest{AccountID: testAccount})

	require.Error(t, err)
	assert.Empty(t, rig.terminator.terminated, "ADR-0003:S7: no terminate without a managed-ownership answer")
	assert.Zero(t, rig.spot.cancelCall+rig.spot.scanCalls, "nothing is touched before the wiring is proven")
}

// ADR-0003:S8. The call returns with outcomes while teardown is outstanding;
// it neither polls the teardown nor blocks on it.
func TestS8_CleanupComputeReturnsWhileTeardownOutstanding(t *testing.T) {
	rig := newComputeRig()
	rig.teardown.outstanding = []ec2instance.OutstandingInstance{
		{InstanceID: runningID, State: "shutting-down"}, {InstanceID: stoppedID, State: "shutting-down"},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	start := time.Now()
	res, err := rig.cleanup().CleanupCompute(ctx, &ComputeRequest{AccountID: testAccount})

	require.NoError(t, err)
	assert.Less(t, time.Since(start), time.Second)
	assert.Equal(t, StatusIncomplete, res.Status)
	assert.Equal(t, ReasonTerminating, itemFor(res, runningID).Reason)
	assert.Equal(t, 1, rig.teardown.reads, "teardown is read once, not polled")
}

func TestCleanupCompute_RejectsBadRequest(t *testing.T) {
	c := newComputeRig().cleanup()
	for _, req := range []*ComputeRequest{nil, {}, {AccountID: "123"}} {
		_, err := c.CleanupCompute(t.Context(), req)
		assert.Error(t, err)
	}
}

// Terminated records expire, so a missing one is never read as a finished teardown.
func TestCleanupCompute_NoOutstandingRecordIsNotProofOfCompletion(t *testing.T) {
	rig := newComputeRig()
	res := rig.run(t, false)

	for _, id := range []string{runningID, stoppedID} {
		assert.Equal(t, DispositionPending, itemFor(res, id).Disposition)
	}
	assert.Equal(t, StatusIncomplete, res.Status)

	rig.inventory.out = listing(nil)
	assert.Equal(t, StatusClean, rig.run(t, false).Status, "a rerun that finds no instance converges")
}

// A disabled request can resume, so it is cancelled; an unknown state is never drained.
func TestS7_CleanupComputeSpotStateIsFailClosed(t *testing.T) {
	rig := newComputeRig()
	rig.spot.scans = []*ec2spotinstance.AccountScan{
		{Active: []ec2spotinstance.AccountSpotRequest{{RequestID: "sir-disabled", State: ec2.SpotInstanceStateDisabled}}},
		{Terminal: []ec2spotinstance.AccountSpotRequest{{RequestID: "sir-disabled", State: ec2.SpotInstanceStateCancelled}}},
	}
	rig.run(t, false)
	assert.Equal(t, []string{"sir-disabled"}, rig.spot.cancelled)

	unknown := newComputeRig()
	unknown.spot.scans = []*ec2spotinstance.AccountScan{{Terminal: []ec2spotinstance.AccountSpotRequest{{RequestID: "sir-odd", State: "mystery"}}}}
	res := unknown.run(t, false)
	assert.Equal(t, ReasonSpotStateUnrecognised, itemFor(res, "sir-odd").Reason)
	assert.Empty(t, unknown.terminator.terminated, "ADR-0003:S7: an unrecognised spot state could still launch, so nothing is terminated")
	assert.Equal(t, StatusIncomplete, res.Status)
}
