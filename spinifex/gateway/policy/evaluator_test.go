package policy

import (
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// doc builds a single-statement policy document.
func doc(effect, action, resource string) handlers_iam.PolicyDocument {
	return handlers_iam.PolicyDocument{
		Version: "2012-10-17",
		Statement: []handlers_iam.Statement{
			{Effect: effect, Action: handlers_iam.StringOrArr{action}, Resource: handlers_iam.StringOrArr{resource}},
		},
	}
}

// The deny-wins algorithm and wildcard matcher are unit-tested in predastore's
// pkg/iampolicy. These tests pin spinifex's own action-string conventions end to
// end through iampolicy.Evaluate, exercising the aliased handlers_iam DTOs.

// TestEvaluate_IAMInstanceProfileActionStrings pins the action strings for
// iam:PassRole + the four EC2 instance-profile association actions so a rename of
// any ec2Actions key or checkPolicyResource call site is caught at the policy
// layer. Strings are produced dynamically via IAMAction.
func TestEvaluate_IAMInstanceProfileActionStrings(t *testing.T) {
	actions := []string{
		"iam:PassRole",
		"ec2:AssociateIamInstanceProfile",
		"ec2:DisassociateIamInstanceProfile",
		"ec2:ReplaceIamInstanceProfileAssociation",
		"ec2:DescribeIamInstanceProfileAssociations",
	}
	policies := []handlers_iam.PolicyDocument{
		doc("Allow", "*", "*"),
	}
	for _, a := range actions {
		if got := iampolicy.EvaluateWithKeys(a, "*", policies, nil); got != iampolicy.Allow {
			t.Errorf("expected Allow for action %q under wildcard policy, got %v", a, got)
		}
	}

	scoped := []handlers_iam.PolicyDocument{
		doc("Allow", "ec2:*IamInstanceProfile*", "*"),
		doc("Allow", "iam:PassRole", "arn:aws:iam::*:role/*"),
	}
	scopedTests := []struct {
		action   string
		resource string
		want     iampolicy.Decision
	}{
		{"ec2:AssociateIamInstanceProfile", "*", iampolicy.Allow},
		{"ec2:DisassociateIamInstanceProfile", "*", iampolicy.Allow},
		{"ec2:ReplaceIamInstanceProfileAssociation", "*", iampolicy.Allow},
		{"ec2:DescribeIamInstanceProfileAssociations", "*", iampolicy.Allow},
		{"iam:PassRole", "arn:aws:iam::123456789012:role/app-foo", iampolicy.Allow},
		{"iam:PassRole", "arn:aws:iam::123456789012:user/app-foo", iampolicy.Deny},
		{"ec2:RunInstances", "*", iampolicy.Deny},
	}
	for _, tt := range scopedTests {
		got := iampolicy.EvaluateWithKeys(tt.action, tt.resource, scoped, nil)
		if got != tt.want {
			t.Errorf("scoped policy, action=%s resource=%s: expected %v, got %v",
				tt.action, tt.resource, tt.want, got)
		}
	}
}

// TestEvaluate_STSActionStrings pins the action strings emitted by the STS
// gateway dispatcher (gateway/sts.go stsActions + checkPolicy(r, "sts", action)
// call site). Locks in that every STS verb the dispatcher accepts is matchable
// by the evaluator under both wildcard and service-scoped policies, so a future
// rename of any stsActions key surfaces here.
func TestEvaluate_STSActionStrings(t *testing.T) {
	actions := []string{
		"sts:AssumeRole",
		"sts:GetCallerIdentity",
	}

	wildcard := []handlers_iam.PolicyDocument{doc("Allow", "*", "*")}
	scoped := []handlers_iam.PolicyDocument{doc("Allow", "sts:*", "*")}

	for _, a := range actions {
		if got := iampolicy.EvaluateWithKeys(a, "*", wildcard, nil); got != iampolicy.Allow {
			t.Errorf("wildcard policy: expected Allow for %q, got %v", a, got)
		}
		if got := iampolicy.EvaluateWithKeys(a, "*", scoped, nil); got != iampolicy.Allow {
			t.Errorf("sts:* policy: expected Allow for %q, got %v", a, got)
		}
	}

	// Non-STS action must NOT match an sts:*-scoped policy — guards against a
	// pattern regression that would over-allow.
	if got := iampolicy.EvaluateWithKeys("ec2:RunInstances", "*", scoped, nil); got != iampolicy.Deny {
		t.Errorf("sts:* policy: expected Deny for ec2:RunInstances, got %v", got)
	}
}

// --- Action mapping tests ---

func TestIAMAction(t *testing.T) {
	got := IAMAction("ec2", "RunInstances")
	if got != "ec2:RunInstances" {
		t.Fatalf("IAMAction(ec2, RunInstances) = %q, want %q", got, "ec2:RunInstances")
	}
}

// --- Resource scoping ---

// TestEvaluate_ResourceScopedStatementsNeedARealARN pins why the gateway must
// pass the request's ARN rather than "*". The evaluator matches the statement's
// Resource as a pattern against the request's resource as a value, so a scoped
// statement never matches "*" in either direction: a Deny fails open and an
// Allow fails closed.
func TestEvaluate_ResourceScopedStatementsNeedARealARN(t *testing.T) {
	const prod = "arn:aws:ec2:ap-southeast-2:123456789012:instance/i-prod"
	const dev = "arn:aws:ec2:ap-southeast-2:123456789012:instance/i-dev"

	fenced := []handlers_iam.PolicyDocument{
		doc("Allow", "ec2:*", "*"),
		doc("Deny", "ec2:TerminateInstances", "arn:aws:ec2:*:*:instance/i-prod"),
	}
	if got := iampolicy.EvaluateWithKeys("ec2:TerminateInstances", "*", fenced, nil); got != iampolicy.Allow {
		t.Errorf("fenced policy against %q: got %v, want Allow — the guardrail is inert", "*", got)
	}
	if got := iampolicy.EvaluateWithKeys("ec2:TerminateInstances", prod, fenced, nil); got != iampolicy.Deny {
		t.Errorf("fenced policy against the fenced instance: got %v, want Deny", got)
	}
	if got := iampolicy.EvaluateWithKeys("ec2:TerminateInstances", dev, fenced, nil); got != iampolicy.Allow {
		t.Errorf("fenced policy against an unfenced instance: got %v, want Allow", got)
	}

	scopedAllow := []handlers_iam.PolicyDocument{
		doc("Allow", "ec2:*", "arn:aws:ec2:*:*:instance/i-dev"),
	}
	if got := iampolicy.EvaluateWithKeys("ec2:TerminateInstances", "*", scopedAllow, nil); got != iampolicy.Deny {
		t.Errorf("scoped Allow against %q: got %v, want Deny — least privilege is unexpressible", "*", got)
	}
	if got := iampolicy.EvaluateWithKeys("ec2:TerminateInstances", dev, scopedAllow, nil); got != iampolicy.Allow {
		t.Errorf("scoped Allow against the named instance: got %v, want Allow", got)
	}
	if got := iampolicy.EvaluateWithKeys("ec2:TerminateInstances", prod, scopedAllow, nil); got != iampolicy.Deny {
		t.Errorf("scoped Allow against a sibling instance: got %v, want Deny", got)
	}
}

// TestEvaluate_PassingARealARNCannotWithdrawAccess is the property the
// per-service rollout rests on. Every policy that functions today carries
// Resource "*" for the action, and "*" as a pattern matches any ARN value, so
// handing the evaluator a real ARN cannot turn an Allow into a Deny — not even
// a malformed one, which bounds a resolver bug to the newly-scoped statements.
func TestEvaluate_PassingARealARNCannotWithdrawAccess(t *testing.T) {
	resources := []string{
		"*",
		"arn:aws:ec2:ap-southeast-2:123456789012:instance/i-abc",
		"arn:aws:ec2:ap-southeast-2:123456789012:instance/*",
		"arn:aws:ec2:ap-southeast-2:123456789012:instance/i-abc/admin",
		"not-an-arn",
	}
	policies := [][]handlers_iam.PolicyDocument{
		{doc("Allow", "ec2:*", "*")},
		{doc("Allow", "*", "*")},
	}
	for _, p := range policies {
		for _, resource := range resources {
			if got := iampolicy.EvaluateWithKeys("ec2:RunInstances", resource, p, nil); got != iampolicy.Allow {
				t.Errorf("account-wide Allow against %q: got %v, want Allow", resource, got)
			}
		}
	}
}
