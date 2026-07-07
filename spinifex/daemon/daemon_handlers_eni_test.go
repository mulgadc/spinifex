package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/qmp"
	spxtypes "github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- eniHotplugErrorCode mapping ---

// TestENIHotplugErrorCode locks down the vm.Manager error → AWS API code
// mapping. Wrong mapping silently breaks AWS-SDK retry semantics: clients
// expect 4xx codes for caller-fixable problems and 5xx for server faults.
func TestENIHotplugErrorCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"ErrInstanceNotFound", vm.ErrInstanceNotFound, awserrors.ErrorInvalidInstanceIDNotFound},
		{"wrapped ErrInstanceNotFound", fmt.Errorf("ctx: %w", vm.ErrInstanceNotFound), awserrors.ErrorInvalidInstanceIDNotFound},
		{"ErrInvalidTransition", vm.ErrInvalidTransition, awserrors.ErrorIncorrectInstanceState},
		{"ErrAttachmentLimitExceeded", vm.ErrAttachmentLimitExceeded, awserrors.ErrorAttachmentLimitExceeded},
		{"ErrENINotAttached", vm.ErrENINotAttached, awserrors.ErrorInvalidAttachmentIDNotFound},
		{"wrapped ErrENINotAttached", fmt.Errorf("%w: eni-x", vm.ErrENINotAttached), awserrors.ErrorInvalidAttachmentIDNotFound},
		{"ErrQMPUnavailable", vm.ErrQMPUnavailable, awserrors.ErrorServerInternal},
		{"ErrENIPipelineTimeout", vm.ErrENIPipelineTimeout, awserrors.ErrorServerInternal},
		{"unknown error", errors.New("network unreachable"), awserrors.ErrorServerInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, eniHotplugErrorCode(tt.err))
		})
	}
}

// --- publishENIHotplugEvent ---

func TestPublishENIHotplugEvent_NilConnNoop(t *testing.T) {
	publishENIHotplugEvent(nil, "vpc.eni-hotplug.attached", "i-x", map[string]any{"k": "v"})
}

func TestPublishENIHotplugEvent_Publishes(t *testing.T) {
	d := createVPCTestDaemon(t)

	got := make(chan *nats.Msg, 1)
	sub, err := d.natsConn.SubscribeSync("vpc.eni-hotplug.attached.i-evt")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	go func() {
		msg, _ := sub.NextMsg(2 * time.Second)
		got <- msg
	}()

	publishENIHotplugEvent(d.natsConn, "vpc.eni-hotplug.attached", "i-evt", map[string]any{
		"eniId": "eni-evt",
		"slot":  1.0,
	})

	msg := <-got
	require.NotNil(t, msg)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(msg.Data, &payload))
	assert.Equal(t, "eni-evt", payload["eniId"])
	assert.Equal(t, 1.0, payload["slot"])
}

// --- shared fixture ---

// eniHotPlugFixture builds a daemon with vpcService + a running VM
// registered in vmMgr, plus a stub DeviceController bound via
// vm.SetHotPlugTestSeams. The returned ENI record is already in the KV
// in "available" state and the VM has 4 free hot-plug slots.
type eniHotPlugFixture struct {
	daemon   *Daemon
	vmInst   *vm.VM
	stub     *vm.StubDeviceController
	subnetID string
	vpcID    string
	eniID    string
	mac      string
}

