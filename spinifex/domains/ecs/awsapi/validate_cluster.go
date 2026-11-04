package awsapi

import (
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

var (
	executeCommandLoggingTypes = []string{"NONE", "DEFAULT", "OVERRIDE"}
	clusterContainerInsights   = []string{"enabled", "disabled", "enhanced"}
)

// checkCreateCluster refuses settings, logging and strategy values outside the
// model's sets and ranges with the messages AWS answers.
func checkCreateCluster(input *ecs.CreateClusterInput) error {
	for _, st := range input.Settings {
		if st == nil {
			continue
		}
		if aws.StringValue(st.Name) != "containerInsights" {
			return awserrors.Errorf(awserrors.ErrorECSInvalidParameter, "Invalid setting 'name'")
		}
		if !slices.Contains(clusterContainerInsights, aws.StringValue(st.Value)) {
			return awserrors.Errorf(awserrors.ErrorECSInvalidParameter, "Invalid setting 'value'")
		}
	}
	if cfg := input.Configuration; cfg != nil && cfg.ExecuteCommandConfiguration != nil {
		if l := cfg.ExecuteCommandConfiguration.Logging; l != nil && !slices.Contains(executeCommandLoggingTypes, *l) {
			return awserrors.Errorf(awserrors.ErrorECSInvalidParameter, "Unknown executeCommandLoggingType '%s'", *l)
		}
	}
	return checkCapacityProviderStrategy(input.DefaultCapacityProviderStrategy)
}

// checkCapacityProviderStrategy range-checks each item's weight and base.
func checkCapacityProviderStrategy(items []*ecs.CapacityProviderStrategyItem) error {
	for _, item := range items {
		if item == nil {
			continue
		}
		if w := item.Weight; w != nil && (*w < 0 || *w > 1000) {
			return awserrors.Errorf(awserrors.ErrorECSInvalidParameter,
				"The specified weight value '%d' is invalid. Specify an integer between 0 and 1000 and try again.", *w)
		}
		if b := item.Base; b != nil && (*b < 0 || *b > 100000) {
			return awserrors.Errorf(awserrors.ErrorECSInvalidParameter,
				"The specified base value '%d' is invalid. Specify an integer between 0 and 100000 and try again.", *b)
		}
	}
	return nil
}
