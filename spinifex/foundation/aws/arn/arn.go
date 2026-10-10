package arn

import "strings"

// Split breaks an arn:aws ARN into its service, region, account and resource
// components. The resource keeps any further colons or slashes verbatim. ok is
// false for anything outside the aws partition, the only one this stack builds.
func Split(s string) (service, region, account, resource string, ok bool) {
	parts := strings.SplitN(s, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" {
		return "", "", "", "", false
	}
	return parts[2], parts[3], parts[4], parts[5], true
}
