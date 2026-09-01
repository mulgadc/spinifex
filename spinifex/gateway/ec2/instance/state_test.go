package gateway_ec2_instance

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewStateChange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		instanceID  string
		currentCode int64
		currentName string
		prevCode    int64
		prevName    string
	}{
		{
			name:        "RunningToStopping",
			instanceID:  "i-1234567890abcdef0",
			currentCode: 64,
			currentName: "stopping",
			prevCode:    16,
			prevName:    "running",
		},
		{
			name:        "StoppedToPending",
			instanceID:  "i-abcdef1234567890",
			currentCode: 0,
			currentName: "pending",
			prevCode:    80,
			prevName:    "stopped",
		},
		{
			name:        "RunningToShuttingDown",
			instanceID:  "i-test123",
			currentCode: 32,
			currentName: "shutting-down",
			prevCode:    16,
			prevName:    "running",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := newStateChange(tt.instanceID, tt.currentCode, tt.currentName, tt.prevCode, tt.prevName)

			require.NotNil(t, result)
			assert.Equal(t, tt.instanceID, *result.InstanceId)

			require.NotNil(t, result.CurrentState)
			assert.Equal(t, tt.currentCode, *result.CurrentState.Code)
			assert.Equal(t, tt.currentName, *result.CurrentState.Name)

			require.NotNil(t, result.PreviousState)
			assert.Equal(t, tt.prevCode, *result.PreviousState.Code)
			assert.Equal(t, tt.prevName, *result.PreviousState.Name)
		})
	}
}
