package accountteardown

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ec2_eip "github.com/mulgadc/spinifex/spinifex/gateway/ec2/eip"
	gateway_ec2_igw "github.com/mulgadc/spinifex/spinifex/gateway/ec2/igw"
	gateway_ec2_image "github.com/mulgadc/spinifex/spinifex/gateway/ec2/image"
	gateway_ec2_instance "github.com/mulgadc/spinifex/spinifex/gateway/ec2/instance"
	gateway_ec2_key "github.com/mulgadc/spinifex/spinifex/gateway/ec2/key"
	gateway_ec2_launchtemplate "github.com/mulgadc/spinifex/spinifex/gateway/ec2/launchtemplate"
	gateway_ec2_placementgroup "github.com/mulgadc/spinifex/spinifex/gateway/ec2/placementgroup"
	gateway_ec2_routetable "github.com/mulgadc/spinifex/spinifex/gateway/ec2/routetable"
	gateway_ec2_snapshot "github.com/mulgadc/spinifex/spinifex/gateway/ec2/snapshot"
	gateway_ec2_volume "github.com/mulgadc/spinifex/spinifex/gateway/ec2/volume"
	gateway_ec2_vpc "github.com/mulgadc/spinifex/spinifex/gateway/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// EC2Reapers returns every EC2-backed reaper in teardown order.
//
// They drive the same NATS-backed operations the gateway serves a customer
// request with, which is what keeps OVN teardown, IPAM release and quota
// accounting correct without teardown having to reimplement any of it.
func EC2Reapers(nc *nats.Conn, expectedNodes int) []Reaper {
	return []Reaper{
		&instanceReaper{nc: nc, expectedNodes: expectedNodes},

		// Snapshots before volumes: a volume with a live snapshot refuses to
		// delete, because snapshot-backed clones read from its chunk prefix.
		&snapshotReaper{nc: nc},
		&imageReaper{nc: nc},
		&volumeReaper{nc: nc, expectedNodes: expectedNodes},

		// Reverse of creation order. The VPC cannot go until everything
		// inside it has, and the gateway enforces that.
		&addressReaper{nc: nc},
		&igwReaper{nc: nc},
		&routeTableReaper{nc: nc},
		&subnetReaper{nc: nc},
		&securityGroupReaper{nc: nc},
		&vpcReaper{nc: nc},

		&keyPairReaper{nc: nc},
		&placementGroupReaper{nc: nc},
		&launchTemplateReaper{nc: nc},
	}
}

// terminalInstanceStates are the states an instance no longer holds anything
// in. Anything else is still occupying a volume, an address and a host.
var terminalInstanceStates = map[string]bool{"terminated": true, "shutting-down": true}

type instanceReaper struct {
	nc            *nats.Conn
	expectedNodes int
}

func (r *instanceReaper) Kind() string { return "instance" }
func (r *instanceReaper) Stage() Stage { return StageCompute }

func (r *instanceReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	// Checked: a partial answer here would read as "no instances left" and let
	// storage deletion start while a guest still holds a volume.
	out, err := gateway_ec2_instance.DescribeInstancesChecked(ctx, &ec2.DescribeInstancesInput{}, r.nc, r.expectedNodes, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, reservation := range out.Reservations {
		for _, instance := range reservation.Instances {
			if instance == nil || instance.InstanceId == nil {
				continue
			}
			state := ""
			if instance.State != nil && instance.State.Name != nil {
				state = *instance.State.Name
			}
			if terminalInstanceStates[state] {
				continue
			}
			found = append(found, Resource{Kind: r.Kind(), ID: *instance.InstanceId, Detail: state})
		}
	}
	return found, nil
}

func (r *instanceReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := gateway_ec2_instance.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []*string{aws.String(resource.ID)},
	}, r.nc, accountID)
	return ignoreAlreadyGone(err)
}

type volumeReaper struct {
	nc            *nats.Conn
	expectedNodes int
}

func (r *volumeReaper) Kind() string { return "volume" }
func (r *volumeReaper) Stage() Stage { return StageStorage }

func (r *volumeReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := gateway_ec2_volume.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{}, r.nc, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, volume := range out.Volumes {
		if volume == nil || volume.VolumeId == nil {
			continue
		}
		resource := Resource{Kind: r.Kind(), ID: *volume.VolumeId}
		// The attachment is the reason a volume will not go, so carry it: it
		// is the difference between "stuck" and "stuck on i-abc".
		for _, attachment := range volume.Attachments {
			if attachment != nil && attachment.InstanceId != nil {
				resource.Detail = "attached to " + *attachment.InstanceId
				break
			}
		}
		found = append(found, resource)
	}
	return found, nil
}

