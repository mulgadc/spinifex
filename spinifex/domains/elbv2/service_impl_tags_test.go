package elbv2

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tagsFor is a small helper that reads back a resource's tags via DescribeTags
// as a plain map for assertions.
func tagsFor(t *testing.T, svc *ELBv2ServiceImpl, arn string) map[string]string {
	t.Helper()
	out, err := svc.DescribeTags(context.Background(), &elbv2.DescribeTagsInput{ResourceArns: []*string{aws.String(arn)}}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.TagDescriptions, 1)
	m := make(map[string]string)
	for _, tag := range out.TagDescriptions[0].Tags {
		m[*tag.Key] = *tag.Value
	}
	return m
}

func TestAddTags_LoadBalancer(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lbOut, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name: aws.String("addtags-lb"),
		Tags: []*elbv2.Tag{{Key: aws.String("Env"), Value: aws.String("dev")}},
	}, testAccountID)
	require.NoError(t, err)
	arn := *lbOut.LoadBalancers[0].LoadBalancerArn

	_, err = svc.AddTags(context.Background(), &elbv2.AddTagsInput{
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
	t.Parallel()
	svc := setupTestService(t)
	lbOut, _ := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{Name: aws.String("addtags-lst-lb")}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{Name: aws.String("addtags-lst-tg")}, testAccountID)
	lstOut, err := svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn},
		},
	}, testAccountID)
	require.NoError(t, err)
	arn := *lstOut.Listeners[0].ListenerArn

	_, err = svc.AddTags(context.Background(), &elbv2.AddTagsInput{
		ResourceArns: []*string{aws.String(arn)},
		Tags:         []*elbv2.Tag{{Key: aws.String("team"), Value: aws.String("platform")}},
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"team": "platform"}, tagsFor(t, svc, arn))
}

func TestAddTags_CreateTimeTagsPreserved(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lbOut, _ := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{Name: aws.String("addtags-tg-lb")}, testAccountID)
	tgOut, err := svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{
		Name: aws.String("addtags-tg"),
		Tags: []*elbv2.Tag{{Key: aws.String("owner"), Value: aws.String("alice")}},
	}, testAccountID)
	require.NoError(t, err)
	_ = lbOut
	arn := *tgOut.TargetGroups[0].TargetGroupArn

	_, err = svc.AddTags(context.Background(), &elbv2.AddTagsInput{
		ResourceArns: []*string{aws.String(arn)},
		Tags:         []*elbv2.Tag{{Key: aws.String("cost-center"), Value: aws.String("42")}},
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"owner": "alice", "cost-center": "42"}, tagsFor(t, svc, arn))
}

func TestAddTags_NotFound(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	_, err := svc.AddTags(context.Background(), &elbv2.AddTagsInput{
		ResourceArns: []*string{aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/missing/lb-deadbeef")},
		Tags:         []*elbv2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2LoadBalancerNotFound)
}

func TestAddTags_CrossAccount(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lbOut, _ := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{Name: aws.String("addtags-xacct")}, testAccountID)
	arn := *lbOut.LoadBalancers[0].LoadBalancerArn

	_, err := svc.AddTags(context.Background(), &elbv2.AddTagsInput{
		ResourceArns: []*string{aws.String(arn)},
		Tags:         []*elbv2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, "999999999999")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2LoadBalancerNotFound)

	// Owner's tags untouched.
	assert.Empty(t, tagsFor(t, svc, arn))
}

func TestAddTags_MissingParams(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lbOut, _ := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{Name: aws.String("addtags-mp")}, testAccountID)
	arn := lbOut.LoadBalancers[0].LoadBalancerArn

	_, err := svc.AddTags(context.Background(), &elbv2.AddTagsInput{ResourceArns: []*string{arn}}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)

	_, err = svc.AddTags(context.Background(), &elbv2.AddTagsInput{Tags: []*elbv2.Tag{{Key: aws.String("k"), Value: aws.String("v")}}}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)
}

func numberedTags(prefix string, n int) []*elbv2.Tag {
	out := make([]*elbv2.Tag, 0, n)
	for i := range n {
		out = append(out, &elbv2.Tag{Key: aws.String(fmt.Sprintf("%s-%02d", prefix, i)), Value: aws.String("v")})
	}
	return out
}

