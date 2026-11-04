package awsapi

import (
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// Include values each Describe* operation accepts, in the order AWS lists them
// in its refusal. Taken from the ECS model rather than the SDK's *_Values().
var (
	clusterIncludeValues           = []string{"ATTACHMENTS", "CONFIGURATIONS", "SETTINGS", "STATISTICS", "TAGS"}
	tagsIncludeValues              = []string{"TAGS"}
	containerInstanceIncludeValues = []string{"TAGS", "CONTAINER_INSTANCE_HEALTH"}
)

// firstInvalidInclude returns the first include value outside allowed.
func firstInvalidInclude(include []*string, allowed []string) (string, bool) {
	for _, v := range include {
		if s := aws.StringValue(v); !slices.Contains(allowed, s) {
			return s, true
		}
	}
	return "", false
}

func includeOneOf(allowed []string) string {
	return "[" + strings.Join(allowed, ",") + "]"
}

func checkClustersInclude(include []*string) error {
	if _, bad := firstInvalidInclude(include, clusterIncludeValues); bad {
		return awserrors.Errorf(awserrors.ErrorECSInvalidParameter, "include should be one of %s", includeOneOf(clusterIncludeValues))
	}
	return nil
}

func checkServicesInclude(include []*string) error {
	if _, bad := firstInvalidInclude(include, tagsIncludeValues); bad {
		return awserrors.Errorf(awserrors.ErrorECSInvalidParameter, "include should be one of %s", includeOneOf(tagsIncludeValues))
	}
	return nil
}

func checkTasksInclude(include []*string) error {
	if _, bad := firstInvalidInclude(include, tagsIncludeValues); bad {
		return awserrors.Errorf(awserrors.ErrorECSInvalidParameter, "include contains an invalid value, should be one of %s", includeOneOf(tagsIncludeValues))
	}
	return nil
}

func checkContainerInstancesInclude(include []*string) error {
	if _, bad := firstInvalidInclude(include, containerInstanceIncludeValues); bad {
		return awserrors.Errorf(awserrors.ErrorECSInvalidParameter, "include should be one of %s", includeOneOf(containerInstanceIncludeValues))
	}
	return nil
}

// checkCapacityProvidersInclude answers ClientException, as AWS does for this
// operation alone, naming the refused value.
func checkCapacityProvidersInclude(include []*string) error {
	if v, bad := firstInvalidInclude(include, tagsIncludeValues); bad {
		return awserrors.Errorf(awserrors.ErrorECSClientException, "%s is not a valid value for include. The value must be one of [%s]", v, includeOneOf(tagsIncludeValues))
	}
	return nil
}
