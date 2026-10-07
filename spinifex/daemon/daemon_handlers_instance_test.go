package daemon

import (
	"encoding/json"
	"errors"
	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2image "github.com/mulgadc/spinifex/spinifex/domains/ec2/image"
	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	vmmock "github.com/mulgadc/spinifex/spinifex/runtime/compute/vm/mock"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests in this file exercise the stopped/terminated daemon handlers in
// daemon_handlers_instance.go against the shared in-memory vm/mock.StateStore
// fake. They cover error-injection paths (KV write/delete failures, retry,
// list errors) that the JetStream-backed integration tests in
// daemon_handlers_test.go cannot reach with a real backing bucket.

// daemonWithFakeStateStore returns a daemon wired with an in-memory NATS
// connection (via createTestDaemon) and the supplied fake StateStore.
// The daemon does not have JetStream initialized. Rewires d.instanceService
// to point at the fake store so handlers that delegate to InstanceService
// (e.g. ModifyInstanceAttribute) see the injected state.
func daemonWithFakeStateStore(t *testing.T, store *vmmock.StateStore) *Daemon {
	t.Helper()
	d := createTestDaemon(t, sharedNATSURL)
	d.stateStore = store
	d.instanceService = ec2instance.NewInstanceServiceImpl(
		d.config, d.resourceMgr.instanceTypes, d.natsConn,
		objectstore.NewMemoryObjectStore(), d.vmMgr, d.resourceMgr, store,
	)
	return d
}

// requestHandler subscribes fn to subject, sends a request with an
// X-Account-ID header, and returns the reply. The subscription is cleaned up
// when the test ends.
func requestHandler(t *testing.T, nc *nats.Conn, subject string, fn nats.MsgHandler, accountID string, body []byte) *nats.Msg {
	t.Helper()
	sub, err := nc.Subscribe(subject, fn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	msg := nats.NewMsg(subject)
	msg.Data = body
	msg.Header.Set(natsmsg.AccountIDHeader, accountID)
	reply, err := nc.RequestMsg(msg, 5*time.Second)
	require.NoError(t, err)
	return reply
}

func decodeError(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var resp map[string]any
	require.NoError(t, json.Unmarshal(data, &resp))
	return resp
}

// stoppedVMFixture builds a minimally-valid stopped VM for handler tests.
func stoppedVMFixture(id, accountID string) *vm.VM {
	return &vm.VM{
		ID:           id,
		Status:       vm.StateStopped,
		InstanceType: "t3.micro",
		AccountID:    accountID,
		Reservation: &ec2.Reservation{
			ReservationId: aws.String("r-" + id),
			OwnerId:       aws.String(accountID),
		},
		Instance: &ec2.Instance{
			InstanceId:   aws.String(id),
			InstanceType: aws.String("t3.micro"),
		},
	}
}

// --- handleEC2StartStoppedInstance ---

func TestHandleEC2StartStoppedInstance_LoadError(t *testing.T) {
	store := vmmock.New()
	store.LoadStoppedErr = errors.New("kv unavailable")
	d := daemonWithFakeStateStore(t, store)

	body, _ := json.Marshal(ec2instance.StartStoppedInstanceInput{InstanceID: "i-load-fail"})
	reply := requestHandler(t, d.natsConn, "ec2.start.test1", asMsgHandler(d.handleEC2StartStoppedInstance), testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2StartStoppedInstance_StateStoreNil(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)
	// d.stateStore intentionally left nil.

	body, _ := json.Marshal(ec2instance.StartStoppedInstanceInput{InstanceID: "i-no-store"})
	reply := requestHandler(t, d.natsConn, "ec2.start.test2", asMsgHandler(d.handleEC2StartStoppedInstance), testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2StartStoppedInstance_CrossTenantRejected(t *testing.T) {
	store := vmmock.New()
	store.Stopped["i-foreign"] = stoppedVMFixture("i-foreign", "999988887777")
	d := daemonWithFakeStateStore(t, store)

	body, _ := json.Marshal(ec2instance.StartStoppedInstanceInput{InstanceID: "i-foreign"})
	reply := requestHandler(t, d.natsConn, "ec2.start.test3", asMsgHandler(d.handleEC2StartStoppedInstance), testAccountID, body)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, decodeError(t, reply.Data)["Code"])

	// The instance must remain in shared KV — cross-tenant rejection cannot
	// also remove it (would be a leak across accounts).
	_, stillStopped := store.Stopped["i-foreign"]
	assert.True(t, stillStopped, "cross-tenant rejection must not delete the stopped instance")
}

func TestHandleEC2StartStoppedInstance_InstanceTypeUnknown(t *testing.T) {
	store := vmmock.New()
	v := stoppedVMFixture("i-unknown-type", testAccountID)
	v.InstanceType = "definitely.not.a.real.type"
	store.Stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	body, _ := json.Marshal(ec2instance.StartStoppedInstanceInput{InstanceID: v.ID})
	reply := requestHandler(t, d.natsConn, "ec2.start.test4", asMsgHandler(d.handleEC2StartStoppedInstance), testAccountID, body)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, decodeError(t, reply.Data)["Code"])
}

// withShortForwardTimeout shrinks startStoppedForwardTimeout for the duration
// of a test so a forced forward timeout doesn't cost real wall-clock seconds.
func withShortForwardTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := startStoppedForwardTimeout
	startStoppedForwardTimeout = d
	t.Cleanup(func() { startStoppedForwardTimeout = orig })
}

// TestHandleEC2StartStoppedInstance_ForwardTimeoutFallsBackLocally pins
// A forward to LastNode that times out (as opposed to an immediate
// ErrNoResponders) must still fall back to a local start attempt instead of
// surfacing a bare ServerInternal. The target subscriber below is alive but
// silent, so nats: timeout is the only error the forward can produce — proof
// that the fallback path, not ErrNoResponders handling, is what fires here.
// An unresolvable instance type turns "local start was attempted" into a
// distinct, assertable response code (InsufficientInstanceCapacity) instead
// of colliding with the old no-fallback ServerInternal response.
func TestHandleEC2StartStoppedInstance_ForwardTimeoutFallsBackLocally(t *testing.T) {
	withShortForwardTimeout(t, 50*time.Millisecond)

	store := vmmock.New()
	v := stoppedVMFixture("i-timeout-fallback", testAccountID)
	v.InstanceType = "definitely.not.a.real.type"
	v.LastNode = "node-other"
	store.Stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	// Simulate a live-but-unresponsive original node: subscribed, so no
	// ErrNoResponders, but it never replies, so the forward times out.
	silentSub, err := d.natsConn.Subscribe("ec2.start.node-other", func(*nats.Msg) {})
	require.NoError(t, err)
	t.Cleanup(func() { _ = silentSub.Unsubscribe() })

	body, _ := json.Marshal(ec2instance.StartStoppedInstanceInput{InstanceID: v.ID})
	reply := requestHandler(t, d.natsConn, "ec2.start.test5", asMsgHandler(d.handleEC2StartStoppedInstance), testAccountID, body)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, decodeError(t, reply.Data)["Code"],
		"a forward timeout must fall back to a local start attempt, not a bare ServerInternal")
}

// TestHandleEC2StartStoppedInstance_ForwardTimeoutAfterRemoteClaim_NoDoubleStart
// If the forward times out on the caller's
// side AFTER the original node already won the atomic claim (removed the
// record from shared KV) and kept working, the caller's local fallback must
// not double-start the instance. It should observe the record already gone
// and fail cleanly, and must never insert a second copy into its own vmMgr.
func TestHandleEC2StartStoppedInstance_ForwardTimeoutAfterRemoteClaim_NoDoubleStart(t *testing.T) {
	withShortForwardTimeout(t, 50*time.Millisecond)

	store := vmmock.New()
	v := stoppedVMFixture("i-race-claim", testAccountID)
	v.LastNode = "node-other"
	store.Stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	// Simulate the original node winning the claim (atomically removing the
	// shared-KV record, as StartStoppedInstance's ClaimStoppedInstance does)
	// but never replying — e.g. still mid-launch when the caller's forward
	// budget expires.
	claimingSub, err := d.natsConn.Subscribe("ec2.start.node-other", func(*nats.Msg) {
		_, claimErr := store.ClaimStoppedInstance(v.ID)
		assert.NoError(t, claimErr)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = claimingSub.Unsubscribe() })

	body, _ := json.Marshal(ec2instance.StartStoppedInstanceInput{InstanceID: v.ID})
	reply := requestHandler(t, d.natsConn, "ec2.start.test6", asMsgHandler(d.handleEC2StartStoppedInstance), testAccountID, body)

	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, decodeError(t, reply.Data)["Code"],
		"local fallback must see the record already claimed and fail without double-starting")
	_, found := d.vmMgr.Get(v.ID)
	assert.False(t, found, "local fallback must not insert a second running copy of an already-claimed instance")
}

