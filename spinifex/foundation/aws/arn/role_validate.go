package arn

import (
	"fmt"
	"regexp"
	"strings"
)

// iamAccountIDPattern matches AWS's 12-digit account ID.
var iamAccountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)

// iamRoleNamePattern matches an IAM role name: up to 64 characters from
// [\w+=,.@-], so no whitespace, quotes, newlines or colons.
var iamRoleNamePattern = regexp.MustCompile(`^[\w+=,.@-]{1,64}$`)

// iamPathPattern matches an IAM path: "/" alone, or printable ASCII between
// a leading and trailing "/".
var iamPathPattern = regexp.MustCompile(`^(/|/[\x21-\x7E]+/)$`)

// ValidateRoleARN reports whether s is syntactically an IAM role ARN,
// arn:aws:iam::<12-digit account>:role<path><name>, under IAM's naming rules.
// It checks syntax only, not that the role exists or who owns it.
func ValidateRoleARN(s string) error {
	service, region, account, resource, ok := Split(s)
	if !ok {
		return fmt.Errorf("not an arn:aws ARN")
	}
	if service != "iam" {
		return fmt.Errorf("not an IAM ARN")
	}
	if region != "" {
		return fmt.Errorf("an IAM ARN does not carry a region")
	}
	if !iamAccountIDPattern.MatchString(account) {
		return fmt.Errorf("account ID is not a 12-digit account ID")
	}
	rest, ok := strings.CutPrefix(resource, string(IAMRole))
	if !ok || !strings.HasPrefix(rest, "/") {
		return fmt.Errorf("ARN resource is not a role")
	}
	lastSlash := strings.LastIndex(rest, "/")
	name := rest[lastSlash+1:]
	path := rest[:lastSlash+1]
	if !iamRoleNamePattern.MatchString(name) {
		return fmt.Errorf("role name does not satisfy IAM's naming rules")
	}
	if !iamPathPattern.MatchString(path) {
		return fmt.Errorf("role path does not satisfy IAM's path rules")
	}
	return nil
}
