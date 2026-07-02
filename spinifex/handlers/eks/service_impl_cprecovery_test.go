package handlers_eks

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCPController stubs the daemon instance service surface the CP-control
// adapter binds to, recording calls and returning configurable responses.
type fakeCPController struct {
	describeOut  *ec2.DescribeInstancesOutput
	describeErr  error
	startErr     error
	startedID    string
	startAccount string
}

func (f *fakeCPController) DescribeInstances(_ *ec2.DescribeInstancesInput, _ string) (*ec2.DescribeInstancesOutput, error) {
	return f.describeOut, f.describeErr
}

func (f *fakeCPController) StartStoppedInstance(input *handlers_ec2_instance.StartStoppedInstanceInput, accountID string) (*handlers_ec2_instance.StartStoppedInstanceOutput, error) {
	f.startedID = input.InstanceID
	f.startAccount = accountID
	return &handlers_ec2_instance.StartStoppedInstanceOutput{Status: "pending"}, f.startErr
}

func describeWithState(instanceID, state string) *ec2.DescribeInstancesOutput {
	return &ec2.DescribeInstancesOutput{
		Reservations: []*ec2.Reservation{{
			Instances: []*ec2.Instance{{
				InstanceId: aws.String(instanceID),
				State:      &ec2.InstanceState{Name: aws.String(state)},
			}},
		}},
	}
}

func TestCPControlAdapter_InstanceStateReturnsStateName(t *testing.T) {
	ctl := &fakeCPController{describeOut: describeWithState("i-cp", "stopped")}
	a := cpControlAdapter{ctl: ctl, accountID: testAccountID}

	state, err := a.InstanceState(context.Background(), "i-cp")

	require.NoError(t, err)
	assert.Equal(t, "stopped", state)
}

func TestCPControlAdapter_InstanceStateNotFound(t *testing.T) {
	ctl := &fakeCPController{describeOut: &ec2.DescribeInstancesOutput{}}
	a := cpControlAdapter{ctl: ctl, accountID: testAccountID}

	_, err := a.InstanceState(context.Background(), "i-cp")

	require.Error(t, err, "an absent instance surfaces an error so the reconciler skips the restart")
}

func TestCPControlAdapter_InstanceStatePropagatesDescribeError(t *testing.T) {
	ctl := &fakeCPController{describeErr: errors.New("describe boom")}
	a := cpControlAdapter{ctl: ctl, accountID: testAccountID}

	_, err := a.InstanceState(context.Background(), "i-cp")

	require.ErrorContains(t, err, "describe boom")
}

func TestCPControlAdapter_StartInstanceForwardsIDAndAccount(t *testing.T) {
	ctl := &fakeCPController{}
	a := cpControlAdapter{ctl: ctl, accountID: testAccountID}

	require.NoError(t, a.StartInstance(context.Background(), "i-cp"))
	assert.Equal(t, "i-cp", ctl.startedID)
	assert.Equal(t, testAccountID, ctl.startAccount)
}

func TestCPControlAdapter_StartInstancePropagatesError(t *testing.T) {
	ctl := &fakeCPController{startErr: errors.New("not stopped")}
	a := cpControlAdapter{ctl: ctl, accountID: testAccountID}

	require.ErrorContains(t, a.StartInstance(context.Background(), "i-cp"), "not stopped")
}
