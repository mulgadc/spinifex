package handlers_eks

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeCPController stubs the NATS-backed instance surface the CP-control adapter
// binds to, recording calls and returning configurable responses.
type fakeCPController struct {
	describeOut  *ec2.DescribeInstancesOutput
	describeErr  error
	recoverErr   error
	recoveredID  string
	recoverAcct  string
	recoverCalls int
	rebootErr    error
	rebootedID   string
	rebootAcct   string
	rebootCalls  int
}

func (f *fakeCPController) DescribeInstances(_ *ec2.DescribeInstancesInput, _ string) (*ec2.DescribeInstancesOutput, error) {
	return f.describeOut, f.describeErr
}

func (f *fakeCPController) RecoverInstance(instanceID, accountID string) error {
	f.recoverCalls++
	f.recoveredID = instanceID
	f.recoverAcct = accountID
	return f.recoverErr
}

func (f *fakeCPController) RebootInstance(instanceID, accountID string) error {
	f.rebootCalls++
	f.rebootedID = instanceID
	f.rebootAcct = accountID
	return f.rebootErr
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
	assert.Equal(t, 1, ctl.recoverCalls)
	assert.Equal(t, "i-cp", ctl.recoveredID)
	assert.Equal(t, testAccountID, ctl.recoverAcct)
}

func TestCPControlAdapter_StartInstancePropagatesError(t *testing.T) {
	ctl := &fakeCPController{recoverErr: errors.New("no owner")}
	a := cpControlAdapter{ctl: ctl, accountID: testAccountID}

	require.ErrorContains(t, a.StartInstance(context.Background(), "i-cp"), "no owner")
}

func TestCPControlAdapter_RebootInstanceForwardsIDAndAccount(t *testing.T) {
	ctl := &fakeCPController{}
	a := cpControlAdapter{ctl: ctl, accountID: testAccountID}

	require.NoError(t, a.RebootInstance(context.Background(), "i-cp"))
	assert.Equal(t, 1, ctl.rebootCalls)
	assert.Equal(t, "i-cp", ctl.rebootedID)
	assert.Equal(t, testAccountID, ctl.rebootAcct)
	assert.Zero(t, ctl.recoverCalls, "reboot must not fall back to the start/recover path")
}

func TestCPControlAdapter_RebootInstancePropagatesError(t *testing.T) {
	ctl := &fakeCPController{rebootErr: errors.New("no owner")}
	a := cpControlAdapter{ctl: ctl, accountID: testAccountID}

	require.ErrorContains(t, a.RebootInstance(context.Background(), "i-cp"), "no owner")
}
