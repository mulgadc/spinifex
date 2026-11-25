package vpcpeering

import (
	"net/netip"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awstags "github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
)

const (
	failedMessage       = "Failed due to incorrect VPC-ID, Account ID, overlapping CIDR range, or VPC Encryption Control posture"
	provisioningMessage = "Provisioning"
	activeMessage       = "Active"
	// expiredMessage was not captured from AWS: a request takes 7 days to expire.
	expiredMessage = "Expired"
)

func initiatingMessage(accepter string) string { return "Initiating Request to " + accepter }

func pendingMessage(accepter string) string { return "Pending Acceptance by " + accepter }

func rejectedMessage(account string) string { return "Rejected by " + account }

func deletedMessage(account string) string { return "Deleted by " + account }

func notFoundError(id string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidVpcPeeringConnectionIDNotFound, "The vpcPeeringConnection ID '%s' does not exist", id)
}

func modifyNotFoundError(id string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidVpcPeeringConnectionIDNotFound, "The peering ID '%s' does not exist", id)
}

func transitionError(id, from, to string) error {
	return awserrors.Errorf(awserrors.ErrorInvalidStateTransition,
		"Invalid state transition for %s, attempted to transition from %s to %s", id, from, to)
}

func alreadyExistsError(id string) error {
	return awserrors.Errorf(awserrors.ErrorVpcPeeringConnectionAlreadyExists,
		"The vpc peering connection identified by %s already exists.", id)
}

func notActiveError(id string) error {
	return awserrors.Errorf(awserrors.ErrorOperationNotPermitted,
		"Peering %s is not active. Peering options can be added only to active peerings.", id)
}

// shape selects which members AWS returns for a connection in a given state.
type shape int

const (
	// shapeRequest: ExpirationTime, the requester in full, the accepter's identity only.
	shapeRequest shape = iota
	// shapeFailedCreate: as shapeRequest, without the requester's PeeringOptions.
	shapeFailedCreate
	// shapeLinked: both sides in full and no ExpirationTime.
	shapeLinked
	// shapeTerminal: both sides' identity only.
	shapeTerminal
)

// shapeFor is the Describe shape of a view's state.
func shapeFor(status string) shape {
	switch status {
	case StatePendingAcceptance, StateInitiatingRequest:
		return shapeRequest
	case StateProvisioning, StateActive:
		return shapeLinked
	}
	return shapeTerminal
}

func vpcInfo(owner, vpcID, cidr, region string, opts *PeeringOptions, full bool) *ec2.VpcPeeringConnectionVpcInfo {
	info := &ec2.VpcPeeringConnectionVpcInfo{
		OwnerId: aws.String(owner),
		VpcId:   aws.String(vpcID),
		Region:  aws.String(region),
	}
	if !full {
		return info
	}
	info.CidrBlock = aws.String(cidr)
	info.CidrBlockSet = []*ec2.CidrBlock{{CidrBlock: aws.String(cidr)}}
	if opts != nil {
		info.PeeringOptions = &ec2.VpcPeeringConnectionOptionsDescription{
			AllowDnsResolutionFromRemoteVpc:            aws.Bool(opts.AllowDnsResolutionFromRemoteVpc),
			AllowEgressFromLocalClassicLinkToRemoteVpc: aws.Bool(opts.AllowEgressFromLocalClassicLinkToRemoteVpc),
			AllowEgressFromLocalVpcToRemoteClassicLink: aws.Bool(opts.AllowEgressFromLocalVpcToRemoteClassicLink),
		}
	}
	return info
}

