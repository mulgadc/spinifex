package handlers_elbv2

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests in this file cover paths NOT exercised by service_impl_test.go:
// filter-by-ARN family, missing-ARN error paths, RegisterTargets nil/not-found edges.

// --- DescribeLoadBalancers: ARN filter (service_impl_test.go covers FilterByName) ---

func TestDescribeLoadBalancers_FilterByArn(t *testing.T) {
	svc := setupTestService(t)

	out1, _ := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{Name: aws.String("lb-arn-a")}, testAccountID)
	_, _ = svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{Name: aws.String("lb-arn-b")}, testAccountID)

	desc, err := svc.DescribeLoadBalancers(context.Background(), &elbv2.DescribeLoadBalancersInput{
		LoadBalancerArns: []*string{out1.LoadBalancers[0].LoadBalancerArn},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.LoadBalancers, 1)
	assert.Equal(t, "lb-arn-a", *desc.LoadBalancers[0].LoadBalancerName)
}

// --- DescribeTargetGroups: by name and by ARN (service_impl_test.go only covers FilterByLBArn) ---

func TestDescribeTargetGroups_FilterByName(t *testing.T) {
	svc := setupTestService(t)

	_, _ = svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{Name: aws.String("tg-alpha")}, testAccountID)
	_, _ = svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{Name: aws.String("tg-beta")}, testAccountID)

	desc, err := svc.DescribeTargetGroups(context.Background(), &elbv2.DescribeTargetGroupsInput{
		Names: []*string{aws.String("tg-alpha")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.TargetGroups, 1)
	assert.Equal(t, "tg-alpha", *desc.TargetGroups[0].TargetGroupName)
}

func TestDescribeTargetGroups_FilterByArn(t *testing.T) {
	svc := setupTestService(t)

	out1, _ := svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{Name: aws.String("tg-arn-a")}, testAccountID)
	_, _ = svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{Name: aws.String("tg-arn-b")}, testAccountID)

	desc, err := svc.DescribeTargetGroups(context.Background(), &elbv2.DescribeTargetGroupsInput{
		TargetGroupArns: []*string{out1.TargetGroups[0].TargetGroupArn},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.TargetGroups, 1)
	assert.Equal(t, "tg-arn-a", *desc.TargetGroups[0].TargetGroupName)
}

// --- DescribeListeners: filter by listener ARN (service_impl_test.go covers FilterByLBArn) ---

func TestDescribeListeners_FilterByListenerArn(t *testing.T) {
	svc := setupTestService(t)

	lb, _ := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{Name: aws.String("lb-larn")}, testAccountID)
	tg, _ := svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{Name: aws.String("tg-larn")}, testAccountID)

	l1, _ := svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: lb.LoadBalancers[0].LoadBalancerArn,
		Port:            aws.Int64(80),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tg.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)
	_, _ = svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: lb.LoadBalancers[0].LoadBalancerArn,
		Port:            aws.Int64(443),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tg.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)

	desc, err := svc.DescribeListeners(context.Background(), &elbv2.DescribeListenersInput{
		ListenerArns: []*string{l1.Listeners[0].ListenerArn},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Listeners, 1)
	assert.Equal(t, int64(80), *desc.Listeners[0].Port)
}

// --- RegisterTargets edge cases ---

func TestRegisterTargets_NilTargetSkipped(t *testing.T) {
	svc := setupTestService(t)

	tg, _ := svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{Name: aws.String("tg-nil"), Port: aws.Int64(80)}, testAccountID)

	_, err := svc.RegisterTargets(context.Background(), &elbv2.RegisterTargetsInput{
		TargetGroupArn: tg.TargetGroups[0].TargetGroupArn,
		Targets: []*elbv2.TargetDescription{
			{Id: nil},
			{Id: aws.String("i-valid")},
		},
	}, testAccountID)
	require.NoError(t, err)

	health, _ := svc.DescribeTargetHealth(context.Background(), &elbv2.DescribeTargetHealthInput{
		TargetGroupArn: tg.TargetGroups[0].TargetGroupArn,
	}, testAccountID)
	require.Len(t, health.TargetHealthDescriptions, 1)
	assert.Equal(t, "i-valid", *health.TargetHealthDescriptions[0].Target.Id)
}

func TestRegisterTargets_TGNotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.RegisterTargets(context.Background(), &elbv2.RegisterTargetsInput{
		TargetGroupArn: aws.String("arn:nonexistent"),
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-abc")}},
	}, testAccountID)
	assert.Error(t, err)
}

func TestDeregisterTargets_TGNotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.DeregisterTargets(context.Background(), &elbv2.DeregisterTargetsInput{
		TargetGroupArn: aws.String("arn:nonexistent"),
	}, testAccountID)
	assert.Error(t, err)
}

// --- Missing-ARN error paths across the service surface ---

func TestMissingArnReturnsError(t *testing.T) {
	svc := setupTestService(t)

	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "DeleteLoadBalancer",
			call: func() error {
				_, err := svc.DeleteLoadBalancer(context.Background(), &elbv2.DeleteLoadBalancerInput{}, testAccountID)
				return err
			},
		},
		{
			name: "DeleteTargetGroup",
			call: func() error {
				_, err := svc.DeleteTargetGroup(context.Background(), &elbv2.DeleteTargetGroupInput{}, testAccountID)
				return err
			},
		},
		{
			name: "DeleteListener",
			call: func() error {
				_, err := svc.DeleteListener(context.Background(), &elbv2.DeleteListenerInput{}, testAccountID)
				return err
			},
		},
		{
			name: "RegisterTargets",
			call: func() error {
				_, err := svc.RegisterTargets(context.Background(), &elbv2.RegisterTargetsInput{}, testAccountID)
				return err
			},
		},
		{
			name: "DeregisterTargets",
			call: func() error {
				_, err := svc.DeregisterTargets(context.Background(), &elbv2.DeregisterTargetsInput{}, testAccountID)
				return err
			},
		},
		{
			name: "DescribeTargetHealth",
			call: func() error {
				_, err := svc.DescribeTargetHealth(context.Background(), &elbv2.DescribeTargetHealthInput{}, testAccountID)
				return err
			},
		},
		{
			name: "CreateListener missing LB ARN",
			call: func() error {
				_, err := svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
					DefaultActions: []*elbv2.Action{{Type: aws.String("forward")}},
				}, testAccountID)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Error(t, tt.call())
		})
	}
}

// --- CreateListener: missing actions (separate from missing-ARN path) ---

func TestCreateListener_MissingActions(t *testing.T) {
	svc := setupTestService(t)
	lb, _ := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{Name: aws.String("lb-noact")}, testAccountID)

	_, err := svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: lb.LoadBalancers[0].LoadBalancerArn,
	}, testAccountID)
	assert.Error(t, err)
}
