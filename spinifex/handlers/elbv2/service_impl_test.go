package handlers_elbv2

import (
	"encoding/xml"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestService(t *testing.T) *ELBv2ServiceImpl {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	svc, err := NewELBv2ServiceImplWithNATS(nil, nc)
	require.NoError(t, err)
	// Seed the certificate ARNs the listener tests attach so the fail-closed
	// cert validation in Create/Modify/AddListenerCertificates resolves them.
	putTestCert(t, svc, testCertArn, testAccountID, "LEAF", "", "KEY")
	putTestCert(t, svc, testCertArn2, testAccountID, "LEAF", "", "KEY")
	return svc
}

// --- Load Balancer tests ---

func TestCreateLoadBalancer(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:           aws.String("my-alb"),
		Subnets:        []*string{aws.String("subnet-aaa")},
		SecurityGroups: []*string{aws.String("sg-111")},
	}, testAccountID)

	require.NoError(t, err)
	require.Len(t, out.LoadBalancers, 1)
	lb := out.LoadBalancers[0]
	assert.Equal(t, "my-alb", *lb.LoadBalancerName)
	assert.Equal(t, "internet-facing", *lb.Scheme)
	assert.Equal(t, "application", *lb.Type)
	assert.Equal(t, "active", *lb.State.Code)
	assert.Contains(t, *lb.DNSName, "my-alb")
	assert.Contains(t, *lb.LoadBalancerArn, "loadbalancer/app/my-alb")
}

func TestCreateLoadBalancer_InternalScheme(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:   aws.String("internal-alb"),
		Scheme: aws.String("internal"),
	}, testAccountID)

	require.NoError(t, err)
	assert.Equal(t, "internal", *out.LoadBalancers[0].Scheme)
}

func TestCreateLoadBalancer_InvalidScheme(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:   aws.String("bad-scheme"),
		Scheme: aws.String("banana"),
	}, testAccountID)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidScheme")
}

func TestCreateLoadBalancer_DuplicateName(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("dup-alb"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("dup-alb"),
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "DuplicateLoadBalancerName")
}

func TestCreateLoadBalancer_MissingName(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{}, testAccountID)
	assert.Error(t, err)
}

func TestCreateLoadBalancer_WithTags(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("tagged-alb"),
		Tags: []*elbv2.Tag{
			{Key: aws.String("Env"), Value: aws.String("test")},
		},
	}, testAccountID)

	require.NoError(t, err)
	// Tags are stored internally, verify via describe
	desc, err := svc.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{
		Names: []*string{aws.String("tagged-alb")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.LoadBalancers, 1)
	assert.Equal(t, *out.LoadBalancers[0].LoadBalancerArn, *desc.LoadBalancers[0].LoadBalancerArn)
}

func TestCreateLoadBalancer_NetworkType(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("my-nlb"),
		Type: aws.String("network"),
	}, testAccountID)

	require.NoError(t, err)
	require.Len(t, out.LoadBalancers, 1)
	lb := out.LoadBalancers[0]
	assert.Equal(t, "network", *lb.Type)
	assert.Contains(t, *lb.LoadBalancerArn, "loadbalancer/net/my-nlb")
	assert.Equal(t, "active", *lb.State.Code)
}

func TestCreateLoadBalancer_NetworkType_RejectsSecurityGroups(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:           aws.String("nlb-with-sg"),
		Type:           aws.String("network"),
		SecurityGroups: []*string{aws.String("sg-111")},
	}, testAccountID)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidConfigurationRequest")
}

func TestCreateLoadBalancer_NLBCrossZoneAttribute(t *testing.T) {
	svc := setupTestService(t)

	// NLB: no stored attributes, Describe should return default cross-zone=false.
	nlbOut, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("nlb-cz"),
		Type: aws.String("network"),
	}, testAccountID)
	require.NoError(t, err)
	nlbRec, err := svc.store.GetLoadBalancerByName("nlb-cz", testAccountID)
	require.NoError(t, err)
	assert.Nil(t, nlbRec.Attributes, "NLB should store nil attributes — defaults come from the handler")

	nlbDesc, err := svc.DescribeLoadBalancerAttributes(&elbv2.DescribeLoadBalancerAttributesInput{
		LoadBalancerArn: nlbOut.LoadBalancers[0].LoadBalancerArn,
	}, testAccountID)
	require.NoError(t, err)
	nlbAttrs := make(map[string]string)
	for _, a := range nlbDesc.Attributes {
		nlbAttrs[*a.Key] = *a.Value
	}
	assert.Equal(t, "false", nlbAttrs["load_balancing.cross_zone.enabled"])
}

func TestCreateLoadBalancer_ALBCrossZoneAttribute(t *testing.T) {
	svc := setupTestService(t)

	// ALB: no stored attributes either, Describe should return default cross-zone=true.
	albOut, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("alb-cz"),
	}, testAccountID)
	require.NoError(t, err)
	albRec, err := svc.store.GetLoadBalancerByName("alb-cz", testAccountID)
	require.NoError(t, err)
	assert.Nil(t, albRec.Attributes, "ALB should no longer seed attributes at create time")

	albDesc, err := svc.DescribeLoadBalancerAttributes(&elbv2.DescribeLoadBalancerAttributesInput{
		LoadBalancerArn: albOut.LoadBalancers[0].LoadBalancerArn,
	}, testAccountID)
	require.NoError(t, err)
	albAttrs := make(map[string]string)
	for _, a := range albDesc.Attributes {
		albAttrs[*a.Key] = *a.Value
	}
	assert.Equal(t, "true", albAttrs["load_balancing.cross_zone.enabled"])
}

func TestCreateLoadBalancer_InvalidType(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("bad-type"),
		Type: aws.String("gateway"),
	}, testAccountID)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestCreateLoadBalancer_ALB_AllowsSecurityGroups(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:           aws.String("alb-with-sg"),
		SecurityGroups: []*string{aws.String("sg-111")},
	}, testAccountID)

	require.NoError(t, err)
	assert.Equal(t, "application", *out.LoadBalancers[0].Type)
	assert.Contains(t, *out.LoadBalancers[0].LoadBalancerArn, "loadbalancer/app/alb-with-sg")
}

func TestDeleteLoadBalancer(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("delete-me"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: out.LoadBalancers[0].LoadBalancerArn,
	}, testAccountID)
	require.NoError(t, err)

	// Verify it's gone
	desc, err := svc.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, desc.LoadBalancers)
}

func TestDeleteLoadBalancer_IdempotentOnAbsent(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/nope/xyz"),
	}, testAccountID)

	// AWS ELBv2 delete is idempotent: absent LB -> success, not NotFound.
	require.NoError(t, err)
	assert.NotNil(t, out)
}

func TestDeleteLoadBalancer_CleansUpListeners(t *testing.T) {
	svc := setupTestService(t)

	// Create LB, TG, and listener
	lbOut, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("lb-cleanup")}, testAccountID)
	require.NoError(t, err)
	lbArn := lbOut.LoadBalancers[0].LoadBalancerArn

	tgOut, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("tg-cleanup")}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbArn,
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)
	require.NoError(t, err)

	// Delete LB should clean up listener
	_, err = svc.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{LoadBalancerArn: lbArn}, testAccountID)
	require.NoError(t, err)

	// Listener should be gone
	lstDesc, err := svc.DescribeListeners(&elbv2.DescribeListenersInput{LoadBalancerArn: lbArn}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, lstDesc.Listeners)
}

func TestDescribeLoadBalancers_FilterByName(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("alb-one")}, testAccountID)
	require.NoError(t, err)
	_, err = svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("alb-two")}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{
		Names: []*string{aws.String("alb-one")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.LoadBalancers, 1)
	assert.Equal(t, "alb-one", *desc.LoadBalancers[0].LoadBalancerName)
}

func TestDescribeLoadBalancers_AccountIsolation(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("acct-alb")}, testAccountID)
	require.NoError(t, err)

	desc, err := svc.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{}, "999999999999")
	require.NoError(t, err)
	assert.Empty(t, desc.LoadBalancers)
}

// --- Target Group tests ---

func TestCreateTargetGroup(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("my-tg"),
		Protocol: aws.String("HTTP"),
		Port:     aws.Int64(8080),
		VpcId:    aws.String("vpc-test"),
	}, testAccountID)

	require.NoError(t, err)
	require.Len(t, out.TargetGroups, 1)
	tg := out.TargetGroups[0]
	assert.Equal(t, "my-tg", *tg.TargetGroupName)
	assert.Equal(t, "HTTP", *tg.Protocol)
	assert.Equal(t, int64(8080), *tg.Port)
	assert.Equal(t, "vpc-test", *tg.VpcId)
	assert.Equal(t, "/", *tg.HealthCheckPath)
	assert.Equal(t, "200", *tg.Matcher.HttpCode)
}

func TestCreateTargetGroup_CustomHealthCheck(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:                       aws.String("custom-hc"),
		HealthCheckEnabled:         aws.Bool(false),
		HealthCheckPath:            aws.String("/healthz"),
		HealthCheckIntervalSeconds: aws.Int64(10),
		HealthyThresholdCount:      aws.Int64(2),
		Matcher:                    &elbv2.Matcher{HttpCode: aws.String("200-299")},
	}, testAccountID)

	require.NoError(t, err)
	tg := out.TargetGroups[0]
	assert.False(t, *tg.HealthCheckEnabled)
	assert.Equal(t, "/healthz", *tg.HealthCheckPath)
	assert.Equal(t, int64(10), *tg.HealthCheckIntervalSeconds)
	assert.Equal(t, int64(2), *tg.HealthyThresholdCount)
	assert.Equal(t, "200-299", *tg.Matcher.HttpCode)
}

func TestCreateTargetGroup_TCPProtocol(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("tcp-tg"),
		Protocol: aws.String("TCP"),
		Port:     aws.Int64(5432),
		VpcId:    aws.String("vpc-test"),
	}, testAccountID)

	require.NoError(t, err)
	require.Len(t, out.TargetGroups, 1)
	tg := out.TargetGroups[0]
	assert.Equal(t, "tcp-tg", *tg.TargetGroupName)
	assert.Equal(t, "TCP", *tg.Protocol)
	assert.Equal(t, int64(5432), *tg.Port)
	// NLB health check defaults: TCP protocol, no path, no matcher.
	assert.Equal(t, "TCP", *tg.HealthCheckProtocol)
	assert.Equal(t, "", *tg.HealthCheckPath)
	assert.Equal(t, "", *tg.Matcher.HttpCode)
	assert.Equal(t, int64(10), *tg.HealthCheckTimeoutSeconds)
	assert.Equal(t, int64(3), *tg.HealthyThresholdCount)
	assert.Equal(t, int64(3), *tg.UnhealthyThresholdCount)
}

func TestCreateTargetGroup_NLBProtocols(t *testing.T) {
	for _, proto := range []string{"TCP", "UDP", "TLS", "TCP_UDP"} {
		t.Run(proto, func(t *testing.T) {
			svc := setupTestService(t)

			out, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
				Name:     aws.String("tg-" + proto),
				Protocol: aws.String(proto),
				Port:     aws.Int64(8080),
			}, testAccountID)

			require.NoError(t, err)
			require.Len(t, out.TargetGroups, 1)
			assert.Equal(t, proto, *out.TargetGroups[0].Protocol)
			// All NLB protocols get NLB health check defaults.
			assert.Equal(t, "TCP", *out.TargetGroups[0].HealthCheckProtocol)
		})
	}
}

func TestCreateTargetGroup_InvalidProtocol(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("bad-proto-tg"),
		Protocol: aws.String("SCTP"),
	}, testAccountID)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestCreateTargetGroup_TCPWithCustomHealthCheck(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:                       aws.String("tcp-custom-hc"),
		Protocol:                   aws.String("TCP"),
		Port:                       aws.Int64(3306),
		HealthCheckProtocol:        aws.String("HTTP"),
		HealthCheckPath:            aws.String("/health"),
		HealthCheckIntervalSeconds: aws.Int64(15),
		Matcher:                    &elbv2.Matcher{HttpCode: aws.String("200")},
	}, testAccountID)

	require.NoError(t, err)
	tg := out.TargetGroups[0]
	assert.Equal(t, "TCP", *tg.Protocol)
	// User overrides should take effect even on NLB target groups.
	assert.Equal(t, "HTTP", *tg.HealthCheckProtocol)
	assert.Equal(t, "/health", *tg.HealthCheckPath)
	assert.Equal(t, "200", *tg.Matcher.HttpCode)
	assert.Equal(t, int64(15), *tg.HealthCheckIntervalSeconds)
}

func TestCreateTargetGroup_RejectsHealthCheckPathInjection(t *testing.T) {
	svc := setupTestService(t)

	cases := map[string]string{
		"newline_haproxy_directive": "/x\nbind 0.0.0.0:9999",
		"trailing_directive":        "/x\n    use_backend other",
		"crlf":                      "/x\r\nbind 0.0.0.0:9999",
		"cr_only":                   "/x\rbind",
		"tab":                       "/x\tuse_backend other",
		"semicolon_drop":            "/x; drop",
		"jndi":                      "${jndi:ldap://x}",
		"space":                     "/path with space",
		"leading_space":             " /healthz",
		"trailing_space":            "/healthz ",
		"whitespace_only":           "   ",
		"multibyte_utf8":            "/healthz・",
		"empty":                     "",
		"too_long":                  strings.Repeat("a", maxHealthCheckPathLen+1),
		"control_byte":              "/x\x00",
	}

	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
				Name:            aws.String("inj-" + name),
				HealthCheckPath: aws.String(payload),
			}, testAccountID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "InvalidParameterValue")
		})
	}
}

