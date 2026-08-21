package handlers_ec2_instance

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/vm"
	vmmock "github.com/mulgadc/spinifex/spinifex/vm/mock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stoppedMonitoredInstance(id, accountID string, enabled *bool) *vm.VM {
	var monitoring *ec2.RunInstancesMonitoringEnabled
	if enabled != nil {
		monitoring = &ec2.RunInstancesMonitoringEnabled{Enabled: enabled}
	}
	return &vm.VM{
		ID:                id,
		AccountID:         accountID,
		Status:            vm.StateStopped,
		Instance:          &ec2.Instance{InstanceId: aws.String(id)},
		RunInstancesInput: &ec2.RunInstancesInput{Monitoring: monitoring},
	}
}

func storedMonitoringEnabled(t *testing.T, v *vm.VM) bool {
	t.Helper()
	require.NotNil(t, v)
	require.NotNil(t, v.RunInstancesInput)
	require.NotNil(t, v.RunInstancesInput.Monitoring)
	return aws.BoolValue(v.RunInstancesInput.Monitoring.Enabled)
}

func TestSetStoppedInstanceMonitoring_Enable(t *testing.T) {
	stored := stoppedMonitoredInstance("i-123", "111122223333", aws.Bool(false))
	store := &vmmock.StateStore{Stopped: map[string]*vm.VM{"i-123": stored}}
	svc := &InstanceServiceImpl{stoppedStore: store}

	require.NoError(t, svc.SetStoppedInstanceMonitoring(context.Background(), "i-123", true, "111122223333"))
	assert.True(t, storedMonitoringEnabled(t, store.Stopped["i-123"]))
	assert.Equal(t, 1, store.UpdateStoppedCalls)
}

func TestSetStoppedInstanceMonitoring_Disable(t *testing.T) {
	stored := stoppedMonitoredInstance("i-123", "111122223333", aws.Bool(true))
	store := &vmmock.StateStore{Stopped: map[string]*vm.VM{"i-123": stored}}
	svc := &InstanceServiceImpl{stoppedStore: store}

	require.NoError(t, svc.SetStoppedInstanceMonitoring(context.Background(), "i-123", false, "111122223333"))
	assert.False(t, storedMonitoringEnabled(t, store.Stopped["i-123"]))
}

// A launch that never asked for monitoring has no Monitoring block at all, so
// enabling one has to create it rather than assume it is there.
func TestSetStoppedInstanceMonitoring_CreatesMissingBlock(t *testing.T) {
	stored := stoppedMonitoredInstance("i-123", "111122223333", nil)
	store := &vmmock.StateStore{Stopped: map[string]*vm.VM{"i-123": stored}}
	svc := &InstanceServiceImpl{stoppedStore: store}

	require.NoError(t, svc.SetStoppedInstanceMonitoring(context.Background(), "i-123", true, "111122223333"))
	assert.True(t, storedMonitoringEnabled(t, store.Stopped["i-123"]))
}

func TestSetStoppedInstanceMonitoring_NotFound(t *testing.T) {
	store := &vmmock.StateStore{Stopped: map[string]*vm.VM{}}
	svc := &InstanceServiceImpl{stoppedStore: store}

	err := svc.SetStoppedInstanceMonitoring(context.Background(), "i-missing", true, "111122223333")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
	assert.Empty(t, store.WroteStopped)
}

func TestSetStoppedInstanceMonitoring_CrossAccountRejected(t *testing.T) {
	stored := stoppedMonitoredInstance("i-123", "999988887777", aws.Bool(false))
	store := &vmmock.StateStore{Stopped: map[string]*vm.VM{"i-123": stored}}
	svc := &InstanceServiceImpl{stoppedStore: store}

	err := svc.SetStoppedInstanceMonitoring(context.Background(), "i-123", true, "111122223333")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
	assert.False(t, storedMonitoringEnabled(t, store.Stopped["i-123"]))
	assert.Empty(t, store.WroteStopped)
}

// A monitoring toggle racing a winning start-claim must not resurrect the
// stopped record the claim deleted between this call's Load and its CAS write.
func TestSetStoppedInstanceMonitoring_ConcurrentClaimDoesNotResurrect(t *testing.T) {
	stored := stoppedMonitoredInstance("i-123", "111122223333", aws.Bool(false))
	store := &vmmock.StateStore{Stopped: map[string]*vm.VM{"i-123": stored}, ClaimAfterLoad: true}
	svc := &InstanceServiceImpl{stoppedStore: store}

	err := svc.SetStoppedInstanceMonitoring(context.Background(), "i-123", true, "111122223333")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
	assert.Empty(t, store.Stopped, "the claimed record must not be resurrected")
	assert.Equal(t, []string{"i-123"}, store.ClaimedStopped)
}

func TestSetStoppedInstanceMonitoring_NoStore(t *testing.T) {
	svc := &InstanceServiceImpl{}
	err := svc.SetStoppedInstanceMonitoring(context.Background(), "i-123", true, "111122223333")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestProjectInstance_MonitoringState(t *testing.T) {
	tests := []struct {
		name  string
		input *ec2.RunInstancesInput
		want  string
	}{
		{"detailed", &ec2.RunInstancesInput{Monitoring: &ec2.RunInstancesMonitoringEnabled{Enabled: aws.Bool(true)}}, ec2.MonitoringStateEnabled},
		{"basic", &ec2.RunInstancesInput{Monitoring: &ec2.RunInstancesMonitoringEnabled{Enabled: aws.Bool(false)}}, ec2.MonitoringStateDisabled},
		{"no monitoring block", &ec2.RunInstancesInput{}, ec2.MonitoringStateDisabled},
		// Instances launched before the field existed carry no request at all.
		{"no launch input", nil, ec2.MonitoringStateDisabled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &vm.VM{
				ID:                "i-123",
				Status:            vm.StateRunning,
				Instance:          &ec2.Instance{InstanceId: aws.String("i-123")},
				RunInstancesInput: tt.input,
			}
			projected, _ := ProjectInstance(v, InstanceProjection{})
			require.NotNil(t, projected.Monitoring)
			assert.Equal(t, tt.want, aws.StringValue(projected.Monitoring.State))
		})
	}
}
