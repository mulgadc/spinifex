package handlers_rds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The wait for a stopping VM, shrunk so a stop that never lands is bounded in
// milliseconds rather than in the minute a real one is given.
const testVMStopTimeout = 40 * time.Millisecond

// fakeInstanceCommander records the power commands the lifecycle ops issue, and
// can refuse them the way a node that no longer holds the VM does.
type fakeInstanceCommander struct {
	calls []string
	// notOnNode makes StartInstance report the VM as held by no node, which is
	// what sends the start down the stopped-instance path.
	notOnNode bool
	// stopNotOnNode is the same for a stop, which then has to confirm the VM is
	// really down rather than assume it.
	stopNotOnNode bool
	err           error
	// The VM state an accepted stop takes down. Nil models a node that accepts
	// the command and never lands it.
	vm *fakeInstanceState
}

var _ instanceCommander = (*fakeInstanceCommander)(nil)

func (f *fakeInstanceCommander) StopInstance(_ context.Context, instanceID string) error {
	f.calls = append(f.calls, "stop:"+instanceID)
	if f.stopNotOnNode {
		return ErrInstanceNotOnNode
	}
	if f.err != nil {
		return f.err
	}
	if f.vm != nil {
		f.vm.stop()
	}
	return nil
}

func (f *fakeInstanceCommander) RebootInstance(_ context.Context, instanceID string) error {
	f.calls = append(f.calls, "reboot:"+instanceID)
	return f.err
}

func (f *fakeInstanceCommander) StartInstance(_ context.Context, instanceID string) error {
	f.calls = append(f.calls, "start:"+instanceID)
	if f.notOnNode {
		return ErrInstanceNotOnNode
	}
	return f.err
}

func (f *fakeInstanceCommander) StartStoppedInstance(_ context.Context, instanceID string) error {
	f.calls = append(f.calls, "start-stopped:"+instanceID)
	return f.err
}

// fakeSnapshots stands in for the EC2 snapshot service. holding is what
// DescribeSnapshots reports against the data volume, which is what decides
// between deleting the volume and retaining it.
type fakeSnapshots struct {
	created       []*ec2.CreateSnapshotInput
	holding       []string
	deleted       []string
	createErr     error
	describeErr   error
	deleteErr     error
	beforeCreate  func()
	afterDescribe func()
	// The tags each created snapshot carries, which is what a tag-filtered
	// describe resolves a DB snapshot identifier through.
	tagged map[string]map[string]string
}

var _ snapshotProvider = (*fakeSnapshots)(nil)

func (f *fakeSnapshots) CreateSnapshot(_ context.Context, input *ec2.CreateSnapshotInput, _ string) (*ec2.Snapshot, error) {
	if f.beforeCreate != nil {
		f.beforeCreate()
	}
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, input)
	id := fmt.Sprintf("snap-%04d", len(f.created))
	if f.tagged == nil {
		f.tagged = map[string]map[string]string{}
	}
	f.tagged[id] = map[string]string{}
	for _, spec := range input.TagSpecifications {
		for _, tag := range spec.Tags {
			f.tagged[id][aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
		}
	}
	// A snapshot pins the chunks of the volume it was taken from, so taking one
	// is also what makes that volume undeletable.
	f.holding = append(f.holding, id)
	return &ec2.Snapshot{SnapshotId: aws.String(id), VolumeId: input.VolumeId}, nil
}

// A tag filter is answered from the recorded tags; anything else reports every
// holder, which is what the volume-release path asks for.
func (f *fakeSnapshots) DescribeSnapshots(_ context.Context, input *ec2.DescribeSnapshotsInput, _ string) (*ec2.DescribeSnapshotsOutput, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	out := &ec2.DescribeSnapshotsOutput{}
	for _, id := range f.holding {
		if !f.matchesFilters(id, input) {
			continue
		}
		out.Snapshots = append(out.Snapshots, &ec2.Snapshot{SnapshotId: aws.String(id)})
	}
	if f.afterDescribe != nil {
		f.afterDescribe()
	}
	return out, nil
}

func (f *fakeSnapshots) DescribeSnapshotsStrict(ctx context.Context, input *ec2.DescribeSnapshotsInput,
	accountID string) (*ec2.DescribeSnapshotsOutput, error) {
	return f.DescribeSnapshots(ctx, input, accountID)
}