// --- handleEC2TerminateStoppedInstance ---

func TestHandleEC2TerminateStoppedInstance_LoadError(t *testing.T) {
	store := vmmock.New()
	store.LoadStoppedErr = errors.New("kv unavailable")
	d := daemonWithFakeStateStore(t, store)

	body, _ := json.Marshal(ec2instance.TerminateStoppedInstanceInput{InstanceID: "i-load-fail"})
	reply := requestHandler(t, d.natsConn, "ec2.terminate.test1", asMsgHandler(handleNATSRequest(d.node, d.instanceService.TerminateStoppedInstance)), testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2TerminateStoppedInstance_StateStoreNil(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)

	body, _ := json.Marshal(ec2instance.TerminateStoppedInstanceInput{InstanceID: "i-no-store"})
	reply := requestHandler(t, d.natsConn, "ec2.terminate.test2", asMsgHandler(handleNATSRequest(d.node, d.instanceService.TerminateStoppedInstance)), testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

// WriteTerminatedInstance failure must abort BEFORE the stopped-bucket
// delete — otherwise an instance can vanish from both buckets.
func TestHandleEC2TerminateStoppedInstance_WriteTerminatedFailureAborts(t *testing.T) {
	store := vmmock.New()
	store.WriteTerminatedErr = errors.New("terminated bucket write failed")
	v := stoppedVMFixture("i-write-term-fail", testAccountID)
	store.Stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	body, _ := json.Marshal(ec2instance.TerminateStoppedInstanceInput{InstanceID: v.ID})
	reply := requestHandler(t, d.natsConn, "ec2.terminate.test3", asMsgHandler(handleNATSRequest(d.node, d.instanceService.TerminateStoppedInstance)), testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])

	_, stillStopped := store.Stopped[v.ID]
	_, inTerminated := store.Terminated[v.ID]
	attempts := store.DeleteAttempts
	assert.True(t, stillStopped, "stopped entry must remain when terminated write fails (caller can retry)")
	assert.False(t, inTerminated, "no terminated entry should exist after write failure")
	assert.Equal(t, 0, attempts, "DeleteStoppedInstance must not be called when terminated write fails")
}

// First stopped-bucket delete fails, second succeeds — instance must end up
// only in the terminated bucket and the handler must still respond success.
func TestHandleEC2TerminateStoppedInstance_DeleteRetrySucceeds(t *testing.T) {
	store := vmmock.New()
	store.DeleteFailFirst = true
	v := stoppedVMFixture("i-retry-success", testAccountID)
	store.Stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	body, _ := json.Marshal(ec2instance.TerminateStoppedInstanceInput{InstanceID: v.ID})
	reply := requestHandler(t, d.natsConn, "ec2.terminate.test4", asMsgHandler(handleNATSRequest(d.node, d.instanceService.TerminateStoppedInstance)), testAccountID, body)

	var resp map[string]string
	require.NoError(t, json.Unmarshal(reply.Data, &resp))
	assert.Equal(t, "terminated", resp["status"])

	_, stillStopped := store.Stopped[v.ID]
	_, inTerminated := store.Terminated[v.ID]
	attempts := store.DeleteAttempts
	assert.False(t, stillStopped, "stopped entry must be removed after retry success")
	assert.True(t, inTerminated, "terminated entry must be present")
	assert.Equal(t, 2, attempts, "DeleteStoppedInstance must be retried exactly once")
}

// Both stopped-bucket deletes fail — the handler must still return success
// (the terminated-bucket write is the source of truth) and must NOT roll back
// the terminated write.
func TestHandleEC2TerminateStoppedInstance_DeleteAlwaysFailsKeepsTerminated(t *testing.T) {
	store := vmmock.New()
	store.DeleteStoppedErr = errors.New("delete persistently broken")
	v := stoppedVMFixture("i-retry-fail", testAccountID)
	store.Stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	body, _ := json.Marshal(ec2instance.TerminateStoppedInstanceInput{InstanceID: v.ID})
	reply := requestHandler(t, d.natsConn, "ec2.terminate.test5", asMsgHandler(handleNATSRequest(d.node, d.instanceService.TerminateStoppedInstance)), testAccountID, body)

	var resp map[string]string
	require.NoError(t, json.Unmarshal(reply.Data, &resp))
	assert.Equal(t, "terminated", resp["status"], "handler must report success — terminated write succeeded")

	_, inTerminated := store.Terminated[v.ID]
	assert.True(t, inTerminated, "terminated entry must NOT be rolled back when stopped delete fails")
}

func TestHandleEC2TerminateStoppedInstance_CrossTenantRejected(t *testing.T) {
	store := vmmock.New()
	store.Stopped["i-foreign-term"] = stoppedVMFixture("i-foreign-term", "999988887777")
	d := daemonWithFakeStateStore(t, store)

	body, _ := json.Marshal(ec2instance.TerminateStoppedInstanceInput{InstanceID: "i-foreign-term"})
	reply := requestHandler(t, d.natsConn, "ec2.terminate.test6", asMsgHandler(handleNATSRequest(d.node, d.instanceService.TerminateStoppedInstance)), testAccountID, body)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, decodeError(t, reply.Data)["Code"])

	_, inTerminated := store.Terminated["i-foreign-term"]
	_, stillStopped := store.Stopped["i-foreign-term"]
	assert.False(t, inTerminated, "foreign-tenant terminate must not write to terminated bucket")
	assert.True(t, stillStopped, "foreign-tenant terminate must not delete the stopped entry")
}

// --- handleEC2ModifyInstanceAttribute ---

func TestHandleEC2ModifyInstanceAttribute_WriteFailureReturnsServerInternal(t *testing.T) {
	store := vmmock.New()
	store.UpdateStoppedErr = errors.New("kv write failed")
	v := stoppedVMFixture("i-mod-write-fail", testAccountID)
	store.Stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(v.ID),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.large")},
	}
	body, _ := json.Marshal(input)
	reply := requestHandler(t, d.natsConn, "ec2.ModifyInstanceAttribute.test1", asMsgHandler(handleNATSRequest(d.node, d.instanceService.ModifyInstanceAttribute)), testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2ModifyInstanceAttribute_LoadFailureReturnsServerInternal(t *testing.T) {
	store := vmmock.New()
	store.LoadStoppedErr = errors.New("kv unavailable")
	d := daemonWithFakeStateStore(t, store)

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("i-mod-load-fail"),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.large")},
	}
	body, _ := json.Marshal(input)
	reply := requestHandler(t, d.natsConn, "ec2.ModifyInstanceAttribute.test2", asMsgHandler(handleNATSRequest(d.node, d.instanceService.ModifyInstanceAttribute)), testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2ModifyInstanceAttribute_NilInstanceFieldGuard(t *testing.T) {
	// Stored VM with a valid status but a nil Instance pointer — the handler
	// must reject this as a data-integrity violation rather than NPE.
	store := vmmock.New()
	v := &vm.VM{
		ID:           "i-mod-nil-inst",
		Status:       vm.StateStopped,
		InstanceType: "t3.micro",
		AccountID:    testAccountID,
		Reservation:  &ec2.Reservation{ReservationId: aws.String("r-x"), OwnerId: aws.String(testAccountID)},
		Instance:     nil,
	}
	store.Stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(v.ID),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.large")},
	}
	body, _ := json.Marshal(input)
	reply := requestHandler(t, d.natsConn, "ec2.ModifyInstanceAttribute.test3", asMsgHandler(handleNATSRequest(d.node, d.instanceService.ModifyInstanceAttribute)), testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2ModifyInstanceAttribute_EmptyInstanceTypeRejected(t *testing.T) {
	store := vmmock.New()
	v := stoppedVMFixture("i-mod-empty-type", testAccountID)
	store.Stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(v.ID),
		InstanceType: &ec2.AttributeValue{Value: aws.String("")},
	}
	body, _ := json.Marshal(input)
	reply := requestHandler(t, d.natsConn, "ec2.ModifyInstanceAttribute.test4", asMsgHandler(handleNATSRequest(d.node, d.instanceService.ModifyInstanceAttribute)), testAccountID, body)
	assert.Equal(t, awserrors.ErrorInvalidInstanceAttributeValue, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2ModifyInstanceAttribute_CrossTenantRejected(t *testing.T) {
	store := vmmock.New()
	store.Stopped["i-mod-foreign"] = stoppedVMFixture("i-mod-foreign", "999988887777")
	d := daemonWithFakeStateStore(t, store)

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("i-mod-foreign"),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.large")},
	}
	body, _ := json.Marshal(input)
	reply := requestHandler(t, d.natsConn, "ec2.ModifyInstanceAttribute.test5", asMsgHandler(handleNATSRequest(d.node, d.instanceService.ModifyInstanceAttribute)), testAccountID, body)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, decodeError(t, reply.Data)["Code"])
}

// --- handleEC2DescribeInstanceAttribute ---

func TestHandleEC2DescribeInstanceAttribute_StoppedFallback_LoadError(t *testing.T) {
	store := vmmock.New()
	store.LoadStoppedErr = errors.New("kv unavailable")
	d := daemonWithFakeStateStore(t, store)
	// d.vmMgr has no running instance, so the handler falls through to the
	// stopped KV branch — which now errors.

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-describe-load-fail"),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}
	body, _ := json.Marshal(input)
	reply := requestHandler(t, d.natsConn, "ec2.DescribeInstanceAttribute.test1", asMsgHandler(handleNATSRequest(d.node, d.instanceService.DescribeInstanceAttribute)), testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2DescribeInstanceAttribute_StoppedFallback_HitsKV(t *testing.T) {
	store := vmmock.New()
	v := stoppedVMFixture("i-describe-stopped", testAccountID)
	v.InstanceType = "t3.medium"
	v.Instance.InstanceType = aws.String("t3.medium")
	store.Stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(v.ID),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}
	body, _ := json.Marshal(input)
	reply := requestHandler(t, d.natsConn, "ec2.DescribeInstanceAttribute.test2", asMsgHandler(handleNATSRequest(d.node, d.instanceService.DescribeInstanceAttribute)), testAccountID, body)

	var output ec2.DescribeInstanceAttributeOutput
	require.NoError(t, json.Unmarshal(reply.Data, &output))
	require.NotNil(t, output.InstanceType)
	require.NotNil(t, output.InstanceType.Value)
	assert.Equal(t, "t3.medium", *output.InstanceType.Value)
}

func TestHandleEC2DescribeInstanceAttribute_StateStoreNil(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)
	// d.stateStore left nil; vmMgr also empty -> falls through to KV branch
	// which short-circuits with ServerInternal.

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-no-store-describe"),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}
	body, _ := json.Marshal(input)
	reply := requestHandler(t, d.natsConn, "ec2.DescribeInstanceAttribute.test3", asMsgHandler(handleNATSRequest(d.node, d.instanceService.DescribeInstanceAttribute)), testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

// --- handleEC2DescribeStoppedInstances / handleEC2DescribeTerminatedInstances ---

func TestHandleEC2DescribeStoppedInstances_ListError(t *testing.T) {
	store := vmmock.New()
	store.ListStoppedErr = errors.New("list failed")
	d := daemonWithFakeStateStore(t, store)

	reply := requestHandler(t, d.natsConn, "ec2.DescribeStoppedInstances.test1", asMsgHandler(handleNATSRequest(d.node, d.instanceService.DescribeStoppedInstances)), testAccountID, []byte("{}"))
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2DescribeTerminatedInstances_ListError(t *testing.T) {
	store := vmmock.New()
	store.ListTerminatedErr = errors.New("list failed")
	d := daemonWithFakeStateStore(t, store)

	reply := requestHandler(t, d.natsConn, "ec2.DescribeTerminatedInstances.test1", asMsgHandler(handleNATSRequest(d.node, d.instanceService.DescribeTerminatedInstances)), testAccountID, []byte("{}"))
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2DescribeStoppedInstances_CrossAccountIsolation(t *testing.T) {
	store := vmmock.New()
	store.Stopped["i-mine"] = stoppedVMFixture("i-mine", testAccountID)
	store.Stopped["i-yours"] = stoppedVMFixture("i-yours", "999988887777")
	d := daemonWithFakeStateStore(t, store)

	reply := requestHandler(t, d.natsConn, "ec2.DescribeStoppedInstances.test3", asMsgHandler(handleNATSRequest(d.node, d.instanceService.DescribeStoppedInstances)), testAccountID, []byte("{}"))

	var output ec2.DescribeInstancesOutput
	require.NoError(t, json.Unmarshal(reply.Data, &output))

	var seen []string
	for _, r := range output.Reservations {
		for _, inst := range r.Instances {
			if inst.InstanceId != nil {
				seen = append(seen, *inst.InstanceId)
			}
		}
	}
	assert.ElementsMatch(t, []string{"i-mine"}, seen, "caller must only see their own instances")
}

// --- handleEC2RunInstances ---

func TestHandleEC2RunInstances_MissingAccountHeader(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)
	body := mustMarshal(t, &ec2.RunInstancesInput{InstanceType: aws.String(getTestInstanceType(t))})

	reply := requestHandler(t, d.natsConn, "ec2.RunInstances.p1-noacct", asMsgHandler(d.handleEC2RunInstances), "", body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
	assert.Zero(t, d.vmMgr.Count(), "an unattributable launch must not reserve anything")
}

func TestHandleEC2RunInstances_InvalidReservationTarget(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)
	body := mustMarshal(t, &ec2.RunInstancesInput{
		InstanceType: aws.String(getTestInstanceType(t)),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		CapacityReservationSpecification: &ec2.CapacityReservationSpecification{
			CapacityReservationTarget: &ec2.CapacityReservationTarget{CapacityReservationId: aws.String("cr-0000000000missing")},
		},
	})

	reply := requestHandler(t, d.natsConn, "ec2.RunInstances.p1-badcr", asMsgHandler(d.handleEC2RunInstances), testAccountID, body)
	assert.Equal(t, awserrors.ErrorInvalidCapacityReservationIdNotFound, decodeError(t, reply.Data)["Code"])
	assert.Zero(t, d.vmMgr.Count())
}

// A gateway-minted reservation ID replaces the one prepare generated, and a
// central tag store that cannot take the launch tags does not fail the launch.
func TestHandleEC2RunInstances_GatewayReservationAndTagStoreFailure(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)
	images := objectstore.NewMemoryObjectStore()
	seedTestAMI(t, images, d.config.Predastore.Bucket, "ami-p1-gateway")
	d.instanceService = ec2instance.NewInstanceServiceImpl(
		d.config, d.resourceMgr.instanceTypes, d.natsConn, images, d.vmMgr, d.resourceMgr, nil)
	d.instanceService.SetRunInstancesDeps(
		ec2image.NewImageServiceImplWithStore(images, d.config.Predastore.Bucket),
		ec2key.NewKeyServiceImplWithStore(images, d.config.Predastore.Bucket), nil, nil)

	_, tagKV := faultBucket(t)
	for _, m := range []string{"Put", "Create", "Update"} {
		tagKV.setFail(m, true)
	}
	d.tagsService = ec2tags.NewTagsServiceImplWithStore(d.config, objectstore.NewMemoryObjectStore(), tagKV)
	t.Cleanup(d.vmMgr.WaitForBackgroundWork)

	subject := "ec2.RunInstances.p1-gateway"
	sub, err := d.natsConn.Subscribe(subject, asMsgHandler(d.handleEC2RunInstances))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	msg := nats.NewMsg(subject)
	msg.Data = mustMarshal(t, &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-p1-gateway"),
		InstanceType: aws.String(getTestInstanceType(t)),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("instance"),
			Tags:         []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("web")}},
		}},
	})
	msg.Header.Set(natsmsg.AccountIDHeader, testAccountID)
	msg.Header.Set(natsmsg.ReservationIDHeader, "r-0gateway000000001")
	reply, err := d.natsConn.RequestMsg(msg, 5*time.Second)
	require.NoError(t, err)

	var reservation ec2.Reservation
	require.NoError(t, json.Unmarshal(reply.Data, &reservation), "%s", reply.Data)
	assert.Equal(t, "r-0gateway000000001", aws.StringValue(reservation.ReservationId))
	require.Len(t, reservation.Instances, 1, "a failed tag projection must not fail the launch")
}

