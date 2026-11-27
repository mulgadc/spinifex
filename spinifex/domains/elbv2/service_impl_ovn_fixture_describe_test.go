package elbv2

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	resourcearn "github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sealedOVN creates the fixture load balancer and clears the recorded calls.
func (h *ovnCreateHarness) sealedOVN(name string) *elbv2.LoadBalancer {
	lb, err := h.create(h.input(name, h.subnetID, h.groups[0]))
	require.NoError(h.t, err)
	h.takeOps()
	return lb
}

func (h *ovnCreateHarness) describe(accountID string, input *elbv2.DescribeLoadBalancersInput) (*elbv2.LoadBalancer, error) {
	out, err := h.svc.DescribeLoadBalancers(h.t.Context(), input, accountID)
	if err != nil {
		return nil, err
	}
	require.Len(h.t, out.LoadBalancers, 1)
	return out.LoadBalancers[0], nil
}

func byARN(arn string) *elbv2.DescribeLoadBalancersInput {
	return &elbv2.DescribeLoadBalancersInput{LoadBalancerArns: []*string{aws.String(arn)}}
}

// requireCalls asserts the calls since the last take, by bucket.
func (h *ovnCreateHarness) requireCalls(want map[string][]string) {
	h.t.Helper()
	for bucket, calls := range h.takeOps() {
		assert.Equal(h.t, want[bucket], calls, "calls on %s", bucket)
	}
}

func TestOVNFixtureDescribe_ExactARNReturnsCreateProjection(t *testing.T) {
	h := newOVNCreateHarness(t)
	created := h.sealedOVN("ovn-nlb")
	arn := aws.StringValue(created.LoadBalancerArn)
	claimGet := []string{"LeaderGet " + LBNameKey("ovn-nlb", testAccountID)}

	got, err := h.describe(testAccountID, byARN(arn))
	require.NoError(t, err)
	assert.Equal(t, created, got, "Describe returns the Create projection")
	assert.Equal(t, StateProvisioning, aws.StringValue(got.State.Code))
	assert.Nil(t, got.DNSName)
	require.Len(t, got.AvailabilityZones, 1)
	assert.Empty(t, got.AvailabilityZones[0].LoadBalancerAddresses)
	h.requireCalls(map[string][]string{KVBucketELBv2: claimGet})

	h.restart()
	got, err = h.describe(testAccountID, byARN(arn))
	require.NoError(t, err)
	assert.Equal(t, created, got, "the root is durable across restart")
	h.requireCalls(map[string][]string{KVBucketELBv2: claimGet})
}

func TestOVNFixtureDescribe_ScopeAndAbsence(t *testing.T) {
	h := newOVNCreateHarness(t)
	created := h.sealedOVN("ovn-nlb")
	arn := aws.StringValue(created.LoadBalancerArn)
	parsed, ok := resourcearn.ParseELBv2(arn)
	require.True(t, ok)
	id := parsed.Resource[len("net/ovn-nlb/"):]

	pending := newFixtureRoot("bbbb000000000002", "ovn-pending")
	_, sealed, err := h.svc.ovn.owners.Begin(t.Context(), pending)
	require.NoError(t, err)
	require.Nil(t, sealed)
	h.takeOps()

	for name, tc := range map[string]struct {
		account, arn string
		reads        []string
	}{
		"other account caller": {otherAccountID, arn, nil},
		"other account ARN":    {testAccountID, resourcearn.FormatELBv2LoadBalancer(fixtureRegion, otherAccountID, "ovn-nlb", id, LoadBalancerTypeNetwork), nil},
		"other region":         {testAccountID, resourcearn.FormatELBv2LoadBalancer("us-west-2", testAccountID, "ovn-nlb", id, LoadBalancerTypeNetwork), nil},
		"application path":     {testAccountID, resourcearn.FormatELBv2LoadBalancer(fixtureRegion, testAccountID, "ovn-nlb", id, LoadBalancerTypeApplication), nil},
		"malformed":            {testAccountID, "arn:aws:elasticloadbalancing:nothing", nil},
		"empty":                {testAccountID, "", nil},
		"other ID":             {testAccountID, resourcearn.FormatELBv2LoadBalancer(fixtureRegion, testAccountID, "ovn-nlb", "cccc000000000003", LoadBalancerTypeNetwork), []string{"LeaderGet " + LBNameKey("ovn-nlb", testAccountID)}},
		"unknown name":         {testAccountID, resourcearn.FormatELBv2LoadBalancer(fixtureRegion, testAccountID, "ovn-other", id, LoadBalancerTypeNetwork), []string{"LeaderGet " + LBNameKey("ovn-other", testAccountID)}},
		"pending owner":        {testAccountID, pending.LoadBalancerArn, []string{"LeaderGet " + LBNameKey("ovn-pending", testAccountID)}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.describe(tc.account, byARN(tc.arn))
			requireFenceCode(t, err, awserrors.ErrorELBv2LoadBalancerNotFound)
			h.requireCalls(map[string][]string{KVBucketELBv2: tc.reads})
		})
	}
}

