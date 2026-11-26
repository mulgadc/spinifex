package elbv2

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// ovnTargetRequest is one requested target: an instance and its explicit
// port, or 0 for the group default.
type ovnTargetRequest struct {
	id   string
	port int64
}

// ovnTargetsShape checks a membership request before any read and returns its
// targets in request order.
func ovnTargetsShape(arn *string, targets []*elbv2.TargetDescription, region, accountID string) ([]ovnTargetRequest, error) {
	if aws.StringValue(arn) == "" || len(targets) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if len(targets) > fixtureMaxTargets {
		return nil, awserrors.Errorf(awserrors.ErrorValidationError, "A request can name at most %d targets", fixtureMaxTargets)
	}
	if err := ovnGroupARN(*arn, region, accountID); err != nil {
		return nil, err
	}
	out := make([]ovnTargetRequest, 0, len(targets))
	for _, t := range targets {
		switch {
		case t == nil || aws.StringValue(t.Id) == "":
			return nil, awserrors.Errorf(awserrors.ErrorValidationError, "A target requires an ID")
		case t.AvailabilityZone != nil:
			return nil, awserrors.Errorf(awserrors.ErrorValidationError, "An availability zone is not supported for an instance target")
		case t.Port != nil && !validFixturePort(*t.Port):
			return nil, awserrors.Errorf(awserrors.ErrorValidationError, "A target port must be between 1 and 65535")
		case !instance.ValidInstanceID(*t.Id):
			return nil, awserrors.Errorf(awserrors.ErrorELBv2InvalidTarget, "The target '%s' is not a valid instance ID", *t.Id)
		}
		out = append(out, ovnTargetRequest{id: *t.Id, port: aws.Int64Value(t.Port)})
	}
	return out, nil
}

// ovnTargetGroupOwner point-reads the exact group and the owner whose listener
// forwards to it. Any other group is refused on the fixture path.
func (s *ELBv2ServiceImpl) ovnTargetGroupOwner(ctx context.Context, op, arn, accountID string) (string, *fixtureOwner, error) {
	tg, err := s.store.GetTargetGroupByArn(ctx, arn)
	if err != nil {
		slog.ErrorContext(ctx, op+": fixture target group read failed", "arn", arn, "err", err)
		return "", nil, errors.New(awserrors.ErrorServerInternal)
	}
	if tg == nil || tg.AccountID != accountID {
		return "", nil, errors.New(awserrors.ErrorELBv2TargetGroupNotFound)
	}
	if tg.Protocol != ProtocolTCP || tg.TargetType != TargetTypeInstance {
		return "", nil, invalidOVNConfiguration("Only a TCP instance target group is supported")
	}
	key, owner, err := s.ovn.owners.groupOwner(ctx, accountID, tg.VpcId, arn)
	if err != nil {
		slog.ErrorContext(ctx, op+": fixture owner read failed", "arn", arn, "err", err)
		return "", nil, errors.New(awserrors.ErrorServerInternal)
	}
	if owner == nil {
		return "", nil, invalidOVNConfiguration("The target group must be the forward target of a load balancer listener")
	}
	return key, owner, nil
}

