package gateway_elbv2_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_elbv2 "github.com/mulgadc/spinifex/spinifex/gateway/elbv2"
	handlers_elbv2 "github.com/mulgadc/spinifex/spinifex/handlers/elbv2"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAccountID = "123456789012"
	testLBArn     = "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/my-alb/abc"
	testTGArn     = "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/my-tg/abc"
	testRuleArn   = "arn:aws:elasticloadbalancing:us-east-1:123456789012:listener-rule/app/my-alb/abc/def/ghi"
	testLsnrArn   = "arn:aws:elasticloadbalancing:us-east-1:123456789012:listener/app/lb/abc/def"
	testCrtArn    = "arn:aws:acm:us-east-1:123456789012:certificate/aaaa-1111"
)

// dispatchCase pins one gateway wrapper to the NATS subject it must publish on,
// the request body the daemon must receive, and the output it must return.
type dispatchCase struct {
	name    string
	subject string
	// input is the value the wrapper is called with; the daemon must receive
	// exactly this, JSON-encoded.
	input any
	// reply is what the stub daemon responds with; want is what the wrapper
	// must return after decoding it.
	reply any
	want  any
	call  func(ctx context.Context, nc *nats.Conn) (any, error)
}

func dispatchCases() []dispatchCase {
	lbIn := &elbv2.CreateLoadBalancerInput{Name: aws.String("my-alb"), Subnets: []*string{aws.String("subnet-1")}}
	lbOut := elbv2.CreateLoadBalancerOutput{LoadBalancers: []*elbv2.LoadBalancer{{
		LoadBalancerArn: aws.String(testLBArn), DNSName: aws.String("my-alb.elb.local"),
	}}}

	delLBIn := &elbv2.DeleteLoadBalancerInput{LoadBalancerArn: aws.String(testLBArn)}
	descLBIn := &elbv2.DescribeLoadBalancersInput{Names: []*string{aws.String("my-alb")}}
	descLBOut := elbv2.DescribeLoadBalancersOutput{LoadBalancers: []*elbv2.LoadBalancer{{
		LoadBalancerArn: aws.String(testLBArn), State: &elbv2.LoadBalancerState{Code: aws.String("active")},
	}}}

	tgIn := &elbv2.CreateTargetGroupInput{Name: aws.String("my-tg"), Port: aws.Int64(80)}
	tgOut := elbv2.CreateTargetGroupOutput{TargetGroups: []*elbv2.TargetGroup{{TargetGroupArn: aws.String(testTGArn)}}}
	modTGIn := &elbv2.ModifyTargetGroupInput{TargetGroupArn: aws.String(testTGArn), HealthCheckPath: aws.String("/healthz")}
	modTGOut := elbv2.ModifyTargetGroupOutput{TargetGroups: []*elbv2.TargetGroup{{
		TargetGroupArn: aws.String(testTGArn), HealthCheckPath: aws.String("/healthz"),
	}}}
	delTGIn := &elbv2.DeleteTargetGroupInput{TargetGroupArn: aws.String(testTGArn)}
	descTGIn := &elbv2.DescribeTargetGroupsInput{LoadBalancerArn: aws.String(testLBArn)}
	descTGOut := elbv2.DescribeTargetGroupsOutput{TargetGroups: []*elbv2.TargetGroup{{TargetGroupArn: aws.String(testTGArn)}}}

	targets := []*elbv2.TargetDescription{{Id: aws.String("i-0123456789abcdef0"), Port: aws.Int64(8080)}}
	regIn := &elbv2.RegisterTargetsInput{TargetGroupArn: aws.String(testTGArn), Targets: targets}
	deregIn := &elbv2.DeregisterTargetsInput{TargetGroupArn: aws.String(testTGArn), Targets: targets}
	healthIn := &elbv2.DescribeTargetHealthInput{TargetGroupArn: aws.String(testTGArn)}
	healthOut := elbv2.DescribeTargetHealthOutput{TargetHealthDescriptions: []*elbv2.TargetHealthDescription{{
		Target: targets[0], TargetHealth: &elbv2.TargetHealth{State: aws.String("healthy")},
	}}}

	actions := []*elbv2.Action{{Type: aws.String("forward"), TargetGroupArn: aws.String(testTGArn)}}
	createListenerIn := &elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(testLBArn), Port: aws.Int64(443), DefaultActions: actions,
	}
	createListenerOut := elbv2.CreateListenerOutput{Listeners: []*elbv2.Listener{{ListenerArn: aws.String(testLsnrArn)}}}
	delListenerIn := &elbv2.DeleteListenerInput{ListenerArn: aws.String(testLsnrArn)}
	modListenerIn := &elbv2.ModifyListenerInput{ListenerArn: aws.String(testLsnrArn), Port: aws.Int64(8443)}
	modListenerOut := elbv2.ModifyListenerOutput{Listeners: []*elbv2.Listener{{
		ListenerArn: aws.String(testLsnrArn), Port: aws.Int64(8443),
	}}}
	descListenersIn := &elbv2.DescribeListenersInput{LoadBalancerArn: aws.String(testLBArn)}
	descListenersOut := elbv2.DescribeListenersOutput{Listeners: []*elbv2.Listener{{ListenerArn: aws.String(testLsnrArn)}}}

	conditions := []*elbv2.RuleCondition{{Field: aws.String("path-pattern"), Values: []*string{aws.String("/api/*")}}}
	createRuleIn := &elbv2.CreateRuleInput{
		ListenerArn: aws.String(testLsnrArn), Priority: aws.Int64(10),
		Conditions: conditions, Actions: actions,
	}
	createRuleOut := elbv2.CreateRuleOutput{Rules: []*elbv2.Rule{{RuleArn: aws.String(testRuleArn), Priority: aws.String("10")}}}
	modRuleIn := &elbv2.ModifyRuleInput{RuleArn: aws.String(testRuleArn), Conditions: conditions}
	modRuleOut := elbv2.ModifyRuleOutput{Rules: []*elbv2.Rule{{RuleArn: aws.String(testRuleArn)}}}
	delRuleIn := &elbv2.DeleteRuleInput{RuleArn: aws.String(testRuleArn)}
	descRulesIn := &elbv2.DescribeRulesInput{ListenerArn: aws.String(testLsnrArn)}
	descRulesOut := elbv2.DescribeRulesOutput{Rules: []*elbv2.Rule{{RuleArn: aws.String(testRuleArn)}}}
	prioIn := &elbv2.SetRulePrioritiesInput{RulePriorities: []*elbv2.RulePriorityPair{{
		RuleArn: aws.String(testRuleArn), Priority: aws.Int64(5),
	}}}
	prioOut := elbv2.SetRulePrioritiesOutput{Rules: []*elbv2.Rule{{RuleArn: aws.String(testRuleArn), Priority: aws.String("5")}}}

	tags := []*elbv2.Tag{{Key: aws.String("env"), Value: aws.String("prod")}}
	descTagsIn := &elbv2.DescribeTagsInput{ResourceArns: []*string{aws.String(testLBArn)}}
	descTagsOut := elbv2.DescribeTagsOutput{TagDescriptions: []*elbv2.TagDescription{{
		ResourceArn: aws.String(testLBArn), Tags: tags,
	}}}
	addTagsIn := &elbv2.AddTagsInput{ResourceArns: []*string{aws.String(testLBArn)}, Tags: tags}
	removeTagsIn := &elbv2.RemoveTagsInput{ResourceArns: []*string{aws.String(testLBArn)}, TagKeys: []*string{aws.String("env")}}

	tgAttrs := []*elbv2.TargetGroupAttribute{{Key: aws.String("deregistration_delay.timeout_seconds"), Value: aws.String("30")}}
	modTGAttrIn := &elbv2.ModifyTargetGroupAttributesInput{TargetGroupArn: aws.String(testTGArn), Attributes: tgAttrs}
	modTGAttrOut := elbv2.ModifyTargetGroupAttributesOutput{Attributes: tgAttrs}
	descTGAttrIn := &elbv2.DescribeTargetGroupAttributesInput{TargetGroupArn: aws.String(testTGArn)}
	descTGAttrOut := elbv2.DescribeTargetGroupAttributesOutput{Attributes: tgAttrs}

	lbAttrs := []*elbv2.LoadBalancerAttribute{{Key: aws.String("idle_timeout.timeout_seconds"), Value: aws.String("60")}}
	modLBAttrIn := &elbv2.ModifyLoadBalancerAttributesInput{LoadBalancerArn: aws.String(testLBArn), Attributes: lbAttrs}
	modLBAttrOut := elbv2.ModifyLoadBalancerAttributesOutput{Attributes: lbAttrs}
	descLBAttrIn := &elbv2.DescribeLoadBalancerAttributesInput{LoadBalancerArn: aws.String(testLBArn)}
	descLBAttrOut := elbv2.DescribeLoadBalancerAttributesOutput{Attributes: lbAttrs}

	sgIn := &elbv2.SetSecurityGroupsInput{
		LoadBalancerArn: aws.String(testLBArn), SecurityGroups: []*string{aws.String("sg-1")},
	}
	sgOut := elbv2.SetSecurityGroupsOutput{SecurityGroupIds: []*string{aws.String("sg-1")}}
	ipTypeIn := &elbv2.SetIpAddressTypeInput{LoadBalancerArn: aws.String(testLBArn), IpAddressType: aws.String("ipv4")}
	ipTypeOut := elbv2.SetIpAddressTypeOutput{IpAddressType: aws.String("ipv4")}
	subnetsIn := &elbv2.SetSubnetsInput{LoadBalancerArn: aws.String(testLBArn), Subnets: []*string{aws.String("subnet-1")}}
	subnetsOut := elbv2.SetSubnetsOutput{AvailabilityZones: []*elbv2.AvailabilityZone{{SubnetId: aws.String("subnet-1")}}}

	certs := []*elbv2.Certificate{{CertificateArn: aws.String(testCrtArn)}}
	addCertIn := &elbv2.AddListenerCertificatesInput{ListenerArn: aws.String(testLsnrArn), Certificates: certs}
	addCertOut := elbv2.AddListenerCertificatesOutput{Certificates: certs}
	removeCertIn := &elbv2.RemoveListenerCertificatesInput{ListenerArn: aws.String(testLsnrArn), Certificates: certs}
	descCertIn := &elbv2.DescribeListenerCertificatesInput{ListenerArn: aws.String(testLsnrArn)}
	descCertOut := elbv2.DescribeListenerCertificatesOutput{Certificates: certs}
	sslIn := &elbv2.DescribeSSLPoliciesInput{Names: []*string{aws.String("ELBSecurityPolicy-2016-08")}}
	sslOut := elbv2.DescribeSSLPoliciesOutput{SslPolicies: []*elbv2.SslPolicy{{Name: aws.String("ELBSecurityPolicy-2016-08")}}}

	heartbeatIn := &handlers_elbv2.LBAgentHeartbeatInput{
		LBID: aws.String("lb-1"),
		Servers: []*handlers_elbv2.LBAgentServerStatus{{
			Backend: aws.String("be1"), Server: aws.String("srv1"), Status: aws.String("UP"),
		}},
	}
	heartbeatOut := handlers_elbv2.LBAgentHeartbeatOutput{Status: aws.String("ok"), ConfigHash: aws.String("h1")}
	lbConfigIn := &handlers_elbv2.GetLBConfigInput{LBID: aws.String("lb-1")}
	lbConfigOut := handlers_elbv2.GetLBConfigOutput{
		ConfigText: aws.String("global\n"), ConfigHash: aws.String("h1"), Engine: aws.String("haproxy"),
	}

	return []dispatchCase{
		{
			name: "CreateLoadBalancer", subject: "elbv2.CreateLoadBalancer",
			input: lbIn, reply: lbOut, want: lbOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.CreateLoadBalancer(ctx, lbIn, nc, testAccountID)
			},
		},
		{
			name: "DeleteLoadBalancer", subject: "elbv2.DeleteLoadBalancer",
			input: delLBIn, reply: elbv2.DeleteLoadBalancerOutput{}, want: elbv2.DeleteLoadBalancerOutput{},
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.DeleteLoadBalancer(ctx, delLBIn, nc, testAccountID)
			},
		},
		{
			name: "DescribeLoadBalancers", subject: "elbv2.DescribeLoadBalancers",
			input: descLBIn, reply: descLBOut, want: descLBOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.DescribeLoadBalancers(ctx, descLBIn, nc, testAccountID)
			},
		},
		{
			name: "CreateTargetGroup", subject: "elbv2.CreateTargetGroup",
			input: tgIn, reply: tgOut, want: tgOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.CreateTargetGroup(ctx, tgIn, nc, testAccountID)
			},
		},
		{
			name: "ModifyTargetGroup", subject: "elbv2.ModifyTargetGroup",
			input: modTGIn, reply: modTGOut, want: modTGOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.ModifyTargetGroup(ctx, modTGIn, nc, testAccountID)
			},
		},
		{
			name: "DeleteTargetGroup", subject: "elbv2.DeleteTargetGroup",
			input: delTGIn, reply: elbv2.DeleteTargetGroupOutput{}, want: elbv2.DeleteTargetGroupOutput{},
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.DeleteTargetGroup(ctx, delTGIn, nc, testAccountID)
			},
		},
		{
			name: "DescribeTargetGroups", subject: "elbv2.DescribeTargetGroups",
			input: descTGIn, reply: descTGOut, want: descTGOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.DescribeTargetGroups(ctx, descTGIn, nc, testAccountID)
			},
		},
		{
			name: "RegisterTargets", subject: "elbv2.RegisterTargets",
			input: regIn, reply: elbv2.RegisterTargetsOutput{}, want: elbv2.RegisterTargetsOutput{},
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.RegisterTargets(ctx, regIn, nc, testAccountID)
			},
		},
		{
			name: "DeregisterTargets", subject: "elbv2.DeregisterTargets",
			input: deregIn, reply: elbv2.DeregisterTargetsOutput{}, want: elbv2.DeregisterTargetsOutput{},
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.DeregisterTargets(ctx, deregIn, nc, testAccountID)
			},
		},
		{
			name: "DescribeTargetHealth", subject: "elbv2.DescribeTargetHealth",
			input: healthIn, reply: healthOut, want: healthOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.DescribeTargetHealth(ctx, healthIn, nc, testAccountID)
			},
		},
		{
			name: "CreateListener", subject: "elbv2.CreateListener",
			input: createListenerIn, reply: createListenerOut, want: createListenerOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.CreateListener(ctx, createListenerIn, nc, testAccountID)
			},
		},
		{
			name: "DeleteListener", subject: "elbv2.DeleteListener",
			input: delListenerIn, reply: elbv2.DeleteListenerOutput{}, want: elbv2.DeleteListenerOutput{},
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.DeleteListener(ctx, delListenerIn, nc, testAccountID)
			},
		},
		{
			name: "ModifyListener", subject: "elbv2.ModifyListener",
			input: modListenerIn, reply: modListenerOut, want: modListenerOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.ModifyListener(ctx, modListenerIn, nc, testAccountID)
			},
		},
		{
			name: "DescribeListeners", subject: "elbv2.DescribeListeners",
			input: descListenersIn, reply: descListenersOut, want: descListenersOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.DescribeListeners(ctx, descListenersIn, nc, testAccountID)
			},
		},
		{
			name: "CreateRule", subject: "elbv2.CreateRule",
			input: createRuleIn, reply: createRuleOut, want: createRuleOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.CreateRule(ctx, createRuleIn, nc, testAccountID)
			},
		},
		{
			name: "ModifyRule", subject: "elbv2.ModifyRule",
			input: modRuleIn, reply: modRuleOut, want: modRuleOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.ModifyRule(ctx, modRuleIn, nc, testAccountID)
			},
		},
		{
			name: "DeleteRule", subject: "elbv2.DeleteRule",
			input: delRuleIn, reply: elbv2.DeleteRuleOutput{}, want: elbv2.DeleteRuleOutput{},
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.DeleteRule(ctx, delRuleIn, nc, testAccountID)
			},
		},
		{
			name: "DescribeRules", subject: "elbv2.DescribeRules",
			input: descRulesIn, reply: descRulesOut, want: descRulesOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.DescribeRules(ctx, descRulesIn, nc, testAccountID)
			},
		},
		{
			name: "SetRulePriorities", subject: "elbv2.SetRulePriorities",
			input: prioIn, reply: prioOut, want: prioOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.SetRulePriorities(ctx, prioIn, nc, testAccountID)
			},
		},
		{
			name: "DescribeTags", subject: "elbv2.DescribeTags",
			input: descTagsIn, reply: descTagsOut, want: descTagsOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.DescribeTags(ctx, descTagsIn, nc, testAccountID)
			},
		},
		{
			name: "AddTags", subject: "elbv2.AddTags",
			input: addTagsIn, reply: elbv2.AddTagsOutput{}, want: elbv2.AddTagsOutput{},
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.AddTags(ctx, addTagsIn, nc, testAccountID)
			},
		},
		{
			name: "RemoveTags", subject: "elbv2.RemoveTags",
			input: removeTagsIn, reply: elbv2.RemoveTagsOutput{}, want: elbv2.RemoveTagsOutput{},
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.RemoveTags(ctx, removeTagsIn, nc, testAccountID)
			},
		},
		{
			name: "ModifyTargetGroupAttributes", subject: "elbv2.ModifyTargetGroupAttributes",
			input: modTGAttrIn, reply: modTGAttrOut, want: modTGAttrOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.ModifyTargetGroupAttributes(ctx, modTGAttrIn, nc, testAccountID)
			},
		},
		{
			name: "DescribeTargetGroupAttributes", subject: "elbv2.DescribeTargetGroupAttributes",
			input: descTGAttrIn, reply: descTGAttrOut, want: descTGAttrOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.DescribeTargetGroupAttributes(ctx, descTGAttrIn, nc, testAccountID)
			},
		},
		{
			name: "ModifyLoadBalancerAttributes", subject: "elbv2.ModifyLoadBalancerAttributes",
			input: modLBAttrIn, reply: modLBAttrOut, want: modLBAttrOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.ModifyLoadBalancerAttributes(ctx, modLBAttrIn, nc, testAccountID)
			},
		},
		{
			name: "DescribeLoadBalancerAttributes", subject: "elbv2.DescribeLoadBalancerAttributes",
			input: descLBAttrIn, reply: descLBAttrOut, want: descLBAttrOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.DescribeLoadBalancerAttributes(ctx, descLBAttrIn, nc, testAccountID)
			},
		},
		{
			name: "SetSecurityGroups", subject: "elbv2.SetSecurityGroups",
			input: sgIn, reply: sgOut, want: sgOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.SetSecurityGroups(ctx, sgIn, nc, testAccountID)
			},
		},
		{
			name: "SetIpAddressType", subject: "elbv2.SetIpAddressType",
			input: ipTypeIn, reply: ipTypeOut, want: ipTypeOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.SetIpAddressType(ctx, ipTypeIn, nc, testAccountID)
			},
		},
		{
			name: "SetSubnets", subject: "elbv2.SetSubnets",
			input: subnetsIn, reply: subnetsOut, want: subnetsOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.SetSubnets(ctx, subnetsIn, nc, testAccountID)
			},
		},
		{
			name: "AddListenerCertificates", subject: "elbv2.AddListenerCertificates",
			input: addCertIn, reply: addCertOut, want: addCertOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.AddListenerCertificates(ctx, addCertIn, nc, testAccountID)
			},
		},
		{
			name: "RemoveListenerCertificates", subject: "elbv2.RemoveListenerCertificates",
			input: removeCertIn, reply: elbv2.RemoveListenerCertificatesOutput{},
			want: elbv2.RemoveListenerCertificatesOutput{},
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.RemoveListenerCertificates(ctx, removeCertIn, nc, testAccountID)
			},
		},
		{
			name: "DescribeListenerCertificates", subject: "elbv2.DescribeListenerCertificates",
			input: descCertIn, reply: descCertOut, want: descCertOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.DescribeListenerCertificates(ctx, descCertIn, nc, testAccountID)
			},
		},
		{
			name: "DescribeSSLPolicies", subject: "elbv2.DescribeSSLPolicies",
			input: sslIn, reply: sslOut, want: sslOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.DescribeSSLPolicies(ctx, sslIn, nc, testAccountID)
			},
		},
		{
			name: "LBAgentHeartbeat", subject: "elbv2.LBAgentHeartbeat",
			input: heartbeatIn, reply: heartbeatOut, want: heartbeatOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.LBAgentHeartbeat(ctx, heartbeatIn, nc, testAccountID)
			},
		},
		{
			name: "GetLBConfig", subject: "elbv2.GetLBConfig",
			input: lbConfigIn, reply: lbConfigOut, want: lbConfigOut,
			call: func(ctx context.Context, nc *nats.Conn) (any, error) {
				return gateway_elbv2.GetLBConfig(ctx, lbConfigIn, nc, testAccountID)
			},
		},
	}
}

