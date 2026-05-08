package vm

import (
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
)

func TestIsValidTransition(t *testing.T) {
	assert.True(t, IsValidTransition(StateRunning, StateStopping))
	assert.True(t, IsValidTransition(StateRunning, StateShuttingDown))
	assert.True(t, IsValidTransition(StateProvisioning, StateRunning))
	assert.True(t, IsValidTransition(StateStopped, StateRunning))
	assert.True(t, IsValidTransition(StateShuttingDown, StateTerminated))
	assert.True(t, IsValidTransition(StateError, StateRunning))

	assert.False(t, IsValidTransition(StateRunning, StateTerminated))
	assert.False(t, IsValidTransition(StateTerminated, StateRunning))
	assert.False(t, IsValidTransition(StateRunning, StateRunning))
	assert.False(t, IsValidTransition(StateStopped, StateStopping))
}

// awsEC2StateCodes is the AWS-documented EC2 InstanceState code/name
// contract (see API_InstanceState). Names come from the SDK's enum
// constants; codes are fixed by the AWS API and have no SDK constant.
// Verifying our production map against an independent literal protects
// against a typo replicated identically in test and production.
var awsEC2StateCodes = map[string]int64{
	ec2.InstanceStateNamePending:      0,
	ec2.InstanceStateNameRunning:      16,
	ec2.InstanceStateNameShuttingDown: 32,
	ec2.InstanceStateNameTerminated:   48,
	ec2.InstanceStateNameStopping:     64,
	ec2.InstanceStateNameStopped:      80,
}

func TestEC2StateCodes_AllInstanceStatesMapped(t *testing.T) {
	for _, s := range []InstanceState{
		StateProvisioning, StatePending, StateRunning, StateStopping,
		StateStopped, StateShuttingDown, StateTerminated, StateError,
	} {
		info, ok := EC2StateCodes[s]
		assert.True(t, ok, "InstanceState %s missing from EC2StateCodes", s)
		assert.NotEmpty(t, info.Name, "InstanceState %s mapped to empty EC2 name", s)
	}
}

func TestEC2StateCodes_MatchAWSContract(t *testing.T) {
	// Production states with a direct AWS equivalent must match the AWS
	// (code, name) tuple exactly. State{Provisioning,Error} are
	// Spinifex-only and verified separately below.
	awsBacked := map[InstanceState]string{
		StatePending:      ec2.InstanceStateNamePending,
		StateRunning:      ec2.InstanceStateNameRunning,
		StateStopping:     ec2.InstanceStateNameStopping,
		StateStopped:      ec2.InstanceStateNameStopped,
		StateShuttingDown: ec2.InstanceStateNameShuttingDown,
		StateTerminated:   ec2.InstanceStateNameTerminated,
	}
	for state, awsName := range awsBacked {
		got := EC2StateCodes[state]
		assert.Equal(t, awsName, got.Name,
			"InstanceState %s name diverges from AWS contract", state)
		assert.Equal(t, awsEC2StateCodes[awsName], got.Code,
			"InstanceState %s code diverges from AWS contract", state)
	}
}

func TestEC2StateCodes_SpinifexOnlyStatesPresentAsPending(t *testing.T) {
	// Provisioning is a pre-launch internal state; it surfaces to AWS
	// callers as "pending" so DescribeInstances returns a code AWS clients
	// understand. Error is a terminal-ish failure state with no AWS
	// equivalent — code 0 is the safest fallback ("pending" code).
	assert.Equal(t, "pending", EC2StateCodes[StateProvisioning].Name)
	assert.Equal(t, int64(0), EC2StateCodes[StateProvisioning].Code)
	assert.Equal(t, int64(0), EC2StateCodes[StateError].Code)
}
