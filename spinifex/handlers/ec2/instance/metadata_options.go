package handlers_ec2_instance

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
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