// ovnTargetTuples resolves each request's port against the listener's group
// default, dropping repeats.
func ovnTargetTuples(reqs []ovnTargetRequest, l *fixtureListener) []fixtureTarget {
	out := make([]fixtureTarget, 0, len(reqs))
	for _, r := range reqs {
		t := fixtureTarget{InstanceID: r.id, Port: cmp.Or(r.port, l.TargetPort)}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// editOVNTargets applies change to the owner's target set. change reports the
// new set and whether it differs, or an error the caller returns as it is.
func (s *ELBv2ServiceImpl) editOVNTargets(ctx context.Context, op, arn, accountID string,
	change func(cur fixtureOwner, tuples []fixtureTarget) ([]fixtureTarget, bool, error), reqs []ovnTargetRequest) error {
	key, owner, err := s.ovnTargetGroupOwner(ctx, op, arn, accountID)
	if err != nil {
		return err
	}
	var refused error
	_, err = s.ovn.owners.Edit(ctx, key, owner.OwnerID, func(cur fixtureOwner) (*fixtureListener, error) {
		refused = nil
		l := cur.Listener
		if l == nil || l.TargetGroupArn != arn {
			refused = invalidOVNConfiguration("The target group must be the forward target of a load balancer listener")
			return nil, refused
		}
		next, changed, cerr := change(cur, ovnTargetTuples(reqs, l))
		if cerr != nil {
			refused = cerr
			return nil, refused
		}
		if !changed {
			return l, nil
		}
		edited := *l
		edited.Targets = next
		return &edited, nil
	})
	switch {
	case refused != nil && errors.Is(err, refused):
		return refused
	case errors.Is(err, errFixtureOwnerLost), errors.Is(err, errFixtureNotSealed):
		return invalidOVNConfiguration("The target group must be the forward target of a load balancer listener")
	case err != nil:
		slog.ErrorContext(ctx, op+": fixture target edit failed", "arn", arn, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	return nil
}

// ovnTargetEligible checks a proposed addition through the EC2 named lookup:
// a visible, running instance of the account with its primary interface in vpcID.
func (s *ELBv2ServiceImpl) ovnTargetEligible(ctx context.Context, accountID, id, vpcID string) error {
	if s.ovn.instances == nil {
		slog.ErrorContext(ctx, "RegisterTargets: fixture has no instance lookup")
		return errors.New(awserrors.ErrorServerInternal)
	}
	n, err := s.ovn.instances.NamedInstance(ctx, accountID, id)
	if err != nil {
		slog.ErrorContext(ctx, "RegisterTargets: fixture instance read failed", "instanceId", id, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	if n == nil || n.InstanceID != id || n.AccountID != accountID || n.State != vm.StateRunning || n.Deleting ||
		n.PrimaryInterfaceID == "" || n.VpcID != vpcID {
		return awserrors.Errorf(awserrors.ErrorELBv2InvalidTarget,
			"The target '%s' is not a running instance in the load balancer's VPC", id)
	}
	return nil
}

// registerOVNFixtureTargets adds the request's missing tuples to the owner's
// set. On a CAS retry the additions and cap are recomputed from the request,
// and every addition is checked before the write.
func (s *ELBv2ServiceImpl) registerOVNFixtureTargets(ctx context.Context, input *elbv2.RegisterTargetsInput, accountID string) (*elbv2.RegisterTargetsOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	reqs, err := ovnTargetsShape(input.TargetGroupArn, input.Targets, s.region, accountID)
	if err != nil {
		return nil, err
	}
	checked := map[string]error{}
	add := func(cur fixtureOwner, tuples []fixtureTarget) ([]fixtureTarget, bool, error) {
		var adds []fixtureTarget
		for _, t := range tuples {
			if !slices.Contains(cur.Listener.Targets, t) {
				adds = append(adds, t)
			}
		}
		if len(adds) == 0 {
			return nil, false, nil
		}
		for _, t := range adds {
			res, ok := checked[t.InstanceID]
			if !ok {
				res = s.ovnTargetEligible(ctx, accountID, t.InstanceID, cur.Root.VpcId)
				checked[t.InstanceID] = res
			}
			if res != nil {
				return nil, false, res
			}
		}
		if len(cur.Listener.Targets)+len(adds) > fixtureMaxTargets {
			return nil, false, invalidOVNConfiguration("A target group of this load balancer can hold at most %d targets", fixtureMaxTargets)
		}
		next := append(slices.Clone(cur.Listener.Targets), adds...)
		slices.SortFunc(next, compareFixtureTargets)
		return next, true, nil
	}
	if err := s.editOVNTargets(ctx, "RegisterTargets", *input.TargetGroupArn, accountID, add, reqs); err != nil {
		return nil, err
	}
	return &elbv2.RegisterTargetsOutput{}, nil
}

// deregisterOVNFixtureTargets removes the request's tuples from the owner's
// set. A missing tuple is a no-op, and no instance is read.
func (s *ELBv2ServiceImpl) deregisterOVNFixtureTargets(ctx context.Context, input *elbv2.DeregisterTargetsInput, accountID string) (*elbv2.DeregisterTargetsOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	reqs, err := ovnTargetsShape(input.TargetGroupArn, input.Targets, s.region, accountID)
	if err != nil {
		return nil, err
	}
	remove := func(cur fixtureOwner, tuples []fixtureTarget) ([]fixtureTarget, bool, error) {
		next := slices.DeleteFunc(slices.Clone(cur.Listener.Targets), func(t fixtureTarget) bool {
			return slices.Contains(tuples, t)
		})
		return next, len(next) != len(cur.Listener.Targets), nil
	}
	if err := s.editOVNTargets(ctx, "DeregisterTargets", *input.TargetGroupArn, accountID, remove, reqs); err != nil {
		return nil, err
	}
	return &elbv2.DeregisterTargetsOutput{}, nil
}