func (f *fakeSnapshots) matchesFilters(id string, input *ec2.DescribeSnapshotsInput) bool {
	if input == nil {
		return true
	}
	for _, filter := range input.Filters {
		key, ok := strings.CutPrefix(aws.StringValue(filter.Name), "tag:")
		if !ok {
			continue
		}
		if !slices.Contains(aws.StringValueSlice(filter.Values), f.tagged[id][key]) {
			return false
		}
	}
	return true
}

func (f *fakeSnapshots) DeleteSnapshot(_ context.Context, input *ec2.DeleteSnapshotInput, _ string) (*ec2.DeleteSnapshotOutput, error) {
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	id := aws.StringValue(input.SnapshotId)
	f.deleted = append(f.deleted, id)
	f.holding = slices.DeleteFunc(f.holding, func(held string) bool { return held == id })
	return &ec2.DeleteSnapshotOutput{}, nil
}

// stubAgent answers the command channel the way the in-guest agent does: it
// replies to whatever lands on the command subject, correlated by command ID.
type stubAgent struct {
	mu     sync.Mutex
	issued []Command
	fail   bool
	// What a successful reply carries back. The apply-params reply reports the
	// settings pending a restart here, since CommandReply has no payload.
	message string
}

// Set before any command is issued; guarded because the reply is built on the
// subscription's goroutine.
func (a *stubAgent) replyWith(message string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.message = message
}

func (a *stubAgent) received() []Command {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Command(nil), a.issued...)
}

