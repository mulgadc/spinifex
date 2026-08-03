package handlers_ec2_instance

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// defaultMetadataHopLimit matches the AWS default applied when none is requested.
const defaultMetadataHopLimit = int64(1)

// buildMetadataOptions returns the metadata-options block stamped at launch.
// hopLimit applies only in the AWS-valid 1-64 range, otherwise falling back to
// the default. An empty httpTokens stamps "required", so an instance that does
// not ask for IMDSv1 never silently gets it; applyPlatformTokenDefault later
// relaxes that for the one platform that cannot speak IMDSv2.
func buildMetadataOptions(hopLimit *int64, httpTokens string) *ec2.InstanceMetadataOptionsResponse {
	limit := defaultMetadataHopLimit
	if hopLimit != nil && *hopLimit >= 1 && *hopLimit <= 64 {
		limit = *hopLimit
	}
	tokens := ec2.HttpTokensStateRequired
	if httpTokens == ec2.HttpTokensStateOptional {
		tokens = ec2.HttpTokensStateOptional
	}
	return &ec2.InstanceMetadataOptionsResponse{
		State:                   aws.String(ec2.InstanceMetadataOptionsStateApplied),
		HttpTokens:              aws.String(tokens),
		HttpEndpoint:            aws.String(ec2.InstanceMetadataEndpointStateEnabled),
		HttpProtocolIpv6:        aws.String(ec2.InstanceMetadataProtocolStateDisabled),
		InstanceMetadataTags:    aws.String(ec2.InstanceMetadataTagsStateDisabled),
		HttpPutResponseHopLimit: aws.Int64(limit),
	}
}

// defaultHTTPTokensForPlatform returns the launch default an image's platform
// implies. Windows guests bootstrap with cloudbase-init, which has no IMDSv2
// token support in any release, so a Windows image has to permit IMDSv1 or its
// agent never reads metadata at all. Every other platform keeps "required".
func defaultHTTPTokensForPlatform(platform *string) string {
	if aws.StringValue(platform) == utils.PlatformWindows {
		return ec2.HttpTokensStateOptional
	}
	return ec2.HttpTokensStateRequired
}

// applyPlatformTokenDefault relaxes the stamped IMDSv2 posture to the platform
// default once the launch has resolved its image. A launch that named
// httpTokens itself is left untouched, so an explicit value always wins.
func applyPlatformTokenDefault(instance *ec2.Instance, requestedTokens string, platform *string) {
	if requestedTokens != "" || instance.MetadataOptions == nil {
		return
	}
	instance.MetadataOptions.HttpTokens = aws.String(defaultHTTPTokensForPlatform(platform))
}

// validateMetadataOptions rejects any request that enables an unmodelled
// feature. Empty values are "leave unchanged" no-ops; the hop limit (AWS-valid
// 1-64) and httpTokens are mutable. Shared by RunInstances and
// ModifyInstanceMetadataOptions.
func validateMetadataOptions(httpTokens, httpEndpoint, ipv6, tags string, hopLimit *int64) error {
	// IMDSv1 is opt-in per instance, matching AWS, so that IMDSv1-only guest
	// agents such as cloudbase-init can bootstrap. The launch default stays
	// "required"; see buildMetadataOptions.
	if httpTokens != "" && httpTokens != ec2.HttpTokensStateRequired && httpTokens != ec2.HttpTokensStateOptional {
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

// applyMetadataOptions stamps the block onto a legacy nil-block instance or
// moves the mutable fields. Callers must guard a nil instance.
func applyMetadataOptions(instance *ec2.Instance, hopLimit *int64, httpTokens string) {
	if instance.MetadataOptions == nil {
		instance.MetadataOptions = buildMetadataOptions(hopLimit, httpTokens)
		return
	}
	if hopLimit != nil {
		instance.MetadataOptions.HttpPutResponseHopLimit = hopLimit
	}
	if httpTokens != "" {
		instance.MetadataOptions.HttpTokens = aws.String(httpTokens)
	}
}
