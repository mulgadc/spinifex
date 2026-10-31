package gateway

// principalContext is the identity envelope set on the request context after
// SigV4 verification succeeds.
type principalContext struct {
	identity          string
	accountID         string
	principalType     string
	assumedRoleARN    string
	assumedRoleID     string
	underlyingRoleARN string
	// userID is aws:userid, resolved where the principal is so the value cannot
	// differ between two policy checks in the same request.
	userID string
	// userARN is an IAM user's stored ARN, path included, from the same read as
	// userID. Empty for every other principal type.
	userARN string
}