// --- handleEC2StartStoppedInstance forwarding ---

func TestHandleEC2StartStoppedInstance_Forwarding(t *testing.T) {
	newFixture := func(t *testing.T, id, lastNode string) (*Daemon, *vmmock.StateStore, []byte) {
		store := vmmock.New()
		v := stoppedVMFixture(id, testAccountID)
		v.InstanceType = "definitely.not.a.real.type"
		v.LastNode = lastNode
		store.Stopped[v.ID] = v
		body, err := json.Marshal(ec2instance.StartStoppedInstanceInput{InstanceID: v.ID})
		require.NoError(t, err)
		return daemonWithFakeStateStore(t, store), store, body
	}
	owner := func(t *testing.T, d *Daemon, node string, fn nats.MsgHandler) {
		sub, err := d.natsConn.Subscribe("ec2.start."+node, fn)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}

	t.Run("owner success is relayed", func(t *testing.T) {
		d, _, body := newFixture(t, "i-p1-fwd-ok", "node-p1-ok")
		owner(t, d, "node-p1-ok", func(m *nats.Msg) { _ = m.Respond([]byte(`{"started":true}`)) })

		reply := requestHandler(t, d.natsConn, "ec2.start.p1-ok", asMsgHandler(d.handleEC2StartStoppedInstance), testAccountID, body)
		assert.JSONEq(t, `{"started":true}`, string(reply.Data))
	})

	t.Run("owner at capacity falls back locally", func(t *testing.T) {
		d, store, body := newFixture(t, "i-p1-fwd-cap", "node-p1-cap")
		// The owner claims the record before answering at capacity, so the local
		// attempt that follows finds it gone: a NotFound reply proves the
		// fallback ran rather than the capacity error being relayed.
		owner(t, d, "node-p1-cap", func(m *nats.Msg) {
			_, _ = store.ClaimStoppedInstance("i-p1-fwd-cap")
			_ = m.Respond(awserrors.GenerateErrorPayload(awserrors.ErrorInsufficientInstanceCapacity))
		})

		reply := requestHandler(t, d.natsConn, "ec2.start.p1-cap", asMsgHandler(d.handleEC2StartStoppedInstance), testAccountID, body)
		assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, decodeError(t, reply.Data)["Code"])
	})

	t.Run("owner not subscribed falls back locally", func(t *testing.T) {
		d, _, body := newFixture(t, "i-p1-fwd-gone", "node-p1-gone")
		reply := requestHandler(t, d.natsConn, "ec2.start.p1-gone", asMsgHandler(d.handleEC2StartStoppedInstance), testAccountID, body)
		assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, decodeError(t, reply.Data)["Code"],
			"the local attempt must run and reject the unresolvable instance type")
	})

	t.Run("relay to a departed caller is an error", func(t *testing.T) {
		d, _, body := newFixture(t, "i-p1-fwd-relay", "node-p1-relay")
		owner(t, d, "node-p1-relay", func(m *nats.Msg) { _ = m.Respond([]byte(`{}`)) })

		msg := nats.NewMsg("ec2.start.p1-relay")
		msg.Data = body
		msg.Header.Set(natsmsg.AccountIDHeader, testAccountID)
		assert.Equal(t, outcomeError, d.handleEC2StartStoppedInstance(msg))
	})
}

