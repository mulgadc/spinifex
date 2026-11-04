package awsapi

import (
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// Messages AWS answers, as ClientException, for each autoScalingGroupProvider
// break, before it looks up the Auto Scaling group.
const (
	msgManagedDraining              = "The specified managed draining setting is invalid. Specify ENABLED or DISABLED and try again."
	msgManagedTerminationProtection = "The specified managed termination protection setting is invalid. Specify ENABLED or DISABLED and try again."
	msgManagedScalingStatus         = "The specified managed scaling status setting is invalid. Specify ENABLED or DISABLED and try again."
	msgTargetCapacity               = "The managed scaling target capacity value is invalid. Specify an integer greater than 0 and less than or equal to 100 and try again."
	msgMinimumScalingStepSize       = "The specified minimum scaling step size is invalid. Specify an integer between 1 and 10000 and try again."
	msgMaximumScalingStepSize       = "The specified maximum scaling step size is invalid. Specify an integer between 1 and 10000 and try again."
	msgInstanceWarmupPeriod         = "The managed scaling instance warmup period value is invalid. Specify an integer greater than or equal to 0 and less than or equal to 9999 and try again."
)

func validEnabledDisabled(v *string) bool {
	return v == nil || *v == "ENABLED" || *v == "DISABLED"
}

func validRange(v *int64, lo, hi int64) bool {
	return v == nil || (*v >= lo && *v <= hi)
}

// checkAutoScalingGroupProvider refuses an enum or range break inside
// autoScalingGroupProvider. AWS refuses an instanceWarmupPeriod of 10000 although
// the model allows it, so the upper bound here is 9999.
func checkAutoScalingGroupProvider(p *ecs.AutoScalingGroupProvider) error {
	if p == nil {
		return nil
	}
	fail := func(msg string) error { return awserrors.Errorf(awserrors.ErrorECSClientException, "%s", msg) }
	if !validEnabledDisabled(p.ManagedDraining) {
		return fail(msgManagedDraining)
	}
	if !validEnabledDisabled(p.ManagedTerminationProtection) {
		return fail(msgManagedTerminationProtection)
	}
	ms := p.ManagedScaling
	if ms == nil {
		return nil
	}
	switch {
	case !validEnabledDisabled(ms.Status):
		return fail(msgManagedScalingStatus)
	case !validRange(ms.TargetCapacity, 1, 100):
		return fail(msgTargetCapacity)
	case !validRange(ms.MinimumScalingStepSize, 1, 10000):
		return fail(msgMinimumScalingStepSize)
	case !validRange(ms.MaximumScalingStepSize, 1, 10000):
		return fail(msgMaximumScalingStepSize)
	case !validRange(ms.InstanceWarmupPeriod, 0, 9999):
		return fail(msgInstanceWarmupPeriod)
	}
	return nil
}
