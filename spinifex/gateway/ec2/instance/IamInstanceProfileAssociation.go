package gateway_ec2_instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	spxtypes "github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// fanOutTimeout bounds the wait for each broadcast on the
// ec2.IamProfileAssociation.* subjects. Daemons always reply (JSON null on
// no-match), so under healthy conditions the expectedNodes collector exits
// well before the timeout — the deadline only matters when a daemon is down.
const fanOutTimeout = 3 * time.Second

// resolveAndAuthorizeProfile resolves a profile reference (name or ARN) and
// enforces iam:PassRole on the underlying role. Returns the resolved profile
// for the caller to use when building the response. Mirrors the gateway-side
// contract used by RunInstances — profile-with-no-role skips PassRole, missing
// profile maps to InvalidIamInstanceProfile.NotFound, denied PassRole returns
// AccessDenied.
func resolveAndAuthorizeProfile(spec *ec2.IamInstanceProfileSpecification, iamSvc handlers_iam.IAMService, accountID string, passRoleCheck PassRoleChecker) (*handlers_iam.InstanceProfile, error) {
	nameOrARN := profileNameOrARN(spec)
	if nameOrARN == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if iamSvc == nil {
		slog.Error("IAM service not available, cannot resolve instance profile", "nameOrARN", nameOrARN)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	profile, err := iamSvc.ResolveInstanceProfile(accountID, nameOrARN)
	if err != nil {
		if err.Error() == awserrors.ErrorIAMNoSuchEntity {
			return nil, errors.New(awserrors.ErrorInvalidIamInstanceProfileNotFound)
		}
		return nil, err
	}
	if profile.RoleName != "" && passRoleCheck != nil {
		roleARN := fmt.Sprintf("arn:aws:iam::%s:role/%s", profile.AccountID, profile.RoleName)
		if err := passRoleCheck(roleARN); err != nil {
			return nil, err
		}
	}
	return profile, nil
}

// AssociateIamInstanceProfile attaches an instance profile to a running
// instance. The gateway resolves the profile and enforces iam:PassRole; the
// owning daemon validates that the instance currently has no profile (returns
// IamInstanceProfileAlreadyAssociated otherwise), generates a fresh
// iip-assoc-… ID, and writes both fields atomically on vm.VM.
func AssociateIamInstanceProfile(input *ec2.AssociateIamInstanceProfileInput, natsConn *nats.Conn, iamSvc handlers_iam.IAMService, accountID string, passRoleCheck PassRoleChecker) (*ec2.AssociateIamInstanceProfileOutput, error) {
	if input == nil || input.InstanceId == nil || *input.InstanceId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.IamInstanceProfile == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	profile, err := resolveAndAuthorizeProfile(input.IamInstanceProfile, iamSvc, accountID, passRoleCheck)
	if err != nil {
		return nil, err
	}

	command := spxtypes.EC2InstanceCommand{
		ID: *input.InstanceId,
		Attributes: spxtypes.EC2CommandAttributes{
			AssociateIamInstanceProfile: true,
		},
		IamProfileAssociationData: &spxtypes.IamProfileAssociationData{
			InstanceProfileArn: profile.ARN,
		},
	}

	subject := fmt.Sprintf("ec2.cmd.%s", *input.InstanceId)
	assoc, err := utils.NATSRequest[ec2.IamInstanceProfileAssociation](
		natsConn, subject, command, fanOutTimeout, accountID)
	if err != nil {
		if errors.Is(err, nats.ErrNoResponders) {
			return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
		}
		return nil, err
	}

	enrichProfileID(assoc, profile)
	return &ec2.AssociateIamInstanceProfileOutput{IamInstanceProfileAssociation: assoc}, nil
}

// DisassociateIamInstanceProfile detaches the profile referenced by
// AssociationId. The gateway broadcasts to all daemons; the owner mutates and
// returns the populated association. All-null means no live instance carries
// that ID.
func DisassociateIamInstanceProfile(input *ec2.DisassociateIamInstanceProfileInput, natsConn *nats.Conn, expectedNodes int, accountID string) (*ec2.DisassociateIamInstanceProfileOutput, error) {
	if input == nil || input.AssociationId == nil || *input.AssociationId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	assoc, err := broadcastForAssociation(natsConn, "ec2.IamProfileAssociation.disassociate", input, expectedNodes, accountID)
	if err != nil {
		return nil, err
	}
	return &ec2.DisassociateIamInstanceProfileOutput{IamInstanceProfileAssociation: assoc}, nil
}

// ReplaceIamInstanceProfileAssociation swaps the profile referenced by an
// existing AssociationId for a new profile. The gateway resolves the new
// profile and enforces iam:PassRole on its role; the owning daemon validates
// the old AssociationId and generates a new one atomically with the swap.
func ReplaceIamInstanceProfileAssociation(input *ec2.ReplaceIamInstanceProfileAssociationInput, natsConn *nats.Conn, iamSvc handlers_iam.IAMService, expectedNodes int, accountID string, passRoleCheck PassRoleChecker) (*ec2.ReplaceIamInstanceProfileAssociationOutput, error) {
	if input == nil || input.AssociationId == nil || *input.AssociationId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if input.IamInstanceProfile == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	profile, err := resolveAndAuthorizeProfile(input.IamInstanceProfile, iamSvc, accountID, passRoleCheck)
	if err != nil {
		return nil, err
	}

	// Normalise the on-wire payload to the resolved canonical ARN — daemons
	// don't have IAM access and can't dereference a Name reference.
	wireInput := &ec2.ReplaceIamInstanceProfileAssociationInput{
		AssociationId:      input.AssociationId,
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Arn: aws.String(profile.ARN)},
	}

	assoc, err := broadcastForAssociation(natsConn, "ec2.IamProfileAssociation.replace", wireInput, expectedNodes, accountID)
	if err != nil {
		return nil, err
	}
	enrichProfileID(assoc, profile)
	return &ec2.ReplaceIamInstanceProfileAssociationOutput{IamInstanceProfileAssociation: assoc}, nil
}

