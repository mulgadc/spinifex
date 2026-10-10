package gateway_iam

import (
	"regexp"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

const maxPathPrefixLength = 512

// The three PathPrefix shapes in the IAM model: identity lists, policy lists
// (a whole path, trailing slash included), and ListEntitiesForPolicy.
var (
	identityPathPrefix = regexp.MustCompile(`^/[\x21-\x7F]*$`)
	policyPathPrefix   = regexp.MustCompile(`^(/[A-Za-z0-9.,+@=_-]+)*/$`)
	entityPathPrefix   = regexp.MustCompile(`^(/|/[\x21-\x7E]+/)$`)
)

// validatePathPrefix checks an optional PathPrefix as AWS does: the pattern
// first, with IAM's own message, then the model's length limit.
func validatePathPrefix(prefix *string, pattern *regexp.Regexp) error {
	if prefix == nil {
		return nil
	}
	if !pattern.MatchString(*prefix) {
		return awserrors.Errorf(awserrors.ErrorValidationError,
			"The specified value for pathPrefix is invalid. It must begin with the / character and contain only alphanumeric characters and/or / characters.")
	}
	if len(*prefix) > maxPathPrefixLength {
		return awserrors.Errorf(awserrors.ErrorValidationError,
			"1 validation error detected: Value at 'pathPrefix' failed to satisfy constraint: Member must have length less than or equal to %d", maxPathPrefixLength)
	}
	return nil
}