// toEC2 renders a connection for viewer with the given status and shape.
func toEC2(rec *PeeringRecord, viewer, status, message string, sh shape) *ec2.VpcPeeringConnection {
	out := &ec2.VpcPeeringConnection{
		VpcPeeringConnectionId: aws.String(rec.ID),
		Status:                 &ec2.VpcPeeringConnectionStateReason{Code: aws.String(status), Message: aws.String(message)},
		Tags:                   awstags.MapToEC2(rec.Tags[viewer]),
	}
	if out.Tags == nil {
		out.Tags = []*ec2.Tag{}
	}
	switch sh {
	case shapeRequest, shapeFailedCreate:
		if !rec.ExpirationTime.IsZero() {
			out.ExpirationTime = aws.Time(rec.ExpirationTime)
		}
		var opts *PeeringOptions
		if sh == shapeRequest {
			opts = &rec.RequesterOptions
		}
		out.RequesterVpcInfo = vpcInfo(rec.RequesterAccountID, rec.RequesterVpcID, rec.RequesterCIDR, rec.Region, opts, true)
		out.AccepterVpcInfo = vpcInfo(rec.AccepterAccountID, rec.AccepterVpcID, rec.AccepterCIDR, rec.Region, nil, false)
	case shapeLinked:
		out.RequesterVpcInfo = vpcInfo(rec.RequesterAccountID, rec.RequesterVpcID, rec.RequesterCIDR, rec.Region, &rec.RequesterOptions, true)
		out.AccepterVpcInfo = vpcInfo(rec.AccepterAccountID, rec.AccepterVpcID, rec.AccepterCIDR, rec.Region, &rec.AccepterOptions, true)
	default:
		out.RequesterVpcInfo = vpcInfo(rec.RequesterAccountID, rec.RequesterVpcID, "", rec.Region, nil, false)
		out.AccepterVpcInfo = vpcInfo(rec.AccepterAccountID, rec.AccepterVpcID, "", rec.Region, nil, false)
	}
	return out
}

// viewToEC2 renders a view in its read-time state.
func viewToEC2(v *view, viewer string) *ec2.VpcPeeringConnection {
	return toEC2(&v.rec, viewer, v.status, v.message, shapeFor(v.status))
}

// existingToEC2 renders the connection a repeated Create returns. AWS shows a
// linked one's accepter with its CidrBlock alone.
func existingToEC2(v *view, viewer string) *ec2.VpcPeeringConnection {
	out := viewToEC2(v, viewer)
	if shapeFor(v.status) == shapeLinked {
		out.AccepterVpcInfo.CidrBlockSet = nil
		out.AccepterVpcInfo.PeeringOptions = nil
	}
	return out
}

// optionsDescription renders one side's options for the Modify output.
func optionsDescription(o PeeringOptions) *ec2.PeeringConnectionOptions {
	return &ec2.PeeringConnectionOptions{
		AllowDnsResolutionFromRemoteVpc:            aws.Bool(o.AllowDnsResolutionFromRemoteVpc),
		AllowEgressFromLocalClassicLinkToRemoteVpc: aws.Bool(o.AllowEgressFromLocalClassicLinkToRemoteVpc),
		AllowEgressFromLocalVpcToRemoteClassicLink: aws.Bool(o.AllowEgressFromLocalVpcToRemoteClassicLink),
	}
}

// classicIPSpace is EC2-Classic's 10.0.0.0/8, less the two /16s AWS exempts.
var (
	classicIPSpace    = netip.MustParsePrefix("10.0.0.0/8")
	classicExemptions = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("10.1.0.0/16")}
)

// overlapsClassicIPSpace reports a VPC range AWS refuses ClassicLink egress for.
func overlapsClassicIPSpace(cidr string) bool {
	p, err := netip.ParsePrefix(cidr)
	if err != nil || !p.Overlaps(classicIPSpace) {
		return false
	}
	for _, ex := range classicExemptions {
		if ex.Bits() <= p.Bits() && ex.Contains(p.Addr()) {
			return false
		}
	}
	return true
}

// cidrsOverlap reports whether two VPC ranges share an address; an
// unparsable range counts as overlapping, so it never links.
func cidrsOverlap(a, b string) bool {
	pa, errA := netip.ParsePrefix(a)
	pb, errB := netip.ParsePrefix(b)
	if errA != nil || errB != nil {
		return true
	}
	return pa.Overlaps(pb)
}

// awsTime is the form EC2 filters compare times in.
func awsTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