func TestCreateTargetGroup_RejectsMatcherInjection(t *testing.T) {
	svc := setupTestService(t)

	cases := map[string]string{
		"newline_use_backend": "200\nuse_backend other",
		"crlf":                "200\r\nuse_backend other",
		"tab":                 "200\t201",
		"alpha":               "abc",
		"empty":               "",
		"whitespace_only":     " ",
		"multibyte_digits":    "２００",
		"too_long":            strings.Repeat("9", maxHealthCheckMatcherLen+1),
		"with_space":          "200 ,201",
	}

	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
				Name:    aws.String("matcher-" + name),
				Matcher: &elbv2.Matcher{HttpCode: aws.String(payload)},
			}, testAccountID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "InvalidParameterValue")
		})
	}
}

func TestCreateTargetGroup_AcceptsValidHealthCheckInputs(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:            aws.String("ok-tg"),
		HealthCheckPath: aws.String("/healthz?probe=lb#frag"),
		Matcher:         &elbv2.Matcher{HttpCode: aws.String("200-299,301")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "/healthz?probe=lb#frag", *out.TargetGroups[0].HealthCheckPath)
	assert.Equal(t, "200-299,301", *out.TargetGroups[0].Matcher.HttpCode)
}

func TestCreateTargetGroup_AcceptsHealthCheckBoundaryLengths(t *testing.T) {
	svc := setupTestService(t)

	maxPath := "/" + strings.Repeat("a", maxHealthCheckPathLen-1)
	maxMatcher := "2" + strings.Repeat("0", maxHealthCheckMatcherLen-1)

	out, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:            aws.String("boundary-tg"),
		HealthCheckPath: aws.String(maxPath),
		Matcher:         &elbv2.Matcher{HttpCode: aws.String(maxMatcher)},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, maxPath, *out.TargetGroups[0].HealthCheckPath)
	assert.Equal(t, maxMatcher, *out.TargetGroups[0].Matcher.HttpCode)
}

func TestCreateTargetGroup_DuplicateName(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:  aws.String("dup-tg"),
		VpcId: aws.String("vpc-1"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:  aws.String("dup-tg"),
		VpcId: aws.String("vpc-1"),
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "DuplicateTargetGroupName")
}

func TestModifyTargetGroup(t *testing.T) {
	svc := setupTestService(t)

	created, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("modify-tg"),
		Protocol: aws.String("HTTP"),
		Port:     aws.Int64(8080),
		VpcId:    aws.String("vpc-test"),
	}, testAccountID)
	require.NoError(t, err)
	arn := created.TargetGroups[0].TargetGroupArn

	out, err := svc.ModifyTargetGroup(&elbv2.ModifyTargetGroupInput{
		TargetGroupArn:             arn,
		HealthCheckEnabled:         aws.Bool(true),
		HealthCheckPath:            aws.String("/healthz"),
		HealthCheckIntervalSeconds: aws.Int64(15),
		HealthyThresholdCount:      aws.Int64(2),
		Matcher:                    &elbv2.Matcher{HttpCode: aws.String("200-299")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.TargetGroups, 1)
	tg := out.TargetGroups[0]
	assert.True(t, *tg.HealthCheckEnabled)
	assert.Equal(t, "/healthz", *tg.HealthCheckPath)
	assert.Equal(t, int64(15), *tg.HealthCheckIntervalSeconds)
	assert.Equal(t, int64(2), *tg.HealthyThresholdCount)
	assert.Equal(t, "200-299", *tg.Matcher.HttpCode)

	described, err := svc.DescribeTargetGroups(&elbv2.DescribeTargetGroupsInput{
		TargetGroupArns: []*string{arn},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, described.TargetGroups, 1)
	assert.Equal(t, "/healthz", *described.TargetGroups[0].HealthCheckPath)
}

func TestModifyTargetGroup_NotFound(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.ModifyTargetGroup(&elbv2.ModifyTargetGroupInput{
		TargetGroupArn: aws.String("arn:aws:elasticloadbalancing:ap-southeast-2:000000000001:targetgroup/missing/tg-doesnotexist"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorELBv2TargetGroupNotFound, err.Error())
}

func TestModifyTargetGroup_MissingArn(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.ModifyTargetGroup(&elbv2.ModifyTargetGroupInput{}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestModifyTargetGroup_AllHealthCheckFields(t *testing.T) {
	svc := setupTestService(t)

	created, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("modify-all-tg"),
		Protocol: aws.String("HTTP"),
		Port:     aws.Int64(8080),
		VpcId:    aws.String("vpc-test"),
	}, testAccountID)
	require.NoError(t, err)
	arn := created.TargetGroups[0].TargetGroupArn

	out, err := svc.ModifyTargetGroup(&elbv2.ModifyTargetGroupInput{
		TargetGroupArn:             arn,
		HealthCheckEnabled:         aws.Bool(false),
		HealthCheckProtocol:        aws.String("HTTPS"),
		HealthCheckPort:            aws.String("8443"),
		HealthCheckPath:            aws.String("/status"),
		HealthCheckIntervalSeconds: aws.Int64(20),
		HealthCheckTimeoutSeconds:  aws.Int64(8),
		HealthyThresholdCount:      aws.Int64(4),
		UnhealthyThresholdCount:    aws.Int64(5),
		Matcher:                    &elbv2.Matcher{HttpCode: aws.String("200")},
	}, testAccountID)
	require.NoError(t, err)
	tg := out.TargetGroups[0]
	assert.False(t, *tg.HealthCheckEnabled)
	assert.Equal(t, "HTTPS", *tg.HealthCheckProtocol)
	assert.Equal(t, "8443", *tg.HealthCheckPort)
	assert.Equal(t, "/status", *tg.HealthCheckPath)
	assert.Equal(t, int64(20), *tg.HealthCheckIntervalSeconds)
	assert.Equal(t, int64(8), *tg.HealthCheckTimeoutSeconds)
	assert.Equal(t, int64(4), *tg.HealthyThresholdCount)
	assert.Equal(t, int64(5), *tg.UnhealthyThresholdCount)
	assert.Equal(t, "200", *tg.Matcher.HttpCode)
}

func TestModifyTargetGroup_InvalidPath(t *testing.T) {
	svc := setupTestService(t)

	created, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name: aws.String("modify-bad-path"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.ModifyTargetGroup(&elbv2.ModifyTargetGroupInput{
		TargetGroupArn:  created.TargetGroups[0].TargetGroupArn,
		HealthCheckPath: aws.String(""),
	}, testAccountID)
	require.Error(t, err)
}

func TestModifyTargetGroup_InvalidMatcher(t *testing.T) {
	svc := setupTestService(t)

	created, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name: aws.String("modify-bad-matcher"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.ModifyTargetGroup(&elbv2.ModifyTargetGroupInput{
		TargetGroupArn: created.TargetGroups[0].TargetGroupArn,
		Matcher:        &elbv2.Matcher{HttpCode: aws.String("")},
	}, testAccountID)
	require.Error(t, err)
}

func TestModifyTargetGroup_WrongAccount(t *testing.T) {
	svc := setupTestService(t)

	created, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name: aws.String("modify-wrong-acct"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.ModifyTargetGroup(&elbv2.ModifyTargetGroupInput{
		TargetGroupArn:     created.TargetGroups[0].TargetGroupArn,
		HealthCheckEnabled: aws.Bool(false),
	}, "999999999999")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorELBv2TargetGroupNotFound, err.Error())
}

func TestDeleteTargetGroup(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("del-tg")}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteTargetGroup(&elbv2.DeleteTargetGroupInput{
		TargetGroupArn: out.TargetGroups[0].TargetGroupArn,
	}, testAccountID)
	require.NoError(t, err)
}

func TestDeleteTargetGroup_InUse(t *testing.T) {
	svc := setupTestService(t)

	tgOut, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("inuse-tg")}, testAccountID)
	require.NoError(t, err)

	lbOut, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("inuse-lb")}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteTargetGroup(&elbv2.DeleteTargetGroupInput{
		TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn,
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ResourceInUse")
}

func TestDeleteTargetGroup_IdempotentOnAbsent(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.DeleteTargetGroup(&elbv2.DeleteTargetGroupInput{
		TargetGroupArn: aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/nope/xyz"),
	}, testAccountID)
	// AWS ELBv2 delete is idempotent: absent TG -> success, not NotFound.
	require.NoError(t, err)
	assert.NotNil(t, out)
}

func TestDescribeTargetGroups_FilterByLBArn(t *testing.T) {
	svc := setupTestService(t)

	tg1, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("tg-linked")}, testAccountID)
	_, _ = svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("tg-unlinked")}, testAccountID)

	lb, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("lb-filter")}, testAccountID)
	_, _ = svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lb.LoadBalancers[0].LoadBalancerArn,
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tg1.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)

	desc, err := svc.DescribeTargetGroups(&elbv2.DescribeTargetGroupsInput{
		LoadBalancerArn: lb.LoadBalancers[0].LoadBalancerArn,
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.TargetGroups, 1)
	assert.Equal(t, "tg-linked", *desc.TargetGroups[0].TargetGroupName)
}

// --- Target registration tests ---

func TestRegisterTargets(t *testing.T) {
	svc := setupTestService(t)

	tgOut, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name: aws.String("reg-tg"),
		Port: aws.Int64(80),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.RegisterTargets(&elbv2.RegisterTargetsInput{
		TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn,
		Targets: []*elbv2.TargetDescription{
			{Id: aws.String("i-aaa111")},
			{Id: aws.String("i-bbb222"), Port: aws.Int64(8080)},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Verify via DescribeTargetHealth
	health, err := svc.DescribeTargetHealth(&elbv2.DescribeTargetHealthInput{
		TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn,
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, health.TargetHealthDescriptions, 2)

	// First target should use TG default port
	assert.Equal(t, "i-aaa111", *health.TargetHealthDescriptions[0].Target.Id)
	assert.Equal(t, int64(80), *health.TargetHealthDescriptions[0].Target.Port)
	assert.Equal(t, "initial", *health.TargetHealthDescriptions[0].TargetHealth.State)

	// Second target should use override port
	assert.Equal(t, "i-bbb222", *health.TargetHealthDescriptions[1].Target.Id)
	assert.Equal(t, int64(8080), *health.TargetHealthDescriptions[1].Target.Port)
}

func TestRegisterTargets_IPType(t *testing.T) {
	svc := setupTestService(t)

	tgOut, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:       aws.String("ip-tg"),
		Port:       aws.Int64(80),
		TargetType: aws.String("ip"),
	}, testAccountID)
	require.NoError(t, err)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn
	assert.Equal(t, "ip", *tgOut.TargetGroups[0].TargetType)

	_, err = svc.RegisterTargets(&elbv2.RegisterTargetsInput{
		TargetGroupArn: tgArn,
		Targets: []*elbv2.TargetDescription{
			{Id: aws.String("10.0.1.20")},
			{Id: aws.String("10.0.1.21"), Port: aws.Int64(8080)},
		},
	}, testAccountID)
	require.NoError(t, err)

	// ip targets must carry the supplied IP as PrivateIP, not an empty
	// ENI-resolution result — otherwise HAProxy/health-check silently drop them.
	tg, err := svc.store.GetTargetGroupByArn(*tgArn)
	require.NoError(t, err)
	require.Len(t, tg.Targets, 2)
	ipByID := make(map[string]string)
	for _, target := range tg.Targets {
		ipByID[target.Id] = target.PrivateIP
	}
	assert.Equal(t, "10.0.1.20", ipByID["10.0.1.20"])
	assert.Equal(t, "10.0.1.21", ipByID["10.0.1.21"])

	health, err := svc.DescribeTargetHealth(&elbv2.DescribeTargetHealthInput{
		TargetGroupArn: tgArn,
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, health.TargetHealthDescriptions, 2)
}

func TestRegisterTargets_IPType_RejectsNonIP(t *testing.T) {
	svc := setupTestService(t)
	tgOut, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:       aws.String("ip-tg-bad"),
		TargetType: aws.String("ip"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.RegisterTargets(&elbv2.RegisterTargetsInput{
		TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn,
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-notanip")}},
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestRegisterTargets_InstanceType_RejectsIP(t *testing.T) {
	svc := setupTestService(t)
	tgOut, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name: aws.String("inst-tg-badid"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.RegisterTargets(&elbv2.RegisterTargetsInput{
		TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn,
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("10.0.0.5")}},
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestCreateTargetGroup_RejectsUnsupportedTargetType(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:       aws.String("lambda-tg"),
		TargetType: aws.String("lambda"),
	}, testAccountID)
	assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
}

func TestCreateTargetGroup_DefaultsToInstanceType(t *testing.T) {
	svc := setupTestService(t)
	tgOut, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name: aws.String("default-tt"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "instance", *tgOut.TargetGroups[0].TargetType)
}

func TestRegisterTargets_Idempotent(t *testing.T) {
	svc := setupTestService(t)

	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("idem-tg")}, testAccountID)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn

	targets := []*elbv2.TargetDescription{{Id: aws.String("i-same")}}

	_, err := svc.RegisterTargets(&elbv2.RegisterTargetsInput{TargetGroupArn: tgArn, Targets: targets}, testAccountID)
	require.NoError(t, err)
	_, err = svc.RegisterTargets(&elbv2.RegisterTargetsInput{TargetGroupArn: tgArn, Targets: targets}, testAccountID)
	require.NoError(t, err)

	health, _ := svc.DescribeTargetHealth(&elbv2.DescribeTargetHealthInput{TargetGroupArn: tgArn}, testAccountID)
	assert.Len(t, health.TargetHealthDescriptions, 1)
}

func TestDeregisterTargets(t *testing.T) {
	svc := setupTestService(t)

	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("dereg-tg"), Port: aws.Int64(80)}, testAccountID)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn

	svc.RegisterTargets(&elbv2.RegisterTargetsInput{
		TargetGroupArn: tgArn,
		Targets: []*elbv2.TargetDescription{
			{Id: aws.String("i-keep")},
			{Id: aws.String("i-remove")},
		},
	}, testAccountID)

	_, err := svc.DeregisterTargets(&elbv2.DeregisterTargetsInput{
		TargetGroupArn: tgArn,
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-remove")}},
	}, testAccountID)
	require.NoError(t, err)

	health, _ := svc.DescribeTargetHealth(&elbv2.DescribeTargetHealthInput{TargetGroupArn: tgArn}, testAccountID)
	require.Len(t, health.TargetHealthDescriptions, 1)
	assert.Equal(t, "i-keep", *health.TargetHealthDescriptions[0].Target.Id)
}

func TestDescribeTargetHealth_FilterByTarget(t *testing.T) {
	svc := setupTestService(t)

	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("filter-tg"), Port: aws.Int64(80)}, testAccountID)
	tgArn := tgOut.TargetGroups[0].TargetGroupArn

	svc.RegisterTargets(&elbv2.RegisterTargetsInput{
		TargetGroupArn: tgArn,
		Targets: []*elbv2.TargetDescription{
			{Id: aws.String("i-one")},
			{Id: aws.String("i-two")},
		},
	}, testAccountID)

	health, err := svc.DescribeTargetHealth(&elbv2.DescribeTargetHealthInput{
		TargetGroupArn: tgArn,
		Targets:        []*elbv2.TargetDescription{{Id: aws.String("i-one")}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, health.TargetHealthDescriptions, 1)
	assert.Equal(t, "i-one", *health.TargetHealthDescriptions[0].Target.Id)
}

func TestDescribeTargetHealth_TGNotFound(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.DescribeTargetHealth(&elbv2.DescribeTargetHealthInput{
		TargetGroupArn: aws.String("arn:nonexistent"),
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "TargetGroupNotFound")
}

// --- Listener tests ---

func TestCreateListener(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("lst-lb")}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("lst-tg")}, testAccountID)

	out, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(8080),
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn},
		},
	}, testAccountID)

	require.NoError(t, err)
	require.Len(t, out.Listeners, 1)
	l := out.Listeners[0]
	assert.Equal(t, "HTTP", *l.Protocol)
	assert.Equal(t, int64(8080), *l.Port)
	assert.Equal(t, *lbOut.LoadBalancers[0].LoadBalancerArn, *l.LoadBalancerArn)
	require.Len(t, l.DefaultActions, 1)
	assert.Equal(t, "forward", *l.DefaultActions[0].Type)
}

