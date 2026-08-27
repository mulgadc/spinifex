// Drives Manager.launch through the unexported relaunchTestManager harness,
// the only seam that reaches the launch path without a real QEMU process.
//
//test:in-package
package vm

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func unschedulableReason() *ec2.StateReason {
	return &ec2.StateReason{
		Code:    aws.String("Server.InsufficientInstanceCapacity"),
		Message: aws.String("instance type m7i.small is not available on this node"),
	}
}

// The node writes StateReason when it stops or fails an instance, so the node
// is also what clears it. A launch clears before it can fail, so a failed
// launch that stamps a fresh reason does not have to fight a stale one.
func TestLaunch_ClearsStaleStateReason(t *testing.T) {
	m, mounter, _, _ := relaunchTestManager(t)

	instance := &VM{
		ID:           "i-stale-reason",
		Status:       StateStopped,
		InstanceType: "m7i.small",
		Instance:     &ec2.Instance{StateReason: unschedulableReason()},
	}
	m.Insert(instance)
	mounter.behavior[instance.ID] = func(*VM) error { return errors.New("mount failed") }

	require.Error(t, m.Run(context.Background(), instance), "the launch is expected to fail at Mount")
	assert.Nil(t, instance.Instance.StateReason,
		"the reason describes the state the instance is leaving and must not outlive the launch attempt")
}

// A launch that never starts must leave the reason alone: it still describes
// the state the instance is actually in.
func TestLaunch_AbortedByTerminate_KeepsStateReason(t *testing.T) {
	m, _, _, _ := relaunchTestManager(t)

	instance := &VM{
		ID:           "i-terminating",
		Status:       StateShuttingDown,
		InstanceType: "m7i.small",
		Instance:     &ec2.Instance{StateReason: unschedulableReason()},
	}
	m.Insert(instance)

	require.NoError(t, m.Run(context.Background(), instance), "a raced terminate aborts the launch quietly")
	require.NotNil(t, instance.Instance.StateReason)
	assert.Equal(t, "Server.InsufficientInstanceCapacity", *instance.Instance.StateReason.Code)
}

// markUnschedulable is the writer this clear is the counterpart of; a record
// with no ec2.Instance must not panic either side.
func TestLaunch_NoEC2Instance_NoPanic(t *testing.T) {
	m, mounter, _, _ := relaunchTestManager(t)

	instance := &VM{ID: "i-bare", Status: StateStopped, InstanceType: "m7i.small"}
	m.Insert(instance)
	mounter.behavior[instance.ID] = func(*VM) error { return errors.New("mount failed") }

	require.Error(t, m.Run(context.Background(), instance))
}
