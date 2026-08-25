package arn

import "fmt"

// IAMResourceType is the resource-type segment of an arn:aws:iam ARN.
type IAMResourceType string

const (
	IAMUser            IAMResourceType = "user"
	IAMRole            IAMResourceType = "role"
	IAMGroup           IAMResourceType = "group"
	IAMPolicy          IAMResourceType = "policy"
	IAMInstanceProfile IAMResourceType = "instance-profile"
	IAMOIDCProvider    IAMResourceType = "oidc-provider"
)

// FormatIAMPath builds an IAM ARN for a path-bearing resource. The path is
// preserved verbatim; callers are responsible for validating it.
func FormatIAMPath(kind IAMResourceType, accountID, resourcePath, name string) string {
	if resourcePath == "" {
		resourcePath = "/"
	}
	return fmt.Sprintf("arn:aws:iam::%s:%s%s%s", accountID, kind, resourcePath, name)
}

// FormatIAMRoot builds the account root ARN, the principal AWS uses for the
// account itself rather than a named IAM resource.
func FormatIAMRoot(accountID string) string {
	return fmt.Sprintf("arn:aws:iam::%s:root", accountID)
}

// FormatIAMResource builds an IAM ARN from an already-formed resource
// component, such as an OIDC provider issuer host/path.
func FormatIAMResource(kind IAMResourceType, accountID, resource string) string {
	return fmt.Sprintf("arn:aws:iam::%s:%s/%s", accountID, kind, resource)
}