func TestCreateListener_RedirectDefault(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("redir-lb")}, testAccountID)

	out, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions: []*elbv2.Action{{
			Type: aws.String("redirect"),
			RedirectConfig: &elbv2.RedirectActionConfig{
				Protocol:   aws.String("HTTPS"),
				Port:       aws.String("443"),
				StatusCode: aws.String("HTTP_301"),
			},
		}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Listeners, 1)
	require.Len(t, out.Listeners[0].DefaultActions, 1)
	a := out.Listeners[0].DefaultActions[0]
	assert.Equal(t, "redirect", *a.Type)
	require.NotNil(t, a.RedirectConfig)
	assert.Equal(t, "HTTPS", *a.RedirectConfig.Protocol)
	assert.Equal(t, "HTTP_301", *a.RedirectConfig.StatusCode)
}

func TestCreateListener_RedirectFullFields(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("redirfull-lb")}, testAccountID)

	out, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions: []*elbv2.Action{{
			Type: aws.String("redirect"),
			RedirectConfig: &elbv2.RedirectActionConfig{
				Protocol:   aws.String("HTTPS"),
				Host:       aws.String("new.example.com"),
				Port:       aws.String("8443"),
				Path:       aws.String("/moved"),
				Query:      aws.String("ref=1"),
				StatusCode: aws.String("HTTP_302"),
			},
		}},
	}, testAccountID)
	require.NoError(t, err)

	rc := out.Listeners[0].DefaultActions[0].RedirectConfig
	require.NotNil(t, rc)
	assert.Equal(t, "new.example.com", *rc.Host)
	assert.Equal(t, "8443", *rc.Port)
	assert.Equal(t, "/moved", *rc.Path)
	assert.Equal(t, "ref=1", *rc.Query)
	assert.Equal(t, "HTTP_302", *rc.StatusCode)

	// Read back through Describe to exercise the stored→SDK path.
	desc, err := svc.DescribeListeners(&elbv2.DescribeListenersInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Listeners, 1)
	assert.Equal(t, "new.example.com", *desc.Listeners[0].DefaultActions[0].RedirectConfig.Host)
}

func TestModifyListener_ToRedirect(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("mod-redir-lb")}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("mod-redir-tg")}, testAccountID)

	lstOut, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn: lstOut.Listeners[0].ListenerArn,
		DefaultActions: []*elbv2.Action{{
			Type:           aws.String("redirect"),
			RedirectConfig: &elbv2.RedirectActionConfig{Protocol: aws.String("HTTPS"), StatusCode: aws.String("HTTP_301")},
		}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "redirect", *out.Listeners[0].DefaultActions[0].Type)

	// A bad redirect on modify is rejected.
	_, err = svc.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn: lstOut.Listeners[0].ListenerArn,
		DefaultActions: []*elbv2.Action{{
			Type:           aws.String("redirect"),
			RedirectConfig: &elbv2.RedirectActionConfig{StatusCode: aws.String("HTTP_999")},
		}},
	}, testAccountID)
	require.Error(t, err)
}

func TestCreateListener_RejectsBadRedirect(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("badredir-lb")}, testAccountID)

	_, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions: []*elbv2.Action{{
			Type:           aws.String("redirect"),
			RedirectConfig: &elbv2.RedirectActionConfig{StatusCode: aws.String("HTTP_500")},
		}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestCreateListener_DuplicatePort(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("dup-port-lb")}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("dup-port-tg")}, testAccountID)

	actions := []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn}}

	_, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Port:            aws.Int64(80),
		DefaultActions:  actions,
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Port:            aws.Int64(80),
		DefaultActions:  actions,
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "DuplicateListener")
}

func TestCreateListener_LBNotFound(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String("arn:nonexistent"),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String("arn:tg")}},
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "LoadBalancerNotFound")
}

func TestDeleteListener(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("dellst-lb")}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("dellst-tg")}, testAccountID)

	lstOut, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteListener(&elbv2.DeleteListenerInput{
		ListenerArn: lstOut.Listeners[0].ListenerArn,
	}, testAccountID)
	require.NoError(t, err)

	// Verify deleted
	desc, _ := svc.DescribeListeners(&elbv2.DescribeListenersInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
	}, testAccountID)
	assert.Empty(t, desc.Listeners)
}

func TestDeleteListener_IdempotentOnAbsent(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.DeleteListener(&elbv2.DeleteListenerInput{
		ListenerArn: aws.String("arn:nonexistent"),
	}, testAccountID)
	// AWS ELBv2 delete is idempotent: absent listener -> success, not NotFound.
	require.NoError(t, err)
	assert.NotNil(t, out)
}

func TestModifyListener_Port(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("mod-lb")}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("mod-tg")}, testAccountID)

	lstOut, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Port:            aws.Int64(80),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn: lstOut.Listeners[0].ListenerArn,
		Port:        aws.Int64(8080),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Listeners, 1)
	assert.Equal(t, int64(8080), *out.Listeners[0].Port)
	assert.Equal(t, "HTTP", *out.Listeners[0].Protocol)
	require.Len(t, out.Listeners[0].DefaultActions, 1)
	assert.Equal(t, *tgOut.TargetGroups[0].TargetGroupArn, *out.Listeners[0].DefaultActions[0].TargetGroupArn)

	desc, _ := svc.DescribeListeners(&elbv2.DescribeListenersInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
	}, testAccountID)
	require.Len(t, desc.Listeners, 1)
	assert.Equal(t, int64(8080), *desc.Listeners[0].Port)
}

func TestModifyListener_Protocol(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("mod-proto-lb")}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("mod-proto-tg")}, testAccountID)

	lstOut, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn:  lstOut.Listeners[0].ListenerArn,
		Protocol:     aws.String("HTTPS"),
		Certificates: []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn)}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "HTTPS", *out.Listeners[0].Protocol)
}

func TestModifyListener_DefaultActions(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("mod-act-lb")}, testAccountID)
	tg1, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("mod-act-tg1")}, testAccountID)
	tg2, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("mod-act-tg2")}, testAccountID)

	lstOut, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tg1.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn: lstOut.Listeners[0].ListenerArn,
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: tg2.TargetGroups[0].TargetGroupArn},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Listeners[0].DefaultActions, 1)
	assert.Equal(t, *tg2.TargetGroups[0].TargetGroupArn, *out.Listeners[0].DefaultActions[0].TargetGroupArn)
}

func TestModifyListener_DuplicatePort(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("mod-dup-lb")}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("mod-dup-tg")}, testAccountID)
	actions := []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn}}

	_, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Port:            aws.Int64(80),
		DefaultActions:  actions,
	}, testAccountID)
	require.NoError(t, err)

	lst443, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Port:            aws.Int64(443),
		DefaultActions:  actions,
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn: lst443.Listeners[0].ListenerArn,
		Port:        aws.Int64(80),
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "DuplicateListener")
}

func TestModifyListener_SamePortNoConflict(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("mod-same-lb")}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("mod-same-tg")}, testAccountID)

	lstOut, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Port:            aws.Int64(80),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)
	require.NoError(t, err)

	// Setting same port should not trigger dup check against self.
	out, err := svc.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn: lstOut.Listeners[0].ListenerArn,
		Port:        aws.Int64(80),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, int64(80), *out.Listeners[0].Port)
}

func TestModifyListener_NotFound(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn: aws.String("arn:nonexistent"),
		Port:        aws.Int64(80),
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ListenerNotFound")
}

func TestModifyListener_WrongAccount(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("mod-acct-lb")}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("mod-acct-tg")}, testAccountID)
	lstOut, _ := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)

	_, err := svc.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn: lstOut.Listeners[0].ListenerArn,
		Port:        aws.Int64(8080),
	}, "999999999999")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "ListenerNotFound")
}

func TestModifyListener_TargetGroupNotFound(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("mod-tgnf-lb")}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("mod-tgnf-tg")}, testAccountID)
	lstOut, _ := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)

	_, err := svc.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn: lstOut.Listeners[0].ListenerArn,
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: aws.String("arn:nonexistent")},
		},
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "TargetGroupNotFound")
}

func TestModifyListener_NoOp(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("mod-noop-lb")}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("mod-noop-tg")}, testAccountID)
	lstOut, _ := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Port:            aws.Int64(8080),
		Protocol:        aws.String("HTTP"),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)

	out, err := svc.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn: lstOut.Listeners[0].ListenerArn,
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Listeners, 1)
	assert.Equal(t, int64(8080), *out.Listeners[0].Port)
	assert.Equal(t, "HTTP", *out.Listeners[0].Protocol)
}

func TestModifyListener_InvalidProtocolForLBType(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("mod-bad-proto-lb")}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("mod-bad-proto-tg")}, testAccountID)
	lstOut, _ := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)

	_, err := svc.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn: lstOut.Listeners[0].ListenerArn,
		Protocol:    aws.String("TCP"),
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestModifyListener_NilInput(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.ModifyListener(nil, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MissingParameter")
}

func TestModifyListener_EmptyArn(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn: aws.String(""),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MissingParameter")

	_, err = svc.ModifyListener(&elbv2.ModifyListenerInput{}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MissingParameter")
}

func TestModifyListener_NLB_AcceptsAllProtocols(t *testing.T) {
	cases := []struct {
		listenerProto string
		tgProto       string
	}{
		{"TCP", "TCP"},
		{"UDP", "UDP"},
		{"TLS", "TCP"},
		{"TCP_UDP", "TCP_UDP"},
	}
	for _, tc := range cases {
		t.Run(tc.listenerProto, func(t *testing.T) {
			svc := setupTestService(t)

			lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
				Name: aws.String("nlb-mod-" + tc.listenerProto),
				Type: aws.String("network"),
			}, testAccountID)
			tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
				Name:     aws.String("tg-nlb-mod-" + tc.listenerProto),
				Protocol: aws.String(tc.tgProto),
				Port:     aws.Int64(8080),
			}, testAccountID)

			lstOut, err := svc.CreateListener(&elbv2.CreateListenerInput{
				LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
				Protocol:        aws.String(tc.tgProto),
				Port:            aws.Int64(8080),
				DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn}},
			}, testAccountID)
			require.NoError(t, err)

			modIn := &elbv2.ModifyListenerInput{
				ListenerArn: lstOut.Listeners[0].ListenerArn,
				Protocol:    aws.String(tc.listenerProto),
			}
			if protocolRequiresCert(tc.listenerProto) {
				modIn.Certificates = []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn)}}
			}
			out, err := svc.ModifyListener(modIn, testAccountID)
			require.NoError(t, err)
			assert.Equal(t, tc.listenerProto, *out.Listeners[0].Protocol)
		})
	}
}

