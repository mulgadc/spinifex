// Package gateway_ec2_spotinstance provides the gateway-side orchestration for
// AWS-compatible Spot Instance Requests. Fulfilment is a mock over the existing
// on-demand RunInstances path: a request synchronously launches real VMs and is
// then reported as active/fulfilled. There is no bidding, interruption, or
// reclamation.
package gateway_ec2_spotinstance

import (
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ec2_instance "github.com/mulgadc/spinifex/spinifex/gateway/ec2/instance"
	handlers_ec2_spotinstance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/spotinstance"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_quota "github.com/mulgadc/spinifex/spinifex/handlers/quota"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// ValidateRequestSpotInstancesInput enforces the required launch specification
// fields and the few constrained scalar parameters. Defaults (InstanceCount 1,
// Type one-time) are applied later and are not validation failures when absent.
func ValidateRequestSpotInstancesInput(input *ec2.RequestSpotInstancesInput) error {
	if input == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	spec := input.LaunchSpecification
	if spec == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if aws.StringValue(spec.ImageId) == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if !strings.HasPrefix(aws.StringValue(spec.ImageId), "ami-") {
		return errors.New(awserrors.ErrorInvalidAMIIDMalformed)
	}
	if aws.StringValue(spec.InstanceType) == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if aws.StringValue(spec.KeyName) == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if input.InstanceCount != nil && *input.InstanceCount < 1 {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	switch aws.StringValue(input.Type) {
	case "", ec2.SpotInstanceTypeOneTime, ec2.SpotInstanceTypePersistent:
	default:
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	return nil
}

// RequestSpotInstances validates the request, launches real VMs via the shared
// on-demand RunInstances path (all-or-nothing: MinCount=MaxCount=InstanceCount,
// ClientToken passed through, same vCPU quota gate), then builds and persists one
// active/fulfilled SpotInstanceRequest per launched instance. On a launch failure
// (including InsufficientInstanceCapacity) it returns the error and persists nothing.
func RequestSpotInstances(input *ec2.RequestSpotInstancesInput, natsConn *nats.Conn, iamSvc handlers_iam.IAMService, accountID, az string, passRoleCheck gateway_ec2_instance.PassRoleChecker, quota *handlers_quota.Service, expectedNodes int) (ec2.RequestSpotInstancesOutput, error) {
	var output ec2.RequestSpotInstancesOutput

	if err := ValidateRequestSpotInstancesInput(input); err != nil {
		return output, err
	}

	count := spotInstanceCount(input)
	runInput := runInputFromLaunchSpec(input, count)

	// Gate on the per-account vCPU cap exactly like the on-demand path; spot
	// launches are real VMs and must not slip past the quota.
	launchQuotaCheck := func() error {
		return quota.EnforceLaunch(accountID, aws.StringValue(runInput.InstanceType), int(count))
	}

	// RunInstances normalises runInput in place (e.g. instance profile to ARN),
	// so the launch spec echoed back is built from runInput afterwards.
	reservation, err := gateway_ec2_instance.RunInstances(runInput, natsConn, iamSvc, accountID, passRoleCheck, launchQuotaCheck, expectedNodes)
	if err != nil {
		return output, err
	}

	// Charge the actual launched vCPUs; a counter write failure is drift for
	// reconcile to correct, so it must not fail the already-successful launch.
	if err := quota.ChargeLaunch(accountID, &reservation); err != nil {
		slog.Warn("RequestSpotInstances: vcpu quota charge failed, reconcile will correct", "account", accountID, "err", err)
	}

	requests := buildSpotRequests(input, runInput, reservation.Instances, az)

	svc := handlers_ec2_spotinstance.NewNATSSpotInstanceService(natsConn)
	if _, err := svc.PutSpotInstanceRequests(&handlers_ec2_spotinstance.PutSpotRequestsInput{Requests: requests}, accountID); err != nil {
		slog.Error("RequestSpotInstances: VMs launched but SIR persist failed", "count", count, "accountID", accountID, "err", err)
		return output, err
	}

	output.SpotInstanceRequests = requests

	// Stamp spot lineage onto each launched VM in the background: it is
	// best-effort (a miss only drops the projection, never the request), so it
	// must not block the response.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("RequestSpotInstances: spot lineage write-back panic", "panic", r)
			}
		}()
		stampSpotLineage(natsConn, requests, accountID)
	}()

	return output, nil
}

// spotInstanceCount returns the requested instance count, defaulting to 1.
func spotInstanceCount(input *ec2.RequestSpotInstancesInput) int64 {
	if input.InstanceCount != nil && *input.InstanceCount >= 1 {
		return *input.InstanceCount
	}
	return 1
}

