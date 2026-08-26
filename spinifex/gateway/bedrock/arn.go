package gateway_bedrock

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// provisionedModelResourceType is the ARN resource-type segment for a
// provisioned-throughput commitment. Bedrock ARNs are slash-separated
// (unlike RDS's colon-separated ones), so the type and id share one segment.
const provisionedModelResourceType = "provisioned-model"

// FormatProvisionedModelARN builds the ARN a CreateProvisionedModelThroughput
// call returns and every other PT op accepts back in place of a raw id.
func FormatProvisionedModelARN(region, accountID, id string) string {
	return fmt.Sprintf("arn:aws:bedrock:%s:%s:%s/%s", region, accountID, provisionedModelResourceType, id)
}

// ParsedProvisionedModelARN is a PT ARN split into the parts a caller acts on.
// Partition and service are validated rather than returned: only
// "arn:aws:bedrock" is ever accepted.
type ParsedProvisionedModelARN struct {
	Region    string
	AccountID string
	ID        string
}

// provisionedModelARNSegmentCount is the number of colon-separated segments in
// arn:aws:bedrock:{region}:{accountID}:provisioned-model/{id}.
const provisionedModelARNSegmentCount = 6

// ParseProvisionedModelARN parses a PT ARN and validates it belongs to the
// caller: wrong partition/service/resource-type, a foreign region or account,
// or a malformed id are all rejected here, so a foreign-account ARN never
// reaches a store lookup at all, mirroring handlers_rds.ParseARN.
func ParseProvisionedModelARN(arn, region, accountID string) (ParsedProvisionedModelARN, error) {
	parts := strings.SplitN(arn, ":", provisionedModelARNSegmentCount)
	if len(parts) != provisionedModelARNSegmentCount {
		return ParsedProvisionedModelARN{}, ptARNError(arn, "expected the form arn:aws:bedrock:{region}:{account}:provisioned-model/{id}")
	}
	if parts[0] != "arn" || parts[1] != "aws" || parts[2] != "bedrock" {
		return ParsedProvisionedModelARN{}, ptARNError(arn, "only arn:aws:bedrock resources are addressable here")
	}
	if parts[3] != region {
		return ParsedProvisionedModelARN{}, ptARNError(arn, fmt.Sprintf("region %q does not match this endpoint's region %q", parts[3], region))
	}
	if parts[4] != accountID {
		return ParsedProvisionedModelARN{}, ptARNError(arn, "the resource belongs to another account")
	}

	resourceType, id, ok := strings.Cut(parts[5], "/")
	if !ok || resourceType != provisionedModelResourceType || id == "" || strings.ContainsAny(id, ":/") {
		return ParsedProvisionedModelARN{}, ptARNError(arn, "the resource name is empty or malformed")
	}

	return ParsedProvisionedModelARN{Region: parts[3], AccountID: parts[4], ID: id}, nil
}

// resolveProvisionedModelID accepts either a bare id or a full ARN (the shape
// every PT op's ProvisionedModelId field allows) and returns the bare id,
// validating region/account ownership when an ARN is given.
func resolveProvisionedModelID(idOrARN, region, accountID string) (string, error) {
	if !strings.HasPrefix(idOrARN, "arn:") {
		return idOrARN, nil
	}
	parsed, err := ParseProvisionedModelARN(idOrARN, region, accountID)
	if err != nil {
		return "", err
	}
	return parsed.ID, nil
}

func ptARNError(arn, why string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%q is not a valid provisioned-model ARN: %s", arn, why)
}

// looksLikeProvisionedModelARN is the cheap shape guard the inference path
// uses to decide whether a modelId needs the (heavier) parse-and-load
// translation at all: a bare modelId or a foundation-model ARN
// ("arn:aws:bedrock:*::foundation-model/...") must fall straight through to
// the existing GlobalAccountID-shorthand path unchanged.
func looksLikeProvisionedModelARN(modelID string) bool {
	return strings.HasPrefix(modelID, "arn:") && strings.Contains(modelID, ":"+provisionedModelResourceType+"/")
}

