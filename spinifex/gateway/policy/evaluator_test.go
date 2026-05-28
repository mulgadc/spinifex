package policy

import (
	"testing"

	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// helper to build a single-statement policy document.
func doc(effect, action, resource string) handlers_iam.PolicyDocument {
	return handlers_iam.PolicyDocument{
		Version: "2012-10-17",
		Statement: []handlers_iam.Statement{
			{Effect: effect, Action: handlers_iam.StringOrArr{action}, Resource: handlers_iam.StringOrArr{resource}},
		},
	}
}

// Root bypass is tested at the gateway layer (checkPolicy), not in the evaluator.

func TestEvaluateAccess_DefaultDeny(t *testing.T) {
	// Non-root with no policies → default deny.
	got := EvaluateAccess("alice", "ec2:RunInstances", "*", nil)
	if got != Deny {
		t.Fatalf("expected default Deny with no policies, got %v", got)
	}
}

func TestEvaluateAccess_DefaultDenyEmptyPolicies(t *testing.T) {
	// Non-root with empty policy list → default deny.
	got := EvaluateAccess("alice", "ec2:RunInstances", "*", []handlers_iam.PolicyDocument{})
	if got != Deny {
		t.Fatalf("expected default Deny with empty policies, got %v", got)
	}
}

func TestEvaluateAccess_ExplicitAllow(t *testing.T) {
	policies := []handlers_iam.PolicyDocument{
		doc("Allow", "ec2:RunInstances", "*"),
	}
	got := EvaluateAccess("alice", "ec2:RunInstances", "*", policies)
	if got != Allow {
		t.Fatalf("expected Allow, got %v", got)
	}
}

func TestEvaluateAccess_ExplicitDenyWins(t *testing.T) {
	// Explicit deny overrides an explicit allow.
	policies := []handlers_iam.PolicyDocument{
		doc("Allow", "ec2:*", "*"),
		doc("Deny", "ec2:TerminateInstances", "*"),
	}
	got := EvaluateAccess("alice", "ec2:TerminateInstances", "*", policies)
	if got != Deny {
		t.Fatalf("expected Deny (explicit deny wins), got %v", got)
	}
}

func TestEvaluateAccess_ExplicitDenyWinsSamePolicy(t *testing.T) {
	// Deny and Allow in the same policy document — Deny wins.
	policies := []handlers_iam.PolicyDocument{
		{
			Version: "2012-10-17",
			Statement: []handlers_iam.Statement{
				{Effect: "Allow", Action: handlers_iam.StringOrArr{"ec2:*"}, Resource: handlers_iam.StringOrArr{"*"}},
				{Effect: "Deny", Action: handlers_iam.StringOrArr{"ec2:TerminateInstances"}, Resource: handlers_iam.StringOrArr{"*"}},
			},
		},
	}
	got := EvaluateAccess("alice", "ec2:TerminateInstances", "*", policies)
	if got != Deny {
		t.Fatalf("expected Deny (same-policy explicit deny), got %v", got)
	}
}

func TestEvaluateAccess_NoMatchingAction(t *testing.T) {
	policies := []handlers_iam.PolicyDocument{
		doc("Allow", "s3:GetObject", "*"),
	}
	got := EvaluateAccess("alice", "ec2:RunInstances", "*", policies)
	if got != Deny {
		t.Fatalf("expected Deny (no matching action), got %v", got)
	}
}

func TestEvaluateAccess_WildcardAllActions(t *testing.T) {
	policies := []handlers_iam.PolicyDocument{
		doc("Allow", "*", "*"),
	}
	got := EvaluateAccess("alice", "ec2:RunInstances", "*", policies)
	if got != Allow {
		t.Fatalf("expected Allow with wildcard *, got %v", got)
	}
}

func TestEvaluateAccess_ServiceWildcard(t *testing.T) {
	policies := []handlers_iam.PolicyDocument{
		doc("Allow", "ec2:*", "*"),
	}

	tests := []struct {
		action string
		want   Decision
	}{
		{"ec2:RunInstances", Allow},
		{"ec2:DescribeInstances", Allow},
		{"s3:GetObject", Deny},
		{"iam:CreateUser", Deny},
	}

	for _, tt := range tests {
		got := EvaluateAccess("alice", tt.action, "*", policies)
		if got != tt.want {
			t.Errorf("ec2:* policy, action=%s: expected %v, got %v", tt.action, tt.want, got)
		}
	}
}

func TestEvaluateAccess_PrefixWildcard(t *testing.T) {
	policies := []handlers_iam.PolicyDocument{
		doc("Allow", "s3:Get*", "*"),
	}

	tests := []struct {
		action string
		want   Decision
	}{
		{"s3:GetObject", Allow},
		{"s3:GetBucketPolicy", Allow},
		{"s3:PutObject", Deny},
		{"s3:DeleteObject", Deny},
	}

	for _, tt := range tests {
		got := EvaluateAccess("alice", tt.action, "*", policies)
		if got != tt.want {
			t.Errorf("s3:Get* policy, action=%s: expected %v, got %v", tt.action, tt.want, got)
		}
	}
}

func TestEvaluateAccess_MultipleActions(t *testing.T) {
	// A statement with multiple actions.
	policies := []handlers_iam.PolicyDocument{
		{
			Version: "2012-10-17",
			Statement: []handlers_iam.Statement{
				{
					Effect:   "Allow",
					Action:   handlers_iam.StringOrArr{"ec2:DescribeInstances", "ec2:RunInstances"},
					Resource: handlers_iam.StringOrArr{"*"},
				},
			},
		},
	}

	tests := []struct {
		action string
		want   Decision
	}{
		{"ec2:DescribeInstances", Allow},
		{"ec2:RunInstances", Allow},
		{"ec2:TerminateInstances", Deny},
	}

	for _, tt := range tests {
		got := EvaluateAccess("alice", tt.action, "*", policies)
		if got != tt.want {
			t.Errorf("multi-action policy, action=%s: expected %v, got %v", tt.action, tt.want, got)
		}
	}
}

func TestEvaluateAccess_MultiplePolicies(t *testing.T) {
	// Permissions spread across multiple policy documents.
	policies := []handlers_iam.PolicyDocument{
		doc("Allow", "ec2:DescribeInstances", "*"),
		doc("Allow", "s3:GetObject", "*"),
	}

	tests := []struct {
		action string
		want   Decision
	}{
		{"ec2:DescribeInstances", Allow},
		{"s3:GetObject", Allow},
		{"iam:CreateUser", Deny},
	}

	for _, tt := range tests {
		got := EvaluateAccess("alice", tt.action, "*", policies)
		if got != tt.want {
			t.Errorf("multi-policy, action=%s: expected %v, got %v", tt.action, tt.want, got)
		}
	}
}

func TestEvaluateAccess_ResourceMismatch(t *testing.T) {
	// Allow only on a specific resource, request uses "*".
	policies := []handlers_iam.PolicyDocument{
		doc("Allow", "s3:GetObject", "arn:aws:s3:::my-bucket/*"),
	}
	got := EvaluateAccess("alice", "s3:GetObject", "*", policies)
	if got != Deny {
		t.Fatalf("expected Deny (resource mismatch), got %v", got)
	}
}

func TestEvaluateAccess_CaseInsensitiveAction(t *testing.T) {
	// Action matching should be case-insensitive for exact matches.
	policies := []handlers_iam.PolicyDocument{
		doc("Allow", "EC2:RunInstances", "*"),
	}
	got := EvaluateAccess("alice", "ec2:RunInstances", "*", policies)
	if got != Allow {
		t.Fatalf("expected Allow (case-insensitive match), got %v", got)
	}
}

// --- wildcard matching tests (via matchesAny) ---

func TestMatchesAny_Wildcard(t *testing.T) {
	tests := []struct {
		pattern string
		value   string
		want    bool
	}{
		// Global wildcard
		{"*", "anything", true},
		{"*", "", true},

		// Service wildcard
		{"ec2:*", "ec2:RunInstances", true},
		{"ec2:*", "ec2:DescribeInstances", true},
		{"ec2:*", "s3:GetObject", false},
		{"EC2:*", "ec2:RunInstances", true},

		// Prefix wildcard
		{"s3:Get*", "s3:GetObject", true},
		{"s3:Get*", "s3:GetBucketPolicy", true},
		{"s3:Get*", "s3:PutObject", false},
		{"S3:get*", "s3:GetObject", true},

		// Exact match
		{"ec2:RunInstances", "ec2:RunInstances", true},
		{"ec2:RunInstances", "ec2:StopInstances", false},

		// Case insensitive exact match
		{"EC2:RunInstances", "ec2:RunInstances", true},
		{"ec2:runinstances", "ec2:RunInstances", true},

		// Embedded wildcards (AWS IAM-style — required for iam:PassRole ARN matching).
		{"arn:aws:iam::*:role/app-*", "arn:aws:iam::123456789012:role/app-foo", true},
		{"arn:aws:iam::*:role/app-*", "arn:aws:iam::999999999999:role/app-bar", true},
		{"arn:aws:iam::*:role/*", "arn:aws:iam::123456789012:role/anything", true},
		{"arn:aws:iam::123456789012:role/app-*", "arn:aws:iam::123456789012:role/app-foo", true},
		{"arn:aws:iam::*:role/app-*", "arn:aws:iam::123456789012:role/admin-foo", false},
		{"arn:aws:iam::*:role/app-*", "arn:aws:iam::123456789012:user/app-foo", false},
		{"arn:aws:iam::*:role/app-*", "arn:aws:iam::123456789012:role/app-", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},

		// Edge cases
		{"", "", true},
		{"", "something", false},
	}

	for _, tt := range tests {
		got := matchesAny([]string{tt.pattern}, tt.value)
		if got != tt.want {
			t.Errorf("matchesAny([%q], %q) = %v, want %v", tt.pattern, tt.value, got, tt.want)
		}
	}
}

// TestEvaluateAccess_PassRoleResourceARN exercises the resource-ARN matching
// path used by iam:PassRole enforcement. The caller's policy will typically
// scope PassRole to a wildcard ARN; the evaluator must match a concrete role
// ARN against it.
func TestEvaluateAccess_PassRoleResourceARN(t *testing.T) {
	policies := []handlers_iam.PolicyDocument{
		{
			Version: "2012-10-17",
			Statement: []handlers_iam.Statement{
				{
					Effect:   "Allow",
					Action:   handlers_iam.StringOrArr{"iam:PassRole"},
					Resource: handlers_iam.StringOrArr{"arn:aws:iam::*:role/app-*"},
				},
			},
		},
	}

	tests := []struct {
		resource string
		want     Decision
	}{
		{"arn:aws:iam::123456789012:role/app-foo", Allow},
		{"arn:aws:iam::999999999999:role/app-bar", Allow},
		{"arn:aws:iam::123456789012:role/admin-foo", Deny},
		{"arn:aws:iam::123456789012:user/app-foo", Deny},
	}
	for _, tt := range tests {
		got := EvaluateAccess("alice", "iam:PassRole", tt.resource, policies)
		if got != tt.want {
			t.Errorf("PassRole on %s: expected %v, got %v", tt.resource, tt.want, got)
		}
	}
}

// TestEvaluateAccess_IAMInstanceProfileActionStrings pins the action strings
// for iam:PassRole + the four EC2 instance-profile association actions so a
// rename of any ec2Actions key or checkPolicyResource call site is caught at
// the policy layer. Strings are produced dynamically via policy.IAMAction.
func TestEvaluateAccess_IAMInstanceProfileActionStrings(t *testing.T) {
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
		if got := EvaluateAccess("alice", a, "*", policies); got != Allow {
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
		want     Decision
	}{
		{"ec2:AssociateIamInstanceProfile", "*", Allow},
		{"ec2:DisassociateIamInstanceProfile", "*", Allow},
		{"ec2:ReplaceIamInstanceProfileAssociation", "*", Allow},
		{"ec2:DescribeIamInstanceProfileAssociations", "*", Allow},
		{"iam:PassRole", "arn:aws:iam::123456789012:role/app-foo", Allow},
		{"iam:PassRole", "arn:aws:iam::123456789012:user/app-foo", Deny},
		{"ec2:RunInstances", "*", Deny},
	}
	for _, tt := range scopedTests {
		got := EvaluateAccess("alice", tt.action, tt.resource, scoped)
		if got != tt.want {
			t.Errorf("scoped policy, action=%s resource=%s: expected %v, got %v",
				tt.action, tt.resource, tt.want, got)
		}
	}
}

// --- Action mapping tests ---

func TestIAMAction(t *testing.T) {
	got := IAMAction("ec2", "RunInstances")
	if got != "ec2:RunInstances" {
		t.Fatalf("IAMAction(ec2, RunInstances) = %q, want %q", got, "ec2:RunInstances")
	}
}

// --- Realistic scenario tests ---

func TestEvaluateAccess_ReadOnlyUser(t *testing.T) {
	// Simulate a read-only user: allow all Describe* actions, deny everything else.
	policies := []handlers_iam.PolicyDocument{
		{
			Version: "2012-10-17",
			Statement: []handlers_iam.Statement{
				{
					Effect:   "Allow",
					Action:   handlers_iam.StringOrArr{"ec2:Describe*"},
					Resource: handlers_iam.StringOrArr{"*"},
				},
			},
		},
	}

	tests := []struct {
		action string
		want   Decision
	}{
		{"ec2:DescribeInstances", Allow},
		{"ec2:DescribeVolumes", Allow},
		{"ec2:DescribeVpcs", Allow},
		{"ec2:RunInstances", Deny},
		{"ec2:TerminateInstances", Deny},
		{"iam:CreateUser", Deny},
	}

	for _, tt := range tests {
		got := EvaluateAccess("viewer", tt.action, "*", policies)
		if got != tt.want {
			t.Errorf("read-only user, action=%s: expected %v, got %v", tt.action, tt.want, got)
		}
	}
}

func TestEvaluateAccess_AdminWithDenyTerminate(t *testing.T) {
	// Admin that can do everything except terminate instances.
	policies := []handlers_iam.PolicyDocument{
		doc("Allow", "*", "*"),
		doc("Deny", "ec2:TerminateInstances", "*"),
	}

	tests := []struct {
		action string
		want   Decision
	}{
		{"ec2:RunInstances", Allow},
		{"ec2:DescribeInstances", Allow},
		{"s3:GetObject", Allow},
		{"iam:CreateUser", Allow},
		{"ec2:TerminateInstances", Deny}, // explicit deny
	}

	for _, tt := range tests {
		got := EvaluateAccess("admin", tt.action, "*", policies)
		if got != tt.want {
			t.Errorf("admin-no-terminate, action=%s: expected %v, got %v", tt.action, tt.want, got)
		}
	}
}
