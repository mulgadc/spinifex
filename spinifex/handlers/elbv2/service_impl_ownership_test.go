package handlers_elbv2

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// attackerAccountID is the caller that owns nothing in these tests.
const attackerAccountID = "999999999999"

func ownedTargetGroup(t *testing.T, svc *ELBv2ServiceImpl, name string) string {
	t.Helper()
	out, err := svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{
		Name: aws.String(name),
		Port: aws.Int64(80),
	}, testAccountID)
	require.NoError(t, err)
	return *out.TargetGroups[0].TargetGroupArn
}

func ownedLoadBalancer(t *testing.T, svc *ELBv2ServiceImpl, name, accountID string) string {
	t.Helper()
	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name: aws.String(name),
	}, accountID)
	require.NoError(t, err)
	return *out.LoadBalancers[0].LoadBalancerArn
}

func storedTargets(t *testing.T, svc *ELBv2ServiceImpl, tgArn string) []Target {
	t.Helper()
	tg, err := svc.store.GetTargetGroupByArn(t.Context(), tgArn)
	require.NoError(t, err)
	require.NotNil(t, tg)
	return tg.Targets
}

func TestRegisterTargets_CrossAccount(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	tgArn := ownedTargetGroup(t, svc, "reg-xacct-tg")

	_, err := svc.RegisterTargets(context.Background(), &elbv2.RegisterTargetsInput{
		TargetGroupArn: aws.String(tgArn),
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-attacker")}},
	}, attackerAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2TargetGroupNotFound)

	assert.Empty(t, storedTargets(t, svc, tgArn))

	// The owner is unaffected.
	_, err = svc.RegisterTargets(context.Background(), &elbv2.RegisterTargetsInput{
		TargetGroupArn: aws.String(tgArn),
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-owner")}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, storedTargets(t, svc, tgArn), 1)
	assert.Equal(t, "i-owner", storedTargets(t, svc, tgArn)[0].Id)
}

func TestDeregisterTargets_CrossAccount(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	tgArn := ownedTargetGroup(t, svc, "dereg-xacct-tg")

	_, err := svc.RegisterTargets(context.Background(), &elbv2.RegisterTargetsInput{
		TargetGroupArn: aws.String(tgArn),
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-owner")}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeregisterTargets(context.Background(), &elbv2.DeregisterTargetsInput{
		TargetGroupArn: aws.String(tgArn),
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-owner")}},
	}, attackerAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2TargetGroupNotFound)

	require.Len(t, storedTargets(t, svc, tgArn), 1)

	_, err = svc.DeregisterTargets(context.Background(), &elbv2.DeregisterTargetsInput{
		TargetGroupArn: aws.String(tgArn),
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-owner")}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, storedTargets(t, svc, tgArn))
}

func TestDescribeTargetHealth_CrossAccount(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	tgArn := ownedTargetGroup(t, svc, "health-xacct-tg")

	_, err := svc.RegisterTargets(context.Background(), &elbv2.RegisterTargetsInput{
		TargetGroupArn: aws.String(tgArn),
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-owner")}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DescribeTargetHealth(context.Background(), &elbv2.DescribeTargetHealthInput{
		TargetGroupArn: aws.String(tgArn),
	}, attackerAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2TargetGroupNotFound)

	health, err := svc.DescribeTargetHealth(context.Background(), &elbv2.DescribeTargetHealthInput{
		TargetGroupArn: aws.String(tgArn),
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, health.TargetHealthDescriptions, 1)
}

func TestCreateListener_CrossAccountLoadBalancer(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lbArn := ownedLoadBalancer(t, svc, "lst-xacct-lb", testAccountID)

	_, err := svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(lbArn),
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions: []*elbv2.Action{{
			Type: aws.String("fixed-response"),
			FixedResponseConfig: &elbv2.FixedResponseActionConfig{
				StatusCode: aws.String("200"),
			},
		}},
	}, attackerAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2LoadBalancerNotFound)

	listeners, err := svc.store.ListListenersByLB(t.Context(), lbArn)
	require.NoError(t, err)
	assert.Empty(t, listeners)

	// The owner can still attach a listener on the same port.
	_, err = svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(lbArn),
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions: []*elbv2.Action{{
			Type: aws.String("fixed-response"),
			FixedResponseConfig: &elbv2.FixedResponseActionConfig{
				StatusCode: aws.String("200"),
			},
		}},
	}, testAccountID)
	require.NoError(t, err)
}

func TestCreateListener_CrossAccountTargetGroup(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	victimTGArn := ownedTargetGroup(t, svc, "lst-xacct-victim-tg")
	attackerLBArn := ownedLoadBalancer(t, svc, "lst-xacct-att-lb", attackerAccountID)

	_, err := svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(attackerLBArn),
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: aws.String(victimTGArn)},
		},
	}, attackerAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2TargetGroupNotFound)

	listeners, err := svc.store.ListListenersByLB(t.Context(), attackerLBArn)
	require.NoError(t, err)
	assert.Empty(t, listeners)
}

func TestModifyListener_CrossAccountTargetGroup(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	victimTGArn := ownedTargetGroup(t, svc, "mod-xacct-victim-tg")
	attackerLBArn := ownedLoadBalancer(t, svc, "mod-xacct-att-lb", attackerAccountID)

	created, err := svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(attackerLBArn),
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions: []*elbv2.Action{{
			Type: aws.String("fixed-response"),
			FixedResponseConfig: &elbv2.FixedResponseActionConfig{
				StatusCode: aws.String("200"),
			},
		}},
	}, attackerAccountID)
	require.NoError(t, err)
	listenerArn := *created.Listeners[0].ListenerArn

	_, err = svc.ModifyListener(context.Background(), &elbv2.ModifyListenerInput{
		ListenerArn: aws.String(listenerArn),
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: aws.String(victimTGArn)},
		},
	}, attackerAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2TargetGroupNotFound)

	stored, err := svc.store.GetListenerByArn(t.Context(), listenerArn)
	require.NoError(t, err)
	require.Len(t, stored.DefaultActions, 1)
	assert.Equal(t, ActionTypeFixedResponse, stored.DefaultActions[0].Type)
}

func TestCreateRule_CrossAccountTargetGroup(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	victimTGArn := ownedTargetGroup(t, svc, "rule-xacct-victim-tg")
	attackerLBArn := ownedLoadBalancer(t, svc, "rule-xacct-att-lb", attackerAccountID)

	created, err := svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(attackerLBArn),
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions: []*elbv2.Action{{
			Type: aws.String("fixed-response"),
			FixedResponseConfig: &elbv2.FixedResponseActionConfig{
				StatusCode: aws.String("200"),
			},
		}},
	}, attackerAccountID)
	require.NoError(t, err)
	listenerArn := *created.Listeners[0].ListenerArn

	_, err = svc.CreateRule(context.Background(), &elbv2.CreateRuleInput{
		ListenerArn: aws.String(listenerArn),
		Priority:    aws.Int64(10),
		Conditions: []*elbv2.RuleCondition{{
			Field:  aws.String("path-pattern"),
			Values: []*string{aws.String("/*")},
		}},
		Actions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: aws.String(victimTGArn)},
		},
	}, attackerAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2TargetGroupNotFound)

	rules, err := svc.store.ListRulesByListener(t.Context(), listenerArn)
	require.NoError(t, err)
	assert.Empty(t, rules)
}