// knowledgeBaseResourceType is the ARN resource-type segment a knowledge base
// renders under. DataSource carries no ARN field in AWS's own shape (only
// KnowledgeBase does), so there is no data-source counterpart.
const knowledgeBaseResourceType = "knowledge-base"

// FormatKnowledgeBaseARN builds the ARN a CreateKnowledgeBase call returns,
// mirroring FormatGuardrailARN's shape.
func FormatKnowledgeBaseARN(region, accountID, id string) string {
	return fmt.Sprintf("arn:aws:bedrock:%s:%s:%s/%s", region, accountID, knowledgeBaseResourceType, id)
}

// guardrailResourceType is the ARN resource-type segment for a guardrail.
const guardrailResourceType = "guardrail"

// FormatGuardrailARN builds the ARN a CreateGuardrail call returns and every
// other guardrail op accepts back in place of a raw id.
func FormatGuardrailARN(region, accountID, id string) string {
	return fmt.Sprintf("arn:aws:bedrock:%s:%s:%s/%s", region, accountID, guardrailResourceType, id)
}

// ParsedGuardrailARN is a guardrail ARN split into the parts a caller acts on.
type ParsedGuardrailARN struct {
	Region    string
	AccountID string
	ID        string
}

// guardrailARNSegmentCount is the number of colon-separated segments in
// arn:aws:bedrock:{region}:{accountID}:guardrail/{id}.
const guardrailARNSegmentCount = 6

// ParseGuardrailARN parses a guardrail ARN and validates it belongs to the
// caller: wrong partition/service/resource-type, a foreign region or account,
// or a malformed id are all rejected here, mirroring ParseProvisionedModelARN.
func ParseGuardrailARN(arn, region, accountID string) (ParsedGuardrailARN, error) {
	parts := strings.SplitN(arn, ":", guardrailARNSegmentCount)
	if len(parts) != guardrailARNSegmentCount {
		return ParsedGuardrailARN{}, guardrailARNError(arn, "expected the form arn:aws:bedrock:{region}:{account}:guardrail/{id}")
	}
	if parts[0] != "arn" || parts[1] != "aws" || parts[2] != "bedrock" {
		return ParsedGuardrailARN{}, guardrailARNError(arn, "only arn:aws:bedrock resources are addressable here")
	}
	if parts[3] != region {
		return ParsedGuardrailARN{}, guardrailARNError(arn, fmt.Sprintf("region %q does not match this endpoint's region %q", parts[3], region))
	}
	if parts[4] != accountID {
		return ParsedGuardrailARN{}, guardrailARNError(arn, "the resource belongs to another account")
	}

	resourceType, id, ok := strings.Cut(parts[5], "/")
	if !ok || resourceType != guardrailResourceType || id == "" || strings.ContainsAny(id, ":/") {
		return ParsedGuardrailARN{}, guardrailARNError(arn, "the resource name is empty or malformed")
	}

	return ParsedGuardrailARN{Region: parts[3], AccountID: parts[4], ID: id}, nil
}

// resolveGuardrailID accepts either a bare id or a full ARN (the shape every
// guardrail op's GuardrailIdentifier field allows) and returns the bare id,
// validating region/account ownership when an ARN is given.
func resolveGuardrailID(idOrARN, region, accountID string) (string, error) {
	if !strings.HasPrefix(idOrARN, "arn:") {
		return idOrARN, nil
	}
	parsed, err := ParseGuardrailARN(idOrARN, region, accountID)
	if err != nil {
		return "", err
	}
	return parsed.ID, nil
}

func guardrailARNError(arn, why string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "%q is not a valid guardrail ARN: %s", arn, why)
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
	if !looksLikeProvisionedModelARN(modelID) {
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