// --- handleSetInstanceTags / handleSetInstanceMonitoring ---

func setTagsCommand(id string) ec2v1.EC2InstanceCommand {
	return ec2v1.EC2InstanceCommand{
		ID:         id,
		Attributes: ec2v1.EC2CommandAttributes{SetInstanceTags: true},
		InstanceTagsData: &ec2v1.InstanceTagsData{
			Tags: map[string]string{"env": "dev"},
		},
	}
}

func TestHandleSetInstanceTags_Failures(t *testing.T) {
	t.Run("record without an instance", func(t *testing.T) {
		const id = "i-p1-tags-norecord"
		d := tagTestDaemon(t, id, nil)
		d.vmMgr.UpdateState(id, func(v *vm.VM) { v.Instance = nil })

		reply := requestHandler(t, d.natsConn, "ec2.cmd."+id, d.handleEC2Events, testAccountID, mustMarshal(t, setTagsCommand(id)))
		assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
	})

	t.Run("central store write fails", func(t *testing.T) {
		const id = "i-p1-tags-central"
		d := tagTestDaemon(t, id, map[string]string{"Name": "web"})
		_, tagKV := faultBucket(t)
		for _, m := range []string{"Put", "Create", "Update"} {
			tagKV.setFail(m, true)
		}
		d.tagsService = ec2tags.NewTagsServiceImplWithStore(d.config, objectstore.NewMemoryObjectStore(), tagKV)

		reply := requestHandler(t, d.natsConn, "ec2.cmd."+id, d.handleEC2Events, testAccountID, mustMarshal(t, setTagsCommand(id)))
		assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
		assert.Equal(t, map[string]string{"Name": "web"}, recordTags(t, d, id),
			"the record must not move ahead of the central store")
	})

	t.Run("persist fails", func(t *testing.T) {
		const id = "i-p1-tags-persist"
		d := tagTestDaemon(t, id, nil)
		d.vmMgr.SetDeps(vm.Deps{NodeID: d.node, StateStore: &vmmock.StateStore{SaveRunningErr: errInjected}})

		reply := requestHandler(t, d.natsConn, "ec2.cmd."+id, d.handleEC2Events, testAccountID, mustMarshal(t, setTagsCommand(id)))
		assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
	})

	t.Run("caller gone after the write", func(t *testing.T) {
		const id = "i-p1-tags-noreply"
		d := tagTestDaemon(t, id, nil)
		instance, ok := d.vmMgr.Get(id)
		require.True(t, ok)

		msg := nats.NewMsg("ec2.cmd." + id)
		msg.Header.Set(natsmsg.AccountIDHeader, testAccountID)
		assert.Equal(t, outcomeSuccess, d.handleSetInstanceTags(t.Context(), msg, setTagsCommand(id), instance))
		assert.Equal(t, map[string]string{"env": "dev"}, recordTags(t, d, id))
	})
}