// invalidNameARNs are network ARNs in the caller's account and region whose
// name segment is outside the load balancer name syntax.
func invalidNameARNs(t *testing.T, id string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, seg := range map[string]string{
		"dot":             "ovn.nlb",
		"wildcard":        "ovn*",
		"full wildcard":   ">",
		"underscore":      "ovn_nlb",
		"leading hyphen":  "-ovn",
		"trailing hyphen": "ovn-",
		"internal prefix": "internal-ovn",
		"too long":        "a234567890123456789012345678901bc",
		"empty":           "",
	} {
		arn := "arn:aws:elasticloadbalancing:" + fixtureRegion + ":" + testAccountID + ":loadbalancer/net/" + seg + "/" + id
		parsed, ok := resourcearn.ParseELBv2(arn)
		require.True(t, ok, "%s parses as an ELBv2 ARN", arn)
		require.Equal(t, fixtureRegion, parsed.Region)
		require.Equal(t, testAccountID, parsed.AccountID)
		require.Equal(t, resourcearn.ELBv2LoadBalancer, parsed.Kind)
		out[name] = arn
	}
	return out
}

func TestOVNFixtureARN_InvalidNameSegmentReadsNothing(t *testing.T) {
	h := newOVNCreateHarness(t)
	created := h.sealedOVN("ovn-nlb")
	parsed, ok := resourcearn.ParseELBv2(aws.StringValue(created.LoadBalancerArn))
	require.True(t, ok)
	id := parsed.Resource[len("net/ovn-nlb/"):]
	elbBefore := h.state(KVBucketELBv2)

	for name, arn := range invalidNameARNs(t, id) {
		t.Run(name, func(t *testing.T) {
			_, err := h.describe(testAccountID, byARN(arn))
			requireFenceCode(t, err, awserrors.ErrorELBv2LoadBalancerNotFound)
			h.requireCalls(nil)

			_, err = h.svc.DeleteLoadBalancer(t.Context(), &elbv2.DeleteLoadBalancerInput{LoadBalancerArn: aws.String(arn)}, testAccountID)
			require.NoError(t, err, "an absent ARN keeps the idempotent delete result")
			for bucket, calls := range h.takeOps() {
				assert.Empty(t, opsOf(calls, opKVWrites), "write on %s", bucket)
				for _, call := range calls {
					assert.NotContains(t, call, KeyPrefixLBName, "fixture claim read on %s", bucket)
					assert.NotContains(t, call, "ovnslot.", "fixture slot read on %s", bucket)
				}
			}
			assert.Equal(t, elbBefore, h.state(KVBucketELBv2))
		})
	}

	got, err := h.describe(testAccountID, byARN(aws.StringValue(created.LoadBalancerArn)))
	require.NoError(t, err, "the exact ARN still resolves")
	assert.Equal(t, created, got)
}

