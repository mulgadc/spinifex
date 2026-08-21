package daemon

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/vm"
	vmmock "github.com/mulgadc/spinifex/spinifex/vm/mock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// monitoringTestDaemon returns a test daemon owning one running instance
// launched at the given tier, plus its stopped store.
func monitoringTestDaemon(t *testing.T, instanceID string, enabled bool) (*Daemon, *vmmock.StateStore) {
	t.Helper()
	d := createTestDaemon(t, sharedNATSURL)

	stopped := vmmock.New()
	d.instanceService = handlers_ec2_instance.NewInstanceServiceImpl(
		d.config, d.resourceMgr.instanceTypes, d.natsConn,
		objectstore.NewMemoryObjectStore(), d.vmMgr, d.resourceMgr, stopped)

	d.vmMgr.Insert(&vm.VM{
		ID:        instanceID,
		Status:    vm.StateRunning,
		AccountID: testAccountID,
		Instance:  &ec2.Instance{InstanceId: aws.String(instanceID)},
		RunInstancesInput: &ec2.RunInstancesInput{
			Monitoring: &ec2.RunInstancesMonitoringEnabled{Enabled: aws.Bool(enabled)},
		},
	})
	return d, stopped
}

func recordMonitoring(t *testing.T, d *Daemon, instanceID string) bool {
	t.Helper()
	got, ok := d.vmMgr.Get(instanceID)
	require.True(t, ok)
	require.NotNil(t, got.RunInstancesInput)
	require.NotNil(t, got.RunInstancesInput.Monitoring)
	return aws.BoolValue(got.RunInstancesInput.Monitoring.Enabled)
}

func TestMonitorInstances_RoutedViaOwner(t *testing.T) {
	const id = "i-mon-owner"
	d, _ := monitoringTestDaemon(t, id, false)
	subscribeOwner(t, d, id)

	out, err := d.monitorInstances(context.Background(), &ec2.MonitorInstancesInput{
		InstanceIds: []*string{aws.String(id)},
	}, testAccountID)
	require.NoError(t, err)

	require.Len(t, out.InstanceMonitorings, 1)
	assert.Equal(t, id, aws.StringValue(out.InstanceMonitorings[0].InstanceId))
	assert.Equal(t, ec2.MonitoringStateEnabled, aws.StringValue(out.InstanceMonitorings[0].Monitoring.State))
	assert.True(t, recordMonitoring(t, d, id))
}

func TestUnmonitorInstances_RoutedViaOwner(t *testing.T) {
	const id = "i-mon-unowner"
	d, _ := monitoringTestDaemon(t, id, true)
	subscribeOwner(t, d, id)

	out, err := d.unmonitorInstances(context.Background(), &ec2.UnmonitorInstancesInput{
		InstanceIds: []*string{aws.String(id)},
	}, testAccountID)
	require.NoError(t, err)

	require.Len(t, out.InstanceMonitorings, 1)
	assert.Equal(t, ec2.MonitoringStateDisabled, aws.StringValue(out.InstanceMonitorings[0].Monitoring.State))
	assert.False(t, recordMonitoring(t, d, id))
}

// A launch that never asked for monitoring has no Monitoring block, so the
// running path has to create one rather than skip the instance.
func TestMonitorInstances_CreatesMissingBlock(t *testing.T) {
	const id = "i-mon-noblock"
	d, _ := monitoringTestDaemon(t, id, false)
	d.vmMgr.Insert(&vm.VM{
		ID:                id,
		Status:            vm.StateRunning,
		AccountID:         testAccountID,
		Instance:          &ec2.Instance{InstanceId: aws.String(id)},
		RunInstancesInput: &ec2.RunInstancesInput{},
	})
	subscribeOwner(t, d, id)

	_, err := d.monitorInstances(context.Background(), &ec2.MonitorInstancesInput{
		InstanceIds: []*string{aws.String(id)},
	}, testAccountID)
	require.NoError(t, err)
	assert.True(t, recordMonitoring(t, d, id))
}

// With no running owner the call falls back to the shared stopped store, so a
// stopped instance can still be moved between tiers.
func TestMonitorInstances_StoppedFallback(t *testing.T) {
	const id = "i-mon-stopped"
	d, stopped := monitoringTestDaemon(t, "i-mon-unrelated", false)
	stopped.Stopped[id] = &vm.VM{
		ID:                id,
		Status:            vm.StateStopped,
		AccountID:         testAccountID,
		Instance:          &ec2.Instance{InstanceId: aws.String(id)},
		RunInstancesInput: &ec2.RunInstancesInput{},
	}

	_, err := d.monitorInstances(context.Background(), &ec2.MonitorInstancesInput{
		InstanceIds: []*string{aws.String(id)},
	}, testAccountID)
	require.NoError(t, err)
	assert.True(t, aws.BoolValue(stopped.Stopped[id].RunInstancesInput.Monitoring.Enabled))
}

// An instance nobody owns and no stopped record covers is NotFound, not a
// silent success that leaves the caller believing the tier changed.
func TestMonitorInstances_NoOwnerNotFound(t *testing.T) {
	d, _ := monitoringTestDaemon(t, "i-mon-unrelated2", false)

	_, err := d.monitorInstances(context.Background(), &ec2.MonitorInstancesInput{
		InstanceIds: []*string{aws.String("i-mon-gone1"), aws.String("i-mon-gone2")},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

// The owner's cross-account rejection is relayed and the tier is untouched.
func TestMonitorInstances_OwnerErrorRelayed(t *testing.T) {
	const id = "i-mon-crossacct"
	const attacker = "999999999999"
	d, _ := monitoringTestDaemon(t, id, false)
	subscribeOwner(t, d, id)

	_, err := d.monitorInstances(context.Background(), &ec2.MonitorInstancesInput{
		InstanceIds: []*string{aws.String(id)},
	}, attacker)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
	assert.False(t, recordMonitoring(t, d, id))
}

// One unreachable instance in a batch fails the whole call rather than
// reporting a tier for the instances that did apply.
func TestMonitorInstances_PartialFailureReportsNoState(t *testing.T) {
	const id = "i-mon-partial"
	d, _ := monitoringTestDaemon(t, id, false)
	subscribeOwner(t, d, id)

	out, err := d.monitorInstances(context.Background(), &ec2.MonitorInstancesInput{
		InstanceIds: []*string{aws.String(id), aws.String("i-mon-partial-gone")},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
	assert.Nil(t, out)
}

func TestMonitorInstances_Validation(t *testing.T) {
	d, _ := monitoringTestDaemon(t, "i-mon-valid", false)

	_, err := d.monitorInstances(context.Background(), nil, testAccountID)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())

	_, err = d.monitorInstances(context.Background(), &ec2.MonitorInstancesInput{}, testAccountID)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())

	_, err = d.unmonitorInstances(context.Background(), nil, testAccountID)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())

	_, err = d.unmonitorInstances(context.Background(), &ec2.UnmonitorInstancesInput{}, testAccountID)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())

	_, err = d.monitorInstances(context.Background(), &ec2.MonitorInstancesInput{
		InstanceIds: []*string{nil},
	}, testAccountID)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDMalformed, err.Error())
}
