package accountteardown

//test:in-package — the reapers and the states they choose to skip are
// unexported, and which states they skip is the substance of them.

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_ec2_eigw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eigw"
	handlers_ec2_natgw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/natgw"
	handlers_ec2_spotinstance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/spotinstance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The embedded interfaces are deliberately nil: a reaper that calls anything
// beyond the two methods it is supposed to use panics rather than passing.
type fakeSpot struct {
	handlers_ec2_spotinstance.SpotInstanceService

	requests  []*ec2.SpotInstanceRequest
	cancelled []string
	err       error
}

func (f *fakeSpot) DescribeSpotInstanceRequests(_ context.Context, _ *ec2.DescribeSpotInstanceRequestsInput, _ string) (*ec2.DescribeSpotInstanceRequestsOutput, error) {
	return &ec2.DescribeSpotInstanceRequestsOutput{SpotInstanceRequests: f.requests}, nil
}

func (f *fakeSpot) CancelSpotInstanceRequests(_ context.Context, in *ec2.CancelSpotInstanceRequestsInput, _ string) (*ec2.CancelSpotInstanceRequestsOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	for _, id := range in.SpotInstanceRequestIds {
		f.cancelled = append(f.cancelled, aws.StringValue(id))
	}
	return &ec2.CancelSpotInstanceRequestsOutput{}, nil
}

type fakeNatGateways struct {
	handlers_ec2_natgw.NatGatewayService

	gateways []*ec2.NatGateway
	deleted  []string
	err      error
}

func (f *fakeNatGateways) DescribeNatGateways(_ context.Context, _ *ec2.DescribeNatGatewaysInput, _ string) (*ec2.DescribeNatGatewaysOutput, error) {
	return &ec2.DescribeNatGatewaysOutput{NatGateways: f.gateways}, nil
}

func (f *fakeNatGateways) DeleteNatGateway(_ context.Context, in *ec2.DeleteNatGatewayInput, _ string) (*ec2.DeleteNatGatewayOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.deleted = append(f.deleted, aws.StringValue(in.NatGatewayId))
	return &ec2.DeleteNatGatewayOutput{}, nil
}

type fakeEgressGateways struct {
	handlers_ec2_eigw.EgressOnlyIGWService

	gateways []*ec2.EgressOnlyInternetGateway
	deleted  []string
	err      error
}

func (f *fakeEgressGateways) DescribeEgressOnlyInternetGateways(_ context.Context, _ *ec2.DescribeEgressOnlyInternetGatewaysInput, _ string) (*ec2.DescribeEgressOnlyInternetGatewaysOutput, error) {
	return &ec2.DescribeEgressOnlyInternetGatewaysOutput{EgressOnlyInternetGateways: f.gateways}, nil
}

func (f *fakeEgressGateways) DeleteEgressOnlyInternetGateway(_ context.Context, in *ec2.DeleteEgressOnlyInternetGatewayInput, _ string) (*ec2.DeleteEgressOnlyInternetGatewayOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.deleted = append(f.deleted, aws.StringValue(in.EgressOnlyInternetGatewayId))
	return &ec2.DeleteEgressOnlyInternetGatewayOutput{}, nil
}

// A cancelled request cannot launch anything, so listing it would leave the
// stage waiting on a row that never clears — spot requests are never removed
// from the account, only moved to a terminal state.
func TestSpotRequestReaperSkipsTerminalRequests(t *testing.T) {
	svc := &fakeSpot{requests: []*ec2.SpotInstanceRequest{
		{SpotInstanceRequestId: aws.String("sir-open"), State: aws.String("open")},
		{SpotInstanceRequestId: aws.String("sir-active"), State: aws.String("active")},
		{SpotInstanceRequestId: aws.String("sir-cancelled"), State: aws.String("cancelled")},
		{SpotInstanceRequestId: aws.String("sir-closed"), State: aws.String("closed")},
		{SpotInstanceRequestId: aws.String("sir-failed"), State: aws.String("failed")},
	}}
	reaper := &spotRequestReaper{svc: svc}

	assert.Equal(t, StageCompute, reaper.Stage())

	found, err := reaper.List(t.Context(), "000000000002")
	require.NoError(t, err)
	require.Len(t, found, 2)
	assert.Equal(t, "sir-open", found[0].ID)
	assert.Equal(t, "open", found[0].Detail)
	assert.Equal(t, "sir-active", found[1].ID)

	require.NoError(t, reaper.Delete(t.Context(), "000000000002", found[0], false))
	assert.Equal(t, []string{"sir-open"}, svc.cancelled)
}

