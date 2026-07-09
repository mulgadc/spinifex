package gateway_ec2_instance

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_placementgroup "github.com/mulgadc/spinifex/spinifex/handlers/ec2/placementgroup"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// PassRoleChecker enforces iam:PassRole on the role inside an instance profile.
// Defined as a callback to avoid an import cycle with the gateway package.
type PassRoleChecker func(roleARN string) error

// LaunchQuotaChecker enforces any gateway-side quota that must run after input
// validation and PassRole authorization, but before the launch is dispatched.
type LaunchQuotaChecker func() error

type RunInstancesResponse struct {
	Reservation *ec2.Reservation `locationName:"RunInstancesResponse"`
}

func ValidateRunInstancesInput(input *ec2.RunInstancesInput) (err error) {
	if input == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if input.MinCount == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if *input.MinCount == 0 {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if input.MaxCount == nil {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if *input.MaxCount == 0 {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if *input.MinCount > *input.MaxCount {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if input.ImageId == nil || *input.ImageId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if input.InstanceType == nil || *input.InstanceType == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if !strings.HasPrefix(*input.ImageId, "ami-") {
		return errors.New(awserrors.ErrorInvalidAMIIDMalformed)
	}

	return err
}

// RunInstances validates input, resolves any IAM instance profile (normalising
// to ARN for daemons), enforces iam:PassRole, then dispatches via NATS.
// When ClientToken is set, wraps the launch in idempotency via the KV store.
// iamSvc may be nil only when no IamInstanceProfile is supplied.
// passRoleCheck may be nil to skip PassRole enforcement.
// launchQuotaCheck may be nil to skip quota enforcement; when present it runs
// after validation and PassRole authorization, but before any launch dispatch.
func RunInstances(ctx context.Context, input *ec2.RunInstancesInput, natsConn *nats.Conn, iamSvc handlers_iam.IAMService, accountID string, passRoleCheck PassRoleChecker, launchQuotaCheck LaunchQuotaChecker, expectedNodes int) (reservation ec2.Reservation, err error) {
	if err = ValidateRunInstancesInput(input); err != nil {
		return reservation, err
	}

	token := aws.StringValue(input.ClientToken)
	if token == "" {
		return runInstancesInner(ctx, input, natsConn, iamSvc, accountID, passRoleCheck, launchQuotaCheck, expectedNodes)
	}

	// ClientToken set: dedup concurrent/retried launches; store failure is fatal
	// to avoid a double launch on retry.
	store, serr := getClientTokenStore(ctx, natsConn)
	if serr != nil {
		slog.ErrorContext(ctx, "RunInstances: client-token store unavailable", "err", serr)
		return reservation, errors.New(awserrors.ErrorServerInternal)
	}
	// Hash before any mutation so the same token+params always matches.
	paramHash := clientTokenParamHash(input)
	return runInstancesWithClientToken(store, accountID, token, paramHash, func() (ec2.Reservation, error) {
		return runInstancesInner(ctx, input, natsConn, iamSvc, accountID, passRoleCheck, launchQuotaCheck, expectedNodes)
	})
}

// runInstancesInner performs the actual launch: profile resolution, placement
// routing, and capacity-aware distribution. Wrapped by RunInstances for idempotency.
func runInstancesInner(ctx context.Context, input *ec2.RunInstancesInput, natsConn *nats.Conn, iamSvc handlers_iam.IAMService, accountID string, passRoleCheck PassRoleChecker, launchQuotaCheck LaunchQuotaChecker, expectedNodes int) (reservation ec2.Reservation, err error) {
	resolvedProfile, err := resolveAndAuthorizeInstanceProfile(input, iamSvc, accountID, passRoleCheck)
	if err != nil {
		return reservation, err
	}
	if launchQuotaCheck != nil {
		if err := launchQuotaCheck(); err != nil {
			return reservation, err
		}
	}

	// Targeted capacity-reservation launch routes straight to the owning node's
	// cr-subject. The gateway does input-only checks (malformed id, the PG+CR
	// combination); the owning daemon does the semantic checks (account, type,
	// full) only it can see, and ErrNoResponders surfaces a gone id as NotFound.
	if crID := capacityReservationTargetID(input); crID != "" {
		if !strings.HasPrefix(crID, "cr-") {
			return reservation, errors.New(awserrors.ErrorInvalidCapacityReservationIdMalformed)
		}
		if placementGroupName(input) != "" {
			return reservation, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		reservationPtr, err := runIntoReservation(ctx, input, natsConn, accountID, crID)
		if err != nil {
			return reservation, err
		}
		enrichReservationWithProfileID(reservationPtr, resolvedProfile)
		return *reservationPtr, nil
	}

	groupName := placementGroupName(input)
	if groupName != "" {
		strategy, err := lookupPlacementGroupStrategy(ctx, natsConn, accountID, groupName)
		if err != nil {
			return reservation, err
		}

		switch strategy {
		case ec2.PlacementStrategySpread:
			reservationPtr, err := distributeInstancesSpread(ctx, input, natsConn, accountID, groupName, expectedNodes)
			if err != nil {
				return reservation, err
			}
			enrichReservationWithProfileID(reservationPtr, resolvedProfile)
			return *reservationPtr, nil
		case ec2.PlacementStrategyCluster:
			reservationPtr, err := distributeInstancesCluster(ctx, input, natsConn, accountID, groupName, expectedNodes)
			if err != nil {
				return reservation, err
			}
			enrichReservationWithProfileID(reservationPtr, resolvedProfile)
			return *reservationPtr, nil
		default:
			return reservation, errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}

	reservationPtr, err := distributeInstances(ctx, input, natsConn, accountID, expectedNodes)
	if err != nil {
		// Distinguish "unknown type" from "no capacity" via DescribeInstanceTypes.
		if err.Error() == awserrors.ErrorInsufficientInstanceCapacity {
			if !isKnownInstanceType(ctx, natsConn, *input.InstanceType) {
				return reservation, errors.New(awserrors.ErrorInvalidInstanceType)
			}
		}
		return reservation, err
	}
	enrichReservationWithProfileID(reservationPtr, resolvedProfile)
	return *reservationPtr, nil
}

// resolveAndAuthorizeInstanceProfile resolves an optional instance profile,
// enforces PassRole, and normalises input to the canonical ARN only.
func resolveAndAuthorizeInstanceProfile(input *ec2.RunInstancesInput, iamSvc handlers_iam.IAMService, accountID string, passRoleCheck PassRoleChecker) (*handlers_iam.InstanceProfile, error) {
	if input.IamInstanceProfile == nil {
		return nil, nil
	}
	profile, err := resolveAndAuthorizeProfile(input.IamInstanceProfile, iamSvc, accountID, passRoleCheck)
	if err != nil {
		return nil, err
	}
	input.IamInstanceProfile = &ec2.IamInstanceProfileSpecification{Arn: aws.String(profile.ARN)}
	return profile, nil
}

// profileNameOrARN returns the ARN or Name from the spec (AWS accepts either).
func profileNameOrARN(spec *ec2.IamInstanceProfileSpecification) string {
	if spec == nil {
		return ""
	}
	if arn := aws.StringValue(spec.Arn); arn != "" {
		return arn
	}
	return aws.StringValue(spec.Name)
}

// enrichReservationWithProfileID fills IamInstanceProfile.Id on every instance.
// Daemons emit Arn only (no IAM access); the gateway adds Id from the resolved profile.
func enrichReservationWithProfileID(reservation *ec2.Reservation, profile *handlers_iam.InstanceProfile) {
	if reservation == nil || profile == nil {
		return
	}
	for _, inst := range reservation.Instances {
		if inst == nil {
			continue
		}
		if inst.IamInstanceProfile == nil {
			inst.IamInstanceProfile = &ec2.IamInstanceProfile{}
		}
		if inst.IamInstanceProfile.Arn == nil {
			inst.IamInstanceProfile.Arn = aws.String(profile.ARN)
		}
		inst.IamInstanceProfile.Id = aws.String(profile.InstanceProfileID)
	}
}

// placementGroupName extracts the placement group name from RunInstancesInput.
func placementGroupName(input *ec2.RunInstancesInput) string {
	if input.Placement != nil && input.Placement.GroupName != nil {
		return aws.StringValue(input.Placement.GroupName)
	}
	return ""
}

// capacityReservationTargetID returns the explicit targeted-launch reservation id
// from the input, or "" when the launch is untargeted (general path). Preference
// is ignored — only a present target id routes to the cr-subject.
func capacityReservationTargetID(input *ec2.RunInstancesInput) string {
	spec := input.CapacityReservationSpecification
	if spec == nil || spec.CapacityReservationTarget == nil {
		return ""
	}
	return aws.StringValue(spec.CapacityReservationTarget.CapacityReservationId)
}

// lookupPlacementGroupStrategy returns the strategy of a placement group, or an error if absent/unavailable.
func lookupPlacementGroupStrategy(ctx context.Context, natsConn *nats.Conn, accountID, groupName string) (string, error) {
	pgSvc := handlers_ec2_placementgroup.NewNATSPlacementGroupService(natsConn)
	out, err := pgSvc.DescribePlacementGroups(ctx, &ec2.DescribePlacementGroupsInput{
		GroupNames: []*string{aws.String(groupName)},
	}, accountID)
	if err != nil {
		return "", err
	}
	if len(out.PlacementGroups) == 0 {
		return "", errors.New(awserrors.ErrorInvalidPlacementGroupUnknown)
	}
	pg := out.PlacementGroups[0]
	if pg.State == nil || *pg.State != ec2.PlacementGroupStateAvailable {
		return "", errors.New(awserrors.ErrorInvalidPlacementGroupUnknown)
	}
	return aws.StringValue(pg.Strategy), nil
}

// isKnownInstanceType checks whether any daemon recognizes the given instance type.
func isKnownInstanceType(ctx context.Context, natsConn *nats.Conn, instanceType string) bool {
	result, err := utils.NatsRequest[ec2.DescribeInstanceTypesOutput](ctx, natsConn, "ec2.DescribeInstanceTypes", &ec2.DescribeInstanceTypesInput{}, 3*time.Second, utils.GlobalAccountID)
	if err != nil || result == nil {
		return false
	}
	for _, t := range result.InstanceTypes {
		if t.InstanceType != nil && *t.InstanceType == instanceType {
			return true
		}
	}
	return false
}
