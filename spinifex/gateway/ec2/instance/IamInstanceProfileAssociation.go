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
// ec2.IamProfileAssociation.* subjects. Daemons always reply (Found=false on
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
	result, err := utils.NATSRequest[spxtypes.IamProfileAssociationResult](
		natsConn, subject, command, fanOutTimeout, accountID)
	if err != nil {
		if errors.Is(err, nats.ErrNoResponders) {
			return nil, errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
		}
		return nil, err
	}

	return &ec2.AssociateIamInstanceProfileOutput{
		IamInstanceProfileAssociation: buildAssociation(result.AssociationId, *input.InstanceId, profile, result.Timestamp),
	}, nil
}

// DisassociateIamInstanceProfile detaches the profile referenced by
// AssociationId. The gateway broadcasts to all daemons; the owner mutates and
// returns Found=true. All-NoOp means no live instance carries that ID.
func DisassociateIamInstanceProfile(input *ec2.DisassociateIamInstanceProfileInput, natsConn *nats.Conn, expectedNodes int, accountID string) (*ec2.DisassociateIamInstanceProfileOutput, error) {
	if input == nil || input.AssociationId == nil || *input.AssociationId == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	req := spxtypes.IamProfileDisassociateRequest{AssociationId: *input.AssociationId}
	result, err := broadcastForAssociation(natsConn, "ec2.IamProfileAssociation.disassociate", req, expectedNodes, accountID)
	if err != nil {
		return nil, err
	}

	// The disassociated state matches AWS behaviour: the association no longer
	// exists, but the response describes what it was for the caller's audit log.
	assoc := &ec2.IamInstanceProfileAssociation{
		AssociationId:      aws.String(result.AssociationId),
		InstanceId:         aws.String(result.InstanceId),
		IamInstanceProfile: &ec2.IamInstanceProfile{Arn: aws.String(result.InstanceProfileArn)},
		State:              aws.String(ec2.IamInstanceProfileAssociationStateDisassociating),
	}
	if !result.Timestamp.IsZero() {
		assoc.Timestamp = aws.Time(result.Timestamp)
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

	req := spxtypes.IamProfileReplaceRequest{
		AssociationId:      *input.AssociationId,
		InstanceProfileArn: profile.ARN,
	}
	result, err := broadcastForAssociation(natsConn, "ec2.IamProfileAssociation.replace", req, expectedNodes, accountID)
	if err != nil {
		return nil, err
	}

	return &ec2.ReplaceIamInstanceProfileAssociationOutput{
		IamInstanceProfileAssociation: buildAssociation(result.AssociationId, result.InstanceId, profile, result.Timestamp),
	}, nil
}

// DescribeIamInstanceProfileAssociations aggregates associations across all
// daemons. Filters are forwarded to the daemons so each daemon only walks
// its own vmMgr once; the gateway then concatenates the results.
func DescribeIamInstanceProfileAssociations(input *ec2.DescribeIamInstanceProfileAssociationsInput, natsConn *nats.Conn, expectedNodes int, accountID string) (*ec2.DescribeIamInstanceProfileAssociationsOutput, error) {
	req := spxtypes.IamProfileDescribeRequest{
		AssociationIds: stringPtrSliceToStrings(input.AssociationIds),
	}
	for _, f := range input.Filters {
		if f == nil || f.Name == nil {
			continue
		}
		values := make([]string, 0, len(f.Values))
		for _, v := range f.Values {
			if v != nil {
				values = append(values, *v)
			}
		}
		switch *f.Name {
		case "instance-id":
			req.InstanceIds = append(req.InstanceIds, values...)
		case "state":
			req.States = append(req.States, values...)
		default:
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}

	records, err := broadcastDescribeAssociations(natsConn, req, expectedNodes, accountID)
	if err != nil {
		return nil, err
	}

	out := &ec2.DescribeIamInstanceProfileAssociationsOutput{}
	for _, r := range records {
		assoc := &ec2.IamInstanceProfileAssociation{
			AssociationId:      aws.String(r.AssociationId),
			InstanceId:         aws.String(r.InstanceId),
			IamInstanceProfile: &ec2.IamInstanceProfile{Arn: aws.String(r.InstanceProfileArn)},
			State:              aws.String(r.State),
		}
		if !r.Timestamp.IsZero() {
			assoc.Timestamp = aws.Time(r.Timestamp)
		}
		out.IamInstanceProfileAssociations = append(out.IamInstanceProfileAssociations, assoc)
	}
	return out, nil
}

// buildAssociation constructs the AWS-facing association response from the
// daemon's mutator result plus the gateway-resolved profile (which carries the
// InstanceProfileID — daemons cannot resolve Id since they have no IAM access).
func buildAssociation(associationID, instanceID string, profile *handlers_iam.InstanceProfile, timestamp time.Time) *ec2.IamInstanceProfileAssociation {
	assoc := &ec2.IamInstanceProfileAssociation{
		AssociationId: aws.String(associationID),
		InstanceId:    aws.String(instanceID),
		IamInstanceProfile: &ec2.IamInstanceProfile{
			Arn: aws.String(profile.ARN),
			Id:  aws.String(profile.InstanceProfileID),
		},
		State: aws.String(ec2.IamInstanceProfileAssociationStateAssociated),
	}
	if !timestamp.IsZero() {
		assoc.Timestamp = aws.Time(timestamp)
	}
	return assoc
}

// broadcastForAssociation fans out a mutation request to all daemons and
// returns the first Found=true response. Returns NoSuchAssociation when every
// reachable daemon replied Found=false.
func broadcastForAssociation(natsConn *nats.Conn, subject string, payload any, expectedNodes int, accountID string) (*spxtypes.IamProfileAssociationResult, error) {
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

		var result spxtypes.IamProfileAssociationResult
		if err := json.Unmarshal(msg.Data, &result); err != nil {
			slog.Warn("fan-out: skipping malformed response", "subject", subject, "err", err)
			continue
		}
		if result.Found {
			return &result, nil
		}
	}

	return nil, errors.New(awserrors.ErrorNoSuchAssociation)
}

// broadcastDescribeAssociations fans out a Describe request and concatenates
// every daemon's matching records. Daemons always reply (empty slice when
// no matches), so the expectedNodes collector exits early under healthy
// conditions; a partial collection is acceptable for Describe semantics.
func broadcastDescribeAssociations(natsConn *nats.Conn, req spxtypes.IamProfileDescribeRequest, expectedNodes int, accountID string) ([]spxtypes.IamProfileAssociationRecord, error) {
	jsonData, err := json.Marshal(req)
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
	var records []spxtypes.IamProfileAssociationRecord
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
			// invalid-filter response instead of an empty success list.
			if clientError == "" && code != "" {
				if info, known := awserrors.ErrorLookup[code]; known && info.HTTPCode >= 400 && info.HTTPCode < 500 {
					clientError = code
				}
			}
			continue
		}

		var resp spxtypes.IamProfileDescribeResponse
		if err := json.Unmarshal(msg.Data, &resp); err != nil {
			slog.Warn("Describe fan-out: skipping malformed response", "err", err)
			continue
		}
		records = append(records, resp.Associations...)
	}

	if clientError != "" && len(records) == 0 {
		return nil, errors.New(clientError)
	}
	return records, nil
}

func stringPtrSliceToStrings(in []*string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p != nil && *p != "" {
			out = append(out, *p)
		}
	}
	return out
}
