package daemon

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	ec2instance "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	vmmock "github.com/mulgadc/spinifex/spinifex/runtime/compute/vm/mock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func metadataCommand(id string, data *ec2v1.InstanceMetadataOptionsData) ec2v1.EC2InstanceCommand {
	return ec2v1.EC2InstanceCommand{
		ID:                          id,
		Attributes:                  ec2v1.EC2CommandAttributes{ModifyInstanceMetadataOptions: true},
		InstanceMetadataOptionsData: data,
	}
}

func terminationCommand(id string, data *ec2v1.DisableApiTerminationData) ec2v1.EC2InstanceCommand {
	return ec2v1.EC2InstanceCommand{
		ID:                        id,
		Attributes:                ec2v1.EC2CommandAttributes{SetDisableApiTermination: true},
		DisableApiTerminationData: data,
	}
}

func TestOwnerDispatch_ModifyInstanceMetadataOptions(t *testing.T) {
	t.Run("success applies the change and sets the handled header", func(t *testing.T) {
		const id = "i-or-meta-ok"
		d, _ := monitoringTestDaemon(t, id, false)
		cmd := metadataCommand(id, &ec2v1.InstanceMetadataOptionsData{
			HttpTokens:              ec2.HttpTokensStateOptional,
			HttpPutResponseHopLimit: aws.Int64(3),
		})

		reply := requestHandler(t, d.natsConn, "ec2.cmd."+id, d.handleEC2Events, testAccountID, mustMarshal(t, cmd))
		assert.Equal(t, "1", reply.Header.Get(ec2v1.HandledHeader))

		got, ok := d.vmMgr.Get(id)
		require.True(t, ok)
		require.NotNil(t, got.Instance.MetadataOptions)
		assert.Equal(t, ec2.HttpTokensStateOptional, aws.StringValue(got.Instance.MetadataOptions.HttpTokens))
		assert.Equal(t, int64(3), aws.Int64Value(got.Instance.MetadataOptions.HttpPutResponseHopLimit))
	})

	t.Run("service error still sets the handled header", func(t *testing.T) {
		const id = "i-or-meta-bad"
		d, _ := monitoringTestDaemon(t, id, false)
		cmd := metadataCommand(id, &ec2v1.InstanceMetadataOptionsData{HttpTokens: "bogus"})

		reply := requestHandler(t, d.natsConn, "ec2.cmd."+id, d.handleEC2Events, testAccountID, mustMarshal(t, cmd))
		assert.Equal(t, "1", reply.Header.Get(ec2v1.HandledHeader))
		assert.Equal(t, awserrors.ErrorUnsupportedOperation, decodeError(t, reply.Data)["Code"])
	})

	t.Run("missing data is a validation error with the handled header", func(t *testing.T) {
		const id = "i-or-meta-nodata"
		d, _ := monitoringTestDaemon(t, id, false)

		reply := requestHandler(t, d.natsConn, "ec2.cmd."+id, d.handleEC2Events, testAccountID, mustMarshal(t, metadataCommand(id, nil)))
		assert.Equal(t, "1", reply.Header.Get(ec2v1.HandledHeader))
		assert.Equal(t, awserrors.ErrorMissingParameter, decodeError(t, reply.Data)["Code"])
	})
}

