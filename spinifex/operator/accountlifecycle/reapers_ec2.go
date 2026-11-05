package accountlifecycle

import (
	"context"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	ec2eipapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/eip"
	ec2igwapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/igw"
	ec2imageapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/image"
	ec2routetableapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/routetable"
	ec2snapshotapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/snapshot"
	ec2vpcapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// EC2Reapers returns every EC2-backed reaper in teardown order.
//
// They drive the same NATS-backed operations the gateway serves a customer
// request with, which is what keeps OVN teardown, IPAM release and quota
// accounting correct without teardown having to reimplement any of it.
func EC2Reapers(nc *nats.Conn, expectedNodes int, nodeIDs []string) []Reaper {
	return []Reaper{
		// Snapshots before volumes: a volume with a live snapshot refuses to
		// delete, because snapshot-backed clones read from its chunk prefix.
		&snapshotReaper{nc: nc},
		&imageReaper{nc: nc},

		// Reverse of creation order. The VPC cannot go until everything
		// inside it has, and the gateway enforces that.
		&addressReaper{nc: nc},

		// After addresses so a released EIP is already off the interface, and
		// before subnets because an interface still in a subnet refuses to let
		// that subnet go — which wedged the whole account, not just the ENI.
		&networkInterfaceReaper{nc: nc},

		&igwReaper{nc: nc},
		&routeTableReaper{nc: nc},
		&subnetReaper{nc: nc},
		&securityGroupReaper{nc: nc},
		&vpcReaper{nc: nc},
	}
}

type snapshotReaper struct{ nc *nats.Conn }

func (r *snapshotReaper) Kind() string { return "snapshot" }
func (r *snapshotReaper) Stage() Stage { return StageStorage }

func (r *snapshotReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := ec2snapshotapi.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
		OwnerIds: []*string{aws.String(accountID)},
	}, r.nc, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, snapshot := range out.Snapshots {
		if snapshot == nil || snapshot.SnapshotId == nil {
			continue
		}
		found = append(found, Resource{Kind: r.Kind(), ID: *snapshot.SnapshotId})
	}
	return found, nil
}

func (r *snapshotReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := ec2snapshotapi.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{
		SnapshotId: aws.String(resource.ID),
	}, r.nc, accountID)
	return ignoreAlreadyGone(err)
}

type imageReaper struct{ nc *nats.Conn }

func (r *imageReaper) Kind() string { return "image" }
func (r *imageReaper) Stage() Stage { return StageStorage }

func (r *imageReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	// Owner-scoped: a public or shared image the account merely sees is not
	// its property and must survive its deletion.
	out, err := ec2imageapi.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []*string{aws.String(accountID)},
	}, r.nc, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, image := range out.Images {
		if image == nil || image.ImageId == nil {
			continue
		}
		found = append(found, Resource{Kind: r.Kind(), ID: *image.ImageId})
	}
	return found, nil
}

func (r *imageReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := ec2imageapi.DeregisterImage(ctx, &ec2.DeregisterImageInput{
		ImageId: aws.String(resource.ID),
	}, r.nc, accountID)
	return ignoreAlreadyGone(err)
}

type addressReaper struct{ nc *nats.Conn }

func (r *addressReaper) Kind() string { return "address" }
func (r *addressReaper) Stage() Stage { return StageNetwork }

func (r *addressReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := ec2eipapi.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{}, r.nc, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, address := range out.Addresses {
		if address == nil || address.AllocationId == nil {
			continue
		}
		resource := Resource{Kind: r.Kind(), ID: *address.AllocationId}
		if address.PublicIp != nil {
			resource.Detail = *address.PublicIp
		}
		found = append(found, resource)
	}
	return found, nil
}

// Delete disassociates before releasing. An associated address cannot be
// released, and the association usually outlives the instance by a moment.
func (r *addressReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	associationID, err := r.associationID(ctx, accountID, resource.ID)
	if err != nil && !isAlreadyGone(err) {
		return err
	}
	if associationID != "" {
		if _, err := ec2eipapi.DisassociateAddress(ctx, &ec2.DisassociateAddressInput{
			AssociationId: aws.String(associationID),
		}, r.nc, accountID); err != nil && !isAlreadyGone(err) && !isNotAssociated(err) {
			return err
		}
	}
	_, err = ec2eipapi.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{
		AllocationId: aws.String(resource.ID),
	}, r.nc, accountID)
	return ignoreAlreadyGone(err)
}

// associationID reads the address's current association rather than carrying it
// from the listing, because a delete may also be driven from an id an operator
// supplied. An empty answer means unattached, which is not an error.
func (r *addressReaper) associationID(ctx context.Context, accountID, allocationID string) (string, error) {
	out, err := ec2eipapi.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
		AllocationIds: []*string{aws.String(allocationID)},
	}, r.nc, accountID)
	if err != nil {
		return "", err
	}
	for _, address := range out.Addresses {
		if address == nil || address.AllocationId == nil || *address.AllocationId != allocationID {
			continue
		}
		return aws.StringValue(address.AssociationId), nil
	}
	return "", nil
}

