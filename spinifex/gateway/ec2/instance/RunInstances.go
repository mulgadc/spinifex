package gateway_ec2_instance

import (
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

// PassRoleChecker enforces iam:PassRole on the role inside an instance profile
// the caller is trying to attach. Implemented by the gateway via
// checkPolicyResource, defined as a callback to avoid an import cycle with
// the gateway package.
type PassRoleChecker func(roleARN string) error

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

	// Additional validation from EC2 spec
	if *input.MinCount > *input.MaxCount {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	if input.ImageId == nil || *input.ImageId == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if input.InstanceType == nil || *input.InstanceType == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if input.KeyName == nil || *input.KeyName == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}

	if !strings.HasPrefix(*input.ImageId, "ami-") {
		return errors.New(awserrors.ErrorInvalidAMIIDMalformed)
	}

	return err
}

// RunInstances dispatches an EC2 RunInstances request after validating the
// input, resolving any supplied IAM instance profile, and enforcing
// iam:PassRole on the role inside that profile. The resolved profile ARN
// is normalised onto input.IamInstanceProfile.Arn before NATS dispatch so
// the daemon sees a single canonical reference. The returned reservation
// is enriched with the InstanceProfileID so callers see {Arn, Id} per the
// AWS contract.
//
// iamSvc may be nil only in pre-IAM compatibility paths; if input carries an
// IamInstanceProfile reference, iamSvc must be set or the call fails.
// passRoleCheck may be nil to skip the iam:PassRole enforcement (used by
// unit tests that don't exercise the policy path).
func RunInstances(input *ec2.RunInstancesInput, natsConn *nats.Conn, iamSvc handlers_iam.IAMService, accountID string, passRoleCheck PassRoleChecker) (reservation ec2.Reservation, err error) {
	// Validate input
	if err = ValidateRunInstancesInput(input); err != nil {
		return reservation, err
	}

	// No ClientToken ⇒ no idempotency contract; launch directly.
	token := aws.StringValue(input.ClientToken)
	if token == "" {
		return runInstancesInner(input, natsConn, iamSvc, accountID, passRoleCheck)
	}

	// ClientToken set: dedup concurrent/retried launches. The caller asked for
	// idempotency, so a store failure fails the call rather than risking a
	// double launch on their retry.
	store, serr := getClientTokenStore(natsConn)
	if serr != nil {
		slog.Error("RunInstances: client-token store unavailable", "err", serr)
		return reservation, errors.New(awserrors.ErrorServerInternal)
	}
	// Hash the request BEFORE any mutation (resolveAndAuthorizeInstanceProfile
	// rewrites IamInstanceProfile) so the same token+params always matches.
	paramHash := clientTokenParamHash(input)
	replay, owned, cerr := store.Claim(accountID, token, paramHash)
	if cerr != nil {
		if errors.Is(cerr, errIdempotentParamMismatch) {
			return reservation, errors.New(awserrors.ErrorIdempotentParameterMismatch)
		}
		slog.Error("RunInstances: client-token claim failed", "token", token, "err", cerr)
		return reservation, errors.New(awserrors.ErrorServerInternal)
	}
	if replay != nil {
		return *replay, nil
	}
	if !owned {
		return reservation, errors.New(awserrors.ErrorServerInternal)
	}

	res, rerr := runInstancesInner(input, natsConn, iamSvc, accountID, passRoleCheck)
	if rerr != nil {
		store.Abort(accountID, token)
		return reservation, rerr
	}
	if ferr := store.Finalize(accountID, token, paramHash, &res); ferr != nil {
		// Launch succeeded; failing to persist the replay record only weakens a
		// future retry's dedup, so do not fail the response.
		slog.Warn("RunInstances: failed to finalize client-token record", "token", token, "err", ferr)
	}
	return res, nil
}

