package awsapi

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
)

// PassedRoleARNs returns the role ARNs action hands to EKS, each needing
// iam:PassRole. An undecodable body passes nothing: the handler decodes the
// same bytes the same way and refuses it before acting.
func PassedRoleARNs(action string, body []byte) []string {
	var roleARN *string
	switch action {
	case "CreateCluster":
		input := new(eks.CreateClusterInput)
		if envelope.UnmarshalJSONBody(body, input) != nil {
			return nil
		}
		roleARN = input.RoleArn
	case "CreateNodegroup":
		input := new(eks.CreateNodegroupInput)
		if envelope.UnmarshalJSONBody(body, input) != nil {
			return nil
		}
		roleARN = input.NodeRole
	case "CreateAddon":
		input := new(eks.CreateAddonInput)
		if envelope.UnmarshalJSONBody(body, input) != nil {
			return nil
		}
		roleARN = input.ServiceAccountRoleArn
	case "UpdateAddon":
		input := new(eks.UpdateAddonInput)
		if envelope.UnmarshalJSONBody(body, input) != nil {
			return nil
		}
		roleARN = input.ServiceAccountRoleArn
	}
	if aws.StringValue(roleARN) == "" {
		return nil
	}
	return []string{*roleARN}
}
