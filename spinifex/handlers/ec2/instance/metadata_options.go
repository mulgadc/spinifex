package handlers_ec2_instance

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// defaultMetadataHopLimit is the IMDS PUT-response hop limit applied at launch
// when the request does not specify one, matching the AWS default of 1.
const defaultMetadataHopLimit = int64(1)

// buildMetadataOptions returns the constant IMDSv2-only metadata-options block
// stamped onto every instance at launch. Every field except the hop limit is a
// platform invariant: HttpTokens=required (IMDSv1 permanently disabled), the
// endpoint is always enabled, and the IPv6/tags metadata paths are not modelled.
// hopLimit overrides the default only when in the AWS-valid range 1-64; any
// other value (including nil) falls back to 1, leaving validation to the
// control-plane callers.
func buildMetadataOptions(hopLimit *int64) *ec2.InstanceMetadataOptionsResponse {
	limit := defaultMetadataHopLimit
	if hopLimit != nil && *hopLimit >= 1 && *hopLimit <= 64 {
		limit = *hopLimit
	}
	return &ec2.InstanceMetadataOptionsResponse{
		State:                   aws.String(ec2.InstanceMetadataOptionsStateApplied),
		HttpTokens:              aws.String(ec2.HttpTokensStateRequired),
		HttpEndpoint:            aws.String(ec2.InstanceMetadataEndpointStateEnabled),
		HttpProtocolIpv6:        aws.String(ec2.InstanceMetadataProtocolStateDisabled),
		InstanceMetadataTags:    aws.String(ec2.InstanceMetadataTagsStateDisabled),
		HttpPutResponseHopLimit: aws.Int64(limit),
	}
}

// validateMetadataOptions rejects any metadata-options request that would weaken
// the IMDSv2-only posture or enable a feature the platform does not model. It is
// shared by RunInstances and ModifyInstanceMetadataOptions. Empty values carry
// AWS "leave unchanged" semantics and pass as idempotent no-ops; only the hop
// limit is mutable, accepted across the AWS-valid 1-64 range. Under permanent
// IMDSv2 enforcement, rejecting http-tokens=optional with UnsupportedOperation is
// AWS-faithful, not a divergence.
func validateMetadataOptions(httpTokens, httpEndpoint, ipv6, tags string, hopLimit *int64) error {
	if httpTokens != "" && httpTokens != ec2.HttpTokensStateRequired {
		return errors.New(awserrors.ErrorUnsupportedOperation)
	}
	if httpEndpoint != "" && httpEndpoint != ec2.InstanceMetadataEndpointStateEnabled {
		return errors.New(awserrors.ErrorUnsupportedOperation)
	}
	if ipv6 != "" && ipv6 != ec2.InstanceMetadataProtocolStateDisabled {
		return errors.New(awserrors.ErrorUnsupportedOperation)
	}
	if tags != "" && tags != ec2.InstanceMetadataTagsStateDisabled {
		return errors.New(awserrors.ErrorUnsupportedOperation)
	}
	if hopLimit != nil && (*hopLimit < 1 || *hopLimit > 64) {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return nil
}

// applyMetadataOptions applies a metadata-options change to an instance. A
// legacy instance launched before the constant block (nil MetadataOptions) is
// stamped with it now; otherwise only the hop limit moves. validateMetadataOptions
// has already constrained hopLimit to 1-64 and rejected every other change, so
// the remaining fields stay at their platform-invariant values. A nil hopLimit
// (e.g. a no-op http-tokens=required) leaves the existing hop limit untouched.
func applyMetadataOptions(instance *ec2.Instance, hopLimit *int64) {
	if instance == nil {
		return
	}
	if instance.MetadataOptions == nil {
		instance.MetadataOptions = buildMetadataOptions(hopLimit)
		return
	}
	if hopLimit != nil {
		instance.MetadataOptions.HttpPutResponseHopLimit = hopLimit
	}
}
