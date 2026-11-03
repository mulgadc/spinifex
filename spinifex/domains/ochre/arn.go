package ochre

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// provisionedModelResourceType is the ARN resource-type segment for a
// provisioned-throughput commitment. Bedrock ARNs are slash-separated
// (unlike RDS's colon-separated ones), so the type and id share one segment.
const provisionedModelResourceType = "provisioned-model"

// knowledgeBaseResourceType is the ARN resource-type segment a knowledge base
// renders under. DataSource carries no ARN field in AWS's own shape (only
// KnowledgeBase does), so there is no data-source counterpart.
const knowledgeBaseResourceType = "knowledge-base"

// guardrailResourceType is the ARN resource-type segment for a guardrail.
const guardrailResourceType = "guardrail"

// ParsedBedrockARN is a Bedrock ARN split into the parts a caller acts on.
// Partition and service are validated rather than returned: only
// "arn:aws:bedrock" is ever accepted.
type ParsedBedrockARN struct {
	Region    string
	AccountID string
	ID        string
}

// ParsedProvisionedModelARN is a parsed provisioned-model ARN.
type ParsedProvisionedModelARN = ParsedBedrockARN

// ParsedGuardrailARN is a parsed guardrail ARN.
type ParsedGuardrailARN = ParsedBedrockARN

func formatBedrockARN(region, accountID, resourceType, id string) string {
	return fmt.Sprintf("arn:aws:bedrock:%s:%s:%s/%s", region, accountID, resourceType, id)
}

// parseBedrockARN parses arn:aws:bedrock:{region}:{account}:{resourceType}/{id}
// and rejects a foreign region or account, a different resource type or a
// malformed id, so a foreign-account ARN never reaches a store lookup.
func parseBedrockARN(s, region, accountID, resourceType string) (ParsedBedrockARN, error) {
	invalid := func(why string) (ParsedBedrockARN, error) {
		return ParsedBedrockARN{}, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%q is not a valid %s ARN: %s", s, resourceType, why)
	}
	service, arnRegion, arnAccount, resource, ok := arn.Split(s)
	if !ok {
		return invalid(fmt.Sprintf("expected the form arn:aws:bedrock:{region}:{account}:%s/{id}", resourceType))
	}
	if service != "bedrock" {
		return invalid("only arn:aws:bedrock resources are addressable here")
	}
	if arnRegion != region {
		return invalid(fmt.Sprintf("region %q does not match this endpoint's region %q", arnRegion, region))
	}
	if arnAccount != accountID {
		return invalid("the resource belongs to another account")
	}
	gotType, id, ok := strings.Cut(resource, "/")
	if !ok || gotType != resourceType || id == "" || strings.ContainsAny(id, ":/") {
		return invalid("the resource name is empty or malformed")
	}
	return ParsedBedrockARN{Region: arnRegion, AccountID: arnAccount, ID: id}, nil
}

// resolveBedrockID returns a bare id unchanged, or parses an ARN of
// resourceType and returns its id after the ownership checks.
func resolveBedrockID(idOrARN, region, accountID, resourceType string) (string, error) {
	if !strings.HasPrefix(idOrARN, "arn:") {
		return idOrARN, nil
	}
	parsed, err := parseBedrockARN(idOrARN, region, accountID, resourceType)
	if err != nil {
		return "", err
	}
	return parsed.ID, nil
}

// FormatProvisionedModelARN builds the ARN a CreateProvisionedModelThroughput
// call returns and every other PT op accepts back in place of a raw id.
func FormatProvisionedModelARN(region, accountID, id string) string {
	return formatBedrockARN(region, accountID, provisionedModelResourceType, id)
}

// ParseProvisionedModelARN parses a PT ARN and validates it belongs to the
// caller, mirroring rdsinstance.ParseARN.
func ParseProvisionedModelARN(s, region, accountID string) (ParsedProvisionedModelARN, error) {
	return parseBedrockARN(s, region, accountID, provisionedModelResourceType)
}