func newENIHotPlugFixture(t *testing.T) *eniHotPlugFixture {
	t.Helper()
	d := createVPCTestDaemon(t)

	vpcOut, err := d.vpcService.CreateVpc(t.Context(), &ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)
	subnetOut, err := d.vpcService.CreateSubnet(t.Context(), &ec2.CreateSubnetInput{
		VpcId:     vpcOut.Vpc.VpcId,
		CidrBlock: aws.String("10.0.1.0/24"),
	}, testAccountID)
	require.NoError(t, err)
	eniOut, err := d.vpcService.CreateNetworkInterface(t.Context(), &ec2.CreateNetworkInterfaceInput{
		SubnetId: subnetOut.Subnet.SubnetId,
	}, testAccountID)
	require.NoError(t, err)

	rec, err := d.vpcService.GetENIRecord(testAccountID, *eniOut.NetworkInterface.NetworkInterfaceId)
	require.NoError(t, err)

	v := &vm.VM{
		ID:        "i-hp-test",
		Status:    vm.StateRunning,
		QMPClient: &qmp.QMPClient{},
		ENIRequests: spxtypes.ENIRequests{
			AvailableSlots:  []int{1, 2, 3, 4},
			AttachedByENIID: map[string]int{},
		},
	}
	d.vmMgr.Insert(v)

	stub := vm.NewStubDeviceController()
	restore := vm.SetHotPlugTestSeams(
		func(*vm.VM) vm.DeviceController { return stub },
		func(time.Duration) {},
	)
	t.Cleanup(restore)

	return &eniHotPlugFixture{
		daemon:   d,
		vmInst:   v,
		stub:     stub,
		subnetID: *subnetOut.Subnet.SubnetId,
		vpcID:    *vpcOut.Vpc.VpcId,
		eniID:    rec.NetworkInterfaceId,
		mac:      rec.MacAddress,
	}
}

// driveHandler subscribes to subject, dispatches the supplied handler
// from the subscriber, and returns the reply payload. Mirrors how the
// daemon dispatches AttachENI / DetachENI off the ec2.cmd channel in
// production.
func driveHandler(t *testing.T, nc *nats.Conn, subject string, handler func(*nats.Msg)) []byte {
	t.Helper()
	sub, err := nc.Subscribe(subject, handler)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	reqMsg := nats.NewMsg(subject)
	reqMsg.Header.Set(utils.AccountIDHeader, testAccountID)
	reply, err := nc.RequestMsg(reqMsg, 5*time.Second)
	require.NoError(t, err)
	return reply.Data
}

// assertErrorCode is shared with daemon_handlers_eks_test.go (same package).

// --- handleAttachNetworkInterface ---

func TestHandleAttachNetworkInterface_NilData(t *testing.T) {
	f := newENIHotPlugFixture(t)
	cmd := spxtypes.EC2InstanceCommand{ID: f.vmInst.ID}
	payload := driveHandler(t, f.daemon.natsConn, "test.attach.nildata", func(msg *nats.Msg) {
		f.daemon.handleAttachNetworkInterface(context.Background(), msg, cmd, f.vmInst)
	})
	assertErrorCode(t, payload, awserrors.ErrorInvalidParameterValue)
}

func TestHandleAttachNetworkInterface_EmptyEniID(t *testing.T) {
	f := newENIHotPlugFixture(t)
	cmd := spxtypes.EC2InstanceCommand{
		ID:            f.vmInst.ID,
		AttachENIData: &spxtypes.AttachENIData{NetworkInterfaceID: "", DeviceIndex: 0},
	}
	payload := driveHandler(t, f.daemon.natsConn, "test.attach.emptyid", func(msg *nats.Msg) {
		f.daemon.handleAttachNetworkInterface(context.Background(), msg, cmd, f.vmInst)
	})
	assertErrorCode(t, payload, awserrors.ErrorInvalidParameterValue)
}

func TestHandleAttachNetworkInterface_NotRunning(t *testing.T) {
	f := newENIHotPlugFixture(t)
	f.vmInst.Status = vm.StateStopped
	cmd := spxtypes.EC2InstanceCommand{
		ID:            f.vmInst.ID,
		AttachENIData: &spxtypes.AttachENIData{NetworkInterfaceID: f.eniID, DeviceIndex: 1},
	}
	payload := driveHandler(t, f.daemon.natsConn, "test.attach.notrunning", func(msg *nats.Msg) {
		f.daemon.handleAttachNetworkInterface(context.Background(), msg, cmd, f.vmInst)
	})
	assertErrorCode(t, payload, awserrors.ErrorIncorrectInstanceState)
}

