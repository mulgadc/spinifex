package daemon

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests in this file exercise the stopped/terminated daemon handlers in
// daemon_handlers_instance.go against an in-memory vm.StateStore fake.
// They cover error-injection paths (KV write/delete failures, retry, list
// errors) that the JetStream-backed integration tests in daemon_handlers_test.go
// cannot reach with a real backing bucket.

// fakeStateStore is an in-memory vm.StateStore for daemon-handler unit tests.
// Each method has an optional error knob so a test can drive specific failure
// branches without standing up an embedded JetStream server.
type fakeStateStore struct {
	mu sync.Mutex

	saveRunningErr error

	stopped          map[string]*vm.VM
	loadStoppedErr   error
	writeStoppedErr  error
	updateStoppedErr error
	listStoppedErr   error
	deleteStoppedErr error

	// deleteStoppedFailFirst makes the first DeleteStoppedInstance call fail
	// and the second succeed — exercises the handler's single retry.
	deleteStoppedFailFirst bool
	deleteStoppedAttempts  int

	claimStoppedErr error

	terminated         map[string]*vm.VM
	writeTerminatedErr error
	listTerminatedErr  error
}

func newFakeStateStore() *fakeStateStore {
	return &fakeStateStore{
		stopped:    map[string]*vm.VM{},
		terminated: map[string]*vm.VM{},
	}
}

func (f *fakeStateStore) SaveRunningState(_ string, _ map[string]*vm.VM) error {
	return f.saveRunningErr
}

func (f *fakeStateStore) LoadRunningState(_ string) (map[string]*vm.VM, error) {
	return map[string]*vm.VM{}, nil
}

