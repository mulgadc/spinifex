package handlers_elbv2

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupNATSELBv2Test creates a NATS client + backend service wired together,
// exercising the full NATS serialization round-trip (same as gateway→daemon path).
func setupNATSELBv2Test(t *testing.T) (ELBv2Service, *ELBv2ServiceImpl) {
	t.Helper()

	_, nc, _ := testutil.StartTestJetStream(t)

	backend, err := NewELBv2ServiceImplWithNATS(nil, nc)
	require.NoError(t, err)

	// Wire backend as NATS subscriber (simulates daemon subscriptions)
	topics := map[string]func(*nats.Msg){
		"elbv2.CreateLoadBalancer":    func(msg *nats.Msg) { handleNATSMsg(msg, backend.CreateLoadBalancer) },
		"elbv2.DeleteLoadBalancer":    func(msg *nats.Msg) { handleNATSMsg(msg, backend.DeleteLoadBalancer) },
		"elbv2.DescribeLoadBalancers": func(msg *nats.Msg) { handleNATSMsg(msg, backend.DescribeLoadBalancers) },
		"elbv2.CreateTargetGroup":     func(msg *nats.Msg) { handleNATSMsg(msg, backend.CreateTargetGroup) },
		"elbv2.DeleteTargetGroup":     func(msg *nats.Msg) { handleNATSMsg(msg, backend.DeleteTargetGroup) },
		"elbv2.DescribeTargetGroups":  func(msg *nats.Msg) { handleNATSMsg(msg, backend.DescribeTargetGroups) },
		"elbv2.RegisterTargets":       func(msg *nats.Msg) { handleNATSMsg(msg, backend.RegisterTargets) },
		"elbv2.DeregisterTargets":     func(msg *nats.Msg) { handleNATSMsg(msg, backend.DeregisterTargets) },
		"elbv2.DescribeTargetHealth":  func(msg *nats.Msg) { handleNATSMsg(msg, backend.DescribeTargetHealth) },
		"elbv2.CreateListener":        func(msg *nats.Msg) { handleNATSMsg(msg, backend.CreateListener) },
		"elbv2.DeleteListener":        func(msg *nats.Msg) { handleNATSMsg(msg, backend.DeleteListener) },
		"elbv2.ModifyListener":        func(msg *nats.Msg) { handleNATSMsg(msg, backend.ModifyListener) },
		"elbv2.DescribeListeners":     func(msg *nats.Msg) { handleNATSMsg(msg, backend.DescribeListeners) },
	}

	for topic, handler := range topics {
		sub, err := nc.Subscribe(topic, handler)
		require.NoError(t, err)
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}

	client := NewNATSELBv2Service(nc)
	return client, backend
}

// handleNATSMsg is the generic NATS handler that mirrors daemon's handleNATSRequest.
// Uses GenerateErrorPayload for errors, matching the real daemon behavior.
func handleNATSMsg[In any, Out any](msg *nats.Msg, fn func(*In, string) (*Out, error)) {
	var input In
	if err := json.Unmarshal(msg.Data, &input); err != nil {
		_ = msg.Respond(utils.GenerateErrorPayload("ServerInternal"))
		return
	}
	accountID := msg.Header.Get(utils.AccountIDHeader)
	result, err := fn(&input, accountID)
	if err != nil {
		_ = msg.Respond(utils.GenerateErrorPayload(err.Error()))
		return
	}
	data, _ := json.Marshal(result)
	_ = msg.Respond(data)
}

// --- Full round-trip E2E tests ---