func TestHandleSetInstanceMonitoring_Failures(t *testing.T) {
	enable := func(id string) ec2v1.EC2InstanceCommand {
		return ec2v1.EC2InstanceCommand{
			ID:                     id,
			Attributes:             ec2v1.EC2CommandAttributes{SetInstanceMonitoring: true},
			InstanceMonitoringData: &ec2v1.InstanceMonitoringData{Enabled: true},
		}
	}

	t.Run("missing data", func(t *testing.T) {
		const id = "i-p1-mon-nodata"
		d, _ := monitoringTestDaemon(t, id, false)
		cmd := enable(id)
		cmd.InstanceMonitoringData = nil

		reply := requestHandler(t, d.natsConn, "ec2.cmd."+id, d.handleEC2Events, testAccountID, mustMarshal(t, cmd))
		assert.Equal(t, awserrors.ErrorMissingParameter, decodeError(t, reply.Data)["Code"])
	})

	t.Run("persist fails", func(t *testing.T) {
		const id = "i-p1-mon-persist"
		d, _ := monitoringTestDaemon(t, id, false)
		d.vmMgr.SetDeps(vm.Deps{NodeID: d.node, StateStore: &vmmock.StateStore{SaveRunningErr: errInjected}})

		reply := requestHandler(t, d.natsConn, "ec2.cmd."+id, d.handleEC2Events, testAccountID, mustMarshal(t, enable(id)))
		assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
	})

	t.Run("caller gone after the write", func(t *testing.T) {
		const id = "i-p1-mon-noreply"
		d, _ := monitoringTestDaemon(t, id, false)
		instance, ok := d.vmMgr.Get(id)
		require.True(t, ok)

		assert.Equal(t, outcomeSuccess, d.handleSetInstanceMonitoring(t.Context(), noReplyMsg("ec2.cmd."+id, nil), enable(id), instance))
		assert.True(t, recordMonitoring(t, d, id))
	})
}
