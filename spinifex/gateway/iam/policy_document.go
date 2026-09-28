package gateway_iam

import (
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go/service/iam"
)

// encodePolicyDocument percent-encodes a policy document the way the IAM Query
// API returns it: everything but letters, digits and -_.~ escaped, space as %20.
// The service layer keeps raw JSON because STS and spx admin parse it in-process.
func encodePolicyDocument(doc *string) *string {
	if doc == nil {
		return nil
	}
	encoded := strings.ReplaceAll(url.QueryEscape(*doc), "+", "%20")
	return &encoded
}

func encodeRoleDocuments(roles ...*iam.Role) {
	for _, r := range roles {
		if r != nil {
			r.AssumeRolePolicyDocument = encodePolicyDocument(r.AssumeRolePolicyDocument)
		}
	}
}

func encodeInstanceProfileDocuments(profiles ...*iam.InstanceProfile) {
	for _, p := range profiles {
		if p != nil {
			encodeRoleDocuments(p.Roles...)
		}
	}
}