func TestModifyListener_NLB_RejectsHTTP(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("nlb-mod-rej-http"),
		Type: aws.String("network"),
	}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("tg-nlb-rej"),
		Protocol: aws.String("TCP"),
		Port:     aws.Int64(8080),
	}, testAccountID)
	lstOut, _ := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("TCP"),
		Port:            aws.Int64(8080),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)

	_, err := svc.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn: lstOut.Listeners[0].ListenerArn,
		Protocol:    aws.String("HTTP"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestModifyListener_ProtocolOnlyChange_TGIncompatible(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("nlb-mod-incompat"),
		Type: aws.String("network"),
	}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("tg-udp-incompat"),
		Protocol: aws.String("UDP"),
		Port:     aws.Int64(8080),
	}, testAccountID)
	lstOut, _ := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("UDP"),
		Port:            aws.Int64(8080),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)

	_, err := svc.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn: lstOut.Listeners[0].ListenerArn,
		Protocol:    aws.String("TCP"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestModifyListener_DefaultActions_IncompatibleProtocol(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("nlb-da-incompat"),
		Type: aws.String("network"),
	}, testAccountID)
	tcpTG, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("tg-tcp-da"),
		Protocol: aws.String("TCP"),
		Port:     aws.Int64(8080),
	}, testAccountID)
	udpTG, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("tg-udp-da"),
		Protocol: aws.String("UDP"),
		Port:     aws.Int64(8080),
	}, testAccountID)

	lstOut, _ := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("TCP"),
		Port:            aws.Int64(8080),
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tcpTG.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)

	_, err := svc.ModifyListener(&elbv2.ModifyListenerInput{
		ListenerArn: lstOut.Listeners[0].ListenerArn,
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: udpTG.TargetGroups[0].TargetGroupArn},
		},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestDescribeListeners_FilterByLBArn(t *testing.T) {
	svc := setupTestService(t)

	lb1, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("dl-lb1")}, testAccountID)
	lb2, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("dl-lb2")}, testAccountID)
	tg, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("dl-tg")}, testAccountID)
	actions := []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tg.TargetGroups[0].TargetGroupArn}}

	svc.CreateListener(&elbv2.CreateListenerInput{LoadBalancerArn: lb1.LoadBalancers[0].LoadBalancerArn, Port: aws.Int64(80), DefaultActions: actions}, testAccountID)
	svc.CreateListener(&elbv2.CreateListenerInput{LoadBalancerArn: lb1.LoadBalancers[0].LoadBalancerArn, Port: aws.Int64(443), DefaultActions: actions}, testAccountID)
	svc.CreateListener(&elbv2.CreateListenerInput{LoadBalancerArn: lb2.LoadBalancers[0].LoadBalancerArn, Port: aws.Int64(80), DefaultActions: actions}, testAccountID)

	desc, err := svc.DescribeListeners(&elbv2.DescribeListenersInput{
		LoadBalancerArn: lb1.LoadBalancers[0].LoadBalancerArn,
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, desc.Listeners, 2)
}

// TestDescribeListeners_AccountIsolation pins the per-ListenerRecord account filter.
// LB-level isolation is tested separately; a regression dropping the listener-side
// check would not be caught by the LB-level test.
func TestDescribeListeners_AccountIsolation(t *testing.T) {
	svc := setupTestService(t)

	lb, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("iso-lb")}, testAccountID)
	tg, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("iso-tg")}, testAccountID)
	svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lb.LoadBalancers[0].LoadBalancerArn,
		DefaultActions:  []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: tg.TargetGroups[0].TargetGroupArn}},
	}, testAccountID)

	desc, err := svc.DescribeListeners(&elbv2.DescribeListenersInput{}, "999999999999")
	require.NoError(t, err)
	assert.Empty(t, desc.Listeners)
}

// --- NLB Listener tests ---

func TestCreateListener_NLB_TCPProtocol(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("nlb-tcp-lst"),
		Type: aws.String("network"),
	}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("tcp-tg-lst"),
		Protocol: aws.String("TCP"),
		Port:     aws.Int64(5432),
	}, testAccountID)

	out, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("TCP"),
		Port:            aws.Int64(5432),
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn},
		},
	}, testAccountID)

	require.NoError(t, err)
	require.Len(t, out.Listeners, 1)
	l := out.Listeners[0]
	assert.Equal(t, "TCP", *l.Protocol)
	assert.Equal(t, int64(5432), *l.Port)
	assert.Equal(t, *lbOut.LoadBalancers[0].LoadBalancerArn, *l.LoadBalancerArn)
}

func TestCreateListener_NLB_AllProtocols(t *testing.T) {
	for _, proto := range []string{"TCP", "UDP", "TLS", "TCP_UDP"} {
		t.Run(proto, func(t *testing.T) {
			svc := setupTestService(t)

			lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
				Name: aws.String("nlb-" + proto),
				Type: aws.String("network"),
			}, testAccountID)
			tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
				Name:     aws.String("tg-" + proto),
				Protocol: aws.String(proto),
				Port:     aws.Int64(8080),
			}, testAccountID)

			createIn := &elbv2.CreateListenerInput{
				LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
				Protocol:        aws.String(proto),
				Port:            aws.Int64(8080),
				DefaultActions: []*elbv2.Action{
					{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn},
				},
			}
			if protocolRequiresCert(proto) {
				createIn.Certificates = []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn)}}
			}
			out, err := svc.CreateListener(createIn, testAccountID)

			require.NoError(t, err)
			assert.Equal(t, proto, *out.Listeners[0].Protocol)
		})
	}
}

func TestCreateListener_NLB_RejectsHTTPProtocol(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("nlb-no-http"),
		Type: aws.String("network"),
	}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name: aws.String("tg-http-nlb"),
	}, testAccountID)

	_, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("HTTP"),
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn},
		},
	}, testAccountID)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestCreateListener_NLB_RejectsHTTPSProtocol(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("nlb-no-https"),
		Type: aws.String("network"),
	}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name: aws.String("tg-https-nlb"),
	}, testAccountID)

	_, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("HTTPS"),
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn},
		},
	}, testAccountID)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestCreateListener_ALB_RejectsTCPProtocol(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("alb-no-tcp"),
	}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("tg-tcp-alb"),
		Protocol: aws.String("TCP"),
		Port:     aws.Int64(8080),
	}, testAccountID)

	_, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("TCP"),
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn},
		},
	}, testAccountID)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestCreateListener_NLB_ProtocolCompatibility_TLSToTCP(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("nlb-tls-tcp"),
		Type: aws.String("network"),
	}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("tg-tcp-compat"),
		Protocol: aws.String("TCP"),
		Port:     aws.Int64(443),
	}, testAccountID)

	// TLS listener -> TCP target group is valid
	out, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("TLS"),
		Port:            aws.Int64(443),
		Certificates:    []*elbv2.Certificate{{CertificateArn: aws.String(testCertArn)}},
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn},
		},
	}, testAccountID)

	require.NoError(t, err)
	assert.Equal(t, "TLS", *out.Listeners[0].Protocol)
}

func TestCreateListener_NLB_ProtocolIncompatible_TCPToUDP(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("nlb-tcp-udp"),
		Type: aws.String("network"),
	}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("tg-udp-incompat"),
		Protocol: aws.String("UDP"),
		Port:     aws.Int64(53),
	}, testAccountID)

	// TCP listener -> UDP target group is invalid
	_, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("TCP"),
		Port:            aws.Int64(53),
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn},
		},
	}, testAccountID)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestCreateListener_NLB_ProtocolIncompatible_UDPToTCP(t *testing.T) {
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("nlb-udp-tcp"),
		Type: aws.String("network"),
	}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name:     aws.String("tg-tcp-incompat"),
		Protocol: aws.String("TCP"),
		Port:     aws.Int64(8080),
	}, testAccountID)

	// UDP listener -> TCP target group is invalid
	_, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("UDP"),
		Port:            aws.Int64(8080),
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn},
		},
	}, testAccountID)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestCreateListener_NLB_DefaultProtocol_Rejected(t *testing.T) {
	// When no protocol is specified, it defaults to HTTP which is invalid for NLB
	svc := setupTestService(t)

	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("nlb-default-proto"),
		Type: aws.String("network"),
	}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name: aws.String("tg-default-proto"),
	}, testAccountID)

	_, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn},
		},
	}, testAccountID)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

// --- HAProxy sync tests ---

func TestCreateListener_PushConfig_NoNATS(t *testing.T) {
	// When NATS conn is nil, CreateListener should still succeed
	// (updateStoredConfig is a no-op when InstanceID is empty)
	svc := setupTestService(t)

	lb, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("sync-lb"),
	}, testAccountID)
	require.NoError(t, err)

	tg, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name: aws.String("sync-tg"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lb.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: tg.TargetGroups[0].TargetGroupArn},
		},
	}, testAccountID)
	require.NoError(t, err) // No panic, no error — updateStoredConfig skipped gracefully
}

// TestDeleteLoadBalancer_NoTerminateWhenEmptyInstanceID pins the nil-safe branch:
// an LB without a systemAMI has InstanceID=="" and must skip TerminateSystemInstance.
// The positive case is covered by TestDeleteLoadBalancer_TerminatesVM_WithPublicIP.
func TestDeleteLoadBalancer_NoTerminateWhenEmptyInstanceID(t *testing.T) {
	svc := setupTestService(t)

	mock := &mockTerminateLauncher{}
	svc.InstanceLauncher = mock

	lb, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("del-lb"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeleteLoadBalancer(&elbv2.DeleteLoadBalancerInput{
		LoadBalancerArn: lb.LoadBalancers[0].LoadBalancerArn,
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, 0, len(mock.terminateCalls), "terminate must not be called when InstanceID is empty")
}

// mockTerminateLauncher records TerminateSystemInstance calls for testing.
type mockTerminateLauncher struct {
	terminateCalls []string
}

func (m *mockTerminateLauncher) LaunchSystemInstance(_ *SystemInstanceInput) (*SystemInstanceOutput, error) {
	return &SystemInstanceOutput{InstanceID: "i-mock", PrivateIP: "10.0.0.1"}, nil
}

func (m *mockTerminateLauncher) TerminateSystemInstance(instanceID string) error {
	m.terminateCalls = append(m.terminateCalls, instanceID)
	return nil
}

// --- Scheme unit tests ---

func TestCreateLoadBalancer_InternetFacingScheme_DNSName(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("web-alb"),
	}, testAccountID)

	require.NoError(t, err)
	lb := out.LoadBalancers[0]
	assert.Equal(t, "internet-facing", *lb.Scheme)
	// Internet-facing should NOT have "internal-" prefix
	assert.NotContains(t, *lb.DNSName, "internal-")
	assert.Contains(t, *lb.DNSName, "web-alb")
	assert.Contains(t, *lb.DNSName, ".elb.spinifex.local")
}

func TestCreateLoadBalancer_InternalScheme_DNSPrefix(t *testing.T) {
	svc := setupTestService(t)

	out, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:   aws.String("backend-alb"),
		Scheme: aws.String("internal"),
	}, testAccountID)

	require.NoError(t, err)
	lb := out.LoadBalancers[0]
	assert.Equal(t, "internal", *lb.Scheme)
	// Internal scheme should have "internal-" DNS prefix
	assert.Contains(t, *lb.DNSName, "internal-backend-alb")
	assert.Contains(t, *lb.DNSName, ".elb.spinifex.local")
}

// --- LBAgentHeartbeat tests ---

func TestLBAgentHeartbeat_TransitionsProvisioningToActive(t *testing.T) {
	svc := setupTestService(t)

	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/hb-lb/lb-hb1",
		LoadBalancerID:  "lb-hb1",
		Name:            "hb-lb",
		State:           StateProvisioning,
		InstanceID:      "i-sys-hb1",
		VPCIP:           "10.0.1.100",
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutLoadBalancer(lb))

	out, err := svc.LBAgentHeartbeat(&LBAgentHeartbeatInput{
		LBID: aws.String("lb-hb1"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, StateActive, *out.Status)

	stored, err := svc.store.GetLoadBalancer("lb-hb1")
	require.NoError(t, err)
	assert.Equal(t, StateActive, stored.State)
	assert.False(t, stored.LastHeartbeat.IsZero())
}

func TestLBAgentHeartbeat_ReturnsConfigHash(t *testing.T) {
	svc := setupTestService(t)

	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/hash-lb/lb-hash1",
		LoadBalancerID:  "lb-hash1",
		Name:            "hash-lb",
		State:           StateActive,
		InstanceID:      "i-sys-hash1",
		ConfigHash:      "abc123def456",
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutLoadBalancer(lb))

	out, err := svc.LBAgentHeartbeat(&LBAgentHeartbeatInput{
		LBID: aws.String("lb-hash1"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "abc123def456", *out.ConfigHash)
}

func TestLBAgentHeartbeat_ProcessesHealthReport(t *testing.T) {
	svc := setupTestService(t)

	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/health-lb/lb-hr1",
		LoadBalancerID:  "lb-hr1",
		Name:            "health-lb",
		State:           StateActive,
		InstanceID:      "i-sys-hr1",
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutLoadBalancer(lb))

	tg := &TargetGroupRecord{
		TargetGroupArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/health-tg/tg-hr1",
		TargetGroupID:  "tg-hr1",
		Port:           80,
		HealthCheck:    DefaultHealthCheck(),
		Targets: []Target{
			{Id: "i-target1", Port: 80, HealthState: TargetHealthInitial, PrivateIP: "10.0.1.20"},
		},
		AccountID: testAccountID,
	}
	require.NoError(t, svc.store.PutTargetGroup(tg))

	// Wire LB → listener → TG so the health checker can resolve the TG from the LBID.
	require.NoError(t, svc.store.PutListener(&ListenerRecord{
		ListenerArn:     lb.LoadBalancerArn + "/listener-1",
		ListenerID:      "lst-hr1",
		LoadBalancerArn: lb.LoadBalancerArn,
		Protocol:        "HTTP",
		Port:            80,
		DefaultActions:  []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: tg.TargetGroupArn}},
		AccountID:       testAccountID,
	}))

	srvName := sanitizeName("srv", "i-target1")
	_, err := svc.LBAgentHeartbeat(&LBAgentHeartbeatInput{
		LBID: aws.String("lb-hr1"),
		Servers: []*LBAgentServerStatus{
			{Backend: aws.String("bk_tg-hr1"), Server: aws.String(srvName), Status: aws.String("UP")},
		},
	}, testAccountID)
	require.NoError(t, err)

	stored, err := svc.store.GetTargetGroup("tg-hr1")
	require.NoError(t, err)
	assert.Equal(t, TargetHealthHealthy, stored.Targets[0].HealthState)
}