func TestNATSE2E_CreateAndDescribeLoadBalancer(t *testing.T) {
	client, _ := setupNATSELBv2Test(t)

	out, err := client.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:           aws.String("e2e-alb"),
		Subnets:        []*string{aws.String("subnet-aaa")},
		SecurityGroups: []*string{aws.String("sg-111")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.LoadBalancers, 1)
	assert.Equal(t, "e2e-alb", *out.LoadBalancers[0].LoadBalancerName)

	desc, err := client.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{
		Names: []*string{aws.String("e2e-alb")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.LoadBalancers, 1)
	assert.Equal(t, *out.LoadBalancers[0].LoadBalancerArn, *desc.LoadBalancers[0].LoadBalancerArn)
}

func TestNATSE2E_FullWorkflow(t *testing.T) {
	client, _ := setupNATSELBv2Test(t)

	// 1. Create target group
	tgOut, err := client.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("e2e-tg"),
		Protocol: aws.String("HTTP"),
		Port:     aws.Int64(80),
		VpcId:    aws.String("vpc-e2e"),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, tgOut.TargetGroups, 1)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn

	// 2. Register targets
	_, err = client.RegisterTargets(&elbv2.RegisterTargetsInput{
		TargetGroupArn: tgArn,
		Targets: []*elbv2.TargetDescription{
			{Id: aws.String("i-instance1")},
			{Id: aws.String("i-instance2"), Port: aws.Int64(8080)},
		},
	}, testAccountID)
	require.NoError(t, err)

	// 3. Verify targets registered
	healthOut, err := client.DescribeTargetHealth(&elbv2.DescribeTargetHealthInput{
		TargetGroupArn: tgArn,
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, healthOut.TargetHealthDescriptions, 2)
	assert.Equal(t, "initial", *healthOut.TargetHealthDescriptions[0].TargetHealth.State)

	// 4. Create load balancer
	lbOut, err := client.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:           aws.String("e2e-lb"),
		Subnets:        []*string{aws.String("subnet-1")},
		SecurityGroups: []*string{aws.String("sg-1")},
	}, testAccountID)
	require.NoError(t, err)
	lbArn := lbOut.LoadBalancers[0].LoadBalancerArn

	// 5. Create listener
	lstOut, err := client.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbArn,
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: tgArn},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, lstOut.Listeners, 1)
	assert.Equal(t, int64(80), *lstOut.Listeners[0].Port)

	// 6. Describe listeners
	lstDesc, err := client.DescribeListeners(&elbv2.DescribeListenersInput{
		LoadBalancerArn: lbArn,
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, lstDesc.Listeners, 1)

	// 7. Describe target groups filtered by LB
	tgDesc, err := client.DescribeTargetGroups(&elbv2.DescribeTargetGroupsInput{
		LoadBalancerArn: lbArn,
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, tgDesc.TargetGroups, 1)
	assert.Equal(t, "e2e-tg", *tgDesc.TargetGroups[0].TargetGroupName)

	// 8. Deregister one target
	_, err = client.DeregisterTargets(&elbv2.DeregisterTargetsInput{
		TargetGroupArn: tgArn,
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-instance1")}},
	}, testAccountID)
	require.NoError(t, err)

	healthOut2, err := client.DescribeTargetHealth(&elbv2.DescribeTargetHealthInput{
		TargetGroupArn: tgArn,
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, healthOut2.TargetHealthDescriptions, 1)
	assert.Equal(t, "i-instance2", *healthOut2.TargetHealthDescriptions[0].Target.Id)

	// 9. Delete listener
	_, err = client.DeleteListener(&elbv2.DeleteListenerInput{
		ListenerArn: lstOut.Listeners[0].ListenerArn,
	}, testAccountID)
	require.NoError(t, err)

	// 10. Delete load balancer
	_, err = client.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: lbArn,
	}, testAccountID)
	require.NoError(t, err)

	// 11. Delete target group (should succeed now — no listener references it)
	_, err = client.DeregisterTargets(&elbv2.DeregisterTargetsInput{
		TargetGroupArn: tgArn,
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-instance2"), Port: aws.Int64(8080)}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = client.DeleteTargetGroup(&elbv2.DeleteTargetGroupInput{
		TargetGroupArn: tgArn,
	}, testAccountID)
	require.NoError(t, err)

	// 12. Verify everything is cleaned up
	lbDesc, err := client.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, lbDesc.LoadBalancers)

	tgDesc2, err := client.DescribeTargetGroups(&elbv2.DescribeTargetGroupsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, tgDesc2.TargetGroups)
}

func TestNATSE2E_DeleteLoadBalancerCascadesListeners(t *testing.T) {
	client, _ := setupNATSELBv2Test(t)

	// Create LB + TG + 2 listeners
	tgOut, _ := client.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name: aws.String("cascade-tg"),
	}, testAccountID)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn

	lbOut, _ := client.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("cascade-lb"),
	}, testAccountID)
	lbArn := lbOut.LoadBalancers[0].LoadBalancerArn

	actions := []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgArn}}
	client.CreateListener(&elbv2.CreateListenerInput{LoadBalancerArn: lbArn, Port: aws.Int64(80), DefaultActions: actions}, testAccountID)
	client.CreateListener(&elbv2.CreateListenerInput{LoadBalancerArn: lbArn, Port: aws.Int64(443), DefaultActions: actions}, testAccountID)

	// Delete LB should cascade-delete listeners
	_, err := client.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{LoadBalancerArn: lbArn}, testAccountID)
	require.NoError(t, err)

	// TG should now be deletable (no listeners reference it)
	_, err = client.DeleteTargetGroup(&elbv2.DeleteTargetGroupInput{TargetGroupArn: tgArn}, testAccountID)
	require.NoError(t, err)
}

func TestNATSE2E_ModifyListener(t *testing.T) {
	client, _ := setupNATSELBv2Test(t)

	tgOut, _ := client.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("e2e-mod-tg")}, testAccountID)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn

	lbOut, _ := client.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("e2e-mod-lb")}, testAccountID)
	lbArn := lbOut.LoadBalancers[0].LoadBalancerArn

	lstOut, err := client.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbArn,
		Port:            aws.Int64(80),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgArn}},
	}, testAccountID)
	require.NoError(t, err)

	modOut, err := client.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn: lstOut.Listeners[0].ListenerArn,
		Port:        aws.Int64(8080),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, modOut.Listeners, 1)
	assert.Equal(t, int64(8080), *modOut.Listeners[0].Port)

	desc, err := client.DescribeListeners(&elbv2.DescribeListenersInput{LoadBalancerArn: lbArn}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Listeners, 1)
	assert.Equal(t, int64(8080), *desc.Listeners[0].Port)
}

func TestNATSE2E_TargetGroupInUseProtection(t *testing.T) {
	client, _ := setupNATSELBv2Test(t)

	tgOut, _ := client.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("protected-tg")}, testAccountID)
	lbOut, _ := client.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("protected-lb")}, testAccountID)

	client.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)

	// Should fail — TG is in use by a listener
	_, err := client.DeleteTargetGroup(&elbv2.DeleteTargetGroupInput{
		TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn,
	}, testAccountID)
	require.Error(t, err)
}