func TestOwnerDispatch_SetDisableApiTermination(t *testing.T) {
	protection := func(t *testing.T, d *Daemon, id string) *bool {
		t.Helper()
		got, ok := d.vmMgr.Get(id)
		require.True(t, ok)
		return got.RunInstancesInput.DisableApiTermination
	}

	t.Run("true then explicit false are both applied", func(t *testing.T) {
		const id = "i-or-term-ok"
		d, _ := monitoringTestDaemon(t, id, false)

		reply := requestHandler(t, d.natsConn, "ec2.cmd."+id, d.handleEC2Events, testAccountID,
			mustMarshal(t, terminationCommand(id, &ec2v1.DisableApiTerminationData{Value: true})))
		assert.Equal(t, "1", reply.Header.Get(ec2v1.HandledHeader))
		require.NotNil(t, protection(t, d, id))
		assert.True(t, *protection(t, d, id))

		reply = requestHandler(t, d.natsConn, "ec2.cmd."+id, d.handleEC2Events, testAccountID,
			mustMarshal(t, terminationCommand(id, &ec2v1.DisableApiTerminationData{Value: false})))
		assert.Equal(t, "1", reply.Header.Get(ec2v1.HandledHeader))
		require.NotNil(t, protection(t, d, id))
		assert.False(t, *protection(t, d, id))
	})

	t.Run("missing data is an error and never clears protection", func(t *testing.T) {
		const id = "i-or-term-nodata"
		d, _ := monitoringTestDaemon(t, id, false)
		d.vmMgr.UpdateState(id, func(v *vm.VM) { v.RunInstancesInput.DisableApiTermination = aws.Bool(true) })

		reply := requestHandler(t, d.natsConn, "ec2.cmd."+id, d.handleEC2Events, testAccountID, mustMarshal(t, terminationCommand(id, nil)))
		assert.Equal(t, "1", reply.Header.Get(ec2v1.HandledHeader))
		assert.Equal(t, awserrors.ErrorMissingParameter, decodeError(t, reply.Data)["Code"])
		require.NotNil(t, protection(t, d, id))
		assert.True(t, *protection(t, d, id))
	})
}

// ownerRoutingDaemon is a daemon on url with its own vmMgr and the shared
// stopped store.
func ownerRoutingDaemon(t *testing.T, url string, stopped *vmmock.StateStore) *Daemon {
	t.Helper()
	d := createTestDaemon(t, url)
	d.instanceService = ec2instance.NewInstanceServiceImpl(
		d.config, d.resourceMgr.instanceTypes, d.natsConn,
		objectstore.NewMemoryObjectStore(), d.vmMgr, d.resourceMgr, stopped)
	return d
}

