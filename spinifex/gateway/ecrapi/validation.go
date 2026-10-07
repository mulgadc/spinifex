package gateway_ecrapi

import (
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/handlers/ecr"
)

// repositoryNamePattern is the expression AWS names when it refuses a
// repositoryName.
const repositoryNamePattern = `[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*`

const (
	minRepositoryNameLength = 2
	maxRepositoryNameLength = 256
)

// ValidateRepositoryName returns InvalidParameterException naming the first
// constraint name fails, or nil.
func ValidateRepositoryName(name string) error {
	switch {
	case name == "":
		return RequiredParameterError("repositoryName")
	case len(name) < minRepositoryNameLength:
		return awserrors.Errorf(awserrors.ErrorECRInvalidParameter,
			"1 validation error detected: Value '%s' at 'repositoryName' failed to satisfy constraint: Member must have length greater than or equal to %d",
			name, minRepositoryNameLength)
	case len(name) > maxRepositoryNameLength:
		return awserrors.Errorf(awserrors.ErrorECRInvalidParameter,
			"1 validation error detected: Value '%s' at 'repositoryName' failed to satisfy constraint: Member must have length less than or equal to %d",
			name, maxRepositoryNameLength)
	case handlers_ecr.ValidateRepoName(name) != nil:
		return ConstraintError("repositoryName", "must satisfy regular expression '"+repositoryNamePattern+"'")
	}
	return nil
}

// RequiredParameterError refuses a request that omits field.
func RequiredParameterError(field string) error {
	return awserrors.Errorf(awserrors.ErrorECRInvalidParameter,
		"1 validation error detected: Value null at '%s' failed to satisfy constraint: Member must not be null", field)
}

// EnumValueError refuses value for field, listing the values accepted.
func EnumValueError(field, value string, allowed ...string) error {
	return awserrors.Errorf(awserrors.ErrorECRInvalidParameter,
		"1 validation error detected: Value '%s' at '%s' failed to satisfy constraint: Member must satisfy enum value set: [%s]",
		value, field, strings.Join(allowed, ", "))
}

// MaxItemsError refuses a list field carrying more than limit members.
func MaxItemsError(field string, limit int) error {
	return awserrors.Errorf(awserrors.ErrorECRInvalidParameter,
		"1 validation error detected: Value at '%s' failed to satisfy constraint: Member must have length less than or equal to %d", field, limit)
}

// ConstraintError refuses field for failing constraint.
func ConstraintError(field, constraint string) error {
	return awserrors.Errorf(awserrors.ErrorECRInvalidParameter,
		"Invalid parameter at '%s' failed to satisfy constraint: '%s'", field, constraint)
}

// MalformedBodyError refuses a body that does not decode into the action's
// input shape.
func MalformedBodyError() error {
	return awserrors.Errorf(awserrors.ErrorECRInvalidParameter,
		"The request body is not valid JSON or does not match the input shape of the action")
}

// ValidateTags refuses a tag list with a missing or empty key.
func ValidateTags(tags []*ecr.Tag) error {
	for i, t := range tags {
		field := fmt.Sprintf("tags.%d.member.key", i+1)
		if t == nil || t.Key == nil {
			return RequiredParameterError(field)
		}
		if aws.StringValue(t.Key) == "" {
			return awserrors.Errorf(awserrors.ErrorECRInvalidParameter,
				"1 validation error detected: Value '' at '%s' failed to satisfy constraint: Member must have length greater than or equal to 1", field)
		}
	}
	return nil
}

// InvalidLifecyclePolicyError refuses a lifecycle policy the evaluation engine
// cannot parse or does not support.
func InvalidLifecyclePolicyError() error {
	return ConstraintError("lifecyclePolicyText", "Lifecycle policy validation failure")
}

func invalidResourceARNError() error {
	return ConstraintError("resourceArn", "must be a repository ARN of the form arn:aws:ecr:<region>:<account>:repository/<name>")
}
