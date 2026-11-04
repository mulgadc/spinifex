package awsapi

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// Enum sets the List* operations accept, in the order AWS lists them in its
// refusal. launchType also accepts FMI, which AWS takes but the model omits.
var (
	listLaunchTypes             = []string{"EC2", "FARGATE", "EXTERNAL", "FMI", "MANAGED_INSTANCES"}
	listSchedulingStrategies    = []string{"REPLICA", "DAEMON"}
	listResourceManagementTypes = []string{"CUSTOMER", "ECS"}
	listSortOrders              = []string{"ASC", "DESC"}
	listTaskDefinitionStatuses  = []string{"ACTIVE", "INACTIVE", "DELETE_IN_PROGRESS"}
	listDesiredStatuses         = []string{"RUNNING", "PENDING", "STOPPED"}
)

// checkListEnum refuses a set value outside allowed, naming the member as
// AWS's message does. An omitted value is left to the operation's default.
func checkListEnum(v *string, label string, allowed []string) error {
	if v == nil || slices.Contains(allowed, *v) {
		return nil
	}
	return awserrors.Errorf(awserrors.ErrorECSInvalidParameter, "%s should be one of [%s]", label, strings.Join(allowed, ","))
}

func checkListServices(body []byte, input *ecs.ListServicesInput) error {
	if err := checkListEnum(input.LaunchType, "launch type", listLaunchTypes); err != nil {
		return err
	}
	if err := checkListEnum(input.SchedulingStrategy, "Scheduling strategy", listSchedulingStrategies); err != nil {
		return err
	}
	// The pinned SDK's ListServicesInput predates resourceManagementType, so it
	// is read off the raw body; a body that fails to parse was already refused.
	var raw struct {
		ResourceManagementType *string `json:"resourceManagementType"`
	}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &raw)
	}
	return checkListEnum(raw.ResourceManagementType, "Resource management type", listResourceManagementTypes)
}

func checkListTaskDefinitions(input *ecs.ListTaskDefinitionsInput) error {
	if err := checkListEnum(input.Sort, "Sort", listSortOrders); err != nil {
		return err
	}
	return checkListEnum(input.Status, "Status", listTaskDefinitionStatuses)
}

func checkListTasks(input *ecs.ListTasksInput) error {
	if err := checkListEnum(input.DesiredStatus, "Desired Status", listDesiredStatuses); err != nil {
		return err
	}
	return checkListEnum(input.LaunchType, "launch type", listLaunchTypes)
}