// badTagCases are tag lists the ELBv2 model or service limits refuse, with
// the error code AWS returns for each.
var badTagCases = []struct {
	name string
	tags []*elbv2.Tag
	code string
}{
	{"EmptyList", []*elbv2.Tag{}, awserrors.ErrorValidationError},
	{"NilKey", []*elbv2.Tag{{Value: aws.String("v")}}, awserrors.ErrorValidationError},
	{"EmptyKey", []*elbv2.Tag{{Key: aws.String(""), Value: aws.String("v")}}, awserrors.ErrorValidationError},
	{"KeyTooLong", []*elbv2.Tag{{Key: aws.String(strings.Repeat("k", 129)), Value: aws.String("v")}}, awserrors.ErrorValidationError},
	{"ValueTooLong", []*elbv2.Tag{{Key: aws.String("k"), Value: aws.String(strings.Repeat("v", 257))}}, awserrors.ErrorValidationError},
	{"KeyPattern", []*elbv2.Tag{{Key: aws.String("bad*key"), Value: aws.String("v")}}, awserrors.ErrorValidationError},
	{"ValuePattern", []*elbv2.Tag{{Key: aws.String("k"), Value: aws.String("bad;value")}}, awserrors.ErrorValidationError},
	{"DuplicateKey", []*elbv2.Tag{{Key: aws.String("k"), Value: aws.String("a")}, {Key: aws.String("k"), Value: aws.String("b")}}, awserrors.ErrorELBv2DuplicateTagKeys},
	{"TooMany", numberedTags("k", 51), awserrors.ErrorELBv2TooManyTags},
}

func TestAddTags_RejectsBadTags(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lbOut, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{Name: aws.String("addtags-bad")}, testAccountID)
	require.NoError(t, err)
	arn := lbOut.LoadBalancers[0].LoadBalancerArn

	for _, tc := range badTagCases {
		if tc.name == "EmptyList" {
			continue // AddTags requires Tags, so an empty list is MissingParameter
		}
		_, err := svc.AddTags(context.Background(), &elbv2.AddTagsInput{ResourceArns: []*string{arn}, Tags: tc.tags}, testAccountID)
		assert.Equal(t, tc.code, awserrors.ValidErrorCodeFromError(err), tc.name)
	}
	assert.Empty(t, tagsFor(t, svc, *arn))
}

// The 50-tag limit counts the resource's existing tags; overwriting an
// existing key does not add to the total.
func TestAddTags_LimitCountsExistingTags(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lbOut, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name: aws.String("addtags-limit"),
		Tags: numberedTags("old", 49),
	}, testAccountID)
	require.NoError(t, err)
	arn := lbOut.LoadBalancers[0].LoadBalancerArn

	_, err = svc.AddTags(context.Background(), &elbv2.AddTagsInput{ResourceArns: []*string{arn}, Tags: numberedTags("new", 2)}, testAccountID)
	assert.Equal(t, awserrors.ErrorELBv2TooManyTags, awserrors.ValidErrorCodeFromError(err))
	assert.Len(t, tagsFor(t, svc, *arn), 49)

	tags := append(numberedTags("old", 1), numberedTags("new", 1)...)
	_, err = svc.AddTags(context.Background(), &elbv2.AddTagsInput{ResourceArns: []*string{arn}, Tags: tags}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, tagsFor(t, svc, *arn), 50)
}

func TestAddTags_MissingValueStoredEmpty(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lbOut, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{Name: aws.String("addtags-noval")}, testAccountID)
	require.NoError(t, err)
	arn := lbOut.LoadBalancers[0].LoadBalancerArn

	_, err = svc.AddTags(context.Background(), &elbv2.AddTagsInput{ResourceArns: []*string{arn}, Tags: []*elbv2.Tag{{Key: aws.String("k")}}}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"k": ""}, tagsFor(t, svc, *arn))
}

func TestCreateLoadBalancer_RejectsBadTags(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	for _, tc := range badTagCases {
		_, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{Name: aws.String("badtags-lb"), Tags: tc.tags}, testAccountID)
		assert.Equal(t, tc.code, awserrors.ValidErrorCodeFromError(err), tc.name)
	}

	// No rejected create may have claimed the name.
	out, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name: aws.String("badtags-lb"),
		Tags: []*elbv2.Tag{{Key: aws.String("k")}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"k": ""}, tagsFor(t, svc, *out.LoadBalancers[0].LoadBalancerArn))
}

