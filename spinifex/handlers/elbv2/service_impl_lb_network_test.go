package handlers_elbv2

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createLBArn(t *testing.T, svc *ELBv2ServiceImpl, name string) string {
	t.Helper()
	out, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String(name),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.LoadBalancers, 1)
	return *out.LoadBalancers[0].LoadBalancerArn
}

func describeLB(t *testing.T, svc *ELBv2ServiceImpl, arn string) *elbv2.LoadBalancer {
	t.Helper()
	out, err := svc.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{
		LoadBalancerArns: []*string{aws.String(arn)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.LoadBalancers, 1)
	return out.LoadBalancers[0]
}

func TestSetIpAddressType_IPv4Idempotent(t *testing.T) {
	svc := setupTestService(t)
	arn := createLBArn(t, svc, "ipt-lb")

	out, err := svc.SetIpAddressType(&elbv2.SetIpAddressTypeInput{
		LoadBalancerArn: aws.String(arn),
		IpAddressType:   aws.String("ipv4"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "ipv4", *out.IpAddressType)
	assert.Equal(t, "ipv4", *describeLB(t, svc, arn).IpAddressType)
}

func TestSetIpAddressType_DualstackRejected(t *testing.T) {
	svc := setupTestService(t)
	arn := createLBArn(t, svc, "ipt-ds")

	_, err := svc.SetIpAddressType(&elbv2.SetIpAddressTypeInput{
		LoadBalancerArn: aws.String(arn),
		IpAddressType:   aws.String("dualstack"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2InvalidConfigurationRequest)
}

func TestSetIpAddressType_MissingParams(t *testing.T) {
	svc := setupTestService(t)
	arn := createLBArn(t, svc, "ipt-mp")

	_, err := svc.SetIpAddressType(&elbv2.SetIpAddressTypeInput{
		LoadBalancerArn: aws.String(arn),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)

	_, err = svc.SetIpAddressType(&elbv2.SetIpAddressTypeInput{
		IpAddressType: aws.String("ipv4"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)
}

func TestSetIpAddressType_NotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.SetIpAddressType(&elbv2.SetIpAddressTypeInput{
		LoadBalancerArn: aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/missing/lb-deadbeef"),
		IpAddressType:   aws.String("ipv4"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2LoadBalancerNotFound)
}

func TestSetSecurityGroups_UpdatesRecord(t *testing.T) {
	svc := setupTestService(t)
	arn := createLBArn(t, svc, "sg-lb")

	out, err := svc.SetSecurityGroups(&elbv2.SetSecurityGroupsInput{
		LoadBalancerArn: aws.String(arn),
		SecurityGroups:  aws.StringSlice([]string{"sg-aaa", "sg-bbb"}),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"sg-aaa", "sg-bbb"}, aws.StringValueSlice(out.SecurityGroupIds))
	assert.Equal(t, []string{"sg-aaa", "sg-bbb"}, aws.StringValueSlice(describeLB(t, svc, arn).SecurityGroups))
}

func TestSetSecurityGroups_EmptyRejected(t *testing.T) {
	svc := setupTestService(t)
	arn := createLBArn(t, svc, "sg-empty")

	_, err := svc.SetSecurityGroups(&elbv2.SetSecurityGroupsInput{
		LoadBalancerArn: aws.String(arn),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)
}

func TestSetSecurityGroups_NLBRejected(t *testing.T) {
	svc := setupTestService(t)
	out, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("sg-nlb"),
		Type: aws.String("network"),
	}, testAccountID)
	require.NoError(t, err)
	arn := *out.LoadBalancers[0].LoadBalancerArn

	_, err = svc.SetSecurityGroups(&elbv2.SetSecurityGroupsInput{
		LoadBalancerArn: aws.String(arn),
		SecurityGroups:  aws.StringSlice([]string{"sg-aaa"}),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2InvalidConfigurationRequest)
}

func TestSetSecurityGroups_CrossAccount(t *testing.T) {
	svc := setupTestService(t)
	arn := createLBArn(t, svc, "sg-xacct")

	_, err := svc.SetSecurityGroups(&elbv2.SetSecurityGroupsInput{
		LoadBalancerArn: aws.String(arn),
		SecurityGroups:  aws.StringSlice([]string{"sg-aaa"}),
	}, "999999999999")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2LoadBalancerNotFound)
}
