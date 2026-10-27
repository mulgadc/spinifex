package awsapi

import (
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/route53"

	route53domain "github.com/mulgadc/spinifex/spinifex/domains/route53"
	"github.com/mulgadc/spinifex/spinifex/domains/route53/ext"
)

// Route 53 ARNs carry no region or account, so the policy gate cannot scope
// them to the caller; the daemon looks every ID up within the caller's account.
const arnPrefix = "arn:aws:route53:::"

// ResourceARNs returns the resources an action acts on, for the policy gate.
// Actions that name no zone or change are authorized against "*".
func ResourceARNs(input any) []string {
	switch in := input.(type) {
	case *route53.GetHostedZoneInput:
		return zoneARN(in.Id)
	case *route53.UpdateHostedZoneCommentInput:
		return zoneARN(in.Id)
	case *route53.DeleteHostedZoneInput:
		return zoneARN(in.Id)
	case *route53.ChangeResourceRecordSetsInput:
		return zoneARN(in.HostedZoneId)
	case *route53.ListResourceRecordSetsInput:
		return zoneARN(in.HostedZoneId)
	case *ext.GetHostedZoneActivationInput:
		return zoneARN(in.Id)
	case *ext.VerifyHostedZoneDelegationInput:
		return zoneARN(in.Id)
	case *route53.GetChangeInput:
		return []string{arnPrefix + "change/" + route53domain.BareChangeID(aws.StringValue(in.Id))}
	case *route53.ChangeTagsForResourceInput:
		return tagARNs(in.ResourceType, in.ResourceId)
	case *route53.ListTagsForResourceInput:
		return tagARNs(in.ResourceType, in.ResourceId)
	case *route53.ListTagsForResourcesInput:
		var out []string
		for _, id := range in.ResourceIds {
			out = append(out, tagARNs(in.ResourceType, id)...)
		}
		if len(out) == 0 {
			return []string{"*"}
		}
		return out
	default:
		return []string{"*"}
	}
}

// zoneARN names the zone by the ID the daemon looks up, so a padded or
// prefixed ID cannot be authorized as one zone and act on another.
func zoneARN(id *string) []string {
	return []string{arnPrefix + "hostedzone/" + route53domain.BareZoneID(aws.StringValue(id))}
}

func tagARNs(resourceType, id *string) []string {
	if aws.StringValue(resourceType) == route53.TagResourceTypeHostedzone {
		return zoneARN(id)
	}
	return []string{arnPrefix + aws.StringValue(resourceType) + "/" + strings.TrimSpace(aws.StringValue(id))}
}