func TestModifyInstanceMetadataOptions_NonOwnerReceivesQueueOwnerAnswers(t *testing.T) {
	const id = "i-or-multinode"
	ns, gateway := testutil.StartTestNATS(t)
	stopped := vmmock.New()

	// B runs the instance; A does not and is the only queue-group member.
	b := ownerRoutingDaemon(t, ns.ClientURL(), stopped)
	b.vmMgr.Insert(&vm.VM{
		ID:        id,
		Status:    vm.StateRunning,
		AccountID: testAccountID,
		Instance:  &ec2.Instance{InstanceId: aws.String(id)},
	})
	subscribeOwner(t, b, id)

	a := ownerRoutingDaemon(t, ns.ClientURL(), stopped)
	sub, err := a.natsConn.QueueSubscribe("ec2.ModifyInstanceMetadataOptions", "spinifex-workers",
		asMsgHandler(handleNATSRequest(a.node, a.instanceService.ModifyInstanceMetadataOptions)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, a.natsConn.Flush())

	for range 9 {
		out, err := ec2instanceapi.ModifyInstanceMetadataOptions(t.Context(), &ec2.ModifyInstanceMetadataOptionsInput{
			InstanceId:              aws.String(id),
			HttpPutResponseHopLimit: aws.Int64(3),
		}, gateway, testAccountID)
		require.NoError(t, err)
		assert.Equal(t, id, aws.StringValue(out.InstanceId))
		assert.Equal(t, int64(3), aws.Int64Value(out.InstanceMetadataOptions.HttpPutResponseHopLimit))
	}

	got, ok := b.vmMgr.Get(id)
	require.True(t, ok)
	assert.Equal(t, int64(3), aws.Int64Value(got.Instance.MetadataOptions.HttpPutResponseHopLimit))
	_, onA := a.vmMgr.Get(id)
	assert.False(t, onA, "the queue-group member never held the instance")
}

// n1CommandType is EC2InstanceCommand as the previous release defined it: the
// current type minus the two attributes and two data blocks this change adds.
func n1CommandType() reflect.Type {
	newFields := map[string]bool{
		"ModifyInstanceMetadataOptions": true, "SetDisableApiTermination": true,
		"InstanceMetadataOptionsData": true, "DisableApiTerminationData": true,
	}
	strip := func(t reflect.Type, swap map[string]reflect.Type) reflect.Type {
		var fields []reflect.StructField
		for f := range t.Fields() {
			if newFields[f.Name] {
				continue
			}
			if st, ok := swap[f.Name]; ok {
				f.Type = st
			}
			fields = append(fields, f)
		}
		return reflect.StructOf(fields)
	}
	attrs := strip(reflect.TypeFor[ec2v1.EC2CommandAttributes](), nil)
	return strip(reflect.TypeFor[ec2v1.EC2InstanceCommand](), map[string]reflect.Type{"Attributes": attrs})
}

// An N-1 owner decodes a new-variant payload with the unknown attribute and
// data dropped, so dispatch sees no attribute and takes the default branch.
func TestS9_N1DispatchRejectsNewVariantsWithoutWriting(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	variants := map[string]func(id string) ec2v1.EC2InstanceCommand{
		"metadata options": func(id string) ec2v1.EC2InstanceCommand {
			return metadataCommand(id, &ec2v1.InstanceMetadataOptionsData{HttpPutResponseHopLimit: aws.Int64(3)})
		},
		"termination protection": func(id string) ec2v1.EC2InstanceCommand {
			return terminationCommand(id, &ec2v1.DisableApiTerminationData{Value: true})
		},
	}
	for name, build := range variants {
		t.Run(name, func(t *testing.T) {
			id := "i-or-n1-" + strings.ReplaceAll(name, " ", "-")
			d, _ := monitoringTestDaemon(t, id, false)
			before, _ := d.vmMgr.Get(id)
			beforeInstance := *before.Instance
			logs.Reset()

			// Current encoder -> N-1 decoder -> what the N-1 owner re-dispatches.
			n1 := reflect.New(n1CommandType())
			require.NoError(t, json.Unmarshal(mustMarshal(t, build(id)), n1.Interface()))
			require.Equal(t, id, n1.Elem().FieldByName("ID").String())
			payload := mustMarshal(t, n1.Interface())
			assert.NotContains(t, string(payload), "modify_instance_metadata_options")
			assert.NotContains(t, string(payload), "set_disable_api_termination")

			reply := requestHandler(t, d.natsConn, "ec2.cmd."+id, d.handleEC2Events, testAccountID, payload)
			assert.Equal(t, awserrors.ErrorServerInternal, decodeError(t, reply.Data)["Code"])
			assert.Empty(t, reply.Header.Get(ec2v1.HandledHeader), "an N-1 rejection carries no handled marker")
			assert.Contains(t, logs.String(), "Unhandled EC2 instance command")

			after, _ := d.vmMgr.Get(id)
			assert.Equal(t, beforeInstance, *after.Instance)
			assert.Nil(t, after.RunInstancesInput.DisableApiTermination)
		})
	}
}

func TestModifyInstanceMetadataOptions_NoOwnerAppliesToRealStoppedRecord(t *testing.T) {
	const id = "i-or-stopped-real"
	ns, gateway := testutil.StartTestNATS(t)
	stopped := vmmock.New()
	stopped.Stopped[id] = stoppedVMFixture(id, testAccountID)

	// No daemon runs the instance; the only queue-group member is the real service.
	a := ownerRoutingDaemon(t, ns.ClientURL(), stopped)
	sub, err := a.natsConn.QueueSubscribe("ec2.ModifyInstanceMetadataOptions", "spinifex-workers",
		asMsgHandler(handleNATSRequest(a.node, a.instanceService.ModifyInstanceMetadataOptions)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, a.natsConn.Flush())

	out, err := ec2instanceapi.ModifyInstanceMetadataOptions(t.Context(), &ec2.ModifyInstanceMetadataOptionsInput{
		InstanceId:              aws.String(id),
		HttpPutResponseHopLimit: aws.Int64(2),
	}, gateway, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), aws.Int64Value(out.InstanceMetadataOptions.HttpPutResponseHopLimit))

	reloaded, err := stopped.LoadStoppedInstance(id)
	require.NoError(t, err)
	require.NotNil(t, reloaded.Instance.MetadataOptions)
	assert.Equal(t, int64(2), aws.Int64Value(reloaded.Instance.MetadataOptions.HttpPutResponseHopLimit))
	require.NotNil(t, stopped.WroteStopped[id], "the service persisted the stopped record")
}

func TestOwnerDispatch_RejectsOtherAccountWithoutWriting(t *testing.T) {
	const id = "i-or-otheracct"
	d, _ := monitoringTestDaemon(t, id, false)
	before, _ := d.vmMgr.Get(id)
	beforeInstance := *before.Instance

	cmds := map[string]ec2v1.EC2InstanceCommand{
		"metadata options": metadataCommand(id, &ec2v1.InstanceMetadataOptionsData{HttpPutResponseHopLimit: aws.Int64(3)}),
		"termination":      terminationCommand(id, &ec2v1.DisableApiTerminationData{Value: true}),
	}
	for name, cmd := range cmds {
		reply := requestHandler(t, d.natsConn, "ec2.cmd."+id, d.handleEC2Events, "999999999999", mustMarshal(t, cmd))
		assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, decodeError(t, reply.Data)["Code"], name)
	}

	after, _ := d.vmMgr.Get(id)
	assert.Equal(t, beforeInstance, *after.Instance)
	assert.Nil(t, after.RunInstancesInput.DisableApiTermination)
}

