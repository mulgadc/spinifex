// Package spotinstance provides the gateway-side orchestration for
// AWS-compatible Spot Instance Requests. Fulfilment is a mock over the existing
// on-demand RunInstances path: a request synchronously launches real VMs and is
// then reported as active/fulfilled. There is no bidding, interruption, or
// reclamation.
package spotinstance

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2idemapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/idem"
	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	ec2spotinstance "github.com/mulgadc/spinifex/spinifex/domains/ec2/spotinstance"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	awstags "github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
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
	if input.InstanceCount != nil && *input.InstanceCount < 1 {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	return validateSpotType(aws.StringValue(input.Type))
}

// RequestSpotInstances validates the request, launches real VMs via the shared
// on-demand RunInstances path (all-or-nothing: MinCount=MaxCount=InstanceCount,
// ClientToken passed through, same vCPU quota gate), then builds and persists one
// active/fulfilled SpotInstanceRequest per launched instance. On a launch failure
// (including InsufficientInstanceCapacity) it returns the error and persists nothing.
func RequestSpotInstances(ctx context.Context, input *ec2.RequestSpotInstancesInput, natsConn *nats.Conn, tokenBucket ec2idemapi.Bucket, iamSvc ec2instanceapi.InstanceProfiles, accountID, az string, passRoleCheck ec2instanceapi.PassRoleChecker, quota LaunchQuota, expectedNodes int) (ec2.RequestSpotInstancesOutput, error) {
	var output ec2.RequestSpotInstancesOutput

	if err := ValidateRequestSpotInstancesInput(input); err != nil {
		return output, err
	}

	count := spotInstanceCount(input)
	runInput := runInputFromLaunchSpec(input, count)
	spotType := aws.StringValue(input.Type)
	tags := awstags.Extract(input.TagSpecifications, ec2.ResourceTypeSpotInstancesRequest)

	_, requests, err := launchSpot(ctx, runInput, spotType, input.SpotPrice, tags, natsConn, tokenBucket, iamSvc, accountID, az, passRoleCheck, quota, expectedNodes)
	if err != nil {
		return output, err
	}
	output.SpotInstanceRequests = requests
	return output, nil
}

// IsSpotLaunch reports whether a RunInstances request asks for the spot market.
func IsSpotLaunch(input *ec2.RunInstancesInput) bool {
	return input != nil && input.InstanceMarketOptions != nil &&
		aws.StringValue(input.InstanceMarketOptions.MarketType) == ec2.MarketTypeSpot
}

// RunSpotInstances serves RunInstances with MarketType=spot: it launches the
// instances like RequestSpotInstances, backs each with a spot request, and
// returns the reservation with the spot lineage set on each instance.
func RunSpotInstances(ctx context.Context, input *ec2.RunInstancesInput, natsConn *nats.Conn, tokenBucket ec2idemapi.Bucket, iamSvc ec2instanceapi.InstanceProfiles, accountID, az string, passRoleCheck ec2instanceapi.PassRoleChecker, quota LaunchQuota, expectedNodes int) (ec2.Reservation, error) {
	var spotType string
	var spotPrice *string
	if opts := input.InstanceMarketOptions.SpotOptions; opts != nil {
		spotType = aws.StringValue(opts.SpotInstanceType)
		spotPrice = opts.MaxPrice
	}
	if err := validateSpotType(spotType); err != nil {
		return ec2.Reservation{}, err
	}
	tags := awstags.Extract(input.TagSpecifications, ec2.ResourceTypeSpotInstancesRequest)

	reservation, requests, err := launchSpot(ctx, input, spotType, spotPrice, tags, natsConn, tokenBucket, iamSvc, accountID, az, passRoleCheck, quota, expectedNodes)
	if err != nil {
		return reservation, err
	}
	sirByInstance := make(map[string]*string, len(requests))
	for _, req := range requests {
		sirByInstance[aws.StringValue(req.InstanceId)] = req.SpotInstanceRequestId
	}
	for _, inst := range reservation.Instances {
		if inst == nil {
			continue
		}
		inst.InstanceLifecycle = aws.String(ec2.InstanceLifecycleTypeSpot)
		inst.SpotInstanceRequestId = sirByInstance[aws.StringValue(inst.InstanceId)]
	}
	return reservation, nil
}

// validateSpotType accepts the AWS spot request types; empty means one-time.
func validateSpotType(spotType string) error {
	switch spotType {
	case "", ec2.SpotInstanceTypeOneTime, ec2.SpotInstanceTypePersistent:
		return nil
	}
	return errors.New(awserrors.ErrorInvalidParameterValue)
}

