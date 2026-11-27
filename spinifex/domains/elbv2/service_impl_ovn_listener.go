package elbv2

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	resourcearn "github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
)

// ovnListenerShape checks the raw request against the one supported listener:
// plain TCP on one port with one flat forward action. It returns the port and
// the target group ARN, and reads nothing.
func ovnListenerShape(input *elbv2.CreateListenerInput) (int64, string, error) {
	if input == nil || aws.StringValue(input.LoadBalancerArn) == "" {
		return 0, "", errors.New(awserrors.ErrorMissingParameter)
	}
	if len(input.DefaultActions) == 0 {
		return 0, "", errors.New(awserrors.ErrorMissingParameter)
	}
	if aws.StringValue(input.Protocol) != ProtocolTCP {
		return 0, "", invalidOVNConfiguration("Only the TCP listener protocol is supported")
	}
	if input.Port == nil || !validFixturePort(*input.Port) {
		return 0, "", awserrors.Errorf(awserrors.ErrorValidationError, "Listener port must be between 1 and 65535")
	}
	if len(input.Certificates) > 0 || input.SslPolicy != nil || len(input.AlpnPolicy) > 0 ||
		input.MutualAuthentication != nil || len(input.Tags) > 0 {
		return 0, "", invalidOVNConfiguration("Certificates, SSL policy, ALPN policy, mutual authentication and tags are not supported")
	}
	if len(input.DefaultActions) != 1 {
		return 0, "", invalidOVNConfiguration("Only one default action is supported")
	}
	a := input.DefaultActions[0]
	if a == nil {
		return 0, "", awserrors.Errorf(awserrors.ErrorValidationError, "A default action is empty")
	}
	if aws.StringValue(a.Type) != ActionTypeForward {
		return 0, "", invalidOVNConfiguration("Only the forward action type is supported")
	}
	if a.ForwardConfig != nil || a.FixedResponseConfig != nil || a.RedirectConfig != nil ||
		a.AuthenticateCognitoConfig != nil || a.AuthenticateOidcConfig != nil || a.Order != nil {
		return 0, "", invalidOVNConfiguration("Only a forward action with a target group ARN is supported")
	}
	if aws.StringValue(a.TargetGroupArn) == "" {
		return 0, "", awserrors.Errorf(awserrors.ErrorValidationError, "A forward action requires a target group ARN")
	}
	return *input.Port, *a.TargetGroupArn, nil
}

// ovnGroupIDPattern matches the target group IDs this stack writes: the
// prefixed form CreateTargetGroup generates and the bare hex form.
var ovnGroupIDPattern = regexp.MustCompile(`^[A-Za-z0-9]+(-[A-Za-z0-9]+)*$`)

// ovnGroupARN checks a caller's target group ARN is a well-formed group of the
// account and Region before any read. The ID becomes the group's KV key.
func ovnGroupARN(arn, region, accountID string) error {
	parsed, ok := resourcearn.ParseELBv2(arn)
	if !ok || parsed.Kind != resourcearn.ELBv2TargetGroup || parsed.Region != region || parsed.AccountID != accountID {
		return errors.New(awserrors.ErrorELBv2TargetGroupNotFound)
	}
	cut := strings.LastIndex(parsed.Resource, "/")
	if cut < 1 || !ovnGroupIDPattern.MatchString(parsed.Resource[cut+1:]) {
		return errors.New(awserrors.ErrorELBv2TargetGroupNotFound)
	}
	return nil
}

