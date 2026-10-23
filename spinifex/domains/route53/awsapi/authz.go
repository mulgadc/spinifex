package awsapi

import (
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/route53"
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
	case *route53.GetChangeInput:
		return []string{arnPrefix + "change/" + strings.TrimPrefix(aws.StringValue(in.Id), "/change/")}
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

func zoneARN(id *string) []string {
	return []string{arnPrefix + "hostedzone/" + strings.TrimPrefix(aws.StringValue(id), "/hostedzone/")}
}

func tagARNs(resourceType, id *string) []string {
	return []string{arnPrefix + aws.StringValue(resourceType) + "/" + aws.StringValue(id)}
}
