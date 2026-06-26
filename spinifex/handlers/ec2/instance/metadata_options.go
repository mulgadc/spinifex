package handlers_ec2_instance

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// defaultMetadataHopLimit matches the AWS default applied when none is requested.
const defaultMetadataHopLimit = int64(1)

// buildMetadataOptions returns the constant IMDSv2-only block stamped at launch.
// Every field but the hop limit is a platform invariant; hopLimit applies only in
// the AWS-valid 1-64 range, otherwise falling back to the default.
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

// validateMetadataOptions rejects any request that weakens the IMDSv2-only
// posture or enables an unmodelled feature. Empty values are "leave unchanged"
// no-ops; only the hop limit (AWS-valid 1-64) is mutable. Shared by RunInstances
// and ModifyInstanceMetadataOptions.
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

// applyMetadataOptions stamps the constant block onto a legacy nil-block instance
// or moves the mutable hop limit. Callers must guard a nil instance.
func applyMetadataOptions(instance *ec2.Instance, hopLimit *int64) {
	if instance.MetadataOptions == nil {
		instance.MetadataOptions = buildMetadataOptions(hopLimit)
		return
	}
	if hopLimit != nil {
		instance.MetadataOptions.HttpPutResponseHopLimit = hopLimit
	}
}