// ResolveProvisionedModelID accepts either a bare id or a full ARN (the shape
// every PT op's ProvisionedModelId field allows) and returns the bare id.
func ResolveProvisionedModelID(idOrARN, region, accountID string) (string, error) {
	return resolveBedrockID(idOrARN, region, accountID, provisionedModelResourceType)
}

// LooksLikeProvisionedModelARN is the cheap shape guard the inference path
// uses to decide whether a modelId needs the (heavier) parse-and-load
// translation at all: a bare modelId or a foundation-model ARN
// ("arn:aws:bedrock:*::foundation-model/...") must fall straight through to
// the existing GlobalAccountID-shorthand path unchanged.
func LooksLikeProvisionedModelARN(modelID string) bool {
	return strings.HasPrefix(modelID, "arn:") && strings.Contains(modelID, ":"+provisionedModelResourceType+"/")
}

// FormatKnowledgeBaseARN builds the ARN a CreateKnowledgeBase call returns.
func FormatKnowledgeBaseARN(region, accountID, id string) string {
	return formatBedrockARN(region, accountID, knowledgeBaseResourceType, id)
}

// FormatGuardrailARN builds the ARN a CreateGuardrail call returns and every
// other guardrail op accepts back in place of a raw id.
func FormatGuardrailARN(region, accountID, id string) string {
	return formatBedrockARN(region, accountID, guardrailResourceType, id)
}

// ParseGuardrailARN parses a guardrail ARN and validates it belongs to the
// caller.
func ParseGuardrailARN(s, region, accountID string) (ParsedGuardrailARN, error) {
	return parseBedrockARN(s, region, accountID, guardrailResourceType)
}

// ResolveGuardrailID accepts either a bare id or a full ARN (the shape every
// guardrail op's GuardrailIdentifier field allows) and returns the bare id.
func ResolveGuardrailID(idOrARN, region, accountID string) (string, error) {
	return resolveBedrockID(idOrARN, region, accountID, guardrailResourceType)
}

// resolveInferenceTarget translates modelID into the (accountID, modelID)
// pair the rest of the inference path — catalog lookup, access grant, and
// endpoint resolution — must act on. A bare modelId (or any ARN that is not
// shaped like a provisioned-model one) passes through unchanged with an
// empty target account, meaning "resolve via the GlobalAccountID shorthand".
// A PT ARN is parsed, its commitment loaded from store, and the
// commitment's own (AccountID, ModelID) is returned instead, so inference
// against a PT ARN reaches the pinned endpoint for that specific commitment
// rather than the shared platform one.
//
// Any failure to resolve a PT ARN — malformed, wrong region, foreign
// account, or an unknown commitment — reports ResourceNotFoundException: to
// an inference caller a PT ARN it cannot use is indistinguishable from one
// that was never a valid model identifier at all. On failure targetModelID
// is the original modelID unchanged, so a caller logging the attempt still
// records what was actually requested.
func resolveInferenceTarget(ctx context.Context, accountID, modelID string, store *ProvisionedStore) (targetAccountID, targetModelID string, err error) {
	if !LooksLikeProvisionedModelARN(modelID) {
		return "", modelID, nil
	}
	if store == nil {
		return "", modelID, errors.New(awserrors.ErrorResourceNotFoundException)
	}

	parsed, parseErr := ParseProvisionedModelARN(modelID, store.region, accountID)
	if parseErr != nil {
		return "", modelID, errors.New(awserrors.ErrorResourceNotFoundException)
	}

	rec, found, err := store.get(ctx, parsed.AccountID, parsed.ID)
	if err != nil {
		return "", modelID, err
	}
	if !found || rec.AccountID != accountID {
		return "", modelID, errors.New(awserrors.ErrorResourceNotFoundException)
	}

	return rec.AccountID, rec.ModelID, nil
}