// CountInstanceProfileAssociations returns the number of live associations
// across all daemons in the caller's account that currently reference
// profileARN. The IAM DeleteInstanceProfile gateway uses this to refuse delete
// while the profile is still attached to any instance. Cross-account records
// are not visible to the daemon walker, so the count is naturally scoped.
func CountInstanceProfileAssociations(natsConn *nats.Conn, expectedNodes int, accountID, profileARN string) (int, error) {
	associations, err := broadcastDescribeAssociations(natsConn, &ec2.DescribeIamInstanceProfileAssociationsInput{}, expectedNodes, accountID)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, a := range associations {
		if a != nil && a.IamInstanceProfile != nil && aws.StringValue(a.IamInstanceProfile.Arn) == profileARN {
			count++
		}
	}
	return count, nil
}

// DescribeIamInstanceProfileAssociations aggregates associations across all
// daemons. Filter names are validated at the gateway so bad inputs fail fast
// without a NATS round-trip; the daemons re-parse the input to do the actual
// filtering against their local VM records.
func DescribeIamInstanceProfileAssociations(input *ec2.DescribeIamInstanceProfileAssociationsInput, natsConn *nats.Conn, expectedNodes int, accountID string) (*ec2.DescribeIamInstanceProfileAssociationsOutput, error) {
	for _, f := range input.Filters {
		if f == nil || f.Name == nil {
			continue
		}
		switch *f.Name {
		case "instance-id", "state":
		default:
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}

	associations, err := broadcastDescribeAssociations(natsConn, input, expectedNodes, accountID)
	if err != nil {
		return nil, err
	}
	return &ec2.DescribeIamInstanceProfileAssociationsOutput{IamInstanceProfileAssociations: associations}, nil
}

// enrichProfileID fills in IamInstanceProfile.Id from the gateway-resolved
// profile — daemons cannot resolve Id since they have no IAM access. Safe to
// call with a nil association (no-op).
func enrichProfileID(assoc *ec2.IamInstanceProfileAssociation, profile *handlers_iam.InstanceProfile) {
	if assoc == nil || profile == nil {
		return
	}
	if assoc.IamInstanceProfile == nil {
		assoc.IamInstanceProfile = &ec2.IamInstanceProfile{}
	}
	assoc.IamInstanceProfile.Id = aws.String(profile.InstanceProfileID)
}

// broadcastForAssociation fans out a mutation request to all daemons and
// returns the first populated response. Returns NoSuchAssociation when every
// reachable daemon replied with JSON null.
func broadcastForAssociation(natsConn *nats.Conn, subject string, payload any, expectedNodes int, accountID string) (*ec2.IamInstanceProfileAssociation, error) {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	inbox := nats.NewInbox()
	sub, err := natsConn.SubscribeSync(inbox)
	if err != nil {
		return nil, fmt.Errorf("failed to create inbox: %w", err)
	}
	defer sub.Unsubscribe()

	pubMsg := nats.NewMsg(subject)
	pubMsg.Reply = inbox
	pubMsg.Data = jsonData
	pubMsg.Header.Set(utils.AccountIDHeader, accountID)
	if err := natsConn.PublishMsg(pubMsg); err != nil {
		return nil, fmt.Errorf("failed to publish request: %w", err)
	}

	deadline := time.Now().Add(fanOutTimeout)
	responsesReceived := 0
	if expectedNodes <= 0 {
		expectedNodes = -1
	}

	for time.Now().Before(deadline) {
		if expectedNodes > 0 && responsesReceived >= expectedNodes {
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		msg, err := sub.NextMsg(remaining)
		if err != nil {
			if err == nats.ErrTimeout {
				break
			}
			return nil, fmt.Errorf("fan-out receive error: %w", err)
		}
		responsesReceived++

		if errPayload, parseErr := utils.ValidateErrorPayload(msg.Data); parseErr != nil {
			// A daemon-side error short-circuits the fan-out — it represents a
			// definitive answer (e.g. IamInstanceProfileAlreadyAssociated would
			// only come from the owning daemon).
			code := awserrors.ErrorServerInternal
			if errPayload.Code != nil {
				code = *errPayload.Code
			}
			return nil, errors.New(code)
		}

		var assoc *ec2.IamInstanceProfileAssociation
		if err := json.Unmarshal(msg.Data, &assoc); err != nil {
			slog.Warn("fan-out: skipping malformed response", "subject", subject, "err", err)
			continue
		}
		if assoc != nil {
			return assoc, nil
		}
	}

	return nil, errors.New(awserrors.ErrorNoSuchAssociation)
}

// broadcastDescribeAssociations fans out a Describe request and concatenates
// every daemon's matching records. Daemons always reply (empty slice when
// no matches), so the expectedNodes collector exits early under healthy
// conditions; a partial collection is acceptable for Describe semantics.
func broadcastDescribeAssociations(natsConn *nats.Conn, input *ec2.DescribeIamInstanceProfileAssociationsInput, expectedNodes int, accountID string) ([]*ec2.IamInstanceProfileAssociation, error) {
	jsonData, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	inbox := nats.NewInbox()
	sub, err := natsConn.SubscribeSync(inbox)
	if err != nil {
		return nil, fmt.Errorf("failed to create inbox: %w", err)
	}
	defer sub.Unsubscribe()

	pubMsg := nats.NewMsg("ec2.IamProfileAssociation.describe")
	pubMsg.Reply = inbox
	pubMsg.Data = jsonData
	pubMsg.Header.Set(utils.AccountIDHeader, accountID)
	if err := natsConn.PublishMsg(pubMsg); err != nil {
		return nil, fmt.Errorf("failed to publish request: %w", err)
	}

	deadline := time.Now().Add(fanOutTimeout)
	responsesReceived := 0
	var associations []*ec2.IamInstanceProfileAssociation
	var clientError string

	if expectedNodes <= 0 {
		expectedNodes = -1
	}

	for time.Now().Before(deadline) {
		if expectedNodes > 0 && responsesReceived >= expectedNodes {
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		msg, err := sub.NextMsg(remaining)
		if err != nil {
			if err == nats.ErrTimeout {
				break
			}
			return nil, fmt.Errorf("fan-out receive error: %w", err)
		}
		responsesReceived++

		if errPayload, parseErr := utils.ValidateErrorPayload(msg.Data); parseErr != nil {
			code := ""
			if errPayload.Code != nil {
				code = *errPayload.Code
			}
			// Capture the first deterministic client error so the caller sees the
			// invalid-filter response instead of an empty success list. 5xx and
			// unknown codes are dropped (treated as transient per-daemon noise)
			// but logged so flaky daemons don't silently corrupt the aggregate
			// — important for CountInstanceProfileAssociations which feeds
			// DeleteInstanceProfile's live-instance gate.
			if clientError == "" && code != "" {
				if info, known := awserrors.ErrorLookup[code]; known && info.HTTPCode >= 400 && info.HTTPCode < 500 {
					clientError = code
					continue
				}
			}
			slog.Warn("Describe fan-out: daemon error dropped from aggregate",
				"subject", "ec2.IamProfileAssociation.describe", "code", code)
			continue
		}

		var resp ec2.DescribeIamInstanceProfileAssociationsOutput
		if err := json.Unmarshal(msg.Data, &resp); err != nil {
			slog.Warn("Describe fan-out: skipping malformed response", "err", err)
			continue
		}
		associations = append(associations, resp.IamInstanceProfileAssociations...)
	}

	if clientError != "" && len(associations) == 0 {
		return nil, errors.New(clientError)
	}
	return associations, nil
}
