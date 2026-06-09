package handlers_elbv2

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tagsFor is a small helper that reads back a resource's tags via DescribeTags
// as a plain map for assertions.
func tagsFor(t *testing.T, svc *ELBv2ServiceImpl, arn string) map[string]string {
	t.Helper()
	out, err := svc.DescribeTags(&elbv2.DescribeTagsInput{ResourceArns: []*string{aws.String(arn)}}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.TagDescriptions, 1)
	m := make(map[string]string)
	for _, tag := range out.TagDescriptions[0].Tags {
		m[*tag.Key] = *tag.Value
	}
	return m
}

func TestAddTags_LoadBalancer(t *testing.T) {
	svc := setupTestService(t)
	lbOut, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("addtags-lb"),
		Tags: []*elbv2.Tag{{Key: aws.String("Env"), Value: aws.String("dev")}},
	}, testAccountID)
	require.NoError(t, err)
	arn := *lbOut.LoadBalancers[0].LoadBalancerArn

	_, err = svc.AddTags(&elbv2.AddTagsInput{
		ResourceArns: []*string{aws.String(arn)},
		Tags: []*elbv2.Tag{
			{Key: aws.String("App"), Value: aws.String("nginx")},
			{Key: aws.String("Env"), Value: aws.String("prod")}, // overwrite
		},
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"App": "nginx", "Env": "prod"}, tagsFor(t, svc, arn))
}

func TestAddTags_Listener(t *testing.T) {
	svc := setupTestService(t)
	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("addtags-lst-lb")}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("addtags-lst-tg")}, testAccountID)
	lstOut, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn},
		},
	}, testAccountID)
	require.NoError(t, err)
	arn := *lstOut.Listeners[0].ListenerArn

	_, err = svc.AddTags(&elbv2.AddTagsInput{
		ResourceArns: []*string{aws.String(arn)},
		Tags:         []*elbv2.Tag{{Key: aws.String("team"), Value: aws.String("platform")}},
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"team": "platform"}, tagsFor(t, svc, arn))
}

func TestAddTags_CreateTimeTagsPreserved(t *testing.T) {
	svc := setupTestService(t)
	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("addtags-tg-lb")}, testAccountID)
	tgOut, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name: aws.String("addtags-tg"),
		Tags: []*elbv2.Tag{{Key: aws.String("owner"), Value: aws.String("alice")}},
	}, testAccountID)
	require.NoError(t, err)
	_ = lbOut
	arn := *tgOut.TargetGroups[0].TargetGroupArn

	_, err = svc.AddTags(&elbv2.AddTagsInput{
		ResourceArns: []*string{aws.String(arn)},
		Tags:         []*elbv2.Tag{{Key: aws.String("cost-center"), Value: aws.String("42")}},
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"owner": "alice", "cost-center": "42"}, tagsFor(t, svc, arn))
}

func TestAddTags_NotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.AddTags(&elbv2.AddTagsInput{
		ResourceArns: []*string{aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/missing/lb-deadbeef")},
		Tags:         []*elbv2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2LoadBalancerNotFound)
}

func TestAddTags_CrossAccount(t *testing.T) {
	svc := setupTestService(t)
	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("addtags-xacct")}, testAccountID)
	arn := *lbOut.LoadBalancers[0].LoadBalancerArn

	_, err := svc.AddTags(&elbv2.AddTagsInput{
		ResourceArns: []*string{aws.String(arn)},
		Tags:         []*elbv2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, "999999999999")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2LoadBalancerNotFound)

	// Owner's tags untouched.
	assert.Empty(t, tagsFor(t, svc, arn))
}

func TestAddTags_MissingParams(t *testing.T) {
	svc := setupTestService(t)
	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("addtags-mp")}, testAccountID)
	arn := lbOut.LoadBalancers[0].LoadBalancerArn

	_, err := svc.AddTags(&elbv2.AddTagsInput{ResourceArns: []*string{arn}}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)

	_, err = svc.AddTags(&elbv2.AddTagsInput{Tags: []*elbv2.Tag{{Key: aws.String("k"), Value: aws.String("v")}}}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)
}

func TestAddTags_EmptyKeyRejected(t *testing.T) {
	svc := setupTestService(t)
	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("addtags-ek")}, testAccountID)
	arn := lbOut.LoadBalancers[0].LoadBalancerArn

	_, err := svc.AddTags(&elbv2.AddTagsInput{
		ResourceArns: []*string{arn},
		Tags:         []*elbv2.Tag{{Key: aws.String(""), Value: aws.String("v")}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
}

func TestRemoveTags_LoadBalancer(t *testing.T) {
	svc := setupTestService(t)
	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("rmtags-lb"),
		Tags: []*elbv2.Tag{
			{Key: aws.String("App"), Value: aws.String("nginx")},
			{Key: aws.String("Env"), Value: aws.String("prod")},
		},
	}, testAccountID)
	arn := *lbOut.LoadBalancers[0].LoadBalancerArn

	_, err := svc.RemoveTags(&elbv2.RemoveTagsInput{
		ResourceArns: []*string{aws.String(arn)},
		TagKeys:      []*string{aws.String("Env")},
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"App": "nginx"}, tagsFor(t, svc, arn))
}

func TestRemoveTags_Idempotent(t *testing.T) {
	svc := setupTestService(t)
	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("rmtags-idem"),
		Tags: []*elbv2.Tag{{Key: aws.String("App"), Value: aws.String("nginx")}},
	}, testAccountID)
	arn := *lbOut.LoadBalancers[0].LoadBalancerArn

	// Removing an absent key is a no-op, not an error.
	_, err := svc.RemoveTags(&elbv2.RemoveTagsInput{
		ResourceArns: []*string{aws.String(arn)},
		TagKeys:      []*string{aws.String("DoesNotExist")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"App": "nginx"}, tagsFor(t, svc, arn))
}

func TestRemoveTags_NotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.RemoveTags(&elbv2.RemoveTagsInput{
		ResourceArns: []*string{aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/missing/tg-deadbeef")},
		TagKeys:      []*string{aws.String("k")},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2TargetGroupNotFound)
}

func TestRemoveTags_MissingParams(t *testing.T) {
	svc := setupTestService(t)
	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("rmtags-mp")}, testAccountID)
	arn := lbOut.LoadBalancers[0].LoadBalancerArn

	_, err := svc.RemoveTags(&elbv2.RemoveTagsInput{ResourceArns: []*string{arn}}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)
}
