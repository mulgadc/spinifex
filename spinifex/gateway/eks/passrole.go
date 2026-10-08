package gateway_eks

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
)

// PassedRoleARNs returns the role ARNs action hands to EKS, each needing
// iam:PassRole. An undecodable body passes nothing: the handler decodes the
// same bytes the same way and refuses it before acting.
func PassedRoleARNs(action string, body []byte) []string {
	var roleARN *string
	switch action {
	case "CreateCluster":
		input := new(eks.CreateClusterInput)
		if unmarshalIfBody(body, input) != nil {
			return nil
		}
		roleARN = input.RoleArn
	case "CreateNodegroup":
		input := new(eks.CreateNodegroupInput)
		if unmarshalIfBody(body, input) != nil {
			return nil
		}
		roleARN = input.NodeRole
	case "CreateAddon":
		input := new(eks.CreateAddonInput)
		if unmarshalIfBody(body, input) != nil {
			return nil
		}
		roleARN = input.ServiceAccountRoleArn
	case "UpdateAddon":
		input := new(eks.UpdateAddonInput)
		if unmarshalIfBody(body, input) != nil {
			return nil
		}
		roleARN = input.ServiceAccountRoleArn
	}
	if aws.StringValue(roleARN) == "" {
		return nil
	}
	return []string{*roleARN}
}