// ovnListenerGroup point-reads the exact target group and checks it is an
// empty TCP instance group in the root's account and VPC.
func (s *ELBv2ServiceImpl) ovnListenerGroup(ctx context.Context, arn string, root *LoadBalancerRecord) (*TargetGroupRecord, error) {
	if err := ovnGroupARN(arn, s.region, root.AccountID); err != nil {
		return nil, err
	}
	tg, err := s.store.GetTargetGroupByArn(ctx, arn)
	if err != nil {
		slog.ErrorContext(ctx, "CreateListener: fixture target group read failed", "arn", arn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	switch {
	case tg == nil || tg.AccountID != root.AccountID:
		return nil, errors.New(awserrors.ErrorELBv2TargetGroupNotFound)
	case tg.Protocol != ProtocolTCP:
		return nil, awserrors.Errorf(awserrors.ErrorELBv2IncompatibleProtocols, "Only a TCP target group is supported")
	case tg.TargetType != TargetTypeInstance:
		return nil, invalidOVNConfiguration("Only the instance target type is supported")
	case tg.VpcId != root.VpcId:
		return nil, invalidOVNConfiguration("The target group must be in the load balancer's VPC")
	case !validFixturePort(tg.Port):
		return nil, invalidOVNConfiguration("The target group port must be between 1 and 65535")
	case len(tg.Targets) > 0:
		return nil, invalidOVNConfiguration("The target group must have no registered targets")
	}
	return tg, nil
}

// createOVNFixtureListener records the one TCP listener in the sealed owner.
// The same settings return the same listener; nothing else is written.
func (s *ELBv2ServiceImpl) createOVNFixtureListener(ctx context.Context, input *elbv2.CreateListenerInput, accountID string) (*elbv2.CreateListenerOutput, error) {
	port, groupArn, err := ovnListenerShape(input)
	if err != nil {
		return nil, err
	}
	root, err := s.ovn.owners.GetSealedByArn(ctx, *input.LoadBalancerArn, s.region, accountID)
	if err != nil {
		slog.ErrorContext(ctx, "CreateListener: fixture root read failed", "arn", *input.LoadBalancerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	if root == nil {
		return nil, errors.New(awserrors.ErrorELBv2LoadBalancerNotFound)
	}
	tg, err := s.ovnListenerGroup(ctx, groupArn, root)
	if err != nil {
		return nil, err
	}

	want := &fixtureListener{
		ListenerArn: resourcearn.FormatELBv2Listener(s.region, accountID, root.Name, root.LoadBalancerID,
			awsidentifiers.GenerateResourceID("lst"), root.Type),
		Port:           port,
		TargetGroupArn: tg.TargetGroupArn,
		TargetPort:     tg.Port,
	}
	var refused error
	owner, err := s.ovn.owners.Edit(ctx, LBNameKey(root.Name, accountID), root.LoadBalancerID,
		func(cur fixtureOwner) (*fixtureListener, error) {
			refused = nil
			switch l := cur.Listener; {
			case l == nil:
				return want, nil
			case l.Port == port && l.TargetGroupArn == groupArn:
				return l, nil
			case l.Port == port:
				refused = awserrors.Errorf(awserrors.ErrorELBv2DuplicateListener,
					"A listener already exists on this port for this load balancer '%s'", root.LoadBalancerArn)
			default:
				refused = awserrors.Errorf(awserrors.ErrorELBv2TooManyListeners,
					"Load balancer '%s' supports only one listener", root.LoadBalancerArn)
			}
			return nil, refused
		})
	switch {
	case refused != nil && errors.Is(err, refused):
		return nil, refused
	case errors.Is(err, errFixtureOwnerLost), errors.Is(err, errFixtureNotSealed):
		return nil, errors.New(awserrors.ErrorELBv2LoadBalancerNotFound)
	case err != nil:
		slog.ErrorContext(ctx, "CreateListener: fixture listener edit failed", "arn", root.LoadBalancerArn, "err", err)
		return nil, errors.New(awserrors.ErrorServerInternal)
	}
	return &elbv2.CreateListenerOutput{Listeners: []*elbv2.Listener{ovnListenerToSDK(owner)}}, nil
}

func ovnListenerToSDK(owner *fixtureOwner) *elbv2.Listener {
	l := owner.Listener
	return &elbv2.Listener{
		ListenerArn:     aws.String(l.ListenerArn),
		LoadBalancerArn: aws.String(owner.Root.LoadBalancerArn),
		Protocol:        aws.String(ProtocolTCP),
		Port:            aws.Int64(l.Port),
		DefaultActions:  []*elbv2.Action{{Type: aws.String(ActionTypeForward), TargetGroupArn: aws.String(l.TargetGroupArn)}},
	}
}

// refuseOVNFixtureListenerDelete refuses deletion of the listener a sealed
// owner holds. Other ARNs take the existing delete path.
func (s *ELBv2ServiceImpl) refuseOVNFixtureListenerDelete(ctx context.Context, arn, accountID string) error {
	parsed, ok := resourcearn.ParseELBv2(arn)
	if !ok || parsed.Kind != resourcearn.ELBv2Listener || parsed.Region != s.region || parsed.AccountID != accountID {
		return nil
	}
	parts := strings.Split(parsed.Resource, "/")
	if len(parts) != 4 || parts[0] != resourcearn.ELBv2LBPathSegment(LoadBalancerTypeNetwork) || lbNameProblem(parts[1]) != "" {
		return nil
	}
	owner, err := s.ovn.owners.GetSealedOwner(ctx, parts[1], accountID)
	if err != nil {
		slog.ErrorContext(ctx, "DeleteListener: fixture owner read failed", "arn", arn, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if owner != nil && owner.Listener != nil && owner.Listener.ListenerArn == arn {
		return awserrors.Errorf(awserrors.ErrorOperationNotPermitted, "Listener '%s' cannot be deleted yet", arn)
	}
	return nil
}

// refuseOVNFixtureGroupChange keeps target group identity and settings stable
// on the fixture path, where no group change is supported yet.
func refuseOVNFixtureGroupChange(op string) error {
	return awserrors.Errorf(awserrors.ErrorOperationNotPermitted, "%s is not supported on this load balancer path", op)
}