func (f *fakeStateStore) WriteStoppedInstance(id string, v *vm.VM) error {
	if f.writeStoppedErr != nil {
		return f.writeStoppedErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped[id] = v
	return nil
}

func (f *fakeStateStore) LoadStoppedInstance(id string) (*vm.VM, error) {
	if f.loadStoppedErr != nil {
		return nil, f.loadStoppedErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.stopped[id]; ok {
		return v, nil
	}
	return nil, nil
}

func (f *fakeStateStore) DeleteStoppedInstance(id string) error {
	f.mu.Lock()
	f.deleteStoppedAttempts++
	attempt := f.deleteStoppedAttempts
	f.mu.Unlock()

	if f.deleteStoppedErr != nil {
		return f.deleteStoppedErr
	}
	if f.deleteStoppedFailFirst && attempt == 1 {
		return errors.New("simulated transient delete failure")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.stopped, id)
	return nil
}

// ClaimStoppedInstance mimics the real atomic-delete claim for tests: under
// the store lock, remove and return the entry, or claimStoppedErr /
// vm.ErrStoppedInstanceClaimed if it is already gone or a test forced an error.
func (f *fakeStateStore) ClaimStoppedInstance(id string) (*vm.VM, error) {
	if f.claimStoppedErr != nil {
		return nil, f.claimStoppedErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.stopped[id]
	if !ok {
		return nil, vm.ErrStoppedInstanceClaimed
	}
	delete(f.stopped, id)
	return v, nil
}

// UpdateStoppedInstance mimics the real CAS semantics for tests: mutate runs
// under the store lock against the stored value, and a missing record
// returns nats.ErrKeyNotFound (matching JetStreamManager's createIfAbsent=false
// behavior) rather than resurrecting it.
func (f *fakeStateStore) UpdateStoppedInstance(id string, mutate func(*vm.VM)) (*vm.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateStoppedErr != nil {
		return nil, f.updateStoppedErr
	}
	v, ok := f.stopped[id]
	if !ok {
		return nil, nats.ErrKeyNotFound
	}
	mutate(v)
	return v, nil
}

func (f *fakeStateStore) ListStoppedInstances() ([]*vm.VM, error) {
	if f.listStoppedErr != nil {
		return nil, f.listStoppedErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*vm.VM, 0, len(f.stopped))
	for _, v := range f.stopped {
		out = append(out, v)
	}
	return out, nil
}

func (f *fakeStateStore) WriteTerminatedInstance(id string, v *vm.VM) error {
	if f.writeTerminatedErr != nil {
		return f.writeTerminatedErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated[id] = v
	return nil
}

func (f *fakeStateStore) ListTerminatedInstances() ([]*vm.VM, error) {
	if f.listTerminatedErr != nil {
		return nil, f.listTerminatedErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*vm.VM, 0, len(f.terminated))
	for _, v := range f.terminated {
		out = append(out, v)
	}
	return out, nil
}

func (f *fakeStateStore) DeleteTerminatedInstance(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.terminated, id)
	return nil
}

// UpdateTerminatedInstance mimics the real CAS semantics for tests: mutate
// runs under the store lock against the stored value.
func (f *fakeStateStore) UpdateTerminatedInstance(id string, mutate func(*vm.VM)) (*vm.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.terminated[id]
	if !ok {
		return nil, errors.New("terminated instance not found")
	}
	mutate(v)
	return v, nil
}

var _ vm.StateStore = (*fakeStateStore)(nil)

// daemonWithFakeStateStore returns a daemon wired with an in-memory NATS
// connection (via createTestDaemon) and the supplied fake StateStore.
// The daemon does not have JetStream initialized. Rewires d.instanceService
// to point at the fake store so handlers that delegate to InstanceService
// (e.g. ModifyInstanceAttribute) see the injected state.
func daemonWithFakeStateStore(t *testing.T, store *fakeStateStore) *Daemon {
	t.Helper()
	d := createTestDaemon(t, sharedNATSURL)
	d.stateStore = store
	d.instanceService = handlers_ec2_instance.NewInstanceServiceImpl(
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
	msg.Header.Set(utils.AccountIDHeader, accountID)
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
	store := newFakeStateStore()
	store.loadStoppedErr = errors.New("kv unavailable")
	d := daemonWithFakeStateStore(t, store)

	body, _ := json.Marshal(handlers_ec2_instance.StartStoppedInstanceInput{InstanceID: "i-load-fail"})
	reply := requestHandler(t, d.natsConn, "ec2.start.test1", d.handleEC2StartStoppedInstance, testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2StartStoppedInstance_StateStoreNil(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)
	// d.stateStore intentionally left nil.

	body, _ := json.Marshal(handlers_ec2_instance.StartStoppedInstanceInput{InstanceID: "i-no-store"})
	reply := requestHandler(t, d.natsConn, "ec2.start.test2", d.handleEC2StartStoppedInstance, testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2StartStoppedInstance_CrossTenantRejected(t *testing.T) {
	store := newFakeStateStore()
	store.stopped["i-foreign"] = stoppedVMFixture("i-foreign", "999988887777")
	d := daemonWithFakeStateStore(t, store)

	body, _ := json.Marshal(handlers_ec2_instance.StartStoppedInstanceInput{InstanceID: "i-foreign"})
	reply := requestHandler(t, d.natsConn, "ec2.start.test3", d.handleEC2StartStoppedInstance, testAccountID, body)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, decodeError(t, reply.Data)["Code"])

	// The instance must remain in shared KV — cross-tenant rejection cannot
	// also remove it (would be a leak across accounts).
	store.mu.Lock()
	_, stillStopped := store.stopped["i-foreign"]
	store.mu.Unlock()
	assert.True(t, stillStopped, "cross-tenant rejection must not delete the stopped instance")
}

func TestHandleEC2StartStoppedInstance_InstanceTypeUnknown(t *testing.T) {
	store := newFakeStateStore()
	v := stoppedVMFixture("i-unknown-type", testAccountID)
	v.InstanceType = "definitely.not.a.real.type"
	store.stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	body, _ := json.Marshal(handlers_ec2_instance.StartStoppedInstanceInput{InstanceID: v.ID})
	reply := requestHandler(t, d.natsConn, "ec2.start.test4", d.handleEC2StartStoppedInstance, testAccountID, body)
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
// siv-481: a forward to LastNode that times out (as opposed to an immediate
// ErrNoResponders) must still fall back to a local start attempt instead of
// surfacing a bare ServerInternal. The target subscriber below is alive but
// silent, so nats: timeout is the only error the forward can produce — proof
// that the fallback path, not ErrNoResponders handling, is what fires here.
// An unresolvable instance type turns "local start was attempted" into a
// distinct, assertable response code (InsufficientInstanceCapacity) instead
// of colliding with the old no-fallback ServerInternal response.
func TestHandleEC2StartStoppedInstance_ForwardTimeoutFallsBackLocally(t *testing.T) {
	withShortForwardTimeout(t, 50*time.Millisecond)

	store := newFakeStateStore()
	v := stoppedVMFixture("i-timeout-fallback", testAccountID)
	v.InstanceType = "definitely.not.a.real.type"
	v.LastNode = "node-other"
	store.stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	// Simulate a live-but-unresponsive original node: subscribed, so no
	// ErrNoResponders, but it never replies, so the forward times out.
	silentSub, err := d.natsConn.Subscribe("ec2.start.node-other", func(*nats.Msg) {})
	require.NoError(t, err)
	t.Cleanup(func() { _ = silentSub.Unsubscribe() })

	body, _ := json.Marshal(handlers_ec2_instance.StartStoppedInstanceInput{InstanceID: v.ID})
	reply := requestHandler(t, d.natsConn, "ec2.start.test5", d.handleEC2StartStoppedInstance, testAccountID, body)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, decodeError(t, reply.Data)["Code"],
		"a forward timeout must fall back to a local start attempt, not a bare ServerInternal")
}

// TestHandleEC2StartStoppedInstance_ForwardTimeoutAfterRemoteClaim_NoDoubleStart
// pins the other half of siv-481: if the forward times out on the caller's
// side AFTER the original node already won the atomic claim (removed the
// record from shared KV) and kept working, the caller's local fallback must
// not double-start the instance. It should observe the record already gone
// and fail cleanly, and must never insert a second copy into its own vmMgr.
func TestHandleEC2StartStoppedInstance_ForwardTimeoutAfterRemoteClaim_NoDoubleStart(t *testing.T) {
	withShortForwardTimeout(t, 50*time.Millisecond)

	store := newFakeStateStore()
	v := stoppedVMFixture("i-race-claim", testAccountID)
	v.LastNode = "node-other"
	store.stopped[v.ID] = v
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

	body, _ := json.Marshal(handlers_ec2_instance.StartStoppedInstanceInput{InstanceID: v.ID})
	reply := requestHandler(t, d.natsConn, "ec2.start.test6", d.handleEC2StartStoppedInstance, testAccountID, body)

	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, decodeError(t, reply.Data)["Code"],
		"local fallback must see the record already claimed and fail without double-starting")
	_, found := d.vmMgr.Get(v.ID)
	assert.False(t, found, "local fallback must not insert a second running copy of an already-claimed instance")
}

// --- handleEC2TerminateStoppedInstance ---

func TestHandleEC2TerminateStoppedInstance_LoadError(t *testing.T) {
	store := newFakeStateStore()
	store.loadStoppedErr = errors.New("kv unavailable")
	d := daemonWithFakeStateStore(t, store)

	body, _ := json.Marshal(handlers_ec2_instance.TerminateStoppedInstanceInput{InstanceID: "i-load-fail"})
	reply := requestHandler(t, d.natsConn, "ec2.terminate.test1", d.handleEC2TerminateStoppedInstance, testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2TerminateStoppedInstance_StateStoreNil(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)

	body, _ := json.Marshal(handlers_ec2_instance.TerminateStoppedInstanceInput{InstanceID: "i-no-store"})
	reply := requestHandler(t, d.natsConn, "ec2.terminate.test2", d.handleEC2TerminateStoppedInstance, testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

// WriteTerminatedInstance failure must abort BEFORE the stopped-bucket
// delete — otherwise an instance can vanish from both buckets.
func TestHandleEC2TerminateStoppedInstance_WriteTerminatedFailureAborts(t *testing.T) {
	store := newFakeStateStore()
	store.writeTerminatedErr = errors.New("terminated bucket write failed")
	v := stoppedVMFixture("i-write-term-fail", testAccountID)
	store.stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	body, _ := json.Marshal(handlers_ec2_instance.TerminateStoppedInstanceInput{InstanceID: v.ID})
	reply := requestHandler(t, d.natsConn, "ec2.terminate.test3", d.handleEC2TerminateStoppedInstance, testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])

	store.mu.Lock()
	_, stillStopped := store.stopped[v.ID]
	_, inTerminated := store.terminated[v.ID]
	attempts := store.deleteStoppedAttempts
	store.mu.Unlock()
	assert.True(t, stillStopped, "stopped entry must remain when terminated write fails (caller can retry)")
	assert.False(t, inTerminated, "no terminated entry should exist after write failure")
	assert.Equal(t, 0, attempts, "DeleteStoppedInstance must not be called when terminated write fails")
}

// First stopped-bucket delete fails, second succeeds — instance must end up
// only in the terminated bucket and the handler must still respond success.
func TestHandleEC2TerminateStoppedInstance_DeleteRetrySucceeds(t *testing.T) {
	store := newFakeStateStore()
	store.deleteStoppedFailFirst = true
	v := stoppedVMFixture("i-retry-success", testAccountID)
	store.stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	body, _ := json.Marshal(handlers_ec2_instance.TerminateStoppedInstanceInput{InstanceID: v.ID})
	reply := requestHandler(t, d.natsConn, "ec2.terminate.test4", d.handleEC2TerminateStoppedInstance, testAccountID, body)

	var resp map[string]string
	require.NoError(t, json.Unmarshal(reply.Data, &resp))
	assert.Equal(t, "terminated", resp["status"])

	store.mu.Lock()
	_, stillStopped := store.stopped[v.ID]
	_, inTerminated := store.terminated[v.ID]
	attempts := store.deleteStoppedAttempts
	store.mu.Unlock()
	assert.False(t, stillStopped, "stopped entry must be removed after retry success")
	assert.True(t, inTerminated, "terminated entry must be present")
	assert.Equal(t, 2, attempts, "DeleteStoppedInstance must be retried exactly once")
}

// Both stopped-bucket deletes fail — the handler must still return success
// (the terminated-bucket write is the source of truth) and must NOT roll back
// the terminated write.
func TestHandleEC2TerminateStoppedInstance_DeleteAlwaysFailsKeepsTerminated(t *testing.T) {
	store := newFakeStateStore()
	store.deleteStoppedErr = errors.New("delete persistently broken")
	v := stoppedVMFixture("i-retry-fail", testAccountID)
	store.stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	body, _ := json.Marshal(handlers_ec2_instance.TerminateStoppedInstanceInput{InstanceID: v.ID})
	reply := requestHandler(t, d.natsConn, "ec2.terminate.test5", d.handleEC2TerminateStoppedInstance, testAccountID, body)

	var resp map[string]string
	require.NoError(t, json.Unmarshal(reply.Data, &resp))
	assert.Equal(t, "terminated", resp["status"], "handler must report success — terminated write succeeded")

	store.mu.Lock()
	_, inTerminated := store.terminated[v.ID]
	store.mu.Unlock()
	assert.True(t, inTerminated, "terminated entry must NOT be rolled back when stopped delete fails")
}

func TestHandleEC2TerminateStoppedInstance_CrossTenantRejected(t *testing.T) {
	store := newFakeStateStore()
	store.stopped["i-foreign-term"] = stoppedVMFixture("i-foreign-term", "999988887777")
	d := daemonWithFakeStateStore(t, store)

	body, _ := json.Marshal(handlers_ec2_instance.TerminateStoppedInstanceInput{InstanceID: "i-foreign-term"})
	reply := requestHandler(t, d.natsConn, "ec2.terminate.test6", d.handleEC2TerminateStoppedInstance, testAccountID, body)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, decodeError(t, reply.Data)["Code"])

	store.mu.Lock()
	_, inTerminated := store.terminated["i-foreign-term"]
	_, stillStopped := store.stopped["i-foreign-term"]
	store.mu.Unlock()
	assert.False(t, inTerminated, "foreign-tenant terminate must not write to terminated bucket")
	assert.True(t, stillStopped, "foreign-tenant terminate must not delete the stopped entry")
}

// --- handleEC2ModifyInstanceAttribute ---

func TestHandleEC2ModifyInstanceAttribute_WriteFailureReturnsServerInternal(t *testing.T) {
	store := newFakeStateStore()
	store.updateStoppedErr = errors.New("kv write failed")
	v := stoppedVMFixture("i-mod-write-fail", testAccountID)
	store.stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(v.ID),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.large")},
	}
	body, _ := json.Marshal(input)
	reply := requestHandler(t, d.natsConn, "ec2.ModifyInstanceAttribute.test1", d.handleEC2ModifyInstanceAttribute, testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2ModifyInstanceAttribute_LoadFailureReturnsServerInternal(t *testing.T) {
	store := newFakeStateStore()
	store.loadStoppedErr = errors.New("kv unavailable")
	d := daemonWithFakeStateStore(t, store)

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("i-mod-load-fail"),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.large")},
	}
	body, _ := json.Marshal(input)
	reply := requestHandler(t, d.natsConn, "ec2.ModifyInstanceAttribute.test2", d.handleEC2ModifyInstanceAttribute, testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2ModifyInstanceAttribute_NilInstanceFieldGuard(t *testing.T) {
	// Stored VM with a valid status but a nil Instance pointer — the handler
	// must reject this as a data-integrity violation rather than NPE.
	store := newFakeStateStore()
	v := &vm.VM{
		ID:           "i-mod-nil-inst",
		Status:       vm.StateStopped,
		InstanceType: "t3.micro",
		AccountID:    testAccountID,
		Reservation:  &ec2.Reservation{ReservationId: aws.String("r-x"), OwnerId: aws.String(testAccountID)},
		Instance:     nil,
	}
	store.stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(v.ID),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.large")},
	}
	body, _ := json.Marshal(input)
	reply := requestHandler(t, d.natsConn, "ec2.ModifyInstanceAttribute.test3", d.handleEC2ModifyInstanceAttribute, testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2ModifyInstanceAttribute_EmptyInstanceTypeRejected(t *testing.T) {
	store := newFakeStateStore()
	v := stoppedVMFixture("i-mod-empty-type", testAccountID)
	store.stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String(v.ID),
		InstanceType: &ec2.AttributeValue{Value: aws.String("")},
	}
	body, _ := json.Marshal(input)
	reply := requestHandler(t, d.natsConn, "ec2.ModifyInstanceAttribute.test4", d.handleEC2ModifyInstanceAttribute, testAccountID, body)
	assert.Equal(t, awserrors.ErrorInvalidInstanceAttributeValue, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2ModifyInstanceAttribute_CrossTenantRejected(t *testing.T) {
	store := newFakeStateStore()
	store.stopped["i-mod-foreign"] = stoppedVMFixture("i-mod-foreign", "999988887777")
	d := daemonWithFakeStateStore(t, store)

	input := &ec2.ModifyInstanceAttributeInput{
		InstanceId:   aws.String("i-mod-foreign"),
		InstanceType: &ec2.AttributeValue{Value: aws.String("t3.large")},
	}
	body, _ := json.Marshal(input)
	reply := requestHandler(t, d.natsConn, "ec2.ModifyInstanceAttribute.test5", d.handleEC2ModifyInstanceAttribute, testAccountID, body)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, decodeError(t, reply.Data)["Code"])
}

// --- handleEC2DescribeInstanceAttribute ---

func TestHandleEC2DescribeInstanceAttribute_StoppedFallback_LoadError(t *testing.T) {
	store := newFakeStateStore()
	store.loadStoppedErr = errors.New("kv unavailable")
	d := daemonWithFakeStateStore(t, store)
	// d.vmMgr has no running instance, so the handler falls through to the
	// stopped KV branch — which now errors.

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String("i-describe-load-fail"),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}
	body, _ := json.Marshal(input)
	reply := requestHandler(t, d.natsConn, "ec2.DescribeInstanceAttribute.test1", d.handleEC2DescribeInstanceAttribute, testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2DescribeInstanceAttribute_StoppedFallback_HitsKV(t *testing.T) {
	store := newFakeStateStore()
	v := stoppedVMFixture("i-describe-stopped", testAccountID)
	v.InstanceType = "t3.medium"
	v.Instance.InstanceType = aws.String("t3.medium")
	store.stopped[v.ID] = v
	d := daemonWithFakeStateStore(t, store)

	input := &ec2.DescribeInstanceAttributeInput{
		InstanceId: aws.String(v.ID),
		Attribute:  aws.String(ec2.InstanceAttributeNameInstanceType),
	}
	body, _ := json.Marshal(input)
	reply := requestHandler(t, d.natsConn, "ec2.DescribeInstanceAttribute.test2", d.handleEC2DescribeInstanceAttribute, testAccountID, body)

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
	reply := requestHandler(t, d.natsConn, "ec2.DescribeInstanceAttribute.test3", d.handleEC2DescribeInstanceAttribute, testAccountID, body)
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

// --- handleEC2DescribeStoppedInstances / handleEC2DescribeTerminatedInstances ---

func TestHandleEC2DescribeStoppedInstances_ListError(t *testing.T) {
	store := newFakeStateStore()
	store.listStoppedErr = errors.New("list failed")
	d := daemonWithFakeStateStore(t, store)

	reply := requestHandler(t, d.natsConn, "ec2.DescribeStoppedInstances.test1", d.handleEC2DescribeStoppedInstances, testAccountID, []byte("{}"))
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2DescribeTerminatedInstances_ListError(t *testing.T) {
	store := newFakeStateStore()
	store.listTerminatedErr = errors.New("list failed")
	d := daemonWithFakeStateStore(t, store)

	reply := requestHandler(t, d.natsConn, "ec2.DescribeTerminatedInstances.test1", d.handleEC2DescribeTerminatedInstances, testAccountID, []byte("{}"))
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2DescribeStoppedInstances_StateStoreNil(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)

	reply := requestHandler(t, d.natsConn, "ec2.DescribeStoppedInstances.test2", d.handleEC2DescribeStoppedInstances, testAccountID, []byte("{}"))
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2DescribeTerminatedInstances_StateStoreNil(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)

	reply := requestHandler(t, d.natsConn, "ec2.DescribeTerminatedInstances.test2", d.handleEC2DescribeTerminatedInstances, testAccountID, []byte("{}"))
	assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
}

func TestHandleEC2DescribeStoppedInstances_CrossAccountIsolation(t *testing.T) {
	store := newFakeStateStore()
	store.stopped["i-mine"] = stoppedVMFixture("i-mine", testAccountID)
	store.stopped["i-yours"] = stoppedVMFixture("i-yours", "999988887777")
	d := daemonWithFakeStateStore(t, store)

	reply := requestHandler(t, d.natsConn, "ec2.DescribeStoppedInstances.test3", d.handleEC2DescribeStoppedInstances, testAccountID, []byte("{}"))

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

func TestHandleEC2DescribeStoppedInstances_EmptyReturnsValidShape(t *testing.T) {
	store := newFakeStateStore()
	d := daemonWithFakeStateStore(t, store)

	reply := requestHandler(t, d.natsConn, "ec2.DescribeStoppedInstances.test4", d.handleEC2DescribeStoppedInstances, testAccountID, []byte("{}"))

	var output ec2.DescribeInstancesOutput
	require.NoError(t, json.Unmarshal(reply.Data, &output))
	assert.Empty(t, output.Reservations, "empty store must produce empty reservation list")
}

// Two instances sharing a ReservationId must collapse into a single
// reservation with both Instances attached.
func TestHandleEC2DescribeStoppedInstances_ReservationGrouping(t *testing.T) {
	store := newFakeStateStore()

	a := stoppedVMFixture("i-grp-a", testAccountID)
	b := stoppedVMFixture("i-grp-b", testAccountID)
	a.Reservation.ReservationId = aws.String("r-shared")
	b.Reservation.ReservationId = aws.String("r-shared")
	store.stopped[a.ID] = a
	store.stopped[b.ID] = b

	d := daemonWithFakeStateStore(t, store)

	reply := requestHandler(t, d.natsConn, "ec2.DescribeStoppedInstances.test5", d.handleEC2DescribeStoppedInstances, testAccountID, []byte("{}"))

	var output ec2.DescribeInstancesOutput
	require.NoError(t, json.Unmarshal(reply.Data, &output))
	require.Len(t, output.Reservations, 1, "shared ReservationId must collapse into one reservation")
	require.NotNil(t, output.Reservations[0].ReservationId)
	assert.Equal(t, "r-shared", *output.Reservations[0].ReservationId)
	assert.Len(t, output.Reservations[0].Instances, 2)
}

// DescribeInstances echoes the consumed capacity reservation so a targeted
// launch reports a CapacityReservationId (without it, Terraform never converges).
func TestHandleEC2DescribeInstances_CapacityReservationEcho(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)

	reservation := &ec2.Reservation{}
	reservation.SetReservationId("r-cr-echo")
	reservation.SetOwnerId(testAccountID)
	instance := &ec2.Instance{}
	instance.SetInstanceId("i-cr-echo")
	instance.SetInstanceType("t3.micro")

	daemon.vmMgr.Insert(&vm.VM{
		ID:                    "i-cr-echo",
		Status:                vm.StateRunning,
		AccountID:             testAccountID,
		CapacityReservationId: "cr-0123456789abcdef0",
		Reservation:           reservation,
		Instance:              instance,
	})

	sub, err := daemon.natsConn.Subscribe("ec2.DescribeInstances", daemon.handleEC2DescribeInstances)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	reqData, _ := json.Marshal(&ec2.DescribeInstancesInput{})
	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeInstances", reqData, 5*time.Second)
	require.NoError(t, err)
	var out ec2.DescribeInstancesOutput
	require.NoError(t, json.Unmarshal(reply.Data, &out))
	require.Len(t, out.Reservations, 1)
	require.Len(t, out.Reservations[0].Instances, 1)

	got := out.Reservations[0].Instances[0]
	assert.Equal(t, "cr-0123456789abcdef0", aws.StringValue(got.CapacityReservationId))
	require.NotNil(t, got.CapacityReservationSpecification)
	assert.Equal(t, ec2.CapacityReservationPreferenceOpen,
		aws.StringValue(got.CapacityReservationSpecification.CapacityReservationPreference))
	require.NotNil(t, got.CapacityReservationSpecification.CapacityReservationTarget)
	assert.Equal(t, "cr-0123456789abcdef0",
		aws.StringValue(got.CapacityReservationSpecification.CapacityReservationTarget.CapacityReservationId))
}

// An instance with no reservation reports no capacity-reservation fields.
func TestHandleEC2DescribeInstances_NoCapacityReservationEcho(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)

	reservation := &ec2.Reservation{}
	reservation.SetReservationId("r-plain")
	reservation.SetOwnerId(testAccountID)
	instance := &ec2.Instance{}
	instance.SetInstanceId("i-plain")
	instance.SetInstanceType("t3.micro")

	daemon.vmMgr.Insert(&vm.VM{
		ID:          "i-plain",
		Status:      vm.StateRunning,
		AccountID:   testAccountID,
		Reservation: reservation,
		Instance:    instance,
	})

	sub, err := daemon.natsConn.Subscribe("ec2.DescribeInstances", daemon.handleEC2DescribeInstances)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	reqData, _ := json.Marshal(&ec2.DescribeInstancesInput{})
	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeInstances", reqData, 5*time.Second)
	require.NoError(t, err)
	var out ec2.DescribeInstancesOutput
	require.NoError(t, json.Unmarshal(reply.Data, &out))
	require.Len(t, out.Reservations, 1)
	require.Len(t, out.Reservations[0].Instances, 1)

	got := out.Reservations[0].Instances[0]
	assert.Empty(t, aws.StringValue(got.CapacityReservationId))
	assert.Nil(t, got.CapacityReservationSpecification)
}

// A spot-launched instance projects InstanceLifecycle + SpotInstanceRequestId.
func TestHandleEC2DescribeInstances_SpotLineageEcho(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)

	reservation := &ec2.Reservation{}
	reservation.SetReservationId("r-spot-echo")
	reservation.SetOwnerId(testAccountID)
	instance := &ec2.Instance{}
	instance.SetInstanceId("i-spot-echo")
	instance.SetInstanceType("t3.micro")

	daemon.vmMgr.Insert(&vm.VM{
		ID:                    "i-spot-echo",
		Status:                vm.StateRunning,
		AccountID:             testAccountID,
		InstanceLifecycle:     ec2.InstanceLifecycleTypeSpot,
		SpotInstanceRequestId: "sir-0123456789abcdef0",
		Reservation:           reservation,
		Instance:              instance,
	})

	sub, err := daemon.natsConn.Subscribe("ec2.DescribeInstances", daemon.handleEC2DescribeInstances)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	reqData, _ := json.Marshal(&ec2.DescribeInstancesInput{})
	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeInstances", reqData, 5*time.Second)
	require.NoError(t, err)
	var out ec2.DescribeInstancesOutput
	require.NoError(t, json.Unmarshal(reply.Data, &out))
	require.Len(t, out.Reservations, 1)
	require.Len(t, out.Reservations[0].Instances, 1)

	got := out.Reservations[0].Instances[0]
	assert.Equal(t, ec2.InstanceLifecycleTypeSpot, aws.StringValue(got.InstanceLifecycle))
	assert.Equal(t, "sir-0123456789abcdef0", aws.StringValue(got.SpotInstanceRequestId))
}

// An on-demand instance reports no spot-lineage fields.
func TestHandleEC2DescribeInstances_NoSpotLineageEcho(t *testing.T) {
	daemon := createTestDaemon(t, sharedNATSURL)

	reservation := &ec2.Reservation{}
	reservation.SetReservationId("r-ondemand")
	reservation.SetOwnerId(testAccountID)
	instance := &ec2.Instance{}
	instance.SetInstanceId("i-ondemand")
	instance.SetInstanceType("t3.micro")

	daemon.vmMgr.Insert(&vm.VM{
		ID:          "i-ondemand",
		Status:      vm.StateRunning,
		AccountID:   testAccountID,
		Reservation: reservation,
		Instance:    instance,
	})

	sub, err := daemon.natsConn.Subscribe("ec2.DescribeInstances", daemon.handleEC2DescribeInstances)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	reqData, _ := json.Marshal(&ec2.DescribeInstancesInput{})
	reply, err := natsRequest(daemon.natsConn, "ec2.DescribeInstances", reqData, 5*time.Second)
	require.NoError(t, err)
	var out ec2.DescribeInstancesOutput
	require.NoError(t, json.Unmarshal(reply.Data, &out))
	require.Len(t, out.Reservations, 1)
	require.Len(t, out.Reservations[0].Instances, 1)

	got := out.Reservations[0].Instances[0]
	assert.Empty(t, aws.StringValue(got.InstanceLifecycle))
	assert.Empty(t, aws.StringValue(got.SpotInstanceRequestId))
}