// networkInterfaceReaper removes the interfaces nothing else will. A running
// instance takes its own interfaces with it, so anything still listed by the
// time the network stage runs was either created by hand or left behind by a
// launch that failed after the interface was made.
//
// Either way it holds a subnet, and a subnet that will not delete stops the VPC
// and leaves the account in TERMINATING for good.
type networkInterfaceReaper struct{ nc *nats.Conn }

func (r *networkInterfaceReaper) Kind() string { return "network-interface" }
func (r *networkInterfaceReaper) Stage() Stage { return StageNetwork }

func (r *networkInterfaceReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := ec2vpcapi.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{}, r.nc, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, eni := range out.NetworkInterfaces {
		if eni == nil || eni.NetworkInterfaceId == nil {
			continue
		}
		resource := Resource{Kind: r.Kind(), ID: *eni.NetworkInterfaceId}
		if eni.SubnetId != nil {
			resource.Detail = *eni.SubnetId
		}
		found = append(found, resource)
	}
	return found, nil
}

// Delete detaches first when the interface is attached, because an in-use
// interface cannot be deleted. The attachment is read fresh rather than carried
// from the listing, so a delete driven from an id an operator supplied behaves
// the same as one driven from a listing.
func (r *networkInterfaceReaper) Delete(ctx context.Context, accountID string, resource Resource, force bool) error {
	attachmentID, err := r.attachmentID(ctx, accountID, resource.ID)
	if err != nil && !isAlreadyGone(err) {
		return err
	}
	if attachmentID != "" {
		if _, err := ec2vpcapi.DetachNetworkInterface(ctx, &ec2.DetachNetworkInterfaceInput{
			AttachmentId: aws.String(attachmentID),
			Force:        aws.Bool(force),
		}, r.nc, accountID); err != nil && !isAlreadyGone(err) {
			return err
		}
	}
	_, err = ec2vpcapi.DeleteNetworkInterface(ctx, &ec2.DeleteNetworkInterfaceInput{
		NetworkInterfaceId: aws.String(resource.ID),
	}, r.nc, accountID)
	return ignoreAlreadyGone(err)
}

// attachmentID reports the interface's current attachment, or empty when it is
// unattached. Unattached is the ordinary case here and is not an error.
func (r *networkInterfaceReaper) attachmentID(ctx context.Context, accountID, interfaceID string) (string, error) {
	out, err := ec2vpcapi.DescribeNetworkInterfaces(ctx, &ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{aws.String(interfaceID)},
	}, r.nc, accountID)
	if err != nil {
		return "", err
	}
	for _, eni := range out.NetworkInterfaces {
		if eni == nil || eni.NetworkInterfaceId == nil || *eni.NetworkInterfaceId != interfaceID {
			continue
		}
		if eni.Attachment == nil {
			return "", nil
		}
		return aws.StringValue(eni.Attachment.AttachmentId), nil
	}
	return "", nil
}

type igwReaper struct{ nc *nats.Conn }

func (r *igwReaper) Kind() string { return "internet-gateway" }
func (r *igwReaper) Stage() Stage { return StageNetwork }

func (r *igwReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := ec2igwapi.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{}, r.nc, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, gateway := range out.InternetGateways {
		if gateway == nil || gateway.InternetGatewayId == nil {
			continue
		}
		resource := Resource{Kind: r.Kind(), ID: *gateway.InternetGatewayId}
		for _, attachment := range gateway.Attachments {
			if attachment != nil && attachment.VpcId != nil {
				resource.Detail = *attachment.VpcId
				break
			}
		}
		found = append(found, resource)
	}
	return found, nil
}

// Delete detaches first: an attached gateway cannot be deleted, and its VPC
// cannot be deleted while it is attached.
func (r *igwReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	if resource.Detail != "" {
		if _, err := ec2igwapi.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(resource.ID),
			VpcId:             aws.String(resource.Detail),
		}, r.nc, accountID); err != nil && !isAlreadyGone(err) {
			return err
		}
	}
	// A gateway whose attach is not yet confirmed reports no attachment, so
	// Detail is empty and the delete is refused. DeleteVpc rejects while the
	// gateway is attached, so the VPC survives and the next sweep, once the
	// attachment is confirmed, takes the branch above.
	_, err := ec2igwapi.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{
		InternetGatewayId: aws.String(resource.ID),
	}, r.nc, accountID)
	return ignoreAlreadyGone(err)
}

type routeTableReaper struct{ nc *nats.Conn }

func (r *routeTableReaper) Kind() string { return "route-table" }
func (r *routeTableReaper) Stage() Stage { return StageNetwork }

// List skips main route tables. A VPC's main table is deleted with the VPC and
// cannot be deleted on its own, so listing it would guarantee a stuck report.
func (r *routeTableReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := ec2routetableapi.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{}, r.nc, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, table := range out.RouteTables {
		if table == nil || table.RouteTableId == nil {
			continue
		}
		var associations []string
		main := false
		for _, association := range table.Associations {
			if association == nil {
				continue
			}
			if association.Main != nil && *association.Main {
				main = true
			}
			if association.RouteTableAssociationId != nil {
				associations = append(associations, *association.RouteTableAssociationId)
			}
		}
		if main {
			continue
		}
		found = append(found, Resource{
			Kind: r.Kind(), ID: *table.RouteTableId, Detail: strings.Join(associations, ","),
		})
	}
	return found, nil
}