// launchSpot launches runInput on the on-demand path under the vCPU quota, then
// persists one active/fulfilled spot request per launched instance. On a launch
// or persist failure it returns the error.
func launchSpot(ctx context.Context, runInput *ec2.RunInstancesInput, spotType string, spotPrice *string, tags map[string]string, natsConn *nats.Conn, tokenBucket ec2idemapi.Bucket, iamSvc ec2instanceapi.InstanceProfiles, accountID, az string, passRoleCheck ec2instanceapi.PassRoleChecker, quota LaunchQuota, expectedNodes int) (ec2.Reservation, []*ec2.SpotInstanceRequest, error) {
	// Gate on the per-account vCPU cap exactly like the on-demand path; spot
	// launches are real VMs and must not slip past the quota.
	launchQuotaCheck := func() error {
		return quota.EnforceLaunch(ctx, accountID, aws.StringValue(runInput.InstanceType), int(aws.Int64Value(runInput.MaxCount)))
	}

	// RunInstances normalises runInput in place (e.g. instance profile to ARN),
	// so the launch spec echoed back is built from runInput afterwards.
	reservation, err := ec2instanceapi.RunInstances(ctx, runInput, natsConn, tokenBucket, iamSvc, accountID, passRoleCheck, launchQuotaCheck, expectedNodes)
	if err != nil {
		return reservation, nil, err
	}

	// Charge the actual launched vCPUs; a counter write failure is drift for
	// reconcile to correct, so it must not fail the already-successful launch.
	if err := quota.ChargeLaunch(ctx, accountID, &reservation); err != nil {
		slog.WarnContext(ctx, "spot launch: vcpu quota charge failed, reconcile will correct", "account", accountID, "err", err)
	}

	requests := buildSpotRequests(runInput, reservation.Instances, az, spotType, spotPrice, tags)

	svc := ec2spotinstance.NewNATSSpotInstanceService(natsConn)
	if _, err := svc.PutSpotInstanceRequests(ctx, &ec2spotinstance.PutSpotRequestsInput{Requests: requests}, accountID); err != nil {
		slog.ErrorContext(ctx, "spot launch: VMs launched but SIR persist failed", "count", len(requests), "accountID", accountID, "err", err)
		return ec2.Reservation{}, nil, err
	}

	// Stamp spot lineage onto each launched VM in the background: it is
	// best-effort (a miss only drops the projection, never the request), so it
	// must not block the response.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.ErrorContext(ctx, "spot launch: spot lineage write-back panic", "panic", r)
			}
		}()
		stampSpotLineage(ctx, natsConn, requests, accountID)
	}()

	return reservation, requests, nil
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
// launched instance, in instance order. An empty spotType means one-time, and
// tags are the spot-instances-request creation-time tags.
func buildSpotRequests(runInput *ec2.RunInstancesInput, instances []*ec2.Instance, az, spotType string, spotPrice *string, tags map[string]string) []*ec2.SpotInstanceRequest {
	if spotType == "" {
		spotType = ec2.SpotInstanceTypeOneTime
	}

	launchSpec := launchSpecFromRunInput(runInput)
	now := time.Now().UTC()

	requests := make([]*ec2.SpotInstanceRequest, 0, len(instances))
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		req := &ec2.SpotInstanceRequest{
			SpotInstanceRequestId:    aws.String(awsidentifiers.GenerateResourceID("sir")),
			State:                    aws.String(ec2.SpotInstanceStateActive),
			Type:                     aws.String(spotType),
			ProductDescription:       aws.String(ec2.RIProductDescriptionLinuxUnix),
			LaunchedAvailabilityZone: aws.String(az),
			CreateTime:               aws.Time(now),
			LaunchSpecification:      launchSpec,
			InstanceId:               inst.InstanceId,
			Tags:                     awstags.MapToEC2(tags),
			Status: &ec2.SpotInstanceStatus{
				Code:       aws.String(ec2spotinstance.SpotStatusCodeFulfilled),
				Message:    aws.String("Your Spot request is fulfilled."),
				UpdateTime: aws.Time(now),
			},
		}
		if spotPrice != nil {
			req.SpotPrice = spotPrice
		}
		requests = append(requests, req)
	}

	return requests
}

// LaunchQuota is the vCPU quota a spot request launches under.
type LaunchQuota interface {
	EnforceLaunch(ctx context.Context, accountID, instanceType string, maxCount int) error
	ChargeLaunch(ctx context.Context, accountID string, reservation *ec2.Reservation) error
}
