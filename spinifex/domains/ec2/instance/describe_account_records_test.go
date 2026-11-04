package instance

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	vmmock "github.com/mulgadc/spinifex/spinifex/runtime/compute/vm/mock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func recordsService(store *vmmock.StateStore) *InstanceServiceImpl {
	return &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{}), stoppedStore: store, config: &config.Config{}}
}

func recordIDs(out *ec2.DescribeInstancesOutput) map[string]string {
	ids := map[string]string{}
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			ids[*inst.InstanceId] = *inst.State.Name
		}
	}
	return ids
}

// The record read answers for instances the stopped listing leaves out: a
// running or drain-stopped record that no node need be holding.
func TestDescribeAccountInstanceRecords_ReturnsEveryNonTerminatedRecordOfTheAccount(t *testing.T) {
	owner := "111122223333"
	store := &vmmock.StateStore{
		Stopped: map[string]*vm.VM{"i-stopped": nilRecordVM("i-stopped", owner, true, true, vm.StateStopped)},
		Saved: map[string]map[string]*vm.VM{"node-gone": {
			"i-running": nilRecordVM("i-running", owner, true, true, vm.StateRunning),
			"i-other":   nilRecordVM("i-other", "999999999999", true, true, vm.StateRunning),
		}},
	}

	out, err := recordsService(store).DescribeAccountInstanceRecords(context.Background(), &ec2.DescribeInstancesInput{}, owner)

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"i-stopped": "stopped", "i-running": "running"}, recordIDs(out),
		"the other account's record is not returned")
}

// A record that cannot be projected fails the read; it is never skipped, since
// the caller takes existence from this answer.
func TestDescribeAccountInstanceRecords_NilReservationOrInstanceFails(t *testing.T) {
	owner := "111122223333"
	for name, bad := range map[string]*vm.VM{
		"nil reservation": nilRecordVM("i-bad", owner, false, true, vm.StateRunning),
		"nil instance":    nilRecordVM("i-bad", owner, true, false, vm.StateRunning),
	} {
		t.Run(name, func(t *testing.T) {
			store := &vmmock.StateStore{
				Stopped: map[string]*vm.VM{"i-good": nilRecordVM("i-good", owner, true, true, vm.StateStopped)},
				Saved:   map[string]map[string]*vm.VM{"node-1": {"i-bad": bad}},
			}

			out, err := recordsService(store).DescribeAccountInstanceRecords(context.Background(), &ec2.DescribeInstancesInput{}, owner)

			require.Error(t, err)
			assert.Nil(t, out, "no shorter list is returned")
			assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
		})
	}
}

func TestDescribeAccountInstanceRecords_StoreErrorFails(t *testing.T) {
	store := &vmmock.StateStore{ListLiveErr: errors.New("kv down")}

	out, err := recordsService(store).DescribeAccountInstanceRecords(context.Background(), &ec2.DescribeInstancesInput{}, "111122223333")

	require.Error(t, err)
	assert.Nil(t, out)
}

func TestDescribeAccountInstanceRecords_NoStoreFails(t *testing.T) {
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{}), config: &config.Config{}}

	out, err := svc.DescribeAccountInstanceRecords(context.Background(), &ec2.DescribeInstancesInput{}, "111122223333")

	require.Error(t, err)
	assert.Nil(t, out)
}
