package daemon

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
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