func (r *volumeReaper) Delete(ctx context.Context, accountID string, resource Resource, force bool) error {
	_, err := gateway_ec2_volume.DeleteVolume(ctx, &ec2.DeleteVolumeInput{
		VolumeId: aws.String(resource.ID),
	}, r.nc, r.expectedNodes, accountID)
	if isAlreadyGone(err) {
		return nil
	}
	if !force {
		return err
	}
	// Force path: the ordinary delete refuses an attached volume, and an
	// instance that will not terminate would otherwise strand it forever.
	return forceDeleteVolume(ctx, r.nc, r.expectedNodes, accountID, resource.ID)
}

type snapshotReaper struct{ nc *nats.Conn }

func (r *snapshotReaper) Kind() string { return "snapshot" }
func (r *snapshotReaper) Stage() Stage { return StageStorage }

func (r *snapshotReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := gateway_ec2_snapshot.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
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
	_, err := gateway_ec2_snapshot.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{
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
	out, err := gateway_ec2_image.DescribeImages(ctx, &ec2.DescribeImagesInput{
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
	_, err := gateway_ec2_image.DeregisterImage(ctx, &ec2.DeregisterImageInput{
		ImageId: aws.String(resource.ID),
	}, r.nc, accountID)
	return ignoreAlreadyGone(err)
}

type addressReaper struct{ nc *nats.Conn }

func (r *addressReaper) Kind() string { return "address" }
func (r *addressReaper) Stage() Stage { return StageNetwork }

func (r *addressReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := gateway_ec2_eip.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{}, r.nc, accountID)
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
		if _, err := gateway_ec2_eip.DisassociateAddress(ctx, &ec2.DisassociateAddressInput{
			AssociationId: aws.String(associationID),
		}, r.nc, accountID); err != nil && !isAlreadyGone(err) && !isNotAssociated(err) {
			return err
		}
	}
	_, err = gateway_ec2_eip.ReleaseAddress(ctx, &ec2.ReleaseAddressInput{
		AllocationId: aws.String(resource.ID),
	}, r.nc, accountID)
	return ignoreAlreadyGone(err)
}

// associationID reads the address's current association rather than carrying it
// from the listing, because a delete may also be driven from an id an operator
// supplied. An empty answer means unattached, which is not an error.
func (r *addressReaper) associationID(ctx context.Context, accountID, allocationID string) (string, error) {
	out, err := gateway_ec2_eip.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{
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

type igwReaper struct{ nc *nats.Conn }

func (r *igwReaper) Kind() string { return "internet-gateway" }
func (r *igwReaper) Stage() Stage { return StageNetwork }

func (r *igwReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := gateway_ec2_igw.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{}, r.nc, accountID)
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
		if _, err := gateway_ec2_igw.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(resource.ID),
			VpcId:             aws.String(resource.Detail),
		}, r.nc, accountID); err != nil && !isAlreadyGone(err) {
			return err
		}
	}
	_, err := gateway_ec2_igw.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{
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
	out, err := gateway_ec2_routetable.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{}, r.nc, accountID)
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
		if _, err := gateway_ec2_routetable.DisassociateRouteTable(ctx, &ec2.DisassociateRouteTableInput{
			AssociationId: aws.String(association),
		}, r.nc, accountID); err != nil && !isAlreadyGone(err) {
			return err
		}
	}
	_, err := gateway_ec2_routetable.DeleteRouteTable(ctx, &ec2.DeleteRouteTableInput{
		RouteTableId: aws.String(resource.ID),
	}, r.nc, accountID)
	return ignoreAlreadyGone(err)
}

type subnetReaper struct{ nc *nats.Conn }

func (r *subnetReaper) Kind() string { return "subnet" }
func (r *subnetReaper) Stage() Stage { return StageNetwork }

func (r *subnetReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := gateway_ec2_vpc.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{}, r.nc, accountID)
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
	_, err := gateway_ec2_vpc.DeleteSubnet(ctx, &ec2.DeleteSubnetInput{
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
	out, err := gateway_ec2_vpc.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{}, r.nc, accountID)
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
	_, err := gateway_ec2_vpc.DeleteSecurityGroup(ctx, &ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(resource.ID),
	}, r.nc, accountID)
	return ignoreAlreadyGone(err)
}

type vpcReaper struct{ nc *nats.Conn }

func (r *vpcReaper) Kind() string { return "vpc" }
func (r *vpcReaper) Stage() Stage { return StageNetwork }

func (r *vpcReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := gateway_ec2_vpc.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{}, r.nc, accountID)
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
	_, err := gateway_ec2_vpc.DeleteVpc(ctx, &ec2.DeleteVpcInput{
		VpcId: aws.String(resource.ID),
	}, r.nc, accountID)
	return ignoreAlreadyGone(err)
}

