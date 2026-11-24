package tgw

import (
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

const (
	enable  = "enable"
	disable = "disable"
)

func transitGatewayNotFound(id string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidTransitGatewayIDNotFound, "Transit Gateway %s was deleted or does not exist.", id)
}

func transitGatewayMalformed(id string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidTransitGatewayIDMalformed, "Invalid Transit Gateway id %s.", id)
}

func attachmentNotFound(id string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidTransitGatewayAttachmentIDNotFound, "Transit Gateway Attachment %s was deleted or does not exist.", id)
}

// AWS words a Delete's not-found answer differently from Describe's.
func vpcAttachmentNotFound(id string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidTransitGatewayAttachmentIDNotFound, "Transit Gateway VPC Attachment %s was deleted or does not exist.", id)
}

func attachmentMalformed() error {
	return awserrors.Errorf(awserrors.ErrorInvalidTransitGatewayAttachmentIDMalformed, "Invalid Transit Gateway Attachment id.")
}

func incorrectState(id string) error {
	return awserrors.Errorf(awserrors.ErrorIncorrectState, "%s is in invalid state", id)
}

func filterInvalid(name string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "Value (%s) for parameter Filter is invalid.", name)
}

func unsupportedValue(param, value string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
		"Value (%s) for parameter %s is not supported: Spinifex Transit Gateways do not offer it.", value, param)
}

func invalidValue(param, value string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "Value (%s) for parameter %s is invalid.", value, param)
}

func notOffered() error {
	return awserrors.Errorf(awserrors.ErrorUnsupportedOperation,
		"Transit Gateways are not available until every node in the formation runs a release that supports them.")
}

// wellFormed reports an ID of prefix followed by 8 or 17 lowercase hex digits.
func wellFormed(id, prefix string) bool {
	suffix, ok := strings.CutPrefix(id, prefix)
	if !ok || (len(suffix) != 8 && len(suffix) != 17) {
		return false
	}
	for _, c := range suffix {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validTransitGatewayID(id string) error {
	if !wellFormed(id, "tgw-") {
		return transitGatewayMalformed(id)
	}
	return nil
}

func validAttachmentID(id string) error {
	if !wellFormed(id, "tgw-attach-") {
		return attachmentMalformed()
	}
	return nil
}

// tagList renders tags sorted by key; an empty map renders an empty list.
func tagList(tags map[string]string) []*ec2.Tag {
	out := make([]*ec2.Tag, 0, len(tags))
	for k, v := range tags {
		out = append(out, &ec2.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	slices.SortFunc(out, func(a, b *ec2.Tag) int { return strings.Compare(*a.Key, *b.Key) })
	return out
}

// transitGatewayToEC2 renders a record; a delete response omits its tags.
func (s *ServiceImpl) transitGatewayToEC2(rec *TransitGatewayRecord, withTags bool) *ec2.TransitGateway {
	out := &ec2.TransitGateway{
		CreationTime: aws.Time(rec.CreatedAt),
		Description:  aws.String(rec.Description),
		Options: &ec2.TransitGatewayOptions{
			AmazonSideAsn:                   aws.Int64(rec.Options.AmazonSideAsn),
			AssociationDefaultRouteTableId:  aws.String(defaultRouteTableID(rec.ID)),
			AutoAcceptSharedAttachments:     aws.String(rec.Options.AutoAcceptSharedAttachments),
			DefaultRouteTableAssociation:    aws.String(enable),
			DefaultRouteTablePropagation:    aws.String(enable),
			DnsSupport:                      aws.String(rec.Options.DnsSupport),
			MulticastSupport:                aws.String(disable),
			PropagationDefaultRouteTableId:  aws.String(defaultRouteTableID(rec.ID)),
			SecurityGroupReferencingSupport: aws.String(rec.Options.SecurityGroupReferencingSupport),
			VpnEcmpSupport:                  aws.String(rec.Options.VpnEcmpSupport),
		},
		OwnerId:           aws.String(rec.AccountID),
		State:             aws.String(rec.State),
		TransitGatewayArn: aws.String(arn.FormatEC2(arn.EC2TransitGateway, s.region, rec.AccountID, rec.ID)),
		TransitGatewayId:  aws.String(rec.ID),
	}
	if withTags {
		out.Tags = tagList(rec.Tags)
	}
	return out
}

// attachmentToEC2 renders a record as Describe and Create return it. A
// deleted or failed attachment lists no subnets, as on AWS.
func attachmentToEC2(rec *AttachmentRecord) *ec2.TransitGatewayVpcAttachment {
	subnets := []*string{}
	if rec.isLive() {
		subnets = aws.StringSlice(rec.SubnetIDs)
	}
	return &ec2.TransitGatewayVpcAttachment{
		CreationTime: aws.Time(rec.CreatedAt),
		Options: &ec2.TransitGatewayVpcAttachmentOptions{
			ApplianceModeSupport:            aws.String(disable),
			DnsSupport:                      aws.String(rec.Options.DnsSupport),
			Ipv6Support:                     aws.String(disable),
			SecurityGroupReferencingSupport: aws.String(rec.Options.SecurityGroupReferencingSupport),
		},
		State:                      aws.String(rec.State),
		SubnetIds:                  subnets,
		Tags:                       tagList(rec.Tags),
		TransitGatewayAttachmentId: aws.String(rec.ID),
		TransitGatewayId:           aws.String(rec.TransitGatewayID),
		VpcId:                      aws.String(rec.VpcID),
		VpcOwnerId:                 aws.String(rec.AccountID),
	}
}

// attachmentDeleteShape is what DeleteTransitGatewayVpcAttachment returns:
// no Options, SubnetIds or Tags.
func attachmentDeleteShape(rec *AttachmentRecord) *ec2.TransitGatewayVpcAttachment {
	return &ec2.TransitGatewayVpcAttachment{
		CreationTime:               aws.Time(rec.CreatedAt),
		State:                      aws.String(rec.State),
		TransitGatewayAttachmentId: aws.String(rec.ID),
		TransitGatewayId:           aws.String(rec.TransitGatewayID),
		VpcId:                      aws.String(rec.VpcID),
		VpcOwnerId:                 aws.String(rec.AccountID),
	}
}
