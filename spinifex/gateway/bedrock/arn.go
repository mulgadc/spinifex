package gateway_bedrock

import (
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
