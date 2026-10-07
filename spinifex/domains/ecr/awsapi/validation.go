package awsapi

import (
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

const (
	minRepositoryNameLength = 2
	maxRepositoryNameLength = 256
)

// ValidateRepositoryName returns InvalidParameterException naming the first
// constraint name fails, or nil.
func ValidateRepositoryName(name string) error {
	switch {
	case name == "":
		return ConstraintError("repositoryName", "must not be null")
	case len(name) < minRepositoryNameLength:
		return ConstraintError("repositoryName", fmt.Sprintf("must have length greater than or equal to %d", minRepositoryNameLength))
	case len(name) > maxRepositoryNameLength:
		return ConstraintError("repositoryName", fmt.Sprintf("must have length less than or equal to %d", maxRepositoryNameLength))
	case handlers_ecr.ValidateRepoName(name) != nil:
		return ConstraintError("repositoryName", "must satisfy regular expression '"+handlers_ecr.RepoNamePattern+"'")
	}
	return nil
}

// RequiredParameterError refuses a request that omits field.
func RequiredParameterError(field string) error {
	return ConstraintError(field, "Member must not be null")
}

// EnumValueError refuses a value of field outside allowed, which lists the
// values AWS's model declares, in the order AWS lists them.
func EnumValueError(field string, allowed ...string) error {
	return ConstraintError(field, "Member must satisfy enum value set: ["+strings.Join(allowed, ", ")+"]")
}

// MaxItemsError refuses a list field carrying more than limit members.
func MaxItemsError(field string, limit int) error {
	return ConstraintError(field, fmt.Sprintf("Member must have length less than or equal to %d", limit))
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

// ValidateTags refuses a tag list with a missing or empty key. AWS answers a
// null key as a parameter error and an empty one as a tag error.
func ValidateTags(tags []*ecr.Tag) error {
	for i, t := range tags {
		field := fmt.Sprintf("tags.%d.member.key", i+1)
		if t == nil || t.Key == nil {
			return RequiredParameterError(field)
		}
		if aws.StringValue(t.Key) == "" {
			return awserrors.Errorf(awserrors.ErrorInvalidTagParameter, "Tag parameters are invalid")
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
	return ConstraintError("resourceArn", "Invalid ARN")
}

// ImageTagMutabilityValues is the imageTagMutability enum AWS's model declares,
// in the order AWS lists it when refusing a value. The exclusion values are
// listed but not accepted.
var ImageTagMutabilityValues = []string{"IMMUTABLE", "MUTABLE", "MUTABLE_WITH_EXCLUSION", "IMMUTABLE_WITH_EXCLUSION"}