func TestOVNFixtureDescribe_RejectsUnsupportedShapes(t *testing.T) {
	h := newOVNCreateHarness(t)
	arn := aws.StringValue(h.sealedOVN("ovn-nlb").LoadBalancerArn)
	for name, input := range map[string]*elbv2.DescribeLoadBalancersInput{
		"no selector":   {},
		"names":         {Names: aws.StringSlice([]string{"ovn-nlb"})},
		"names and ARN": {Names: aws.StringSlice([]string{"ovn-nlb"}), LoadBalancerArns: aws.StringSlice([]string{arn})},
		"two ARNs":      {LoadBalancerArns: aws.StringSlice([]string{arn, arn})},
		"ARN and page":  {LoadBalancerArns: aws.StringSlice([]string{arn}), PageSize: aws.Int64(1)},
		"page size":     {PageSize: aws.Int64(1)},
		"marker":        {Marker: aws.String("m")},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.describe(testAccountID, input)
			requireFenceCode(t, err, awserrors.ErrorValidationError)
			h.requireCalls(nil)
		})
	}
}

func TestOVNFixtureDescribe_ReadFailureIsServerError(t *testing.T) {
	h := newOVNCreateHarness(t)
	arn := aws.StringValue(h.sealedOVN("ovn-nlb").LoadBalancerArn)
	h.elbKV.setFailGetPrefix(KeyPrefixLBName, errors.New("simulated read failure"))

	_, err := h.describe(testAccountID, byARN(arn))
	requireFenceCode(t, err, awserrors.ErrorServerInternal)
	_, err = h.svc.DeleteLoadBalancer(t.Context(), &elbv2.DeleteLoadBalancerInput{LoadBalancerArn: aws.String(arn)}, testAccountID)
	requireFenceCode(t, err, awserrors.ErrorServerInternal)
	for bucket, calls := range h.takeOps() {
		assert.Empty(t, opsOf(calls, opKVWrites), "write on %s after a failed read", bucket)
		assert.Empty(t, opsOf(calls, opKVLists), "bucket-wide read on %s", bucket)
	}
}

func TestOVNFixtureDelete_RefusesSealedRootAndKeepsParents(t *testing.T) {
	h := newOVNCreateHarness(t)
	created := h.sealedOVN("ovn-nlb")
	arn := aws.StringValue(created.LoadBalancerArn)
	elbBefore := h.state(KVBucketELBv2)
	sideBefore := h.sideEffects()

	_, err := h.svc.DeleteLoadBalancer(t.Context(), &elbv2.DeleteLoadBalancerInput{LoadBalancerArn: aws.String(arn)}, testAccountID)
	requireFenceCode(t, err, awserrors.ErrorOperationNotPermitted)
	h.requireCalls(map[string][]string{KVBucketELBv2: {"LeaderGet " + LBNameKey("ovn-nlb", testAccountID)}})

	got, err := h.describe(testAccountID, byARN(arn))
	require.NoError(t, err)
	assert.Equal(t, created, got, "the refused delete keeps the root describable")
	assert.Equal(t, elbBefore, h.state(KVBucketELBv2), "claim and both slots survive; no health record")
	assert.Equal(t, sideBefore, h.sideEffects())
	assert.Zero(t, h.launches())
	h.launcher.mu.Lock()
	assert.Empty(t, h.launcher.terminateCalls)
	h.launcher.mu.Unlock()

	_, err = h.vpcs.DeleteSubnet(t.Context(), &ec2.DeleteSubnetInput{SubnetId: aws.String(h.subnetID)}, testAccountID)
	require.ErrorContains(t, err, awserrors.ErrorDependencyViolation, "the subnet slot still blocks")
}