func (r *routeTableReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	for association := range strings.SplitSeq(resource.Detail, ",") {
		if association == "" {
			continue
		}
		if _, err := ec2routetableapi.DisassociateRouteTable(ctx, &ec2.DisassociateRouteTableInput{
			AssociationId: aws.String(association),
		}, r.nc, accountID); err != nil && !isAlreadyGone(err) {
			return err
		}
	}
	_, err := ec2routetableapi.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{
		RouteTableId: aws.String(resource.ID),
	}, r.nc, accountID)
	return ignoreAlreadyGone(err)
}

type subnetReaper struct{ nc *nats.Conn }

func (r *subnetReaper) Kind() string { return "subnet" }
func (r *subnetReaper) Stage() Stage { return StageNetwork }

func (r *subnetReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := ec2vpcapi.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{}, r.nc, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, subnet := range out.Subnets {
		if subnet == nil || subnet.SubnetId == nil {
			continue
		}
		resource := Resource{Kind: r.Kind(), ID: *subnet.SubnetId}
		if subnet.VpcId != nil {
			resource.Detail = *subnet.VpcId
		}
		found = append(found, resource)
	}
	return found, nil
}

func (r *subnetReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := ec2vpcapi.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{
		SubnetId: aws.String(resource.ID),
	}, r.nc, accountID)
	return ignoreAlreadyGone(err)
}

type securityGroupReaper struct{ nc *nats.Conn }

func (r *securityGroupReaper) Kind() string { return "security-group" }
func (r *securityGroupReaper) Stage() Stage { return StageNetwork }

// List skips the default group of each VPC: it is created and destroyed with
// the VPC and cannot be deleted on its own.
func (r *securityGroupReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := ec2vpcapi.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{}, r.nc, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, group := range out.SecurityGroups {
		if group == nil || group.GroupId == nil {
			continue
		}
		if group.GroupName != nil && *group.GroupName == "default" {
			continue
		}
		found = append(found, Resource{Kind: r.Kind(), ID: *group.GroupId})
	}
	return found, nil
}

func (r *securityGroupReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := ec2vpcapi.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(resource.ID),
	}, r.nc, accountID)
	return ignoreAlreadyGone(err)
}

type vpcReaper struct{ nc *nats.Conn }

func (r *vpcReaper) Kind() string { return "vpc" }
func (r *vpcReaper) Stage() Stage { return StageNetwork }

func (r *vpcReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := ec2vpcapi.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{}, r.nc, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, vpc := range out.Vpcs {
		if vpc == nil || vpc.VpcId == nil {
			continue
		}
		resource := Resource{Kind: r.Kind(), ID: *vpc.VpcId}
		if vpc.IsDefault != nil && *vpc.IsDefault {
			resource.Detail = "default"
		}
		found = append(found, resource)
	}
	return found, nil
}

func (r *vpcReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := ec2vpcapi.DeleteVpc(ctx, &ec2.DeleteVpcInput{
		VpcId: aws.String(resource.ID),
	}, r.nc, accountID)
	return ignoreAlreadyGone(err)
}

// notFoundMarkers are the AWS error-code fragments that mean the resource is
// already gone. Teardown re-runs after a crash, so treating these as failures
// would make a second pass unable to finish what the first one started.
var notFoundMarkers = []string{
	".NotFound", "NoSuchEntity", "InvalidAllocationID.NotFound",
	awserrors.ErrorNatGatewayNotFound,
	awserrors.ErrorECSClusterNotFound, awserrors.ErrorECSServiceNotFound,
	awserrors.ErrorEKSResourceNotFound,
	awserrors.ErrorDBInstanceNotFound,
	awserrors.ErrorDBSubnetGroupNotFound, awserrors.ErrorDBParameterGroupNotFound,
	awserrors.ErrorELBv2LoadBalancerNotFound, awserrors.ErrorELBv2TargetGroupNotFound,
	awserrors.ErrorResourceNotFound,
	s3.ErrCodeNoSuchBucket, s3.ErrCodeNoSuchKey, s3.ErrCodeNoSuchUpload,
}

func isAlreadyGone(err error) bool {
	if err == nil {
		return true
	}
	message := err.Error()
	return slices.ContainsFunc(notFoundMarkers, func(marker string) bool {
		return strings.Contains(message, marker)
	})
}

// isNotAssociated reports the benign case of disassociating something that was
// never associated, which is the normal state of an unattached address.
func isNotAssociated(err error) bool {
	return err != nil && strings.Contains(err.Error(), "InvalidAssociationID")
}

func ignoreAlreadyGone(err error) error {
	if isAlreadyGone(err) {
		return nil
	}
	return err
}