// Each other create call that accepts tags runs them through the same validator.
func TestCreateTaggedResources_RejectBadTags(t *testing.T) {
	t.Parallel()
	env := newRuleTestEnv(t, "badtags")
	cases := []struct {
		tags []*elbv2.Tag
		code string
	}{
		{numberedTags("k", 51), awserrors.ErrorELBv2TooManyTags},
		{[]*elbv2.Tag{{Key: aws.String("k"), Value: aws.String("a")}, {Key: aws.String("k"), Value: aws.String("b")}}, awserrors.ErrorELBv2DuplicateTagKeys},
		{[]*elbv2.Tag{{Key: aws.String("bad*key")}}, awserrors.ErrorValidationError},
	}

	for _, tc := range cases {
		_, err := env.svc.CreateTargetGroup(context.Background(), &elbv2.CreateTargetGroupInput{Name: aws.String("badtags-tg3"), Tags: tc.tags}, testAccountID)
		assert.Equal(t, tc.code, awserrors.ValidErrorCodeFromError(err), "CreateTargetGroup")
		_, err = env.svc.CreateListener(context.Background(), &elbv2.CreateListenerInput{
			LoadBalancerArn: aws.String(env.lbArn),
			Protocol:        aws.String("HTTP"),
			Port:            aws.Int64(8080),
			DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgArn)}},
			Tags:            tc.tags,
		}, testAccountID)
		assert.Equal(t, tc.code, awserrors.ValidErrorCodeFromError(err), "CreateListener")
		_, err = env.svc.CreateRule(context.Background(), &elbv2.CreateRuleInput{
			ListenerArn: aws.String(env.listenerArn),
			Priority:    aws.Int64(10),
			Conditions:  []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: aws.StringSlice([]string{"/x"})}},
			Actions:     []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(env.tgArn)}},
			Tags:        tc.tags,
		}, testAccountID)
		assert.Equal(t, tc.code, awserrors.ValidErrorCodeFromError(err), "CreateRule")
	}
}

func TestRemoveTags_LoadBalancer(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lbOut, _ := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name: aws.String("rmtags-lb"),
		Tags: []*elbv2.Tag{
			{Key: aws.String("App"), Value: aws.String("nginx")},
			{Key: aws.String("Env"), Value: aws.String("prod")},
		},
	}, testAccountID)
	arn := *lbOut.LoadBalancers[0].LoadBalancerArn

	_, err := svc.RemoveTags(context.Background(), &elbv2.RemoveTagsInput{
		ResourceArns: []*string{aws.String(arn)},
		TagKeys:      []*string{aws.String("Env")},
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"App": "nginx"}, tagsFor(t, svc, arn))
}

func TestRemoveTags_Idempotent(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lbOut, _ := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name: aws.String("rmtags-idem"),
		Tags: []*elbv2.Tag{{Key: aws.String("App"), Value: aws.String("nginx")}},
	}, testAccountID)
	arn := *lbOut.LoadBalancers[0].LoadBalancerArn

	// Removing an absent key is a no-op, not an error.
	_, err := svc.RemoveTags(context.Background(), &elbv2.RemoveTagsInput{
		ResourceArns: []*string{aws.String(arn)},
		TagKeys:      []*string{aws.String("DoesNotExist")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"App": "nginx"}, tagsFor(t, svc, arn))
}

func TestRemoveTags_NotFound(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	_, err := svc.RemoveTags(context.Background(), &elbv2.RemoveTagsInput{
		ResourceArns: []*string{aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/missing/tg-deadbeef")},
		TagKeys:      []*string{aws.String("k")},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2TargetGroupNotFound)
}

func TestRemoveTags_MissingParams(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lbOut, _ := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{Name: aws.String("rmtags-mp")}, testAccountID)
	arn := lbOut.LoadBalancers[0].LoadBalancerArn

	_, err := svc.RemoveTags(context.Background(), &elbv2.RemoveTagsInput{ResourceArns: []*string{arn}}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)
}

func TestRemoveTags_LaterUnknownARNRemovesNothing(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lbOut, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name: aws.String("rmtags-atomic"),
		Tags: []*elbv2.Tag{{Key: aws.String("App"), Value: aws.String("nginx")}},
	}, testAccountID)
	require.NoError(t, err)
	arn := *lbOut.LoadBalancers[0].LoadBalancerArn
	missing := "arn:aws:elasticloadbalancing:us-east-1:" + testAccountID + ":targetgroup/missing/0123456789abcdef"

	_, err = svc.RemoveTags(context.Background(), &elbv2.RemoveTagsInput{
		ResourceArns: []*string{aws.String(arn), aws.String(missing)},
		TagKeys:      []*string{aws.String("App")},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorELBv2TargetGroupNotFound, err.Error())
	assert.Equal(t, map[string]string{"App": "nginx"}, tagsFor(t, svc, arn))
}