// TestLBAgentHeartbeat_BuildsConfigOnActivation covers the create-burst race where
// updateStoredConfig no-ops while InstanceID is empty; the first heartbeat
// (provisioning→active) must build the full config so the agent gets a ConfigHash.
func TestLBAgentHeartbeat_BuildsConfigOnActivation(t *testing.T) {
	svc := setupTestService(t)

	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/net/act-lb/lb-act1",
		LoadBalancerID:  "lb-act1",
		Name:            "act-lb",
		Type:            LoadBalancerTypeNetwork,
		Scheme:          SchemeInternal,
		State:           StateProvisioning,
		InstanceID:      "i-sys-act1",
		VPCIP:           "10.0.1.100",
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutLoadBalancer(lb))

	tg := &TargetGroupRecord{
		TargetGroupArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/act-tg/tg-act1",
		TargetGroupID:  "tg-act1",
		Protocol:       ProtocolTCP,
		Port:           80,
		HealthCheck:    DefaultHealthCheck(),
		Targets: []Target{
			{Id: "i-target1", Port: 80, HealthState: TargetHealthInitial, PrivateIP: "10.0.1.20"},
		},
		AccountID: testAccountID,
	}
	require.NoError(t, svc.store.PutTargetGroup(tg))

	require.NoError(t, svc.store.PutListener(&ListenerRecord{
		ListenerArn:     lb.LoadBalancerArn + "/listener-1",
		ListenerID:      "lst-act1",
		LoadBalancerArn: lb.LoadBalancerArn,
		Protocol:        ProtocolTCP,
		Port:            80,
		DefaultActions:  []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: tg.TargetGroupArn}},
		AccountID:       testAccountID,
	}))

	// Pre-condition: config was never built during provisioning.
	pre, err := svc.store.GetLoadBalancer("lb-act1")
	require.NoError(t, err)
	require.Empty(t, pre.ConfigHash, "config must be empty before activation")

	out, err := svc.LBAgentHeartbeat(&LBAgentHeartbeatInput{
		LBID: aws.String("lb-act1"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, StateActive, *out.Status)
	assert.NotEmpty(t, aws.StringValue(out.ConfigHash), "first heartbeat must return a built ConfigHash")

	stored, err := svc.store.GetLoadBalancer("lb-act1")
	require.NoError(t, err)
	assert.NotEmpty(t, stored.ConfigText, "data-plane config must be built on activation")
	assert.NotEmpty(t, stored.ConfigHash)
	require.Len(t, stored.HealthTargets, 1, "NLB health target must be populated for the registered backend")
	assert.Equal(t, "10.0.1.20:80", stored.HealthTargets[0].Address)
}

func TestLBAgentHeartbeat_MissingLBID(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.LBAgentHeartbeat(&LBAgentHeartbeatInput{}, testAccountID)
	assert.Error(t, err)
}

func TestLBAgentHeartbeat_LBNotFound(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.LBAgentHeartbeat(&LBAgentHeartbeatInput{
		LBID: aws.String("lb-nonexistent"),
	}, testAccountID)
	assert.Error(t, err)
}

// --- GetLBConfig tests ---

func TestGetLBConfig_ReturnsStoredConfig(t *testing.T) {
	svc := setupTestService(t)

	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/cfg-lb/lb-cfg1",
		LoadBalancerID:  "lb-cfg1",
		Name:            "cfg-lb",
		State:           StateActive,
		InstanceID:      "i-sys-cfg1",
		ConfigText:      "global\n    log stdout\n",
		ConfigHash:      "deadbeef",
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutLoadBalancer(lb))

	out, err := svc.GetLBConfig(&GetLBConfigInput{
		LBID: aws.String("lb-cfg1"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "global\n    log stdout\n", *out.ConfigText)
	assert.Equal(t, "deadbeef", *out.ConfigHash)
}

func TestGetLBConfig_DeliversHealthTargetsForNLB(t *testing.T) {
	svc := setupTestService(t)

	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/net/nlb-hc/lb-nlbhc",
		LoadBalancerID:  "lb-nlbhc",
		Name:            "nlb-hc",
		Type:            LoadBalancerTypeNetwork,
		State:           StateActive,
		ConfigText:      "stream {}\n",
		ConfigHash:      "hc1",
		AccountID:       testAccountID,
		HealthTargets: []HealthTargetSpec{
			{ServerName: "srv_i-1", Address: "10.0.0.1:80", Protocol: ProtocolTCP},
		},
	}
	require.NoError(t, svc.store.PutLoadBalancer(lb))

	out, err := svc.GetLBConfig(&GetLBConfigInput{LBID: aws.String("lb-nlbhc")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, EngineNginx, *out.Engine)
	require.Len(t, out.HealthTargets, 1)
	assert.Equal(t, "srv_i-1", *out.HealthTargets[0].ServerName)
	assert.Equal(t, "10.0.0.1:80", *out.HealthTargets[0].Address)
	assert.Equal(t, ProtocolTCP, *out.HealthTargets[0].Protocol)
}

func TestGetLBConfig_HealthTargetsWireShape(t *testing.T) {
	svc := setupTestService(t)
	require.NoError(t, svc.store.PutLoadBalancer(&LoadBalancerRecord{
		LoadBalancerID: "lb-wire",
		Type:           LoadBalancerTypeNetwork,
		AccountID:      testAccountID,
		ConfigText:     "stream {}\n",
		ConfigHash:     "w1",
		HealthTargets: []HealthTargetSpec{
			{ServerName: "srv_i-1", Address: "10.0.0.1:80", Protocol: ProtocolHTTP, Path: "/healthz"},
		},
	}))

	out, err := svc.GetLBConfig(&GetLBConfigInput{LBID: aws.String("lb-wire")}, testAccountID)
	require.NoError(t, err)

	// Confirm the marshalled member shape matches what the lb-agent parses
	// (GetLBConfigResult>HealthTargets>member>{ServerName,Address,Protocol,Path}).
	payload := utils.GenerateIAMXMLPayload("GetLBConfig", *out)
	xmlBytes, err := utils.MarshalToXML(payload)
	require.NoError(t, err)
	var parsed struct {
		Members []struct {
			ServerName string `xml:"ServerName"`
			Address    string `xml:"Address"`
			Protocol   string `xml:"Protocol"`
			Path       string `xml:"Path"`
		} `xml:"GetLBConfigResult>HealthTargets>member"`
	}
	require.NoError(t, xml.Unmarshal(xmlBytes, &parsed))
	require.Len(t, parsed.Members, 1)
	assert.Equal(t, "srv_i-1", parsed.Members[0].ServerName)
	assert.Equal(t, "10.0.0.1:80", parsed.Members[0].Address)
	assert.Equal(t, ProtocolHTTP, parsed.Members[0].Protocol)
	assert.Equal(t, "/healthz", parsed.Members[0].Path)
}

func TestGetLBConfig_NoHealthTargetsForALB(t *testing.T) {
	svc := setupTestService(t)

	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/alb-hc/lb-albhc",
		LoadBalancerID:  "lb-albhc",
		Name:            "alb-hc",
		Type:            LoadBalancerTypeApplication,
		State:           StateActive,
		ConfigText:      "global\n",
		ConfigHash:      "hc2",
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutLoadBalancer(lb))

	out, err := svc.GetLBConfig(&GetLBConfigInput{LBID: aws.String("lb-albhc")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, EngineHAProxy, *out.Engine)
	assert.Empty(t, out.HealthTargets) // ALBs report health via HAProxy stats
}

func TestGetLBConfig_MissingLBID(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.GetLBConfig(&GetLBConfigInput{}, testAccountID)
	assert.Error(t, err)
}

func TestGetLBConfig_LBNotFound(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.GetLBConfig(&GetLBConfigInput{
		LBID: aws.String("lb-missing"),
	}, testAccountID)
	assert.Error(t, err)
}

func TestLBAgentHeartbeat_WrongAccount(t *testing.T) {
	svc := setupTestService(t)

	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/auth-lb/lb-auth1",
		LoadBalancerID:  "lb-auth1",
		Name:            "auth-lb",
		State:           StateActive,
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutLoadBalancer(lb))

	_, err := svc.LBAgentHeartbeat(&LBAgentHeartbeatInput{
		LBID: aws.String("lb-auth1"),
	}, "999999999999")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "LoadBalancerNotFound")
}

func TestLBAgentHeartbeat_SystemAccountAllowed(t *testing.T) {
	svc := setupTestService(t)

	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/sys-lb/lb-sys1",
		LoadBalancerID:  "lb-sys1",
		Name:            "sys-lb",
		State:           StateActive,
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutLoadBalancer(lb))

	out, err := svc.LBAgentHeartbeat(&LBAgentHeartbeatInput{
		LBID: aws.String("lb-sys1"),
	}, utils.GlobalAccountID)
	require.NoError(t, err)
	assert.Equal(t, StateActive, *out.Status)
}

func TestLBAgentHeartbeat_SkipsWriteWhenHeartbeatFresh(t *testing.T) {
	svc := setupTestService(t)

	recentHeartbeat := time.Now().UTC().Add(-10 * time.Second) // well within 60s threshold
	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/fresh-lb/lb-fresh1",
		LoadBalancerID:  "lb-fresh1",
		Name:            "fresh-lb",
		State:           StateActive,
		LastHeartbeat:   recentHeartbeat,
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutLoadBalancer(lb))

	_, err := svc.LBAgentHeartbeat(&LBAgentHeartbeatInput{
		LBID: aws.String("lb-fresh1"),
	}, testAccountID)
	require.NoError(t, err)

	// LastHeartbeat should NOT have been updated because the stored value is fresh.
	stored, err := svc.store.GetLoadBalancer("lb-fresh1")
	require.NoError(t, err)
	assert.Equal(t, recentHeartbeat, stored.LastHeartbeat)
}

func TestLBAgentHeartbeat_PersistsWhenHeartbeatStale(t *testing.T) {
	svc := setupTestService(t)

	staleHeartbeat := time.Now().UTC().Add(-2 * time.Minute) // beyond 60s threshold
	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/stale-lb/lb-stale1",
		LoadBalancerID:  "lb-stale1",
		Name:            "stale-lb",
		State:           StateActive,
		LastHeartbeat:   staleHeartbeat,
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutLoadBalancer(lb))

	_, err := svc.LBAgentHeartbeat(&LBAgentHeartbeatInput{
		LBID: aws.String("lb-stale1"),
	}, testAccountID)
	require.NoError(t, err)

	// LastHeartbeat should have been refreshed.
	stored, err := svc.store.GetLoadBalancer("lb-stale1")
	require.NoError(t, err)
	assert.True(t, stored.LastHeartbeat.After(staleHeartbeat))
}

func TestGetLBConfig_WrongAccount(t *testing.T) {
	svc := setupTestService(t)

	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/auth-cfg/lb-authcfg1",
		LoadBalancerID:  "lb-authcfg1",
		Name:            "auth-cfg",
		State:           StateActive,
		ConfigText:      "global\n",
		ConfigHash:      "aaa",
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutLoadBalancer(lb))

	_, err := svc.GetLBConfig(&GetLBConfigInput{
		LBID: aws.String("lb-authcfg1"),
	}, "999999999999")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "LoadBalancerNotFound")
}

func TestGetLBConfig_SystemAccountAllowed(t *testing.T) {
	svc := setupTestService(t)

	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/sys-cfg/lb-syscfg1",
		LoadBalancerID:  "lb-syscfg1",
		Name:            "sys-cfg",
		State:           StateActive,
		ConfigText:      "global\n    log stdout\n",
		ConfigHash:      "bbb",
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutLoadBalancer(lb))

	out, err := svc.GetLBConfig(&GetLBConfigInput{
		LBID: aws.String("lb-syscfg1"),
	}, utils.GlobalAccountID)
	require.NoError(t, err)
	assert.Equal(t, "global\n    log stdout\n", *out.ConfigText)
}

// --- updateStoredConfig tests ---