func TestOVNFixtureDelete_OtherARNsKeepExistingResult(t *testing.T) {
	h := newOVNCreateHarness(t)
	created := h.sealedOVN("ovn-nlb")
	arn := aws.StringValue(created.LoadBalancerArn)
	pending := newFixtureRoot("bbbb000000000002", "ovn-pending")
	_, _, err := h.svc.ovn.owners.Begin(t.Context(), pending)
	require.NoError(t, err)
	elbBefore := h.state(KVBucketELBv2)
	h.takeOps()

	for name, tc := range map[string]struct{ account, arn string }{
		"other account caller": {otherAccountID, arn},
		"unknown ARN":          {testAccountID, resourcearn.FormatELBv2LoadBalancer(fixtureRegion, testAccountID, "ovn-other", "cccc000000000003", LoadBalancerTypeNetwork)},
		"pending owner":        {testAccountID, pending.LoadBalancerArn},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.svc.DeleteLoadBalancer(t.Context(), &elbv2.DeleteLoadBalancerInput{LoadBalancerArn: aws.String(tc.arn)}, tc.account)
			require.NoError(t, err, "an absent or unowned ARN keeps the idempotent result")
			for bucket, calls := range h.takeOps() {
				assert.Empty(t, opsOf(calls, opKVWrites), "write on %s", bucket)
			}
			assert.Equal(t, elbBefore, h.state(KVBucketELBv2))
		})
	}
	got, err := h.describe(testAccountID, byARN(arn))
	require.NoError(t, err)
	assert.Equal(t, created, got)
}

func TestOVNFixtureARN_OtherOperationsWriteNothing(t *testing.T) {
	h := newOVNCreateHarness(t)
	created := h.sealedOVN("ovn-nlb")
	arn := aws.StringValue(created.LoadBalancerArn)
	elbBefore := h.state(KVBucketELBv2)
	sideBefore := h.sideEffects()
	ctx := t.Context()
	lbArn := aws.String(arn)
	tgArn := aws.String(resourcearn.FormatELBv2TargetGroup(fixtureRegion, testAccountID, "ovn-tg", "dddd000000000004"))

	for name, tc := range map[string]struct {
		call func() error
		code string
	}{
		"CreateListener": {func() error {
			_, err := h.svc.CreateListener(ctx, &elbv2.CreateListenerInput{
				LoadBalancerArn: lbArn, Protocol: aws.String("TCP"), Port: aws.Int64(80),
				DefaultActions: []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgArn}},
			}, testAccountID)
			return err
		}, awserrors.ErrorELBv2TargetGroupNotFound},
		"DescribeListeners": {func() error {
			_, err := h.svc.DescribeListeners(ctx, &elbv2.DescribeListenersInput{LoadBalancerArn: lbArn}, testAccountID)
			return err
		}, awserrors.ErrorELBv2LoadBalancerNotFound},
		"AddTags": {func() error {
			_, err := h.svc.AddTags(ctx, &elbv2.AddTagsInput{ResourceArns: []*string{lbArn}, Tags: []*elbv2.Tag{{Key: aws.String("k"), Value: aws.String("v")}}}, testAccountID)
			return err
		}, awserrors.ErrorELBv2LoadBalancerNotFound},
		"RemoveTags": {func() error {
			_, err := h.svc.RemoveTags(ctx, &elbv2.RemoveTagsInput{ResourceArns: []*string{lbArn}, TagKeys: aws.StringSlice([]string{"k"})}, testAccountID)
			return err
		}, awserrors.ErrorELBv2LoadBalancerNotFound},
		"DescribeTags": {func() error {
			_, err := h.svc.DescribeTags(ctx, &elbv2.DescribeTagsInput{ResourceArns: []*string{lbArn}}, testAccountID)
			return err
		}, awserrors.ErrorELBv2LoadBalancerNotFound},
		"ModifyLoadBalancerAttributes": {func() error {
			_, err := h.svc.ModifyLoadBalancerAttributes(ctx, &elbv2.ModifyLoadBalancerAttributesInput{
				LoadBalancerArn: lbArn, Attributes: []*elbv2.LoadBalancerAttribute{{Key: aws.String("deletion_protection.enabled"), Value: aws.String("true")}},
			}, testAccountID)
			return err
		}, awserrors.ErrorELBv2LoadBalancerNotFound},
		"DescribeLoadBalancerAttributes": {func() error {
			_, err := h.svc.DescribeLoadBalancerAttributes(ctx, &elbv2.DescribeLoadBalancerAttributesInput{LoadBalancerArn: lbArn}, testAccountID)
			return err
		}, awserrors.ErrorELBv2LoadBalancerNotFound},
		"SetSubnets": {func() error {
			_, err := h.svc.SetSubnets(ctx, &elbv2.SetSubnetsInput{LoadBalancerArn: lbArn, Subnets: aws.StringSlice([]string{h.subnet2ID})}, testAccountID)
			return err
		}, awserrors.ErrorELBv2LoadBalancerNotFound},
		"SetSecurityGroups": {func() error {
			_, err := h.svc.SetSecurityGroups(ctx, &elbv2.SetSecurityGroupsInput{LoadBalancerArn: lbArn, SecurityGroups: aws.StringSlice([]string{h.groups[1]})}, testAccountID)
			return err
		}, awserrors.ErrorELBv2LoadBalancerNotFound},
		"SetIpAddressType": {func() error {
			_, err := h.svc.SetIpAddressType(ctx, &elbv2.SetIpAddressTypeInput{LoadBalancerArn: lbArn, IpAddressType: aws.String(IPAddressTypeIPv4)}, testAccountID)
			return err
		}, awserrors.ErrorELBv2LoadBalancerNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			h.takeOps()
			requireFenceCode(t, tc.call(), tc.code)
			for bucket, calls := range h.takeOps() {
				assert.Empty(t, opsOf(calls, opKVWrites), "write on %s", bucket)
			}
			assert.Equal(t, elbBefore, h.state(KVBucketELBv2), "no listener, tag, attribute or health write")
			assert.Equal(t, sideBefore, h.sideEffects(), "no ENI, group, route or allocation write")
			assert.Zero(t, h.launches())
		})
	}
	got, err := h.describe(testAccountID, byARN(arn))
	require.NoError(t, err)
	assert.Equal(t, created, got)
}

