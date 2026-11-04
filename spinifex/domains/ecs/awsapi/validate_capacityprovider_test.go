package awsapi

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each break answers AWS's ClientException and message before dispatch. A valid
// request reaches dispatch, which fails here only because nc is nil.
func TestCreateCapacityProviderAutoScalingGroupProvider(t *testing.T) {
	body := func(asg string) []byte {
		return []byte(`{"name":"cp","autoScalingGroupProvider":{"autoScalingGroupArn":"arn:aws:autoscaling:x"` + asg + `}}`)
	}
	refused := []struct {
		name, asg, msg string
	}{
		{"managedDraining", `,"managedDraining":"BOGUS"`, msgManagedDraining},
		{"managedTerminationProtection", `,"managedTerminationProtection":"enabled"`, msgManagedTerminationProtection},
		{"status", `,"managedScaling":{"status":"BOGUS"}`, msgManagedScalingStatus},
		{"targetCapacity/0", `,"managedScaling":{"targetCapacity":0}`, msgTargetCapacity},
		{"targetCapacity/101", `,"managedScaling":{"targetCapacity":101}`, msgTargetCapacity},
		{"minimumScalingStepSize/0", `,"managedScaling":{"minimumScalingStepSize":0}`, msgMinimumScalingStepSize},
		{"maximumScalingStepSize/10001", `,"managedScaling":{"maximumScalingStepSize":10001}`, msgMaximumScalingStepSize},
		{"instanceWarmupPeriod/-1", `,"managedScaling":{"instanceWarmupPeriod":-1}`, msgInstanceWarmupPeriod},
		{"instanceWarmupPeriod/10000", `,"managedScaling":{"instanceWarmupPeriod":10000}`, msgInstanceWarmupPeriod},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateCapacityProvider(context.Background(), nil, "123456789012", body(tc.asg), nil)
			code, msg, ok := awserrors.ResolveErrorDetail(err)
			require.True(t, ok, "err = %v", err)
			assert.Equal(t, awserrors.ErrorECSClientException, code)
			assert.Equal(t, tc.msg, msg)
		})
	}

	valid := []string{
		``,
		`,"managedDraining":"ENABLED","managedTerminationProtection":"DISABLED","managedScaling":{"status":"ENABLED","targetCapacity":1,"minimumScalingStepSize":1,"maximumScalingStepSize":10000,"instanceWarmupPeriod":0}`,
		`,"managedDraining":"DISABLED","managedTerminationProtection":"ENABLED","managedScaling":{"status":"DISABLED","targetCapacity":100,"minimumScalingStepSize":10000,"maximumScalingStepSize":1,"instanceWarmupPeriod":9999}`,
	}
	for _, asg := range valid {
		_, err := CreateCapacityProvider(context.Background(), nil, "123456789012", body(asg), nil)
		_, _, refusedErr := awserrors.ResolveErrorDetail(err)
		assert.False(t, refusedErr, "asg %q: err = %v", asg, err)
	}
}