func TestUpdateStoredConfig_StoresConfigAndHash(t *testing.T) {
	svc := setupTestService(t)

	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/upd-lb/lb-upd1",
		LoadBalancerID:  "lb-upd1",
		Name:            "upd-lb",
		State:           StateActive,
		InstanceID:      "i-sys-upd1",
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutLoadBalancer(lb))

	tg := &TargetGroupRecord{
		TargetGroupArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/upd-tg/tg-upd1",
		TargetGroupID:  "tg-upd1",
		Port:           80,
		HealthCheck:    DefaultHealthCheck(),
		Targets: []Target{
			{Id: "i-srv1", Port: 80, HealthState: TargetHealthHealthy, PrivateIP: "10.0.1.30"},
		},
		AccountID: testAccountID,
	}
	require.NoError(t, svc.store.PutTargetGroup(tg))

	listener := &ListenerRecord{
		ListenerArn:     "arn:aws:elasticloadbalancing:us-east-1:123456789012:listener/app/upd-lb/lb-upd1/lst-upd1",
		ListenerID:      "lst-upd1",
		LoadBalancerArn: lb.LoadBalancerArn,
		Protocol:        ProtocolHTTP,
		Port:            80,
		DefaultActions:  []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: tg.TargetGroupArn}},
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutListener(listener))

	require.NoError(t, svc.updateStoredConfig(lb))

	stored, err := svc.store.GetLoadBalancer("lb-upd1")
	require.NoError(t, err)
	assert.NotEmpty(t, stored.ConfigText)
	assert.NotEmpty(t, stored.ConfigHash)
	assert.Len(t, stored.ConfigHash, 64) // SHA256 hex
}

func TestUpdateStoredConfig_SkipsWhenNoInstance(t *testing.T) {
	svc := setupTestService(t)

	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/noinst/lb-noinst",
		LoadBalancerID:  "lb-noinst",
		Name:            "noinst-lb",
		State:           StateActive,
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutLoadBalancer(lb))

	require.NoError(t, svc.updateStoredConfig(lb))

	stored, err := svc.store.GetLoadBalancer("lb-noinst")
	require.NoError(t, err)
	assert.Empty(t, stored.ConfigText)
	assert.Empty(t, stored.ConfigHash)
}

// --- Service lifecycle and setter tests ---

// TestGetSystemInstanceType_ColdStart pins the no-resolver-wired path —
// cold-start must return empty rather than panic on a nil func.
func TestGetSystemInstanceType_ColdStart(t *testing.T) {
	svc := setupTestService(t)

	assert.Empty(t, svc.getSystemInstanceType())

	svc.SetSystemInstanceTypeFunc(func() string { return "t3.micro" })
	assert.Equal(t, "t3.micro", svc.getSystemInstanceType())
}

func TestGetSystemInstanceType_RetryOnEmpty(t *testing.T) {
	svc := setupTestService(t)

	calls := 0
	svc.SetSystemInstanceTypeFunc(func() string {
		calls++
		if calls < 2 {
			return ""
		}
		return "t3.micro"
	})

	assert.Empty(t, svc.getSystemInstanceType())
	assert.Equal(t, 1, calls)

	assert.Equal(t, "t3.micro", svc.getSystemInstanceType())
	assert.Equal(t, 2, calls)

	// Cached
	assert.Equal(t, "t3.micro", svc.getSystemInstanceType())
	assert.Equal(t, 2, calls)
}

func TestGetSystemInstanceType_Concurrent(t *testing.T) {
	svc := setupTestService(t)
	svc.SetSystemInstanceTypeFunc(func() string { return "t3.micro" })

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			got := svc.getSystemInstanceType()
			assert.Equal(t, "t3.micro", got)
		})
	}
	wg.Wait()
}

func TestIsCompatibleProtocol_UnknownListenerProtocol(t *testing.T) {
	assert.False(t, isCompatibleProtocol("SCTP", ProtocolTCP))
	assert.False(t, isCompatibleProtocol("", ProtocolHTTP))
}

func TestCreateListener_TargetGroupNotFound(t *testing.T) {
	svc := setupTestService(t)

	lbOut, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("tg-missing-lb"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String(ProtocolHTTP),
		Port:            aws.Int64(80),
		DefaultActions: []*elbv2.Action{
			{
				Type:           aws.String(ActionTypeForward),
				TargetGroupArn: aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/no-exist/tg-nope"),
			},
		},
	}, testAccountID)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "TargetGroupNotFound")
}

func TestUpdateStoredConfig_MissingTargetGroup(t *testing.T) {
	svc := setupTestService(t)

	lb := &LoadBalancerRecord{
		LoadBalancerArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/miss-tg/lb-misstg",
		LoadBalancerID:  "lb-misstg",
		Name:            "miss-tg",
		State:           StateActive,
		InstanceID:      "i-sys-misstg",
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutLoadBalancer(lb))

	listener := &ListenerRecord{
		ListenerArn:     "arn:aws:elasticloadbalancing:us-east-1:123456789012:listener/app/miss-tg/lb-misstg/lst-misstg",
		ListenerID:      "lst-misstg",
		LoadBalancerArn: lb.LoadBalancerArn,
		Protocol:        ProtocolHTTP,
		Port:            80,
		DefaultActions:  []ListenerAction{{Type: ActionTypeForward, TargetGroupArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/gone/tg-gone"}},
		AccountID:       testAccountID,
	}
	require.NoError(t, svc.store.PutListener(listener))

	// Should not error — skips missing TG and generates config with no backends
	require.NoError(t, svc.updateStoredConfig(lb))

	stored, err := svc.store.GetLoadBalancer("lb-misstg")
	require.NoError(t, err)
	assert.NotEmpty(t, stored.ConfigHash)
}

// --- Attribute tests ---

func TestDescribeTargetGroupAttributes_Defaults(t *testing.T) {
	svc := setupTestService(t)
	tgOut, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("tg-attr-defaults")}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DescribeTargetGroupAttributes(&elbv2.DescribeTargetGroupAttributesInput{
		TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn,
	}, testAccountID)
	require.NoError(t, err)

	defaults := DefaultTargetGroupAttributes()
	assert.Len(t, out.Attributes, len(defaults))
	attrMap := make(map[string]string)
	for _, a := range out.Attributes {
		attrMap[*a.Key] = *a.Value
	}
	for k, v := range defaults {
		assert.Equal(t, v, attrMap[k], "default mismatch for key %s", k)
	}
}

func TestDescribeLoadBalancerAttributes_ALBDefaults(t *testing.T) {
	svc := setupTestService(t)
	lbOut, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("alb-attr-defaults")}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DescribeLoadBalancerAttributes(&elbv2.DescribeLoadBalancerAttributesInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
	}, testAccountID)
	require.NoError(t, err)

	defaults := DefaultLoadBalancerAttributes(LoadBalancerTypeApplication)
	assert.Len(t, out.Attributes, len(defaults))
	attrMap := make(map[string]string)
	for _, a := range out.Attributes {
		attrMap[*a.Key] = *a.Value
	}
	// Every default key/value must round-trip through Describe, not just
	// cross-zone. ALB default cross-zone is true — from the per-type default.
	for k, v := range defaults {
		assert.Equal(t, v, attrMap[k], "default mismatch for key %s", k)
	}
	assert.Equal(t, "true", attrMap["load_balancing.cross_zone.enabled"])
}

func TestDescribeLoadBalancerAttributes_NLBDefaults(t *testing.T) {
	svc := setupTestService(t)
	lbOut, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("nlb-attr-defaults"),
		Type: aws.String("network"),
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DescribeLoadBalancerAttributes(&elbv2.DescribeLoadBalancerAttributesInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
	}, testAccountID)
	require.NoError(t, err)

	attrMap := make(map[string]string)
	for _, a := range out.Attributes {
		attrMap[*a.Key] = *a.Value
	}
	// NLB should fall through to default false
	assert.Equal(t, "false", attrMap["load_balancing.cross_zone.enabled"])
}

// TestDefaultLoadBalancerAttributes_ALBCoversTerraformKeys guards against missing ALB
// attribute keys that Terraform's default ModifyLoadBalancerAttributes call sends,
// which would surface as ValidationError.
func TestDefaultLoadBalancerAttributes_ALBCoversTerraformKeys(t *testing.T) {
	attrs := DefaultLoadBalancerAttributes(LoadBalancerTypeApplication)

	expected := map[string]string{
		"deletion_protection.enabled":                              "false",
		"load_balancing.cross_zone.enabled":                        "true",
		"access_logs.s3.enabled":                                   "false",
		"access_logs.s3.bucket":                                    "",
		"access_logs.s3.prefix":                                    "",
		"connection_logs.s3.enabled":                               "false",
		"connection_logs.s3.bucket":                                "",
		"connection_logs.s3.prefix":                                "",
		"idle_timeout.timeout_seconds":                             "60",
		"client_keep_alive.seconds":                                "3600",
		"routing.http.desync_mitigation_mode":                      "defensive",
		"routing.http.drop_invalid_header_fields.enabled":          "false",
		"routing.http.preserve_host_header.enabled":                "false",
		"routing.http.x_amzn_tls_version_and_cipher_suite.enabled": "false",
		"routing.http.xff_client_port.enabled":                     "false",
		"routing.http.xff_header_processing.mode":                  "append",
		"routing.http2.enabled":                                    "true",
		"waf.fail_open.enabled":                                    "false",
		"zonal_shift.config.enabled":                               "false",
	}

	for k, v := range expected {
		got, ok := attrs[k]
		assert.True(t, ok, "ALB default attributes missing key %q — terraform will hit ValidationError", k)
		assert.Equal(t, v, got, "ALB default value mismatch for key %s", k)
	}
}

// TestDefaultLoadBalancerAttributes_NLBCoversExpectedKeys is the NLB
// counterpart guard. NLBs have a smaller but distinct key set.
func TestDefaultLoadBalancerAttributes_NLBCoversExpectedKeys(t *testing.T) {
	attrs := DefaultLoadBalancerAttributes(LoadBalancerTypeNetwork)

	expected := map[string]string{
		"deletion_protection.enabled":       "false",
		"load_balancing.cross_zone.enabled": "false",
		"access_logs.s3.enabled":            "false",
		"access_logs.s3.bucket":             "",
		"access_logs.s3.prefix":             "",
		"dns_record.client_routing_policy":  "any_availability_zone",
		"ipv6.deny_all_igw_traffic":         "false",
		"zonal_shift.config.enabled":        "false",
	}

	for k, v := range expected {
		got, ok := attrs[k]
		assert.True(t, ok, "NLB default attributes missing key %q", k)
		assert.Equal(t, v, got, "NLB default value mismatch for key %s", k)
	}

	// ALB-only keys must not leak into NLB defaults.
	_, hasIdle := attrs["idle_timeout.timeout_seconds"]
	assert.False(t, hasIdle, "idle_timeout.timeout_seconds is ALB-only, must not appear on NLB")
	_, hasHTTP2 := attrs["routing.http2.enabled"]
	assert.False(t, hasHTTP2, "routing.http2.enabled is ALB-only, must not appear on NLB")
}

// TestModifyLoadBalancerAttributes_AcceptsConnectionLogsKey is a regression
// guard: terraform sends connection_logs.s3.enabled on every aws_lb apply and
// the handler must accept it.
func TestModifyLoadBalancerAttributes_AcceptsConnectionLogsKey(t *testing.T) {
	svc := setupTestService(t)
	lbOut, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("alb-conn-logs"),
	}, testAccountID)
	require.NoError(t, err)
	arn := lbOut.LoadBalancers[0].LoadBalancerArn

	_, err = svc.ModifyLoadBalancerAttributes(&elbv2.ModifyLoadBalancerAttributesInput{
		LoadBalancerArn: arn,
		Attributes: []*elbv2.LoadBalancerAttribute{
			{Key: aws.String("connection_logs.s3.enabled"), Value: aws.String("false")},
			{Key: aws.String("routing.http.desync_mitigation_mode"), Value: aws.String("defensive")},
			{Key: aws.String("waf.fail_open.enabled"), Value: aws.String("false")},
			{Key: aws.String("zonal_shift.config.enabled"), Value: aws.String("false")},
		},
	}, testAccountID)
	require.NoError(t, err, "terraform-sent attribute keys must be accepted")
}

