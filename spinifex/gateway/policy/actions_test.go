package policy

import "testing"

// TestIAMAction_BedrockNamespace asserts the Bedrock wire-service family
// collapses to the shared "bedrock:" IAM namespace, while other services keep
// their own prefix, so AWS-shaped policies authorize as they do on AWS.
func TestIAMAction_BedrockNamespace(t *testing.T) {
	cases := []struct {
		service, action, want string
	}{
		{"bedrock", "Converse", "bedrock:Converse"},
		{"bedrock-runtime", "ConverseStream", "bedrock:ConverseStream"},
		{"bedrock-runtime", "InvokeModel", "bedrock:InvokeModel"},
		{"bedrock-agent", "CreateKnowledgeBase", "bedrock:CreateKnowledgeBase"},
		{"bedrock-agent-runtime", "Retrieve", "bedrock:Retrieve"},
		{"bedrock-agent-runtime", "RetrieveAndGenerate", "bedrock:RetrieveAndGenerate"},
		{"ecr", "GetAuthorizationToken", "ecr:GetAuthorizationToken"},
		{"ecs", "RunTask", "ecs:RunTask"},
		{"s3", "GetObject", "s3:GetObject"},
	}
	for _, c := range cases {
		if got := IAMAction(c.service, c.action); got != c.want {
			t.Errorf("IAMAction(%q, %q) = %q, want %q", c.service, c.action, got, c.want)
		}
	}
}