func TestOVNFixture_DefaultServiceKeepsApplianceDescribeAndDelete(t *testing.T) {
	svc, _, subnetID := setupMicrovmTestService(t)
	require.Nil(t, svc.ovn)
	svc.InstanceLauncher = &mockSystemInstanceLauncher{launchResult: &SystemInstanceOutput{InstanceID: "i-default", PrivateIP: "10.0.1.9"}}
	out, err := svc.CreateLoadBalancer(t.Context(), &elbv2.CreateLoadBalancerInput{
		Name: aws.String("appliance-nlb"), Type: aws.String(LoadBalancerTypeNetwork),
		Scheme: aws.String(SchemeInternal), Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	svc.WaitLaunches()
	arn := out.LoadBalancers[0].LoadBalancerArn

	desc, err := svc.DescribeLoadBalancers(t.Context(), &elbv2.DescribeLoadBalancersInput{Names: aws.StringSlice([]string{"appliance-nlb"})}, testAccountID)
	require.NoError(t, err, "the appliance path still describes by name")
	require.Len(t, desc.LoadBalancers, 1)
	assert.Equal(t, aws.StringValue(arn), aws.StringValue(desc.LoadBalancers[0].LoadBalancerArn))
	assert.NotNil(t, desc.LoadBalancers[0].DNSName)

	_, err = svc.DeleteLoadBalancer(t.Context(), &elbv2.DeleteLoadBalancerInput{LoadBalancerArn: arn}, testAccountID)
	require.NoError(t, err, "the appliance path still deletes")
	_, err = svc.DescribeLoadBalancers(t.Context(), &elbv2.DescribeLoadBalancersInput{LoadBalancerArns: []*string{arn}}, testAccountID)
	requireFenceCode(t, err, awserrors.ErrorELBv2LoadBalancerNotFound)
}