// TestModifyLoadBalancerAttributes_EmptyStringClears verifies that a non-nil
// empty-string Value is treated as a real value, not skipped as invalid. AWS
// uses "" to clear access_logs.s3.bucket, so the handler must persist it.
func TestModifyLoadBalancerAttributes_EmptyStringClears(t *testing.T) {
	svc := setupTestService(t)
	lbOut, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("alb-clear")}, testAccountID)
	require.NoError(t, err)
	arn := lbOut.LoadBalancers[0].LoadBalancerArn

	// Set a bucket, then clear it with an empty string.
	_, err = svc.ModifyLoadBalancerAttributes(&elbv2.ModifyLoadBalancerAttributesInput{
		LoadBalancerArn: arn,
		Attributes: []*elbv2.LoadBalancerAttribute{
			{Key: aws.String("access_logs.s3.bucket"), Value: aws.String("my-logs")},
		},
	}, testAccountID)
	require.NoError(t, err)

	modOut, err := svc.ModifyLoadBalancerAttributes(&elbv2.ModifyLoadBalancerAttributesInput{
		LoadBalancerArn: arn,
		Attributes: []*elbv2.LoadBalancerAttribute{
			{Key: aws.String("access_logs.s3.bucket"), Value: aws.String("")},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, modOut.Attributes, 1, "empty-string Value must be echoed, not skipped")
	assert.Equal(t, "", *modOut.Attributes[0].Value)

	descOut, err := svc.DescribeLoadBalancerAttributes(&elbv2.DescribeLoadBalancerAttributesInput{
		LoadBalancerArn: arn,
	}, testAccountID)
	require.NoError(t, err)
	attrMap := make(map[string]string)
	for _, a := range descOut.Attributes {
		attrMap[*a.Key] = *a.Value
	}
	assert.Equal(t, "", attrMap["access_logs.s3.bucket"], "empty-string clear must persist")
}

// --- Attribute mirror-pair tests (table-driven over TG/LB) ---
// Modify*/Describe* are mirror pairs differing only by record type, store methods,
// default set, and not-found error; tests run once per kind via t.Run.

// rawAttr models one submitted SDK attribute, including the invalid shapes a
// handler must skip: a nil slice element, a nil Key, or a nil Value.
type rawAttr struct {
	nilElem bool
	key     *string
	val     *string
}

func kvAttr(k, v string) rawAttr { return rawAttr{key: aws.String(k), val: aws.String(v)} }

func kvAttrs(kv ...[2]string) []rawAttr {
	out := make([]rawAttr, len(kv))
	for i, p := range kv {
		out[i] = kvAttr(p[0], p[1])
	}
	return out
}

type echoPair struct{ Key, Val string }

func echoMap(pairs []echoPair) map[string]string {
	m := make(map[string]string, len(pairs))
	for _, p := range pairs {
		m[p.Key] = p.Val
	}
	return m
}

// attrArnPtr returns nil for an empty ARN so the MissingArn cases exercise the
// real "no ARN supplied" path rather than an empty-string ARN.
func attrArnPtr(arn string) *string {
	if arn == "" {
		return nil
	}
	return aws.String(arn)
}

// attrKind bundles everything the table-driven attribute tests need to drive
// one resource type (target group or load balancer).
type attrKind struct {
	name       string
	notFound   string
	missingArn string

	keyA, valA, valA2 string // primary key, value, and an overwrite value
	keyB, valB        string // secondary key/value
	defaultKey        string // a default present after create
	defaultVal        string
	unknownKey        string // a key the handler must reject

	create   func(t *testing.T, svc *ELBv2ServiceImpl, name string) string
	modify   func(svc *ELBv2ServiceImpl, arn, accountID string, attrs []rawAttr) ([]echoPair, error)
	describe func(svc *ELBv2ServiceImpl, arn, accountID string) ([]echoPair, error)
	revision func(t *testing.T, svc *ELBv2ServiceImpl, arn string) uint64
}

func attrKinds() []attrKind {
	return []attrKind{
		{
			name:       "TargetGroup",
			notFound:   awserrors.ErrorELBv2TargetGroupNotFound,
			missingArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/missing/tg-missing",
			keyA:       "deregistration_delay.timeout_seconds", valA: "45", valA2: "30",
			keyB: "stickiness.enabled", valB: "true",
			defaultKey: "stickiness.type", defaultVal: "lb_cookie",
			unknownKey: "stickness.enabled", // typo of "stickiness"
			create: func(t *testing.T, svc *ELBv2ServiceImpl, name string) string {
				out, err := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String(name)}, testAccountID)
				require.NoError(t, err)
				return *out.TargetGroups[0].TargetGroupArn
			},
			modify: func(svc *ELBv2ServiceImpl, arn, accountID string, attrs []rawAttr) ([]echoPair, error) {
				sdk := make([]*elbv2.TargetGroupAttribute, len(attrs))
				for i, a := range attrs {
					if a.nilElem {
						continue
					}
					sdk[i] = &elbv2.TargetGroupAttribute{Key: a.key, Value: a.val}
				}
				out, err := svc.ModifyTargetGroupAttributes(&elbv2.ModifyTargetGroupAttributesInput{
					TargetGroupArn: attrArnPtr(arn), Attributes: sdk,
				}, accountID)
				if err != nil {
					return nil, err
				}
				pairs := make([]echoPair, len(out.Attributes))
				for i, a := range out.Attributes {
					pairs[i] = echoPair{*a.Key, *a.Value}
				}
				return pairs, nil
			},
			describe: func(svc *ELBv2ServiceImpl, arn, accountID string) ([]echoPair, error) {
				out, err := svc.DescribeTargetGroupAttributes(&elbv2.DescribeTargetGroupAttributesInput{
					TargetGroupArn: attrArnPtr(arn),
				}, accountID)
				if err != nil {
					return nil, err
				}
				pairs := make([]echoPair, len(out.Attributes))
				for i, a := range out.Attributes {
					pairs[i] = echoPair{*a.Key, *a.Value}
				}
				return pairs, nil
			},
			revision: func(t *testing.T, svc *ELBv2ServiceImpl, arn string) uint64 {
				tg, err := svc.store.GetTargetGroupByArn(arn)
				require.NoError(t, err)
				entry, err := svc.store.kv.Get(KeyPrefixTG + tg.TargetGroupID)
				require.NoError(t, err)
				return entry.Revision()
			},
		},
		{
			name:       "LoadBalancer",
			notFound:   awserrors.ErrorELBv2LoadBalancerNotFound,
			missingArn: "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/missing/lb-missing",
			keyA:       "idle_timeout.timeout_seconds", valA: "120", valA2: "90",
			keyB: "deletion_protection.enabled", valB: "true",
			defaultKey: "routing.http2.enabled", defaultVal: "true",
			unknownKey: "stickiness.enabled", // valid TG key — cross-product mistake
			create: func(t *testing.T, svc *ELBv2ServiceImpl, name string) string {
				out, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String(name)}, testAccountID)
				require.NoError(t, err)
				return *out.LoadBalancers[0].LoadBalancerArn
			},
			modify: func(svc *ELBv2ServiceImpl, arn, accountID string, attrs []rawAttr) ([]echoPair, error) {
				sdk := make([]*elbv2.LoadBalancerAttribute, len(attrs))
				for i, a := range attrs {
					if a.nilElem {
						continue
					}
					sdk[i] = &elbv2.LoadBalancerAttribute{Key: a.key, Value: a.val}
				}
				out, err := svc.ModifyLoadBalancerAttributes(&elbv2.ModifyLoadBalancerAttributesInput{
					LoadBalancerArn: attrArnPtr(arn), Attributes: sdk,
				}, accountID)
				if err != nil {
					return nil, err
				}
				pairs := make([]echoPair, len(out.Attributes))
				for i, a := range out.Attributes {
					pairs[i] = echoPair{*a.Key, *a.Value}
				}
				return pairs, nil
			},
			describe: func(svc *ELBv2ServiceImpl, arn, accountID string) ([]echoPair, error) {
				out, err := svc.DescribeLoadBalancerAttributes(&elbv2.DescribeLoadBalancerAttributesInput{
					LoadBalancerArn: attrArnPtr(arn),
				}, accountID)
				if err != nil {
					return nil, err
				}
				pairs := make([]echoPair, len(out.Attributes))
				for i, a := range out.Attributes {
					pairs[i] = echoPair{*a.Key, *a.Value}
				}
				return pairs, nil
			},
			revision: func(t *testing.T, svc *ELBv2ServiceImpl, arn string) uint64 {
				lb, err := svc.store.GetLoadBalancerByArn(arn)
				require.NoError(t, err)
				entry, err := svc.store.kv.Get(KeyPrefixLB + lb.LoadBalancerID)
				require.NoError(t, err)
				return entry.Revision()
			},
		},
	}
}

func TestAttributeRoundTrip(t *testing.T) {
	for _, k := range attrKinds() {
		t.Run(k.name, func(t *testing.T) {
			svc := setupTestService(t)
			arn := k.create(t, svc, "attr-rt")

			// Assert the exact echoed key/value pairs, not just the length — a
			// regression that pointed at the wrong source or dropped Value would
			// pass a length check.
			echoed, err := k.modify(svc, arn, testAccountID, kvAttrs([2]string{k.keyA, k.valA}, [2]string{k.keyB, k.valB}))
			require.NoError(t, err)
			require.Len(t, echoed, 2)
			em := echoMap(echoed)
			assert.Equal(t, k.valA, em[k.keyA])
			assert.Equal(t, k.valB, em[k.keyB])

			desc, err := k.describe(svc, arn, testAccountID)
			require.NoError(t, err)
			dm := echoMap(desc)
			assert.Equal(t, k.valA, dm[k.keyA])
			assert.Equal(t, k.valB, dm[k.keyB])
			// Unmodified defaults should still be present.
			assert.Equal(t, k.defaultVal, dm[k.defaultKey])
		})
	}
}

func TestAttributeNotFound(t *testing.T) {
	for _, k := range attrKinds() {
		t.Run(k.name, func(t *testing.T) {
			svc := setupTestService(t)

			_, err := k.modify(svc, k.missingArn, testAccountID, kvAttrs([2]string{k.keyB, k.valB}))
			assert.EqualError(t, err, k.notFound)

			_, err = k.describe(svc, k.missingArn, testAccountID)
			assert.EqualError(t, err, k.notFound)
		})
	}
}

func TestAttributeMissingArn(t *testing.T) {
	for _, k := range attrKinds() {
		t.Run(k.name, func(t *testing.T) {
			svc := setupTestService(t)

			_, err := k.modify(svc, "", testAccountID, kvAttrs([2]string{k.keyB, k.valB}))
			assert.EqualError(t, err, awserrors.ErrorMissingParameter)

			_, err = k.describe(svc, "", testAccountID)
			assert.EqualError(t, err, awserrors.ErrorMissingParameter)
		})
	}
}

func TestAttributeWrongAccount(t *testing.T) {
	for _, k := range attrKinds() {
		t.Run(k.name, func(t *testing.T) {
			svc := setupTestService(t)
			arn := k.create(t, svc, "attr-wrong-acct")

			_, err := k.modify(svc, arn, "999999999999", kvAttrs([2]string{k.keyB, k.valB}))
			assert.EqualError(t, err, k.notFound)

			_, err = k.describe(svc, arn, "999999999999")
			assert.EqualError(t, err, k.notFound)

			// The rejected modify must not have mutated the record — read back as
			// the real owner and confirm keyB is still at its default, not valB.
			desc, err := k.describe(svc, arn, testAccountID)
			require.NoError(t, err)
			assert.NotEqual(t, k.valB, echoMap(desc)[k.keyB], "wrong-account modify must not mutate the record")
		})
	}
}

// TestAttributeCrossAccountIsolation verifies that two accounts each holding a
// resource never see each other's attributes: a Modify by account A must not be
// visible to a Describe by account B, even for similarly-named resources.
func TestAttributeCrossAccountIsolation(t *testing.T) {
	const accountB = "222222222222"
	for _, k := range attrKinds() {
		t.Run(k.name, func(t *testing.T) {
			svcA := setupTestService(t)
			arnA := k.create(t, svcA, "attr-iso")

			// Account A sets keyB.
			_, err := k.modify(svcA, arnA, testAccountID, kvAttrs([2]string{k.keyB, k.valB}))
			require.NoError(t, err)

			// Account B cannot see or describe A's resource at all.
			_, err = k.describe(svcA, arnA, accountB)
			assert.EqualError(t, err, k.notFound)
			_, err = k.modify(svcA, arnA, accountB, kvAttrs([2]string{k.keyB, "false"}))
			assert.EqualError(t, err, k.notFound)

			// A's value is untouched.
			desc, err := k.describe(svcA, arnA, testAccountID)
			require.NoError(t, err)
			assert.Equal(t, k.valB, echoMap(desc)[k.keyB])
		})
	}
}

// TestAttributeSkipsInvalidEntries verifies that nil slice elements, nil Keys,
// and nil Values are skipped (with a warning) rather than panicking or being
// silently dropped. Valid attributes in the same call must still be applied.
func TestAttributeSkipsInvalidEntries(t *testing.T) {
	for _, k := range attrKinds() {
		t.Run(k.name, func(t *testing.T) {
			svc := setupTestService(t)
			arn := k.create(t, svc, "attr-skip")

			echoed, err := k.modify(svc, arn, testAccountID, []rawAttr{
				{nilElem: true}, // nil element must not panic
				{key: nil, val: aws.String("v")},
				{key: aws.String("k"), val: nil},
				kvAttr(k.keyB, k.valB),
			})
			require.NoError(t, err)
			// Only the one valid attribute should be returned.
			require.Len(t, echoed, 1)
			assert.Equal(t, k.keyB, echoed[0].Key)
			assert.Equal(t, k.valB, echoed[0].Val)

			// The valid attribute should have been persisted.
			desc, err := k.describe(svc, arn, testAccountID)
			require.NoError(t, err)
			assert.Equal(t, k.valB, echoMap(desc)[k.keyB])
		})
	}
}

// TestAttributeAllInvalidReturnsError guards against silent success when every
// attribute trips the nil guard, returning 200 OK with nothing applied.
func TestAttributeAllInvalidReturnsError(t *testing.T) {
	for _, k := range attrKinds() {
		t.Run(k.name, func(t *testing.T) {
			svc := setupTestService(t)
			arn := k.create(t, svc, "attr-all-invalid")

			_, err := k.modify(svc, arn, testAccountID, []rawAttr{
				{nilElem: true},
				{key: nil, val: aws.String("v")},
				{key: aws.String("k"), val: nil},
			})
			assert.EqualError(t, err, awserrors.ErrorInvalidParameterValue)
		})
	}
}