// serveOnce answers a single request on subject with reply and hands the
// received message back. A wrapper publishing on any other subject gets no
// responder, so the subject itself is under assertion.
func serveOnce(t *testing.T, nc *nats.Conn, subject string, reply []byte) <-chan *nats.Msg {
	t.Helper()
	got := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		got <- msg
		_ = msg.Respond(reply)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, nc.Flush())
	return got
}

func TestGatewayDispatch(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	for _, tc := range dispatchCases() {
		t.Run(tc.name, func(t *testing.T) {
			replyBody, err := json.Marshal(tc.reply)
			require.NoError(t, err)
			got := serveOnce(t, nc, tc.subject, replyBody)

			out, err := tc.call(context.Background(), nc)
			require.NoError(t, err)
			assert.Equal(t, tc.want, out)

			var msg *nats.Msg
			select {
			case msg = <-got:
			case <-time.After(2 * time.Second):
				t.Fatalf("no request observed on %s", tc.subject)
			}

			wantBody, err := json.Marshal(tc.input)
			require.NoError(t, err)
			assert.JSONEq(t, string(wantBody), string(msg.Data),
				"%s must forward its own input unmodified", tc.name)
			assert.Equal(t, testAccountID, msg.Header.Get(utils.AccountIDHeader))
		})
	}
}