// runInputFromLaunchSpec translates a spot launch specification into a
// RunInstancesInput. MinCount=MaxCount=count maps the single spot InstanceCount
// to all-or-nothing fulfilment, and ClientToken is passed through for idempotency.
func runInputFromLaunchSpec(input *ec2.RequestSpotInstancesInput, count int64) *ec2.RunInstancesInput {
	spec := input.LaunchSpecification
	runInput := &ec2.RunInstancesInput{
		MinCount:            aws.Int64(count),
		MaxCount:            aws.Int64(count),
		ImageId:             spec.ImageId,
		InstanceType:        spec.InstanceType,
		KeyName:             spec.KeyName,
		SubnetId:            spec.SubnetId,
		SecurityGroupIds:    spec.SecurityGroupIds,
		UserData:            spec.UserData,
		BlockDeviceMappings: spec.BlockDeviceMappings,
		IamInstanceProfile:  spec.IamInstanceProfile,
		NetworkInterfaces:   spec.NetworkInterfaces,
		ClientToken:         input.ClientToken,
	}
	if spec.Placement != nil && aws.StringValue(spec.Placement.GroupName) != "" {
		runInput.Placement = &ec2.Placement{GroupName: spec.Placement.GroupName}
	}
	return runInput
}

// launchSpecFromRunInput builds the SpotInstanceRequest launch specification
// echoed in Describe responses from the resolved RunInstancesInput. The launch
// input carries security groups as IDs; the read-side spec carries GroupIdentifiers.
func launchSpecFromRunInput(runInput *ec2.RunInstancesInput) *ec2.LaunchSpecification {
	spec := &ec2.LaunchSpecification{
		ImageId:             runInput.ImageId,
		InstanceType:        runInput.InstanceType,
		KeyName:             runInput.KeyName,
		SubnetId:            runInput.SubnetId,
		UserData:            runInput.UserData,
		BlockDeviceMappings: runInput.BlockDeviceMappings,
		IamInstanceProfile:  runInput.IamInstanceProfile,
		NetworkInterfaces:   runInput.NetworkInterfaces,
	}
	for _, sgID := range runInput.SecurityGroupIds {
		spec.SecurityGroups = append(spec.SecurityGroups, &ec2.GroupIdentifier{GroupId: sgID})
	}
	if runInput.Placement != nil && aws.StringValue(runInput.Placement.GroupName) != "" {
		spec.Placement = &ec2.SpotPlacement{GroupName: runInput.Placement.GroupName}
	}
	return spec
}

// buildSpotRequests constructs one active/fulfilled SpotInstanceRequest per
// launched instance, mapping instances to requests by order. Creation-time tags
// come from the spot-instances-request TagSpecification.
func buildSpotRequests(input *ec2.RequestSpotInstancesInput, runInput *ec2.RunInstancesInput, instances []*ec2.Instance, az string) []*ec2.SpotInstanceRequest {
	count := spotInstanceCount(input)

	spotType := aws.StringValue(input.Type)
	if spotType == "" {
		spotType = ec2.SpotInstanceTypeOneTime
	}

	tags := utils.MapToEC2Tags(utils.ExtractTags(input.TagSpecifications, ec2.ResourceTypeSpotInstancesRequest))
	launchSpec := launchSpecFromRunInput(runInput)
	now := time.Now().UTC()

	requests := make([]*ec2.SpotInstanceRequest, 0, count)
	for range count {
		req := &ec2.SpotInstanceRequest{
			SpotInstanceRequestId:    aws.String(utils.GenerateResourceID("sir")),
			State:                    aws.String(ec2.SpotInstanceStateActive),
			Type:                     aws.String(spotType),
			ProductDescription:       aws.String(ec2.RIProductDescriptionLinuxUnix),
			LaunchedAvailabilityZone: aws.String(az),
			CreateTime:               aws.Time(now),
			LaunchSpecification:      launchSpec,
			Tags:                     tags,
			Status: &ec2.SpotInstanceStatus{
				Code:       aws.String(handlers_ec2_spotinstance.SpotStatusCodeFulfilled),
				Message:    aws.String("Your Spot request is fulfilled."),
				UpdateTime: aws.Time(now),
			},
		}
		if input.SpotPrice != nil {
			req.SpotPrice = input.SpotPrice
		}
		requests = append(requests, req)
	}

	// Map launched instances to requests by order. All-or-nothing fulfilment
	// makes len(instances) == count on success; the guard tolerates any short read.
	for i, inst := range instances {
		if i >= len(requests) {
			break
		}
		if inst != nil {
			requests[i].InstanceId = inst.InstanceId
		}
	}
	return requests
}