func TestHandleAttachNetworkInterface_ENINotFound(t *testing.T) {
	f := newENIHotPlugFixture(t)
	cmd := spxtypes.EC2InstanceCommand{
		ID:            f.vmInst.ID,
		AttachENIData: &spxtypes.AttachENIData{NetworkInterfaceID: "eni-missing", DeviceIndex: 1},
	}
	payload := driveHandler(t, f.daemon.natsConn, "test.attach.eninotfound", func(msg *nats.Msg) {
		f.daemon.handleAttachNetworkInterface(context.Background(), msg, cmd, f.vmInst)
	})
	assertErrorCode(t, payload, awserrors.ErrorInvalidNetworkInterfaceIDNotFound)
}

func TestHandleAttachNetworkInterface_WrongOwner(t *testing.T) {
	f := newENIHotPlugFixture(t)
	_, err := f.daemon.vpcService.AttachENI(testAccountID, f.eniID, "i-other-instance", 0)
	require.NoError(t, err)

	cmd := spxtypes.EC2InstanceCommand{
		ID:            f.vmInst.ID,
		AttachENIData: &spxtypes.AttachENIData{NetworkInterfaceID: f.eniID, DeviceIndex: 1},
	}
	payload := driveHandler(t, f.daemon.natsConn, "test.attach.wrongowner", func(msg *nats.Msg) {
		f.daemon.handleAttachNetworkInterface(context.Background(), msg, cmd, f.vmInst)
	})
	assertErrorCode(t, payload, awserrors.ErrorInvalidNetworkInterfaceInUse)
}

func TestHandleAttachNetworkInterface_Success(t *testing.T) {
	f := newENIHotPlugFixture(t)

	gotEvent := make(chan []byte, 1)
	evtSub, err := f.daemon.natsConn.Subscribe("vpc.eni-hotplug.attached."+f.vmInst.ID, func(msg *nats.Msg) {
		gotEvent <- msg.Data
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = evtSub.Unsubscribe() })

	cmd := spxtypes.EC2InstanceCommand{
		ID:            f.vmInst.ID,
		AttachENIData: &spxtypes.AttachENIData{NetworkInterfaceID: f.eniID, DeviceIndex: 1},
	}
	payload := driveHandler(t, f.daemon.natsConn, "test.attach.success", func(msg *nats.Msg) {
		f.daemon.handleAttachNetworkInterface(context.Background(), msg, cmd, f.vmInst)
	})

	var out ec2.AttachNetworkInterfaceOutput
	require.NoError(t, json.Unmarshal(payload, &out))
	require.NotNil(t, out.AttachmentId)
	assert.Contains(t, *out.AttachmentId, "eni-attach-")

	rec, err := f.daemon.vpcService.GetENIRecord(testAccountID, f.eniID)
	require.NoError(t, err)
	assert.Equal(t, "attached", rec.AttachmentStatus)
	assert.Equal(t, 1, rec.HotPlugSlot)
	assert.Equal(t, f.vmInst.ID, rec.InstanceId)

	assert.True(t, f.stub.HasDevice("net-eni-1"))
	assert.True(t, f.stub.HasNetdev("hostnet-eni-1"))

	select {
	case evtData := <-gotEvent:
		var evt map[string]any
		require.NoError(t, json.Unmarshal(evtData, &evt))
		assert.Equal(t, f.eniID, evt["eniId"])
		assert.Equal(t, float64(1), evt["hotPlugSlot"])
	case <-time.After(1 * time.Second):
		t.Fatal("expected vpc.eni-hotplug.attached event, none received")
	}
}

func TestHandleAttachNetworkInterface_HotPlugFails_RollsBackKV(t *testing.T) {
	f := newENIHotPlugFixture(t)
	f.stub.SetFailNext("device_add", errors.New("simulated QMP failure"))

	cmd := spxtypes.EC2InstanceCommand{
		ID:            f.vmInst.ID,
		AttachENIData: &spxtypes.AttachENIData{NetworkInterfaceID: f.eniID, DeviceIndex: 1},
	}
	payload := driveHandler(t, f.daemon.natsConn, "test.attach.hotplugfails", func(msg *nats.Msg) {
		f.daemon.handleAttachNetworkInterface(context.Background(), msg, cmd, f.vmInst)
	})
	assertErrorCode(t, payload, awserrors.ErrorServerInternal)

	rec, err := f.daemon.vpcService.GetENIRecord(testAccountID, f.eniID)
	require.NoError(t, err)
	assert.Equal(t, "available", rec.Status, "KV should have rolled back to available")
	assert.Empty(t, rec.AttachmentStatus, "AttachmentStatus should be cleared on rollback")
	assert.Empty(t, rec.InstanceId)
	assert.NotEmpty(t, rec.LastAttachError, "LastAttachError should record the rollback reason")
}

