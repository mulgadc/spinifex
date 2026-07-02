package daemon

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// handleSetSpotLineage stamps InstanceLifecycle=spot + the SIR id onto the VM,
// dispatched through handleEC2Events on ec2.cmd.{id} after the ownership check.
func TestHandleSetSpotLineage_Stamps(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)

	d.vmMgr.Insert(&vm.VM{
		ID:        "i-spot-stamp",
		Status:    vm.StateRunning,
		AccountID: testAccountID,
		Instance:  &ec2.Instance{InstanceId: aws.String("i-spot-stamp")},
	})

	command := types.EC2InstanceCommand{
		ID:              "i-spot-stamp",
		Attributes:      types.EC2CommandAttributes{SetSpotLineage: true},
		SpotLineageData: &types.SpotLineageData{SpotInstanceRequestId: "sir-abc123"},
	}
	body, _ := json.Marshal(command)
	reply := requestHandler(t, d.natsConn, "ec2.cmd.i-spot-stamp", d.handleEC2Events, testAccountID, body)
	assert.JSONEq(t, `{}`, string(reply.Data))

	got, ok := d.vmMgr.Get("i-spot-stamp")
	require.True(t, ok)
	assert.Equal(t, ec2.InstanceLifecycleTypeSpot, got.InstanceLifecycle)
	assert.Equal(t, "sir-abc123", got.SpotInstanceRequestId)
}

// Missing SpotLineageData is rejected with MissingParameter and leaves the VM unmarked.
func TestHandleSetSpotLineage_RejectsMissingData(t *testing.T) {
	d := createTestDaemon(t, sharedNATSURL)

	d.vmMgr.Insert(&vm.VM{
		ID:        "i-spot-nodata",
		Status:    vm.StateRunning,
		AccountID: testAccountID,
		Instance:  &ec2.Instance{InstanceId: aws.String("i-spot-nodata")},
	})

	command := types.EC2InstanceCommand{
		ID:         "i-spot-nodata",
		Attributes: types.EC2CommandAttributes{SetSpotLineage: true},
	}
	body, _ := json.Marshal(command)
	reply := requestHandler(t, d.natsConn, "ec2.cmd.i-spot-nodata", d.handleEC2Events, testAccountID, body)
	assert.Equal(t, awserrors.ErrorMissingParameter, decodeError(t, reply.Data)["Code"])

	got, ok := d.vmMgr.Get("i-spot-nodata")
	require.True(t, ok)
	assert.Empty(t, got.InstanceLifecycle)
	assert.Empty(t, got.SpotInstanceRequestId)
}