// Guards that reject before any NATS call. A nil conn proves the request was
// never attempted: reaching NATS would return ErrClusterUnavailable instead.
func TestGatewayDispatch_ValidationGuards(t *testing.T) {
	cases := []struct {
		name    string
		wantErr string
		call    func() error
	}{
		{"CreateRule/NilInput", awserrors.ErrorInvalidParameterValue, func() error {
			_, err := gateway_elbv2.CreateRule(context.Background(), nil, nil, testAccountID)
			return err
		}},
		{"CreateRule/MissingListenerArn", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.CreateRule(context.Background(), &elbv2.CreateRuleInput{}, nil, testAccountID)
			return err
		}},
		{"CreateRule/MissingPriority", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.CreateRule(context.Background(), &elbv2.CreateRuleInput{
				ListenerArn: aws.String(testLsnrArn),
			}, nil, testAccountID)
			return err
		}},
		{"CreateRule/MissingConditions", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.CreateRule(context.Background(), &elbv2.CreateRuleInput{
				ListenerArn: aws.String(testLsnrArn), Priority: aws.Int64(1),
			}, nil, testAccountID)
			return err
		}},
		{"CreateRule/MissingActions", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.CreateRule(context.Background(), &elbv2.CreateRuleInput{
				ListenerArn: aws.String(testLsnrArn), Priority: aws.Int64(1),
				Conditions: []*elbv2.RuleCondition{{Field: aws.String("path-pattern")}},
			}, nil, testAccountID)
			return err
		}},
		{"ModifyRule/NilInput", awserrors.ErrorInvalidParameterValue, func() error {
			_, err := gateway_elbv2.ModifyRule(context.Background(), nil, nil, testAccountID)
			return err
		}},
		{"ModifyRule/MissingRuleArn", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.ModifyRule(context.Background(), &elbv2.ModifyRuleInput{}, nil, testAccountID)
			return err
		}},
		// Either conditions or actions satisfies the guard; neither does not.
		{"ModifyRule/NoConditionsOrActions", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.ModifyRule(context.Background(), &elbv2.ModifyRuleInput{
				RuleArn: aws.String(testRuleArn),
			}, nil, testAccountID)
			return err
		}},
		{"DeleteRule/NilInput", awserrors.ErrorInvalidParameterValue, func() error {
			_, err := gateway_elbv2.DeleteRule(context.Background(), nil, nil, testAccountID)
			return err
		}},
		{"DeleteRule/MissingRuleArn", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.DeleteRule(context.Background(), &elbv2.DeleteRuleInput{}, nil, testAccountID)
			return err
		}},
		{"DescribeRules/NilInput", awserrors.ErrorInvalidParameterValue, func() error {
			_, err := gateway_elbv2.DescribeRules(context.Background(), nil, nil, testAccountID)
			return err
		}},
		{"DescribeRules/NeitherListenerNorRules", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.DescribeRules(context.Background(), &elbv2.DescribeRulesInput{}, nil, testAccountID)
			return err
		}},
		{"SetRulePriorities/NilInput", awserrors.ErrorInvalidParameterValue, func() error {
			_, err := gateway_elbv2.SetRulePriorities(context.Background(), nil, nil, testAccountID)
			return err
		}},
		{"SetRulePriorities/MissingPriorities", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.SetRulePriorities(context.Background(), &elbv2.SetRulePrioritiesInput{}, nil, testAccountID)
			return err
		}},
		{"AddTags/NilInput", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.AddTags(context.Background(), nil, nil, testAccountID)
			return err
		}},
		{"AddTags/MissingTags", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.AddTags(context.Background(), &elbv2.AddTagsInput{
				ResourceArns: []*string{aws.String(testLBArn)},
			}, nil, testAccountID)
			return err
		}},
		{"RemoveTags/MissingTagKeys", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.RemoveTags(context.Background(), &elbv2.RemoveTagsInput{
				ResourceArns: []*string{aws.String(testLBArn)},
			}, nil, testAccountID)
			return err
		}},
		{"DescribeTags/MissingResourceArns", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.DescribeTags(context.Background(), &elbv2.DescribeTagsInput{}, nil, testAccountID)
			return err
		}},
		{"SetSecurityGroups/MissingLBArn", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.SetSecurityGroups(context.Background(), &elbv2.SetSecurityGroupsInput{}, nil, testAccountID)
			return err
		}},
		{"SetSecurityGroups/MissingGroups", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.SetSecurityGroups(context.Background(), &elbv2.SetSecurityGroupsInput{
				LoadBalancerArn: aws.String(testLBArn),
			}, nil, testAccountID)
			return err
		}},
		{"SetIpAddressType/MissingLBArn", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.SetIpAddressType(context.Background(), &elbv2.SetIpAddressTypeInput{}, nil, testAccountID)
			return err
		}},
		{"SetIpAddressType/MissingType", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.SetIpAddressType(context.Background(), &elbv2.SetIpAddressTypeInput{
				LoadBalancerArn: aws.String(testLBArn),
			}, nil, testAccountID)
			return err
		}},
		{"SetSubnets/MissingLBArn", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.SetSubnets(context.Background(), &elbv2.SetSubnetsInput{}, nil, testAccountID)
			return err
		}},
		// SubnetMappings is the accepted alternative to Subnets; neither fails.
		{"SetSubnets/NoSubnetsOrMappings", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.SetSubnets(context.Background(), &elbv2.SetSubnetsInput{
				LoadBalancerArn: aws.String(testLBArn),
			}, nil, testAccountID)
			return err
		}},
		{"LBAgentHeartbeat/NilInput", awserrors.ErrorInvalidParameterValue, func() error {
			_, err := gateway_elbv2.LBAgentHeartbeat(context.Background(), nil, nil, testAccountID)
			return err
		}},
		{"LBAgentHeartbeat/MissingLBID", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.LBAgentHeartbeat(context.Background(), &handlers_elbv2.LBAgentHeartbeatInput{}, nil, testAccountID)
			return err
		}},
		{"GetLBConfig/NilInput", awserrors.ErrorInvalidParameterValue, func() error {
			_, err := gateway_elbv2.GetLBConfig(context.Background(), nil, nil, testAccountID)
			return err
		}},
		{"GetLBConfig/MissingLBID", awserrors.ErrorMissingParameter, func() error {
			_, err := gateway_elbv2.GetLBConfig(context.Background(), &handlers_elbv2.GetLBConfigInput{}, nil, testAccountID)
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.EqualError(t, tc.call(), tc.wantErr)
		})
	}
}