// --- handleDetachNetworkInterface ---

// attachedFixture extends the base fixture by pre-attaching the ENI to
// the running VM via the full handler, so detach tests start from the
// realistic post-attach state.
func attachedFixture(t *testing.T) (*eniHotPlugFixture, string) {
	t.Helper()
	f := newENIHotPlugFixture(t)
	attachID, err := f.daemon.vpcService.AttachENI(testAccountID, f.eniID, f.vmInst.ID, 1)
	require.NoError(t, err)
	require.NoError(t, f.daemon.vpcService.UpdateENI(testAccountID, f.eniID, func(r *handlers_ec2_vpc.ENIRecord) {
		r.AttachmentStatus = "attached"
		r.HotPlugSlot = 1
	}))
	f.vmInst.ENIRequests.Mu.Lock()
	f.vmInst.ENIRequests.AvailableSlots = []int{2, 3, 4}
	f.vmInst.ENIRequests.AttachedByENIID[f.eniID] = 1
	f.vmInst.ENIRequests.Mu.Unlock()
	// Pre-seed the stub so HotUnplugENI's device_del + query-pci absence
	// poll has something to remove.
	require.NoError(t, f.stub.NetdevAdd(map[string]any{"id": "hostnet-eni-1"}))
	require.NoError(t, f.stub.DeviceAdd(map[string]any{"id": "net-eni-1", "bus": "hotplug-eni1"}))
	return f, attachID
}

func TestHandleDetachNetworkInterface_NilData(t *testing.T) {
	f, _ := attachedFixture(t)
	cmd := spxtypes.EC2InstanceCommand{ID: f.vmInst.ID}
	payload := driveHandler(t, f.daemon.natsConn, "test.detach.nildata", func(msg *nats.Msg) {
		f.daemon.handleDetachNetworkInterface(context.Background(), msg, cmd, f.vmInst)
	})
	assertErrorCode(t, payload, awserrors.ErrorInvalidParameterValue)
}

func TestHandleDetachNetworkInterface_EmptyAttachID(t *testing.T) {
	f, _ := attachedFixture(t)
	cmd := spxtypes.EC2InstanceCommand{
		ID:            f.vmInst.ID,
		DetachENIData: &spxtypes.DetachENIData{AttachmentID: ""},
	}
	payload := driveHandler(t, f.daemon.natsConn, "test.detach.emptyid", func(msg *nats.Msg) {
		f.daemon.handleDetachNetworkInterface(context.Background(), msg, cmd, f.vmInst)
	})
	assertErrorCode(t, payload, awserrors.ErrorInvalidParameterValue)
}

func TestHandleDetachNetworkInterface_UnknownAttachment(t *testing.T) {
	f, _ := attachedFixture(t)
	cmd := spxtypes.EC2InstanceCommand{
		ID:            f.vmInst.ID,
		DetachENIData: &spxtypes.DetachENIData{AttachmentID: "eni-attach-missing"},
	}
	payload := driveHandler(t, f.daemon.natsConn, "test.detach.unknown", func(msg *nats.Msg) {
		f.daemon.handleDetachNetworkInterface(context.Background(), msg, cmd, f.vmInst)
	})
	assertErrorCode(t, payload, awserrors.ErrorInvalidAttachmentIDNotFound)
}

