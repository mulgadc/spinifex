package tags

import (
	"strings"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awstags "github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
)

// ReservedKeyError refuses a tag key a customer may not set: the aws: prefix,
// which AWS reserves in any case, and the platform's own keys.
func ReservedKeyError(key string) error {
	if strings.HasPrefix(strings.ToLower(key), "aws:") {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"Value ( %s ) for parameter key is invalid. Tag keys starting with 'aws:' are reserved for internal use", key)
	}
	return platformKeyError(key)
}

// platformKeyError refuses a platform-owned tag key with the error AWS gives
// for a reserved aws: key.
func platformKeyError(key string) error {
	if !strings.EqualFold(key, awstags.ManagedByKey) {
		return nil
	}
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
		"Value ( %s ) for parameter key is invalid. Tag key '%s' is reserved for internal use", key, awstags.ManagedByKey)
}
