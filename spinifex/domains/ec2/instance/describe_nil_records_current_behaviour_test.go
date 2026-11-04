package instance

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	vmmock "github.com/mulgadc/spinifex/spinifex/runtime/compute/vm/mock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func nilRecordVM(id, owner string, withReservation, withInstance bool, status vm.InstanceState) *vm.VM {
	v := &vm.VM{ID: id, InstanceType: "t3.micro", Status: status, AccountID: owner}
	if withReservation {
		v.Reservation = &ec2.Reservation{ReservationId: aws.String("r-" + id), OwnerId: aws.String(owner)}
	}
	if withInstance {
		v.Instance = &ec2.Instance{InstanceId: aws.String(id), InstanceType: aws.String("t3.micro")}
	}
	return v
}

// Pins today: the node path drops an instance whose Reservation or Instance is
// nil without a log line or an error, so it reads as absent from a listing the
// caller may treat as complete.
func TestCurrentBehaviour_DescribeInstances_NodePathSkipsNilReservationAndInstanceSilently(t *testing.T) {
	owner := "111122223333"
	vms := map[string]*vm.VM{
		"i-good":    nilRecordVM("i-good", owner, true, true, vm.StateRunning),
		"i-no-res":  nilRecordVM("i-no-res", owner, false, true, vm.StateRunning),
		"i-no-inst": nilRecordVM("i-no-inst", owner, true, false, vm.StateRunning),
	}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(vms), config: &config.Config{}}

	out, err := svc.DescribeInstances(context.Background(), &ec2.DescribeInstancesInput{}, owner)

	require.NoError(t, err, "CURRENT: no error for the two skipped instances")
	require.Len(t, out.Reservations, 1)
	require.Len(t, out.Reservations[0].Instances, 1)
	assert.Equal(t, "i-good", *out.Reservations[0].Instances[0].InstanceId, "CURRENT: only the complete record is returned")
}

// Pins today: the KV path skips the same records, with a warning, and still
// answers success.
func TestCurrentBehaviour_DescribeStoppedInstances_KVPathSkipsNilReservationAndInstance(t *testing.T) {
	owner := "111122223333"
	store := &vmmock.StateStore{Stopped: map[string]*vm.VM{
		"i-good":    nilRecordVM("i-good", owner, true, true, vm.StateStopped),
		"i-no-res":  nilRecordVM("i-no-res", owner, false, true, vm.StateStopped),
		"i-no-inst": nilRecordVM("i-no-inst", owner, true, false, vm.StateStopped),
	}}
	svc := &InstanceServiceImpl{vmMgr: mgrWith(map[string]*vm.VM{}), stoppedStore: store, config: &config.Config{}}

	out, err := svc.DescribeStoppedInstances(context.Background(), &ec2.DescribeInstancesInput{}, owner)

	require.NoError(t, err, "CURRENT: no error for the two skipped instances")
	require.Len(t, out.Reservations, 1)
	require.Len(t, out.Reservations[0].Instances, 1)
	assert.Equal(t, "i-good", *out.Reservations[0].Instances[0].InstanceId, "CURRENT: only the complete record is returned")
}
