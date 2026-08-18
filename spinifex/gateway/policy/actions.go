package policy

// bedrockIAMFamily lists the Bedrock wire/signing service names that all
// authorize under the single "bedrock:" IAM namespace in AWS (runtime, agent,
// and agent-runtime actions are namespaced as bedrock: on AWS).
var bedrockIAMFamily = map[string]bool{
	"bedrock-runtime":       true,
	"bedrock-agent":         true,
	"bedrock-agent-runtime": true,
}

// iamNamespace maps a wire service name to its IAM policy namespace, collapsing
// the Bedrock family to the shared "bedrock:" namespace. Every other service
// keeps its own name.
func iamNamespace(service string) string {
	if bedrockIAMFamily[service] {
		return "bedrock"
	}
	return service
}

// IAMAction formats a service and action as the IAM policy string
// "namespace:ActionName", collapsing the Bedrock family to the shared bedrock:
// namespace so AWS-shaped policies authorize as they do on AWS.
func IAMAction(service, action string) string {
	return iamNamespace(service) + ":" + action
}
