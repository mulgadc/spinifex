package handlers_ecs

import (
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// Task network modes (AWS ECS enum). EC2 launch type defaults to bridge when a
// task definition omits networkMode.
const (
	NetworkModeAwsvpc = "awsvpc"
	NetworkModeBridge = "bridge"
	NetworkModeHost   = "host"
	NetworkModeNone   = "none"

	defaultNetworkMode = NetworkModeBridge
)

// awsvpcConfig is the resolved subnet + security-group selection for an awsvpc
// task, extracted from RunTask's networkConfiguration.
type awsvpcConfig struct {
	Subnets        []string
	SecurityGroups []string
}

// resolveNetworkMode normalises a task definition's networkMode, defaulting to
// bridge when unset (AWS EC2-launch-type behaviour).
func resolveNetworkMode(td *TaskDefRecord) string {
	mode := strings.ToLower(strings.TrimSpace(td.NetworkMode))
	switch mode {
	case NetworkModeAwsvpc, NetworkModeBridge, NetworkModeHost, NetworkModeNone:
		return mode
	default:
		return defaultNetworkMode
	}
}

// parseAwsvpcConfig pulls the awsvpcConfiguration off a RunTask input. It returns
// an error only when the mode is awsvpc and no usable subnet is supplied; for
// other modes it returns an empty config and nil.
func parseAwsvpcConfig(input *ecs.RunTaskInput, mode string) (awsvpcConfig, error) {
	var cfg awsvpcConfig
	if input != nil && input.NetworkConfiguration != nil && input.NetworkConfiguration.AwsvpcConfiguration != nil {
		v := input.NetworkConfiguration.AwsvpcConfiguration
		cfg.Subnets = awsStringSlice(v.Subnets)
		cfg.SecurityGroups = awsStringSlice(v.SecurityGroups)
	}
	if mode == NetworkModeAwsvpc && len(cfg.Subnets) == 0 {
		return cfg, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return cfg, nil
}

// firstSubnet returns the subnet the task ENI is allocated in. v1 takes the
// first supplied subnet; AZ-affinity to the placed instance is a future refinement.
func (c awsvpcConfig) firstSubnet() string {
	if len(c.Subnets) == 0 {
		return ""
	}
	return c.Subnets[0]
}

// awsStringSliceFromConfig adapts the config security groups to the *string slice
// shape the EC2 CreateNetworkInterface input expects.
func (c awsvpcConfig) securityGroupPtrs() []*string {
	if len(c.SecurityGroups) == 0 {
		return nil
	}
	out := make([]*string, 0, len(c.SecurityGroups))
	for _, g := range c.SecurityGroups {
		out = append(out, aws.String(g))
	}
	return out
}