func newStubAgent(t *testing.T, nc *nats.Conn, accountID, dbID string, fail bool) *stubAgent {
	t.Helper()
	agent := &stubAgent{fail: fail}
	replySubject := BusCommandReplySubject(accountID, dbID)

	sub, err := nc.Subscribe(BusCommandSubject(accountID, dbID), func(msg *nats.Msg) {
		var cmd Command
		if err := json.Unmarshal(msg.Data, &cmd); err != nil {
			t.Logf("stub agent: undecodable command: %v", err)
			return
		}
		agent.mu.Lock()
		agent.issued = append(agent.issued, cmd)
		reply := CommandReply{CommandID: cmd.CommandID, Status: CommandStatusSucceeded, Message: agent.message}
		if agent.fail {
			reply = CommandReply{CommandID: cmd.CommandID, Status: CommandStatusFailed, Message: "the engine did not stop"}
		}
		agent.mu.Unlock()
		payload, err := json.Marshal(reply)
		if err != nil {
			t.Logf("stub agent: marshal reply: %v", err)
			return
		}
		if err := nc.Publish(replySubject, payload); err != nil {
			t.Logf("stub agent: publish reply: %v", err)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return agent
}

type lifecycleHarness struct {
	svc      *Service
	nc       *nats.Conn
	cmdr     *fakeInstanceCommander
	snaps    *fakeSnapshots
	enis     *fakeENIs
	launcher *fakeLauncher
	volumes  *fakeVolumes
	agent    *stubAgent
	vmState  *fakeInstanceState
}

// agentFails makes the stub agent reject every command, which is how the
// graceful-stop fallback is exercised without waiting out a real budget.
func newLifecycleHarness(t *testing.T, agentFails bool) *lifecycleHarness {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	// The state an available instance's VM is in; a case that needs the VM
	// down for a stop-confirmation path sets it.
	vmState := &fakeInstanceState{state: instanceStateRunning}
	h := &lifecycleHarness{
		nc:       nc,
		cmdr:     &fakeInstanceCommander{vm: vmState},
		snaps:    &fakeSnapshots{},
		enis:     &fakeENIs{},
		launcher: &fakeLauncher{},
		volumes:  &fakeVolumes{},
		vmState:  vmState,
	}
	h.agent = newStubAgent(t, nc, testAccountID, testDBID, agentFails)
	h.svc = NewService(nc, testRegion).WithDeps(Deps{
		LoadCA:        newTestCA(t),
		MasterKey:     testMasterKey,
		Instances:     h.cmdr,
		Snapshots:     h.snaps,
		InstanceState: h.vmState,
		VMStopTimeout: testVMStopTimeout,
		Launch: LaunchDeps{
			VPC:      h.enis,
			Instance: h.launcher,
			Volume:   h.volumes,
		},
	})
	return h
}

// availableRecord is an instance in the state every lifecycle op starts from.
func availableRecord() DBInstanceRecord {
	rec := defaultRecord()
	rec.Status = StatusAvailable
	rec.ENIID = "eni-cust01"
	rec.SystemENIID = "eni-sys01"
	rec.DataVolumeID = "vol-rdsdata01"
	rec.CreatedAt = time.Now().UTC().Add(-time.Hour)
	return rec
}

func (h *lifecycleHarness) record(t *testing.T) DBInstanceRecord {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec DBInstanceRecord
	found, err := getJSON(t.Context(), kv, DBInstanceKey(testDBID), &rec)
	require.NoError(t, err)
	require.True(t, found)
	return rec
}

func (h *lifecycleHarness) events(t *testing.T) []Event {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var ring eventRing
	found, err := getJSON(t.Context(), kv, EventRingKey(EventSourceTypeDBInstance, testDBID), &ring)
	require.NoError(t, err)
	if !found {
		return nil
	}
	return ring.Events
}

// The engine is asked to stop cleanly first, so the reboot is a restart of a
// checkpointed cluster rather than a crash it has to replay.
func TestRebootDBInstance_StopsTheEngineThenTheVM(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	rec := availableRecord()
	rec.FormatAuthorized = true
	seedInstance(t, h.svc, rec)

	out, err := h.svc.RebootDBInstance(t.Context(),
		&rds.RebootDBInstanceInput{DBInstanceIdentifier: aws.String(testDBID)}, testAccountID)
	require.NoError(t, err)

	issued := h.agent.received()
	require.Len(t, issued, 1)
	assert.Equal(t, CommandStopEngine, issued[0].Type)
	assert.Equal(t, []string{"reboot:" + testInstance}, h.cmdr.calls)

	// Reported as rebooting, not available: the engine has to come back and say
	// so before the reconciler calls it that.
	assert.Equal(t, string(StatusRebooting), aws.StringValue(out.DBInstance.DBInstanceStatus))
	stored := h.record(t)
	assert.Equal(t, StatusRebooting, stored.Status)
	assert.False(t, stored.FormatAuthorized, "reboot must revoke create-time formatting")
}

// The static parameters are already in the engine's config, so the restart
// is what applies them and the record stops advertising them.
func TestRebootDBInstance_ClearsThePendingRebootParameters(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	rec := availableRecord()
	rec.PendingRebootParameters = []string{"shared_buffers"}
	seedInstance(t, h.svc, rec)

	_, err := h.svc.RebootDBInstance(t.Context(),
		&rds.RebootDBInstanceInput{DBInstanceIdentifier: aws.String(testDBID)}, testAccountID)
	require.NoError(t, err)

	assert.Empty(t, h.record(t).PendingRebootParameters)
	assert.Contains(t, eventMessages(h.events(t)), "Applied the parameters that were pending a reboot.")
}

// There is no standby to fail over to, so silently ignoring the flag would
// report a failover that never happened.
func TestRebootDBInstance_RejectsForceFailover(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	seedInstance(t, h.svc, availableRecord())

	_, err := h.svc.RebootDBInstance(t.Context(), &rds.RebootDBInstanceInput{
		DBInstanceIdentifier: aws.String(testDBID),
		ForceFailover:        aws.Bool(true),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ForceFailover")
	assert.Equal(t, StatusAvailable, h.record(t).Status)
}

// An engine that did not stop cleanly replays its WAL on the next start.
// Refusing to stop the VM over it would leave a customer unable to stop an
// instance whose agent is wedged, so it is recorded and the stop continues.
func TestStopDBInstance_RecordsAFailedGracefulStopAndContinues(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, true)
	seedInstance(t, h.svc, availableRecord())

	out, err := h.svc.StopDBInstance(t.Context(),
		&rds.StopDBInstanceInput{DBInstanceIdentifier: aws.String(testDBID)}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, []string{"stop:" + testInstance}, h.cmdr.calls)
	assert.Equal(t, string(StatusStopped), aws.StringValue(out.DBInstance.DBInstanceStatus))
	assert.Equal(t, StatusStopped, h.record(t).Status)

	var recorded bool
	for _, message := range eventMessages(h.events(t)) {
		if strings.Contains(message, "could not be shut down cleanly") {
			recorded = true
		}
	}
	assert.True(t, recorded, "a degraded shutdown must reach the customer's event ring")
}

// A node that is partitioned or restarting answers a power command exactly the
// way one that never held the VM does. Writing stopped on that alone would
// report a VM still serving on the customer ENI as stopped.
func TestStopDBInstance_FailsWhenNoNodeAnsweredButTheVMIsStillRunning(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	h.cmdr.stopNotOnNode = true
	h.vmState.state = instanceStateRunning
	seedInstance(t, h.svc, availableRecord())

	_, err := h.svc.StopDBInstance(t.Context(),
		&rds.StopDBInstanceInput{DBInstanceIdentifier: aws.String(testDBID)}, testAccountID)
	require.Error(t, err)

	rec := h.record(t)
	assert.Equal(t, StatusFailed, rec.Status)
	assert.Contains(t, rec.FailureReason, `"running", not stopped`)
}

// The same unanswered command is the normal shape of a VM that is genuinely
// down, and that stop has to converge rather than fail.
func TestStopDBInstance_CompletesWhenTheFleetConfirmsTheVMIsDown(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	h.cmdr.stopNotOnNode = true
	h.vmState.state = "stopped"
	seedInstance(t, h.svc, availableRecord())

	_, err := h.svc.StopDBInstance(t.Context(),
		&rds.StopDBInstanceInput{DBInstanceIdentifier: aws.String(testDBID)}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, StatusStopped, h.record(t).Status)
}

// A node accepts a stop milliseconds after it is issued but takes seconds to
// drain and detach the data volume, so the first reading of the fleet has the VM
// stopping. That is not down: the stop waits it out rather than returning on it.
func TestStopDBInstance_WaitsForTheFleetToReportTheVMDown(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	h.vmState.detachReads = 1
	seedInstance(t, h.svc, availableRecord())

	_, err := h.svc.StopDBInstance(t.Context(),
		&rds.StopDBInstanceInput{DBInstanceIdentifier: aws.String(testDBID)}, testAccountID)
	require.NoError(t, err)

	assert.Len(t, h.vmState.calls, 2, "the first reading still had the VM stopping")
	assert.Equal(t, StatusStopped, h.record(t).Status)
}

// The wait is bounded, and a VM the node accepted the stop for but never took
// down has to end as a failure rather than as a stop that never returns.
func TestStopDBInstance_FailsWhenTheVMNeverStops(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	h.cmdr.vm = nil
	seedInstance(t, h.svc, availableRecord())

	_, err := h.svc.StopDBInstance(t.Context(),
		&rds.StopDBInstanceInput{DBInstanceIdentifier: aws.String(testDBID)}, testAccountID)
	require.Error(t, err)

	rec := h.record(t)
	assert.Equal(t, StatusFailed, rec.Status)
	assert.Contains(t, rec.FailureReason, "did not stop within")
	assert.Greater(t, len(h.vmState.calls), 1, "the fleet is polled, not read once")
}

// The reconciler resumes a stop with no caller watching, so it needs the same
// confirmation before it calls an unanswered command a completed stop.
func TestReconciler_DoesNotCallAStillRunningVMStopped(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	h.cmdr.stopNotOnNode = true
	h.vmState.state = instanceStateRunning
	rec := availableRecord()
	rec.Status = StatusStopping
	started := time.Now().UTC()
	rec.TransitionStartedAt = &started
	seedInstance(t, h.svc, rec)

	require.NoError(t, NewReconciler(h.svc, "node-a").reconcileOnce(t.Context()))

	// Left stopping so the next pass retries; the bound is what ends it.
	assert.Equal(t, StatusStopping, h.record(t).Status)
}

// The data volume, the customer ENI and the DNS record are all retained,
// so a start comes back on the same datadir at the same address.
func TestStopDBInstance_RetainsTheEndpointAndTheVolume(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	seed := availableRecord()
	seed.FormatAuthorized = true
	seedInstance(t, h.svc, seed)

	_, err := h.svc.StopDBInstance(t.Context(),
		&rds.StopDBInstanceInput{DBInstanceIdentifier: aws.String(testDBID)}, testAccountID)
	require.NoError(t, err)

	assert.Empty(t, h.volumes.deleted, "a stop must not delete the data volume")
	assert.Empty(t, h.enis.deleted, "a stop must not delete the endpoint ENI")

	rec := h.record(t)
	assert.Equal(t, "vol-rdsdata01", rec.DataVolumeID)
	assert.Equal(t, "eni-cust01", rec.ENIID)
	assert.False(t, rec.FormatAuthorized, "stop must leave no grant for a later start")
}

// A pre-stop snapshot is a later phase's. Accepting it here would report a
// snapshot the customer would then not find.
func TestStopDBInstance_RejectsASnapshotIdentifier(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	seedInstance(t, h.svc, availableRecord())

	_, err := h.svc.StopDBInstance(t.Context(), &rds.StopDBInstanceInput{
		DBInstanceIdentifier: aws.String(testDBID),
		DBSnapshotIdentifier: aws.String("orders-db-pre-stop"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DBSnapshotIdentifier")
	assert.Equal(t, StatusAvailable, h.record(t).Status)
}

func TestStartDBInstance_StartsTheVMAndReportsStarting(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	rec := availableRecord()
	rec.Status = StatusStopped
	rec.FormatAuthorized = true
	seedInstance(t, h.svc, rec)

	out, err := h.svc.StartDBInstance(t.Context(),
		&rds.StartDBInstanceInput{DBInstanceIdentifier: aws.String(testDBID)}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, []string{"start:" + testInstance}, h.cmdr.calls)
	assert.Equal(t, string(StatusStarting), aws.StringValue(out.DBInstance.DBInstanceStatus))
	stored := h.record(t)
	assert.Equal(t, StatusStarting, stored.Status)
	assert.False(t, stored.FormatAuthorized, "start must never restore format permission")
}

// A node restart drops the VM from memory, so the start relaunches it from the
// persisted stopped-instance record rather than failing.
func TestStartDBInstance_FallsBackWhenNoNodeHoldsTheVM(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	h.cmdr.notOnNode = true
	rec := availableRecord()
	rec.Status = StatusStopped
	seedInstance(t, h.svc, rec)

	_, err := h.svc.StartDBInstance(t.Context(),
		&rds.StartDBInstanceInput{DBInstanceIdentifier: aws.String(testDBID)}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, []string{"start:" + testInstance, "start-stopped:" + testInstance}, h.cmdr.calls)
}

// The transition is rejected rather than racing an operation already running.
func TestLifecycleOps_RejectAnIllegalStartingState(t *testing.T) {
	cases := []struct {
		name string
		from Status
		call func(*Service) error
	}{
		{"StartAnAvailableInstance", StatusAvailable, func(s *Service) error {
			_, err := s.StartDBInstance(context.Background(),
				&rds.StartDBInstanceInput{DBInstanceIdentifier: aws.String(testDBID)}, testAccountID)
			return err
		}},
		{"StopAStoppedInstance", StatusStopped, func(s *Service) error {
			_, err := s.StopDBInstance(context.Background(),
				&rds.StopDBInstanceInput{DBInstanceIdentifier: aws.String(testDBID)}, testAccountID)
			return err
		}},
		{"RebootACreatingInstance", StatusCreating, func(s *Service) error {
			_, err := s.RebootDBInstance(context.Background(),
				&rds.RebootDBInstanceInput{DBInstanceIdentifier: aws.String(testDBID)}, testAccountID)
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newLifecycleHarness(t, false)
			rec := availableRecord()
			rec.Status = tc.from
			seedInstance(t, h.svc, rec)

			err := tc.call(h.svc)
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorDBInstanceInvalidState)
			assert.Equal(t, tc.from, h.record(t).Status, "a rejected op must not move the instance")
			assert.Empty(t, h.cmdr.calls)
		})
	}
}

// A failed transition must not be left looking like one still in progress: the
// customer would wait on a reboot that is never coming.
func TestRebootDBInstance_MarksFailedWhenTheVMCommandFails(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	h.cmdr.err = errors.New("no node answered")
	seedInstance(t, h.svc, availableRecord())

	_, err := h.svc.RebootDBInstance(t.Context(),
		&rds.RebootDBInstanceInput{DBInstanceIdentifier: aws.String(testDBID)}, testAccountID)
	require.Error(t, err)

	rec := h.record(t)
	assert.Equal(t, StatusFailed, rec.Status)
	assert.Contains(t, rec.FailureReason, "could not be rebooted")
	assert.Nil(t, rec.TransitionStartedAt)
}

func eventMessages(events []Event) []string {
	messages := make([]string, 0, len(events))
	for _, event := range events {
		messages = append(messages, event.Message)
	}
	return messages
}

// restartingRecord is an instance mid-reboot: the transition began at started,
// and the agent last reported at lastSeen.
func restartingRecord(status Status, started, lastSeen time.Time) DBInstanceRecord {
	rec := availableRecord()
	rec.Status = status
	rec.TransitionStartedAt = &started
	rec.Agent = AgentState{
		InstanceID:   testInstance,
		EngineHealth: EngineHealthHealthy,
		LastSeen:     &lastSeen,
	}
	return rec
}

// The API call that begins a reboot or a start returns before the engine is
// back, so this is what actually lands the instance in available.
func TestReconciler_CompletesARestartOnAHeartbeatFromTheRestartedEngine(t *testing.T) {
	for _, status := range []Status{StatusRebooting, StatusStarting} {
		t.Run(string(status), func(t *testing.T) {
			h := newLifecycleHarness(t, false)
			now := time.Now().UTC()
			seedInstance(t, h.svc, restartingRecord(status, now.Add(-time.Minute), now))

			require.NoError(t, NewReconciler(h.svc, "node-a").reconcileOnce(t.Context()))

			rec := h.record(t)
			assert.Equal(t, StatusAvailable, rec.Status)
		})
	}
}

// The beat that ends a restart usually reaches the leader through KV, where it
// is at most a persist floor behind the engine. Judging it by the raw stale
// window left a database that came back cleanly rebooting until it timed out.
func TestReconciler_CompletesARestartOnAPersistedHeartbeatInsideTheFloor(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	// Older than the stale window, younger than the window plus the floor, and
	// still after the transition it proves finished.
	persisted := time.Now().UTC().Add(-HeartbeatStaleAfter - time.Minute)
	seedInstance(t, h.svc, restartingRecord(StatusRebooting, persisted.Add(-time.Second), persisted))

	require.NoError(t, NewReconciler(h.svc, "node-a").reconcileOnce(t.Context()))

	assert.Equal(t, StatusAvailable, h.record(t).Status)
}

// The VM keeps its instance ID across a restart, so a beat sent before the
// transition began would otherwise report the reboot as finished the instant it
// started.
func TestReconciler_IgnoresAHeartbeatPredatingTheRestart(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	now := time.Now().UTC()
	seedInstance(t, h.svc, restartingRecord(StatusRebooting, now, now.Add(-time.Minute)))

	require.NoError(t, NewReconciler(h.svc, "node-a").reconcileOnce(t.Context()))

	assert.Equal(t, StatusRebooting, h.record(t).Status)
}

// A restart that never comes back has to end somewhere: the customer sees a
// broken instance either way, and failed is the state they can act on.
func TestReconciler_MarksFailedWhenARestartOverrunsItsBound(t *testing.T) {
	t.Parallel()
	h := newLifecycleHarness(t, false)
	now := time.Now().UTC()
	started := now.Add(-2 * transitionTimeout)
	seedInstance(t, h.svc, restartingRecord(StatusStarting, started, started.Add(-time.Minute)))

	require.NoError(t, NewReconciler(h.svc, "node-a").reconcileOnce(t.Context()))

	rec := h.record(t)
	assert.Equal(t, StatusFailed, rec.Status)
	assert.Contains(t, rec.FailureReason, "did not report healthy")
}

// What the next start recovers is the engine's own guarantee. Telling a MariaDB
// customer a write-ahead log will bring back an Aria or MyISAM table would be a
// false assurance about exactly the data most at risk.
func TestUncleanStopMessage_IsEngineAware(t *testing.T) {
	t.Parallel()
	postgres := uncleanStopMessage(t.Context(), "postgres", "stopping the instance")
	assert.Contains(t, postgres, "could not be shut down cleanly before stopping the instance")
	assert.Contains(t, postgres, "write-ahead log")

	mariadb := uncleanStopMessage(t.Context(), "mariadb", "rebooting the instance")
	assert.Contains(t, mariadb, "could not be shut down cleanly before rebooting the instance")
	assert.Contains(t, mariadb, "InnoDB tables will recover")
	assert.Contains(t, mariadb, "may be left inconsistent")
	assert.NotContains(t, mariadb, "write-ahead log")

	// The VM is going down either way, so an engine this build cannot resolve
	// still gets the half of the warning that does not depend on knowing it.
	unknown := uncleanStopMessage(t.Context(), "oracle", "stopping the instance")
	assert.Equal(t, "The database engine could not be shut down cleanly before stopping the instance.", unknown)
}