func TestHandleDetachNetworkInterface_WrongOwner(t *testing.T) {
	f, attachID := attachedFixture(t)
	otherVM := &vm.VM{
		ID:     "i-other",
		Status: vm.StateRunning,
		ENIRequests: spxtypes.ENIRequests{
			AvailableSlots:  []int{1},
			AttachedByENIID: map[string]int{},
		},
	}
	f.daemon.vmMgr.Insert(otherVM)
	cmd := spxtypes.EC2InstanceCommand{
		ID:            otherVM.ID,
		DetachENIData: &spxtypes.DetachENIData{AttachmentID: attachID},
	}
	payload := driveHandler(t, f.daemon.natsConn, "test.detach.wrongowner", func(msg *nats.Msg) {
		f.daemon.handleDetachNetworkInterface(context.Background(), msg, cmd, otherVM)
	})
	assertErrorCode(t, payload, awserrors.ErrorInvalidAttachmentIDNotFound)
}

func TestHandleDetachNetworkInterface_NotRunning(t *testing.T) {
	f, attachID := attachedFixture(t)
	f.vmInst.Status = vm.StateStopped
	cmd := spxtypes.EC2InstanceCommand{
		ID:            f.vmInst.ID,
		DetachENIData: &spxtypes.DetachENIData{AttachmentID: attachID},
	}
	payload := driveHandler(t, f.daemon.natsConn, "test.detach.notrunning", func(msg *nats.Msg) {
		f.daemon.handleDetachNetworkInterface(context.Background(), msg, cmd, f.vmInst)
	})
	assertErrorCode(t, payload, awserrors.ErrorIncorrectInstanceState)
}

func TestHandleDetachNetworkInterface_Success(t *testing.T) {
	f, attachID := attachedFixture(t)

	gotEvent := make(chan []byte, 1)
	evtSub, err := f.daemon.natsConn.Subscribe("vpc.eni-hotplug.detached."+f.vmInst.ID, func(msg *nats.Msg) {
		gotEvent <- msg.Data
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = evtSub.Unsubscribe() })

	cmd := spxtypes.EC2InstanceCommand{
		ID:            f.vmInst.ID,
		DetachENIData: &spxtypes.DetachENIData{AttachmentID: attachID, Force: false},
	}
	payload := driveHandler(t, f.daemon.natsConn, "test.detach.success", func(msg *nats.Msg) {
		f.daemon.handleDetachNetworkInterface(context.Background(), msg, cmd, f.vmInst)
	})
	var out ec2.DetachNetworkInterfaceOutput
	require.NoError(t, json.Unmarshal(payload, &out))

	rec, err := f.daemon.vpcService.GetENIRecord(testAccountID, f.eniID)
	require.NoError(t, err)
	assert.Equal(t, "available", rec.Status)
	assert.Empty(t, rec.AttachmentStatus)
	assert.False(t, rec.DetachInFlight)
	assert.False(t, rec.DetachForce)
	assert.Equal(t, 0, rec.HotPlugSlot)

	assert.False(t, f.stub.HasDevice("net-eni-1"))

	select {
	case <-gotEvent:
	case <-time.After(1 * time.Second):
		t.Fatal("expected vpc.eni-hotplug.detached event, none received")
	}
}

func TestHandleDetachNetworkInterface_HotUnplugFails(t *testing.T) {
	f, attachID := attachedFixture(t)
	f.stub.SetFailNext("device_del", errors.New("simulated QMP failure"))

	cmd := spxtypes.EC2InstanceCommand{
		ID:            f.vmInst.ID,
		DetachENIData: &spxtypes.DetachENIData{AttachmentID: attachID, Force: false},
	}
	payload := driveHandler(t, f.daemon.natsConn, "test.detach.hotunplugfails", func(msg *nats.Msg) {
		f.daemon.handleDetachNetworkInterface(context.Background(), msg, cmd, f.vmInst)
	})
	assertErrorCode(t, payload, awserrors.ErrorServerInternal)

	rec, err := f.daemon.vpcService.GetENIRecord(testAccountID, f.eniID)
	require.NoError(t, err)
	assert.Equal(t, "in-use", rec.Status, "KV should remain in-use after detach failure")
	assert.Equal(t, "detaching", rec.AttachmentStatus, "AttachmentStatus stays detaching for reconciler pickup")
	assert.False(t, rec.DetachInFlight, "DetachInFlight cleared so reconciler can re-claim")
}