// A daemon-side awserrors envelope must surface as the error, not decode into
// a zero-valued success.
func TestGatewayDispatch_ErrorEnvelopePropagates(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	serveOnce(t, nc, "elbv2.DeleteLoadBalancer",
		utils.GenerateErrorPayload(awserrors.ErrorELBv2LoadBalancerNotFound))

	_, err := gateway_elbv2.DeleteLoadBalancer(context.Background(),
		&elbv2.DeleteLoadBalancerInput{LoadBalancerArn: aws.String(testLBArn)}, nc, testAccountID)
	require.EqualError(t, err, awserrors.ErrorELBv2LoadBalancerNotFound)
}

// No subscriber on the subject: the wrapper reports the missing responder
// rather than returning an empty result.
func TestGatewayDispatch_NoResponder(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	_, err := gateway_elbv2.DescribeLoadBalancers(context.Background(), &elbv2.DescribeLoadBalancersInput{}, nc, testAccountID)
	require.ErrorIs(t, err, nats.ErrNoResponders)
}

// A nil connection is refused before any request is attempted.
func TestGatewayDispatch_NilConnection(t *testing.T) {
	_, err := gateway_elbv2.DescribeLoadBalancers(context.Background(), &elbv2.DescribeLoadBalancersInput{}, nil, testAccountID)
	require.ErrorIs(t, err, utils.ErrClusterUnavailable)
}
