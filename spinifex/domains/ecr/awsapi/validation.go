package awsapi

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecr"
	ecrdomain "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

const (
	minRepositoryNameLength = 2
	maxRepositoryNameLength = 256

	minLifecyclePolicyTextLength  = 100
	maxLifecyclePolicyTextLength  = 30720
	maxRepositoryPolicyTextLength = 10240

	minImageTagLength     = 1
	maxImageTagLength     = 300
	maxMediaTypeListItems = 100
	maxRepositoryNameList = 100

	minMaxResults           = 1
	maxMaxResults           = 1000
	maxKMSKeyLength         = 2048
	maxExclusionFilter      = 5
	maxExclusionFilterValue = 128
	maxRegistryIDList       = 10
)

var (
	exclusionFilterPattern = regexp.MustCompile(`^[0-9a-zA-Z._*-]{1,128}$`)
	registryIDPattern      = regexp.MustCompile(`^[0-9]{12}$`)
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
	case ecrdomain.ValidateRepoName(name) != nil:
		return ConstraintError("repositoryName", "must satisfy regular expression '"+ecrdomain.RepoNamePattern+"'")
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

// validateListLength refuses a list field whose member count falls outside
// [minLen, maxLen], the bounds of field's model shape.
func validateListLength(field string, n, minLen, maxLen int) error {
	if n < minLen {
		return ConstraintError(field, fmt.Sprintf("Member must have length greater than or equal to %d", minLen))
	}
	if n > maxLen {
		return MaxItemsError(field, maxLen)
	}
	return nil
}

// validateImageIDs checks imageIds against ImageIdentifierList: 1-100 members,
// each tag 1-300 characters. An omitted list is refused only when required.
func validateImageIDs(ids []imageIdentifier, required bool) error {
	if ids == nil {
		if required {
			return RequiredParameterError("imageIds")
		}
		return nil
	}
	if err := validateListLength("imageIds", len(ids), 1, maxImageBatch); err != nil {
		return err
	}
	for i, id := range ids {
		if id.ImageTag == nil {
			continue
		}
		field := fmt.Sprintf("imageIds.%d.member.imageTag", i+1)
		if err := ValidateTextLength(field, *id.ImageTag, minImageTagLength, maxImageTagLength); err != nil {
			return err
		}
	}
	return nil
}

// ValidateTextLength refuses text whose length in characters falls outside
// [minLen, maxLen], the bounds of field's model shape.
func ValidateTextLength(field, text string, minLen, maxLen int) error {
	switch n := utf8.RuneCountInString(text); {
	case n < minLen:
		return ConstraintError(field, fmt.Sprintf("Member must have length greater than or equal to %d", minLen))
	case n > maxLen:
		return ConstraintError(field, fmt.Sprintf("Member must have length less than or equal to %d", maxLen))
	}
	return nil
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

// ValidateTags refuses a tag list with a missing or empty key or a missing
// value. AWS answers a null member as a parameter error and an empty key as a
// tag error; an empty value is valid.
func ValidateTags(tags []*ecr.Tag) error {
	for i, t := range tags {
		field := fmt.Sprintf("tags.%d.member.key", i+1)
		if t == nil || t.Key == nil {
			return RequiredParameterError(field)
		}
		if aws.StringValue(t.Key) == "" {
			return awserrors.Errorf(awserrors.ErrorInvalidTagParameter, "Tag parameters are invalid")
		}
		if t.Value == nil {
			return RequiredParameterError(fmt.Sprintf("tags.%d.member.value", i+1))
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

// TagStatusValues is the filter tagStatus enum AWS's model declares.
var TagStatusValues = []string{"TAGGED", "UNTAGGED", "ANY"}

// ImageStatusValues is the filter imageStatus enum, in the order AWS lists it
// when refusing a value.
var ImageStatusValues = []string{"ARCHIVED", "ACTIVE", "ANY", "ACTIVATING"}

// validateMaxResults refuses a maxResults outside MaxResults' range of
// 1-1000; an omitted value is valid.
func validateMaxResults(n *int64) error {
	switch {
	case n == nil:
		return nil
	case *n < minMaxResults:
		return ConstraintError("maxResults", fmt.Sprintf("Member must have value greater than or equal to %d", minMaxResults))
	case *n > maxMaxResults:
		return ConstraintError("maxResults", fmt.Sprintf("Member must have value less than or equal to %d", maxMaxResults))
	}
	return nil
}

// exclusionFilter is one imageTagMutabilityExclusionFilters member.
type exclusionFilter struct {
	FilterType *string `json:"filterType"`
	Filter     *string `json:"filter"`
}

// validateExclusionFilters checks imageTagMutabilityExclusionFilters against
// its model: 1-5 members, each a WILDCARD filterType and a filter matching the
// model's pattern. The filters are checked only, never applied.
func validateExclusionFilters(filters []exclusionFilter) error {
	const field = "imageTagMutabilityExclusionFilters"
	if filters == nil {
		return nil
	}
	if err := validateListLength(field, len(filters), 1, maxExclusionFilter); err != nil {
		return err
	}
	for i, f := range filters {
		member := fmt.Sprintf("%s.%d.member", field, i+1)
		switch {
		case f.FilterType == nil:
			return RequiredParameterError(member + ".filterType")
		case *f.FilterType != "WILDCARD":
			return EnumValueError(member+".filterType", "WILDCARD")
		case f.Filter == nil:
			return RequiredParameterError(member + ".filter")
		}
		if exclusionFilterPattern.MatchString(*f.Filter) {
			continue
		}
		constraint := "Member must satisfy regular expression pattern: " + exclusionFilterPattern.String()
		switch n := utf8.RuneCountInString(*f.Filter); {
		case n < 1:
			constraint += "; Member must have length greater than or equal to 1"
		case n > maxExclusionFilterValue:
			constraint += fmt.Sprintf("; Member must have length less than or equal to %d", maxExclusionFilterValue)
		}
		return ConstraintError(member+".filter", constraint)
	}
	return nil
}

// ValidateAuthorizationTokenRequest checks GetAuthorizationToken's deprecated
// registryIds against its model: 1-10 members, each 12 digits. The ids carry
// no meaning, as on AWS, so they are only checked.
func ValidateAuthorizationTokenRequest(body []byte) error {
	var req struct {
		RegistryIDs []string `json:"registryIds"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return MalformedBodyError()
		}
	}
	if req.RegistryIDs == nil {
		return nil
	}
	if err := validateListLength("registryIds", len(req.RegistryIDs), 1, maxRegistryIDList); err != nil {
		return err
	}
	for _, id := range req.RegistryIDs {
		if !registryIDPattern.MatchString(id) {
			return ConstraintError("registryIds", "Member must satisfy constraint: [Member must satisfy regular expression pattern: [0-9]{12}]")
		}
	}
	return nil
}