// A request cancelled by someone else between the listing and the delete is
// the ordinary second-pass case, not a failure.
func TestSpotRequestReaperTreatsAMissingRequestAsCancelled(t *testing.T) {
	svc := &fakeSpot{err: errors.New("InvalidSpotInstanceRequestID.NotFound: no such request")}
	reaper := &spotRequestReaper{svc: svc}

	err := reaper.Delete(t.Context(), "000000000002", Resource{ID: "sir-gone"}, false)
	assert.NoError(t, err)
}

// A deleting gateway still holds its address and its subnet, so it has to keep
// listing until it is actually gone or the address reaper runs too early.
func TestNatGatewayReaperKeepsDeletingGateways(t *testing.T) {
	svc := &fakeNatGateways{gateways: []*ec2.NatGateway{
		{NatGatewayId: aws.String("nat-live"), State: aws.String("available"), SubnetId: aws.String("subnet-a")},
		{NatGatewayId: aws.String("nat-going"), State: aws.String("deleting"), SubnetId: aws.String("subnet-b")},
		{NatGatewayId: aws.String("nat-done"), State: aws.String("deleted"), SubnetId: aws.String("subnet-c")},
	}}
	reaper := &natGatewayReaper{svc: svc}

	assert.Equal(t, StageNetwork, reaper.Stage())

	found, err := reaper.List(t.Context(), "000000000002")
	require.NoError(t, err)
	require.Len(t, found, 2)
	assert.Equal(t, "nat-live", found[0].ID)
	assert.Equal(t, "available in subnet-a", found[0].Detail)
	assert.Equal(t, "nat-going", found[1].ID)

	require.NoError(t, reaper.Delete(t.Context(), "000000000002", found[0], false))
	assert.Equal(t, []string{"nat-live"}, svc.deleted)
}

func TestNatGatewayReaperTreatsAMissingGatewayAsDeleted(t *testing.T) {
	svc := &fakeNatGateways{err: errors.New("InvalidNatGatewayID.NotFound: no such gateway")}
	reaper := &natGatewayReaper{svc: svc}

	assert.NoError(t, reaper.Delete(t.Context(), "000000000002", Resource{ID: "nat-gone"}, false))
}

func TestEgressOnlyIGWReaperCarriesItsVPC(t *testing.T) {
	svc := &fakeEgressGateways{gateways: []*ec2.EgressOnlyInternetGateway{
		{
			EgressOnlyInternetGatewayId: aws.String("eigw-a"),
			Attachments:                 []*ec2.InternetGatewayAttachment{{VpcId: aws.String("vpc-1")}},
		},
		{EgressOnlyInternetGatewayId: aws.String("eigw-detached")},
	}}
	reaper := &egressOnlyIGWReaper{svc: svc}

	assert.Equal(t, StageNetwork, reaper.Stage())

	found, err := reaper.List(t.Context(), "000000000002")
	require.NoError(t, err)
	require.Len(t, found, 2)
	assert.Equal(t, "vpc-1", found[0].Detail)
	assert.Empty(t, found[1].Detail)

	require.NoError(t, reaper.Delete(t.Context(), "000000000002", found[0], false))
	assert.Equal(t, []string{"eigw-a"}, svc.deleted)
}

func TestEgressOnlyIGWReaperTreatsAMissingGatewayAsDeleted(t *testing.T) {
	svc := &fakeEgressGateways{err: errors.New("InvalidGatewayID.NotFound: no such gateway")}
	reaper := &egressOnlyIGWReaper{svc: svc}

	assert.NoError(t, reaper.Delete(t.Context(), "000000000002", Resource{ID: "eigw-gone"}, false))
}
