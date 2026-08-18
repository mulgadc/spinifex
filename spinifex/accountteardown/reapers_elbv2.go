package accountteardown

import (
	"context"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/nats-io/nats.go"
)

// ELBv2Reapers returns the load balancer reapers in teardown order.
//
// Both sit in the network stage: a load balancer holds subnets and security
// groups, so it has to go before either can be deleted. Target groups follow
// their balancers because a group still referenced by a listener refuses.
func ELBv2Reapers(nc *nats.Conn) []Reaper {
	svc := handlers_elbv2.NewNATSELBv2Service(nc)
	return []Reaper{
		&loadBalancerReaper{svc: svc},
		&targetGroupReaper{svc: svc},
	}
}

type loadBalancerReaper struct {
	svc handlers_elbv2.ELBv2Service
}

func (r *loadBalancerReaper) Kind() string { return "load-balancer" }
func (r *loadBalancerReaper) Stage() Stage { return StageNetwork }

func (r *loadBalancerReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := r.svc.DescribeLoadBalancers(ctx, &elbv2.DescribeLoadBalancersInput{}, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, lb := range out.LoadBalancers {
		if lb == nil || lb.LoadBalancerArn == nil {
			continue
		}
		// The ARN is what deletes; the name is what an operator recognises.
		found = append(found, Resource{
			Kind:   r.Kind(),
			ID:     aws.StringValue(lb.LoadBalancerArn),
			Detail: aws.StringValue(lb.LoadBalancerName),
		})
	}
	return found, nil
}

func (r *loadBalancerReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := r.svc.DeleteLoadBalancer(ctx, &elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: aws.String(resource.ID),
	}, accountID)
	return ignoreAlreadyGone(err)
}

type targetGroupReaper struct {
	svc handlers_elbv2.ELBv2Service
}

func (r *targetGroupReaper) Kind() string { return "target-group" }
func (r *targetGroupReaper) Stage() Stage { return StageNetwork }

func (r *targetGroupReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := r.svc.DescribeTargetGroups(ctx, &elbv2.DescribeTargetGroupsInput{}, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, group := range out.TargetGroups {
		if group == nil || group.TargetGroupArn == nil {
			continue
		}
		found = append(found, Resource{
			Kind:   r.Kind(),
			ID:     aws.StringValue(group.TargetGroupArn),
			Detail: aws.StringValue(group.TargetGroupName),
		})
	}
	return found, nil
}

func (r *targetGroupReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := r.svc.DeleteTargetGroup(ctx, &elbv2.DeleteTargetGroupInput{
		TargetGroupArn: aws.String(resource.ID),
	}, accountID)
	return ignoreAlreadyGone(err)
}