// TestAttributeSequentialMerge verifies successive Modify calls accumulate keys.
// `rec.Attributes = newMap` would pass single-call tests but silently wipe
// previous attributes on every subsequent call.
func TestAttributeSequentialMerge(t *testing.T) {
	for _, k := range attrKinds() {
		t.Run(k.name, func(t *testing.T) {
			svc := setupTestService(t)
			arn := k.create(t, svc, "attr-seq-merge")

			// Call 1: set keyA.
			_, err := k.modify(svc, arn, testAccountID, kvAttrs([2]string{k.keyA, k.valA}))
			require.NoError(t, err)
			// Call 2: set a different key; must not wipe the first.
			_, err = k.modify(svc, arn, testAccountID, kvAttrs([2]string{k.keyB, k.valB}))
			require.NoError(t, err)

			desc, err := k.describe(svc, arn, testAccountID)
			require.NoError(t, err)
			dm := echoMap(desc)
			assert.Equal(t, k.valA, dm[k.keyA], "first-call key must survive second Modify")
			assert.Equal(t, k.valB, dm[k.keyB], "second-call key must be present")

			// Call 3: overwrite keyA with a new value.
			_, err = k.modify(svc, arn, testAccountID, kvAttrs([2]string{k.keyA, k.valA2}))
			require.NoError(t, err)

			desc, err = k.describe(svc, arn, testAccountID)
			require.NoError(t, err)
			dm = echoMap(desc)
			assert.Equal(t, k.valA2, dm[k.keyA], "same-key overwrite must replace the value")
			assert.Equal(t, k.valB, dm[k.keyB], "unrelated key must still survive")
		})
	}
}

// TestAttributeNoopSkipsPersist verifies that re-submitting identical values
// does not bump the KV revision — Terraform's drift check hits Modify on every
// apply, so the steady-state path must be a storage-layer no-op.
func TestAttributeNoopSkipsPersist(t *testing.T) {
	for _, k := range attrKinds() {
		t.Run(k.name, func(t *testing.T) {
			svc := setupTestService(t)
			arn := k.create(t, svc, "attr-noop")

			// First modify — must hit the store.
			_, err := k.modify(svc, arn, testAccountID, kvAttrs([2]string{k.keyA, k.valA}))
			require.NoError(t, err)
			revBefore := k.revision(t, svc, arn)

			// Second modify with identical values — must skip the Put.
			echoed, err := k.modify(svc, arn, testAccountID, kvAttrs([2]string{k.keyA, k.valA}))
			require.NoError(t, err)
			require.Len(t, echoed, 1)
			assert.Equal(t, revBefore, k.revision(t, svc, arn), "identical modify must not increment KV revision")

			// Empty attribute list is also a no-op.
			_, err = k.modify(svc, arn, testAccountID, nil)
			require.NoError(t, err)
			assert.Equal(t, revBefore, k.revision(t, svc, arn), "empty modify must not increment KV revision")
		})
	}
}

// TestAttributeRejectsUnknownKey guards against silently persisting typo'd keys.
// AWS rejects unknowns with ValidationError; matching that surfaces typos at plan
// time rather than letting them drift into KV.
func TestAttributeRejectsUnknownKey(t *testing.T) {
	for _, k := range attrKinds() {
		t.Run(k.name, func(t *testing.T) {
			svc := setupTestService(t)
			arn := k.create(t, svc, "attr-unknown-key")

			_, err := k.modify(svc, arn, testAccountID, kvAttrs([2]string{k.unknownKey, "true"}))
			assert.EqualError(t, err, awserrors.ErrorValidationError)

			// The rejected key must not have been persisted.
			desc, err := k.describe(svc, arn, testAccountID)
			require.NoError(t, err)
			_, present := echoMap(desc)[k.unknownKey]
			assert.False(t, present, "unknown key must not appear in Describe")
		})
	}
}

// TestAttributeSortedOrder verifies attributes are returned in a stable,
// sorted-by-key order. Go map iteration is randomised, so without explicit
// sorting Terraform would see spurious plan diffs between describe calls.
func TestAttributeSortedOrder(t *testing.T) {
	for _, k := range attrKinds() {
		t.Run(k.name, func(t *testing.T) {
			svc := setupTestService(t)
			arn := k.create(t, svc, "attr-sorted")

			// Modify a couple of attributes so the merged map mixes defaults and
			// overrides.
			_, err := k.modify(svc, arn, testAccountID, kvAttrs([2]string{k.keyA, k.valA}, [2]string{k.keyB, k.valB}))
			require.NoError(t, err)

			var firstKeys []string
			for i := range 5 {
				desc, err := k.describe(svc, arn, testAccountID)
				require.NoError(t, err)
				keys := make([]string, len(desc))
				for j, p := range desc {
					keys[j] = p.Key
				}
				assert.True(t, sort.StringsAreSorted(keys), "attributes must be sorted by key, got %v", keys)
				if i == 0 {
					firstKeys = keys
				} else {
					assert.Equal(t, firstKeys, keys, "attribute order must be stable across calls")
				}
			}
		})
	}
}

// --- DescribeTags tests ---

func TestDescribeTags_LoadBalancer(t *testing.T) {
	svc := setupTestService(t)
	lbOut, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("tags-lb"),
		Tags: []*elbv2.Tag{
			{Key: aws.String("Env"), Value: aws.String("prod")},
			{Key: aws.String("App"), Value: aws.String("nginx")},
		},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.DescribeTags(&elbv2.DescribeTagsInput{
		ResourceArns: []*string{lbOut.LoadBalancers[0].LoadBalancerArn},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.TagDescriptions, 1)

	td := out.TagDescriptions[0]
	assert.Equal(t, *lbOut.LoadBalancers[0].LoadBalancerArn, *td.ResourceArn)
	require.Len(t, td.Tags, 2)
	// Sorted by key: App, Env
	assert.Equal(t, "App", *td.Tags[0].Key)
	assert.Equal(t, "nginx", *td.Tags[0].Value)
	assert.Equal(t, "Env", *td.Tags[1].Key)
	assert.Equal(t, "prod", *td.Tags[1].Value)
}

func TestDescribeTags_Listener_NoTags(t *testing.T) {
	svc := setupTestService(t)
	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("tags-lst-lb")}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("tags-lst-tg")}, testAccountID)
	lstOut, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Listener created without tags must still return a TagDescription (with an
	// empty Tags slice), not an error. This is the case the Terraform AWS
	// provider hits during post-create refresh.
	out, err := svc.DescribeTags(&elbv2.DescribeTagsInput{
		ResourceArns: []*string{lstOut.Listeners[0].ListenerArn},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.TagDescriptions, 1)
	assert.Equal(t, *lstOut.Listeners[0].ListenerArn, *out.TagDescriptions[0].ResourceArn)
	assert.Empty(t, out.TagDescriptions[0].Tags)
}

func TestDescribeTags_ListenerRule_NoTags(t *testing.T) {
	svc := setupTestService(t)
	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{Name: aws.String("tags-rule-lb")}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{Name: aws.String("tags-rule-tg")}, testAccountID)
	lstOut, err := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn},
		},
	}, testAccountID)
	require.NoError(t, err)

	ruleOut, err := svc.CreateRule(&elbv2.CreateRuleInput{
		ListenerArn: lstOut.Listeners[0].ListenerArn,
		Priority:    aws.Int64(10),
		Conditions: []*elbv2.RuleCondition{
			{Field: aws.String("host-header"), Values: aws.StringSlice([]string{"app.example.com"})},
		},
		Actions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Rule created without tags must still return a TagDescription (with an
	// empty Tags slice), not an error. This is the case the Terraform AWS
	// provider hits during post-create refresh of aws_lb_listener_rule.
	out, err := svc.DescribeTags(&elbv2.DescribeTagsInput{
		ResourceArns: []*string{ruleOut.Rules[0].RuleArn},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.TagDescriptions, 1)
	assert.Equal(t, *ruleOut.Rules[0].RuleArn, *out.TagDescriptions[0].ResourceArn)
	assert.Empty(t, out.TagDescriptions[0].Tags)
}

func TestDescribeTags_RuleNotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.DescribeTags(&elbv2.DescribeTagsInput{
		ResourceArns: []*string{
			aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:listener-rule/app/missing/lb-x/lst-y/rule-deadbeef"),
		},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2RuleNotFound)
}

func TestDescribeTags_MultipleArns(t *testing.T) {
	svc := setupTestService(t)
	lbOut, _ := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("multi-lb"),
		Tags: []*elbv2.Tag{{Key: aws.String("Name"), Value: aws.String("lb")}},
	}, testAccountID)
	tgOut, _ := svc.CreateTargetGroup(&elbv2.CreateTargetGroupInput{
		Name: aws.String("multi-tg"),
		Tags: []*elbv2.Tag{{Key: aws.String("Name"), Value: aws.String("tg")}},
	}, testAccountID)
	lstOut, _ := svc.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: lbOut.LoadBalancers[0].LoadBalancerArn,
		Protocol:        aws.String("HTTP"),
		Port:            aws.Int64(80),
		DefaultActions: []*elbv2.Action{
			{Type: aws.String("forward"), TargetGroupArn: tgOut.TargetGroups[0].TargetGroupArn},
		},
	}, testAccountID)

	out, err := svc.DescribeTags(&elbv2.DescribeTagsInput{
		ResourceArns: []*string{
			lbOut.LoadBalancers[0].LoadBalancerArn,
			tgOut.TargetGroups[0].TargetGroupArn,
			lstOut.Listeners[0].ListenerArn,
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.TagDescriptions, 3)

	// Order matches input order
	assert.Equal(t, *lbOut.LoadBalancers[0].LoadBalancerArn, *out.TagDescriptions[0].ResourceArn)
	assert.Equal(t, *tgOut.TargetGroups[0].TargetGroupArn, *out.TagDescriptions[1].ResourceArn)
	assert.Equal(t, *lstOut.Listeners[0].ListenerArn, *out.TagDescriptions[2].ResourceArn)

	assert.Equal(t, "lb", *out.TagDescriptions[0].Tags[0].Value)
	assert.Equal(t, "tg", *out.TagDescriptions[1].Tags[0].Value)
	assert.Empty(t, out.TagDescriptions[2].Tags)
}

func TestDescribeTags_LoadBalancerNotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.DescribeTags(&elbv2.DescribeTagsInput{
		ResourceArns: []*string{
			aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/missing/lb-deadbeef"),
		},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2LoadBalancerNotFound)
}

func TestDescribeTags_TargetGroupNotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.DescribeTags(&elbv2.DescribeTagsInput{
		ResourceArns: []*string{
			aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/missing/tg-deadbeef"),
		},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2TargetGroupNotFound)
}

func TestDescribeTags_ListenerNotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.DescribeTags(&elbv2.DescribeTagsInput{
		ResourceArns: []*string{
			aws.String("arn:aws:elasticloadbalancing:us-east-1:123456789012:listener/app/missing/lb-x/lst-deadbeef"),
		},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2ListenerNotFound)
}

func TestDescribeTags_CrossAccount(t *testing.T) {
	svc := setupTestService(t)
	lbOut, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("xa-lb"),
	}, testAccountID)
	require.NoError(t, err)

	// Same ARN, different account => not-found (no existence leak)
	_, err = svc.DescribeTags(&elbv2.DescribeTagsInput{
		ResourceArns: []*string{lbOut.LoadBalancers[0].LoadBalancerArn},
	}, "999999999999")
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorELBv2LoadBalancerNotFound)
}

func TestDescribeTags_InvalidArn(t *testing.T) {
	svc := setupTestService(t)
	cases := []string{
		"not-an-arn",
		"arn:aws:s3:::my-bucket", // wrong service
		"arn:aws:elasticloadbalancing:us-east-1:123:capacityreservation/abc", // unknown ELBv2 resource type
		"arn:aws:elasticloadbalancing:us-east-1:123:loadbalancer",            // missing slash
	}
	for _, arn := range cases {
		_, err := svc.DescribeTags(&elbv2.DescribeTagsInput{
			ResourceArns: []*string{aws.String(arn)},
		}, testAccountID)
		require.Error(t, err, "arn=%q should be rejected", arn)
		assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue, "arn=%q", arn)
	}
}

func TestDescribeTags_MissingParameter(t *testing.T) {
	svc := setupTestService(t)

	// Empty ResourceArns
	_, err := svc.DescribeTags(&elbv2.DescribeTagsInput{}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)

	// Nil entry inside a non-empty list
	_, err = svc.DescribeTags(&elbv2.DescribeTagsInput{
		ResourceArns: []*string{nil},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorMissingParameter)
}

func TestDescribeTags_UntaggedLoadBalancer(t *testing.T) {
	svc := setupTestService(t)
	lbOut, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name: aws.String("untagged-lb"),
	}, testAccountID)
	require.NoError(t, err)

	// Bare LB with no Tags input must still return a TagDescription with
	// the ARN and an empty/nil Tags slice — exercises the exact path the
	// failing nginx-alb terraform demo hits.
	out, err := svc.DescribeTags(&elbv2.DescribeTagsInput{
		ResourceArns: []*string{lbOut.LoadBalancers[0].LoadBalancerArn},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.TagDescriptions, 1)
	assert.Equal(t, *lbOut.LoadBalancers[0].LoadBalancerArn, *out.TagDescriptions[0].ResourceArn)
	assert.Empty(t, out.TagDescriptions[0].Tags)
}