func TestAddTags_LaterEmptyARNTagsNothing(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lbOut, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{Name: aws.String("addtags-empty-arn")}, testAccountID)
	require.NoError(t, err)
	arn := *lbOut.LoadBalancers[0].LoadBalancerArn

	_, err = svc.AddTags(context.Background(), &elbv2.AddTagsInput{
		ResourceArns: []*string{aws.String(arn), aws.String("")},
		Tags:         []*elbv2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
	assert.Empty(t, tagsFor(t, svc, arn))
}

func TestAddTags_RepeatedARN(t *testing.T) {
	t.Parallel()
	svc := setupTestService(t)
	lbOut, err := svc.CreateLoadBalancer(context.Background(), &elbv2.CreateLoadBalancerInput{
		Name: aws.String("addtags-repeat"),
		Tags: []*elbv2.Tag{{Key: aws.String("App"), Value: aws.String("nginx")}},
	}, testAccountID)
	require.NoError(t, err)
	arn := *lbOut.LoadBalancers[0].LoadBalancerArn

	_, err = svc.AddTags(context.Background(), &elbv2.AddTagsInput{
		ResourceArns: []*string{aws.String(arn), aws.String(arn)},
		Tags:         []*elbv2.Tag{{Key: aws.String("Env"), Value: aws.String("prod")}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"App": "nginx", "Env": "prod"}, tagsFor(t, svc, arn))
}

// hookKV runs hook once, just before the first Get of trigger.
type hookKV struct {
	jetstream.KeyValue

	once    sync.Once
	trigger string
	hook    func()
}

func (k *hookKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if key == k.trigger {
		k.once.Do(k.hook)
	}
	return k.KeyValue.Get(ctx, key)
}

func TestAddTags_KeepsWriteMadeWhileLaterARNIsValidated(t *testing.T) {
	svc := setupTestService(t)
	ctx := context.Background()
	o1, err := svc.CreateLoadBalancer(ctx, &elbv2.CreateLoadBalancerInput{Name: aws.String("concurrent-lb1")}, testAccountID)
	require.NoError(t, err)
	o2, err := svc.CreateLoadBalancer(ctx, &elbv2.CreateLoadBalancerInput{Name: aws.String("concurrent-lb2")}, testAccountID)
	require.NoError(t, err)
	arn1, arn2 := *o1.LoadBalancers[0].LoadBalancerArn, *o2.LoadBalancers[0].LoadBalancerArn
	lb1, err := svc.store.GetLoadBalancerByArn(ctx, arn1)
	require.NoError(t, err)
	lb2, err := svc.store.GetLoadBalancerByArn(ctx, arn2)
	require.NoError(t, err)

	inner := svc.store.kv
	svc.store.kv = &hookKV{KeyValue: inner, trigger: KeyPrefixLB + lb2.LoadBalancerID, hook: func() {
		cur, gerr := svc.store.GetLoadBalancer(ctx, lb1.LoadBalancerID)
		require.NoError(t, gerr)
		cur.State = "concurrent-state"
		b, merr := json.Marshal(cur)
		require.NoError(t, merr)
		_, perr := inner.Put(ctx, KeyPrefixLB+lb1.LoadBalancerID, b)
		require.NoError(t, perr)
	}}

	_, err = svc.AddTags(ctx, &elbv2.AddTagsInput{
		ResourceArns: []*string{aws.String(arn1), aws.String(arn2)},
		Tags:         []*elbv2.Tag{{Key: aws.String("k"), Value: aws.String("v")}},
	}, testAccountID)
	require.NoError(t, err)

	svc.store.kv = inner
	got, err := svc.store.GetLoadBalancer(ctx, lb1.LoadBalancerID)
	require.NoError(t, err)
	assert.Equal(t, "concurrent-state", got.State)
	assert.Equal(t, map[string]string{"k": "v"}, got.Tags)
}
