package accountteardown

import (
	"context"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/handlers/ec2/eigw"
	"github.com/mulgadc/spinifex/spinifex/handlers/ec2/natgw"
	"github.com/mulgadc/spinifex/spinifex/handlers/ec2/spotinstance"
	"github.com/nats-io/nats.go"
)

// EC2ExtraReapers returns the EC2 reapers that hang off the VPC rather than off
// an instance.
//
// Spot requests come first and in the compute stage: a live request can launch
// a replacement after the instance reaper has drained, so leaving one running
// makes teardown race its own fulfilment rather than merely leak.
func EC2ExtraReapers(nc *nats.Conn) []Reaper {
	return []Reaper{
		&spotRequestReaper{svc: handlers_ec2_spotinstance.NewNATSSpotInstanceService(nc)},
		&natGatewayReaper{svc: handlers_ec2_natgw.NewNATSNatGatewayService(nc)},
		&egressOnlyIGWReaper{svc: handlers_ec2_eigw.NewNATSEgressOnlyIGWService(nc)},
	}
}

// terminalSpotStates are the states a request can no longer launch from.
// Anything else may still produce an instance.
var terminalSpotStates = map[string]bool{"cancelled": true, "closed": true, "failed": true}

type spotRequestReaper struct {
	svc handlers_ec2_spotinstance.SpotInstanceService
}

func (r *spotRequestReaper) Kind() string { return "spot-request" }
func (r *spotRequestReaper) Stage() Stage { return StageCompute }

func (r *spotRequestReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := r.svc.DescribeSpotInstanceRequests(ctx, &ec2.DescribeSpotInstanceRequestsInput{}, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, request := range out.SpotInstanceRequests {
		if request == nil || request.SpotInstanceRequestId == nil {
			continue
		}
		state := aws.StringValue(request.State)
		if terminalSpotStates[state] {
			continue
		}
		found = append(found, Resource{
			Kind:   r.Kind(),
			ID:     aws.StringValue(request.SpotInstanceRequestId),
			Detail: state,
		})
	}
	return found, nil
}

func (r *spotRequestReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := r.svc.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []*string{aws.String(resource.ID)},
	}, accountID)
	return ignoreAlreadyGone(err)
}

// deletedNatGatewayStates are the states a NAT gateway has already released its
// address and its subnet placement in.
var deletedNatGatewayStates = map[string]bool{"deleted": true}

type natGatewayReaper struct {
	svc handlers_ec2_natgw.NatGatewayService
}

func (r *natGatewayReaper) Kind() string { return "nat-gateway" }
func (r *natGatewayReaper) Stage() Stage { return StageNetwork }

// List keeps a gateway that is merely deleting, because it still holds its EIP
// and its place in the subnet until it is actually gone — and those are the two
// things the reapers after it are waiting on.
func (r *natGatewayReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := r.svc.DescribeNatGateways(ctx, &ec2.DescribeNatGatewaysInput{}, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, gateway := range out.NatGateways {
		if gateway == nil || gateway.NatGatewayId == nil {
			continue
		}
		state := aws.StringValue(gateway.State)
		if deletedNatGatewayStates[state] {
			continue
		}
		found = append(found, Resource{
			Kind:   r.Kind(),
			ID:     aws.StringValue(gateway.NatGatewayId),
			Detail: natGatewayDetail(gateway, state),
		})
	}
	return found, nil
}

// natGatewayDetail names the subnet, because a subnet that will not delete with
// a NAT gateway still in it is the case an operator has to be able to read.
func natGatewayDetail(gateway *ec2.NatGateway, state string) string {
	subnet := aws.StringValue(gateway.SubnetId)
	if subnet == "" {
		return state
	}
	return state + " in " + subnet
}

func (r *natGatewayReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := r.svc.DeleteNatGateway(ctx, &ec2.DeleteNatGatewayInput{
		NatGatewayId: aws.String(resource.ID),
	}, accountID)
	return ignoreAlreadyGone(err)
}

type egressOnlyIGWReaper struct {
	svc handlers_ec2_eigw.EgressOnlyIGWService
}

func (r *egressOnlyIGWReaper) Kind() string { return "egress-only-internet-gateway" }
func (r *egressOnlyIGWReaper) Stage() Stage { return StageNetwork }

func (r *egressOnlyIGWReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := r.svc.DescribeEgressOnlyInternetGateways(ctx, &ec2.DescribeEgressOnlyInternetGatewaysInput{}, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, gateway := range out.EgressOnlyInternetGateways {
		if gateway == nil || gateway.EgressOnlyInternetGatewayId == nil {
			continue
		}
		resource := Resource{Kind: r.Kind(), ID: aws.StringValue(gateway.EgressOnlyInternetGatewayId)}
		for _, attachment := range gateway.Attachments {
			if attachment != nil && aws.StringValue(attachment.VpcId) != "" {
				resource.Detail = aws.StringValue(attachment.VpcId)
				break
			}
		}
		found = append(found, resource)
	}
	return found, nil
}

func (r *egressOnlyIGWReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := r.svc.DeleteEgressOnlyInternetGateway(ctx, &ec2.DeleteEgressOnlyInternetGatewayInput{
		EgressOnlyInternetGatewayId: aws.String(resource.ID),
	}, accountID)
	return ignoreAlreadyGone(err)
}