type keyPairReaper struct{ nc *nats.Conn }

func (r *keyPairReaper) Kind() string { return "key-pair" }
func (r *keyPairReaper) Stage() Stage { return StagePlatform }

func (r *keyPairReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := gateway_ec2_key.DescribeKeyPairs(ctx, &ec2.DescribeKeyPairsInput{}, r.nc, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, pair := range out.KeyPairs {
		if pair == nil || pair.KeyName == nil {
			continue
		}
		found = append(found, Resource{Kind: r.Kind(), ID: *pair.KeyName})
	}
	return found, nil
}

func (r *keyPairReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := gateway_ec2_key.DeleteKeyPair(ctx, &ec2.DeleteKeyPairInput{
		KeyName: aws.String(resource.ID),
	}, r.nc, accountID)
	return ignoreAlreadyGone(err)
}

type placementGroupReaper struct{ nc *nats.Conn }

func (r *placementGroupReaper) Kind() string { return "placement-group" }
func (r *placementGroupReaper) Stage() Stage { return StagePlatform }

func (r *placementGroupReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := gateway_ec2_placementgroup.DescribePlacementGroups(ctx, &ec2.DescribePlacementGroupsInput{}, r.nc, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, group := range out.PlacementGroups {
		if group == nil || group.GroupName == nil {
			continue
		}
		found = append(found, Resource{Kind: r.Kind(), ID: *group.GroupName})
	}
	return found, nil
}

func (r *placementGroupReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := gateway_ec2_placementgroup.DeletePlacementGroup(ctx, &ec2.DeletePlacementGroupInput{
		GroupName: aws.String(resource.ID),
	}, r.nc, accountID)
	return ignoreAlreadyGone(err)
}

type launchTemplateReaper struct{ nc *nats.Conn }

func (r *launchTemplateReaper) Kind() string { return "launch-template" }
func (r *launchTemplateReaper) Stage() Stage { return StagePlatform }

func (r *launchTemplateReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	out, err := gateway_ec2_launchtemplate.DescribeLaunchTemplates(ctx, &ec2.DescribeLaunchTemplatesInput{}, r.nc, accountID)
	if err != nil {
		return nil, err
	}

	var found []Resource
	for _, template := range out.LaunchTemplates {
		if template == nil || template.LaunchTemplateId == nil {
			continue
		}
		found = append(found, Resource{Kind: r.Kind(), ID: *template.LaunchTemplateId})
	}
	return found, nil
}

func (r *launchTemplateReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := gateway_ec2_launchtemplate.DeleteLaunchTemplate(ctx, &ec2.DeleteLaunchTemplateInput{
		LaunchTemplateId: aws.String(resource.ID),
	}, r.nc, accountID)
	return ignoreAlreadyGone(err)
}

// notFoundMarkers are the AWS error-code fragments that mean the resource is
// already gone. Teardown re-runs after a crash, so treating these as failures
// would make a second pass unable to finish what the first one started.
var notFoundMarkers = []string{
	".NotFound", "NoSuchEntity", "InvalidAllocationID.NotFound",
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
	for _, marker := range notFoundMarkers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
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

// forceDetachSubject clears a volume's attachment in the control plane only.
// It is answered by any node, unlike the ordinary detach, which routes to the
// host running the instance and so cannot help when that host is the problem.
const forceDetachSubject = "ec2.ForceDetachVolume"

// forceDetachTimeout bounds the metadata-only detach. It touches one document
// and needs nothing from the guest, so a slow answer means the cluster is
// unwell rather than that the work is large.
const forceDetachTimeout = 30 * time.Second

// forceDeleteVolume clears the attachment the ordinary delete refuses to
// override, then deletes.
//
// This is the deadlock the force path exists for: a volume attached to an
// instance that will not terminate leaves both undeletable, and the account
// can never be emptied.
func forceDeleteVolume(ctx context.Context, nc *nats.Conn, expectedNodes int, accountID, volumeID string) error {
	_, err := utils.NATSRequest[ec2.VolumeAttachment](ctx, nc, forceDetachSubject,
		&ec2.DetachVolumeInput{VolumeId: aws.String(volumeID), Force: aws.Bool(true)},
		forceDetachTimeout, accountID)
	if err != nil && !isAlreadyGone(err) {
		return fmt.Errorf("force detach %s: %w", volumeID, err)
	}

	_, err = gateway_ec2_volume.DeleteVolume(ctx, &ec2.DeleteVolumeInput{
		VolumeId: aws.String(volumeID),
	}, nc, expectedNodes, accountID)
	return ignoreAlreadyGone(err)
}
