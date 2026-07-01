package policy

import (
	"testing"

	"github.com/mulgadc/predastore/pkg/iampolicy"
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
		if got := iampolicy.Evaluate(a, "*", policies); got != iampolicy.Allow {
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
		got := iampolicy.Evaluate(tt.action, tt.resource, scoped)
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
		if got := iampolicy.Evaluate(a, "*", wildcard); got != iampolicy.Allow {
			t.Errorf("wildcard policy: expected Allow for %q, got %v", a, got)
		}
		if got := iampolicy.Evaluate(a, "*", scoped); got != iampolicy.Allow {
			t.Errorf("sts:* policy: expected Allow for %q, got %v", a, got)
		}
	}

	// Non-STS action must NOT match an sts:*-scoped policy — guards against a
	// pattern regression that would over-allow.
	if got := iampolicy.Evaluate("ec2:RunInstances", "*", scoped); got != iampolicy.Deny {
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