func TestModifyInstanceAttribute_DisableApiTerminationRoutedToOwner(t *testing.T) {
	const id = "i-or-termroute"
	ns, gateway := testutil.StartTestNATS(t)
	stopped := vmmock.New()

	b := ownerRoutingDaemon(t, ns.ClientURL(), stopped)
	b.vmMgr.Insert(&vm.VM{
		ID:                id,
		Status:            vm.StateRunning,
		AccountID:         testAccountID,
		Instance:          &ec2.Instance{InstanceId: aws.String(id)},
		RunInstancesInput: &ec2.RunInstancesInput{},
	})
	subscribeOwner(t, b, id)

	a := ownerRoutingDaemon(t, ns.ClientURL(), stopped)
	sub, err := a.natsConn.QueueSubscribe("ec2.ModifyInstanceAttribute", "spinifex-workers",
		asMsgHandler(handleNATSRequest(a.node, a.instanceService.ModifyInstanceAttribute)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, a.natsConn.Flush())

	for range 9 {
		_, err := ec2instanceapi.ModifyInstanceAttribute(t.Context(), &ec2.ModifyInstanceAttributeInput{
			InstanceId:            aws.String(id),
			DisableApiTermination: &ec2.AttributeBooleanValue{Value: aws.Bool(true)},
		}, gateway, testAccountID)
		require.NoError(t, err)
	}

	got, ok := b.vmMgr.Get(id)
	require.True(t, ok)
	require.NotNil(t, got.RunInstancesInput.DisableApiTermination)
	assert.True(t, *got.RunInstancesInput.DisableApiTermination)

	// The node that enforces the guard now refuses the terminate.
	reply := requestHandler(t, b.natsConn, "ec2.cmd."+id+".enforce", b.handleEC2Events, testAccountID,
		mustMarshal(t, ec2v1.EC2InstanceCommand{ID: id, Attributes: ec2v1.EC2CommandAttributes{TerminateInstance: true}}))
	assert.Equal(t, awserrors.ErrorOperationNotPermitted, decodeError(t, reply.Data)["Code"])
}
