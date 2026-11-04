package iam

import (
	"errors"
	"maps"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// attachPolicyARN records arn on an entity's attached policies, reporting
// false when it is already attached, which AWS treats as a no-op.
func attachPolicyARN(attached *[]string, arn string) bool {
	if slices.Contains(*attached, arn) {
		return false
	}
	*attached = append(*attached, arn)
	return true
}

// detachPolicyARN drops arn from an entity's attached policies, or refuses
// with NoSuchEntity when it is not attached.
func detachPolicyARN(attached *[]string, arn string) error {
	idx := slices.Index(*attached, arn)
	if idx < 0 {
		return policyNotFound(arn)
	}
	*attached = slices.Delete(*attached, idx, idx+1)
	return nil
}

// validateInlinePolicy checks a Put{User,Group,Role}Policy request before any
// record is read: the policy name, the document length and its grammar.
func validateInlinePolicy(entityType, entityName, policyName, doc string) error {
	if err := validateIAMName("policyName", policyName, 128); err != nil {
		return err
	}
	if err := checkPolicyDocumentLength(doc); err != nil {
		return err
	}
	if _, err := ValidatePolicyDocument(doc); err != nil {
		return awserrors.Errorf(awserrors.ErrorIAMMalformedPolicyDocument,
			"policy %q on %s %q: %w", policyName, entityType, entityName, err)
	}
	return nil
}

// putInlinePolicy upserts doc as policyName unless the entity's inline
// policies would then exceed quota in total.
func putInlinePolicy(inline *map[string]string, entityType, entityName string, quota int, policyName, doc string) error {
	if err := checkInlinePolicySize(entityType, entityName, quota, *inline, policyName, doc); err != nil {
		return err
	}
	if *inline == nil {
		*inline = map[string]string{}
	}
	(*inline)[policyName] = doc
	return nil
}

// deleteInlinePolicy removes policyName, or refuses with NoSuchEntity naming
// kind (e.g. "user policy") when the entity has no such inline policy.
func deleteInlinePolicy(inline map[string]string, kind, policyName string) error {
	if _, ok := inline[policyName]; !ok {
		return noSuchEntity(kind, policyName)
	}
	delete(inline, policyName)
	return nil
}

// inlinePolicyNames returns an entity's inline policy names, sorted for
// deterministic List{User,Group,Role}Policies output.
func inlinePolicyNames(inline map[string]string) []*string {
	return aws.StringSlice(slices.Sorted(maps.Keys(inline)))
}

// applyTags upserts add into tags, refusing with LimitExceeded and leaving
// tags untouched when the result would exceed maxTagsPerResource.
func applyTags(tags *[]Tag, add []*iam.Tag, keys keyCase) error {
	merged := mergeTags(*tags, add, keys)
	if len(merged) > maxTagsPerResource {
		return errors.New(awserrors.ErrorIAMLimitExceeded)
	}
	*tags = merged
	return nil
}
