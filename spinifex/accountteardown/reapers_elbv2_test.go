package accountteardown

//test:in-package — both reapers are unexported, and the order they are
// registered in is what keeps a target group from outliving its listener.

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The embedded interface is nil, so a reaper reaching for anything beyond the
// four methods below panics rather than passing.
type fakeELBv2 struct {
	handlers_elbv2.ELBv2Service

	loadBalancers []*elbv2.LoadBalancer
	targetGroups  []*elbv2.TargetGroup
	deleted       []string
	err           error
}

func (f *fakeELBv2) DescribeLoadBalancers(_ context.Context, _ *elbv2.DescribeLoadBalancersInput, _ string) (*elbv2.DescribeLoadBalancersOutput, error) {
	return &elbv2.DescribeLoadBalancersOutput{LoadBalancers: f.loadBalancers}, nil
}

func (f *fakeELBv2) DeleteLoadBalancer(_ context.Context, in *elbv2.DeleteLoadBalancerInput, _ string) (*elbv2.DeleteLoadBalancerOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.deleted = append(f.deleted, aws.StringValue(in.LoadBalancerArn))
	return &elbv2.DeleteLoadBalancerOutput{}, nil
}

func (f *fakeELBv2) DescribeTargetGroups(_ context.Context, _ *elbv2.DescribeTargetGroupsInput, _ string) (*elbv2.DescribeTargetGroupsOutput, error) {
	return &elbv2.DescribeTargetGroupsOutput{TargetGroups: f.targetGroups}, nil
}

func (f *fakeELBv2) DeleteTargetGroup(_ context.Context, in *elbv2.DeleteTargetGroupInput, _ string) (*elbv2.DeleteTargetGroupOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.deleted = append(f.deleted, aws.StringValue(in.TargetGroupArn))
	return &elbv2.DeleteTargetGroupOutput{}, nil
}

func TestLoadBalancerReaperListsByARNAndNamesTheBalancer(t *testing.T) {
	svc := &fakeELBv2{loadBalancers: []*elbv2.LoadBalancer{
		{LoadBalancerArn: aws.String("arn:lb/web"), LoadBalancerName: aws.String("web")},
		{LoadBalancerName: aws.String("no-arn")},
	}}
	reaper := &loadBalancerReaper{svc: svc}

	assert.Equal(t, StageNetwork, reaper.Stage())

	found, err := reaper.List(t.Context(), "000000000002")
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "arn:lb/web", found[0].ID)
	assert.Equal(t, "web", found[0].Detail)

	require.NoError(t, reaper.Delete(t.Context(), "000000000002", found[0], false))
	assert.Equal(t, []string{"arn:lb/web"}, svc.deleted)
}

func TestTargetGroupReaperListsAndDeletesByARN(t *testing.T) {
	svc := &fakeELBv2{targetGroups: []*elbv2.TargetGroup{
		{TargetGroupArn: aws.String("arn:tg/web"), TargetGroupName: aws.String("web")},
	}}
	reaper := &targetGroupReaper{svc: svc}

	assert.Equal(t, StageNetwork, reaper.Stage())

	found, err := reaper.List(t.Context(), "000000000002")
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.Equal(t, "arn:tg/web", found[0].ID)

	require.NoError(t, reaper.Delete(t.Context(), "000000000002", found[0], false))
	assert.Equal(t, []string{"arn:tg/web"}, svc.deleted)
}

func TestELBv2ReapersTreatAMissingResourceAsDeleted(t *testing.T) {
	lbGone := &loadBalancerReaper{svc: &fakeELBv2{err: errors.New("LoadBalancerNotFound: no such balancer")}}
	assert.NoError(t, lbGone.Delete(t.Context(), "000000000002", Resource{ID: "arn:lb/web"}, false))

	tgGone := &targetGroupReaper{svc: &fakeELBv2{err: errors.New("TargetGroupNotFound: no such group")}}
	assert.NoError(t, tgGone.Delete(t.Context(), "000000000002", Resource{ID: "arn:tg/web"}, false))
}

// A target group still referenced by a listener refuses to delete, which is
// why the balancer has to be reaped first — and the sort that groups reapers
// by stage is stable, so registration order is what guarantees it.
func TestELBv2ReapersDeleteBalancersBeforeTargetGroups(t *testing.T) {
	reapers := ELBv2Reapers(nil)
	require.Len(t, reapers, 2)
	assert.Equal(t, "load-balancer", reapers[0].Kind())
	assert.Equal(t, "target-group", reapers[1].Kind())
}