// runInstancesInner performs the actual launch (profile resolution, placement
// routing, capacity-aware distribution). It is wrapped by RunInstances, which
// layers ClientToken idempotency on top.
func runInstancesInner(input *ec2.RunInstancesInput, natsConn *nats.Conn, iamSvc handlers_iam.IAMService, accountID string, passRoleCheck PassRoleChecker) (reservation ec2.Reservation, err error) {
	resolvedProfile, err := resolveAndAuthorizeInstanceProfile(input, iamSvc, accountID, passRoleCheck)
	if err != nil {
		return reservation, err
	}

	// Placement group routing: when a placement group is specified, validate it
	// and route based on its strategy (spread or cluster).
	groupName := placementGroupName(input)
	if groupName != "" {
		strategy, err := lookupPlacementGroupStrategy(natsConn, accountID, groupName)
		if err != nil {
			return reservation, err
		}

		switch strategy {
		case ec2.PlacementStrategySpread:
			reservationPtr, err := distributeInstancesSpread(input, natsConn, accountID, groupName)
			if err != nil {
				return reservation, err
			}
			enrichReservationWithProfileID(reservationPtr, resolvedProfile)
			return *reservationPtr, nil
		case ec2.PlacementStrategyCluster:
			reservationPtr, err := distributeInstancesCluster(input, natsConn, accountID, groupName)
			if err != nil {
				return reservation, err
			}
			enrichReservationWithProfileID(reservationPtr, resolvedProfile)
			return *reservationPtr, nil
		default:
			return reservation, errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}

	// Capacity-aware routing: query all nodes for capacity and distribute
	// instances across nodes with best-effort spread. This applies to both
	// single-instance (count=1) and batch (count>1) launches, ensuring fair
	// distribution across the cluster.
	reservationPtr, err := distributeInstances(input, natsConn, accountID)
	if err != nil {
		// When no nodes have capacity, distinguish between "unknown instance type"
		// and "all nodes full" by checking DescribeInstanceTypes.
		if err.Error() == awserrors.ErrorInsufficientInstanceCapacity {
			if !isKnownInstanceType(natsConn, *input.InstanceType) {
				return reservation, errors.New(awserrors.ErrorInvalidInstanceType)
			}
		}
		return reservation, err
	}
	enrichReservationWithProfileID(reservationPtr, resolvedProfile)
	return *reservationPtr, nil
}

// resolveAndAuthorizeInstanceProfile resolves and authorizes the optional
// instance profile in RunInstancesInput, normalising the input so the daemon
// sees only the canonical ARN (clearing Name avoids double-resolution if the
// profile is renamed between gateway resolution and daemon launch).
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

// profileNameOrARN extracts whichever field of IamInstanceProfileSpecification
// was set by the caller. AWS accepts either Name or Arn (not both).
func profileNameOrARN(spec *ec2.IamInstanceProfileSpecification) string {
	if spec == nil {
		return ""
	}
	if arn := aws.StringValue(spec.Arn); arn != "" {
		return arn
	}
	return aws.StringValue(spec.Name)
}

// enrichReservationWithProfileID fills in Instance.IamInstanceProfile.Id on
// every instance in the reservation when a profile was resolved at the
// gateway. Daemons emit only Arn (they have no IAM access); the gateway
// adds Id from the already-resolved profile to match the AWS response shape.
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

// lookupPlacementGroupStrategy validates that a placement group exists and returns its strategy.
func lookupPlacementGroupStrategy(natsConn *nats.Conn, accountID, groupName string) (string, error) {
	pgSvc := handlers_ec2_placementgroup.NewNATSPlacementGroupService(natsConn)
	out, err := pgSvc.DescribePlacementGroups(&ec2.DescribePlacementGroupsInput{
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
func isKnownInstanceType(natsConn *nats.Conn, instanceType string) bool {
	result, err := utils.NATSRequest[ec2.DescribeInstanceTypesOutput](
		natsConn, "ec2.DescribeInstanceTypes", &ec2.DescribeInstanceTypesInput{}, 3*time.Second, utils.GlobalAccountID)
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
