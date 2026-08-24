//test:in-package — the fidelity test reads ec2Scopes and each scope's
// unexported params to prove they name a parameter the handler parses.

package gateway_ec2

import (
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awsec2query"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testRegion    = "ap-southeast-2"
	testAccountID = "123456789012"
)

func ec2arn(kind, id string) string {
	return "arn:aws:ec2:" + testRegion + ":" + testAccountID + ":" + kind + "/" + id
}

func resolve(t *testing.T, action string, q map[string]string) []string {
	t.Helper()
	got, err := ResourceARNs(action, testRegion, testAccountID, q)
	require.NoError(t, err)
	return got
}

// TestResourceARNs pins the ARN each scoped action hands the evaluator. The
// order is the scope order, which the denial log reproduces.
func TestResourceARNs(t *testing.T) {
	tests := []struct {
		action string
		q      map[string]string
		want   []string
	}{
		// Instances.
		{"RunInstances", map[string]string{
			"ImageId": "ami-1", "SubnetId": "subnet-1", "SecurityGroupId.1": "sg-1", "KeyName": "k1",
		}, []string{
			ec2arn("instance", "*"), ec2arn("volume", "*"), ec2arn("image", "ami-1"),
			ec2arn("subnet", "subnet-1"), ec2arn("security-group", "sg-1"), ec2arn("key-pair", "k1"),
		}},
		{"RunInstances", map[string]string{"ImageId": "ami-1"},
			[]string{ec2arn("instance", "*"), ec2arn("volume", "*"), ec2arn("image", "ami-1")}},
		{"StartInstances", map[string]string{"InstanceId.1": "i-1"}, []string{ec2arn("instance", "i-1")}},
		{"StopInstances", map[string]string{"InstanceId.1": "i-1"}, []string{ec2arn("instance", "i-1")}},
		{"RebootInstances", map[string]string{"InstanceId.1": "i-1"}, []string{ec2arn("instance", "i-1")}},
		{"MonitorInstances", map[string]string{"InstanceId.1": "i-1"}, []string{ec2arn("instance", "i-1")}},
		{"UnmonitorInstances", map[string]string{"InstanceId.1": "i-1"}, []string{ec2arn("instance", "i-1")}},
		{"TerminateInstances", map[string]string{"InstanceId.1": "i-1", "InstanceId.2": "i-2"},
			[]string{ec2arn("instance", "i-1"), ec2arn("instance", "i-2")}},
		{"ModifyInstanceAttribute", map[string]string{"InstanceId": "i-1"}, []string{ec2arn("instance", "i-1")}},
		{"ModifyInstanceMetadataOptions", map[string]string{"InstanceId": "i-1"}, []string{ec2arn("instance", "i-1")}},
		{"GetConsoleOutput", map[string]string{"InstanceId": "i-1"}, []string{ec2arn("instance", "i-1")}},
		{"GetPasswordData", map[string]string{"InstanceId": "i-1"}, []string{ec2arn("instance", "i-1")}},
		{"AssociateIamInstanceProfile", map[string]string{"InstanceId": "i-1"}, []string{ec2arn("instance", "i-1")}},
		{"DisassociateIamInstanceProfile", map[string]string{"AssociationId": "iip-assoc-1"}, []string{"*"}},
		{"ReplaceIamInstanceProfileAssociation", map[string]string{"AssociationId": "iip-assoc-1"}, []string{"*"}},

		// Volumes and snapshots.
		{"CreateVolume", map[string]string{}, []string{ec2arn("volume", "*")}},
		{"CreateVolume", map[string]string{"SnapshotId": "snap-1"},
			[]string{ec2arn("volume", "*"), ec2arn("snapshot", "snap-1")}},
		{"DeleteVolume", map[string]string{"VolumeId": "vol-1"}, []string{ec2arn("volume", "vol-1")}},
		{"ModifyVolume", map[string]string{"VolumeId": "vol-1"}, []string{ec2arn("volume", "vol-1")}},
		{"AttachVolume", map[string]string{"VolumeId": "vol-1", "InstanceId": "i-1"},
			[]string{ec2arn("volume", "vol-1"), ec2arn("instance", "i-1")}},
		{"DetachVolume", map[string]string{"VolumeId": "vol-1"}, []string{ec2arn("volume", "vol-1")}},
		{"DetachVolume", map[string]string{"VolumeId": "vol-1", "InstanceId": "i-1"},
			[]string{ec2arn("volume", "vol-1"), ec2arn("instance", "i-1")}},
		{"CreateSnapshot", map[string]string{"VolumeId": "vol-1"},
			[]string{ec2arn("snapshot", "*"), ec2arn("volume", "vol-1")}},
		{"DeleteSnapshot", map[string]string{"SnapshotId": "snap-1"}, []string{ec2arn("snapshot", "snap-1")}},
		{"CopySnapshot", map[string]string{"SourceSnapshotId": "snap-1", "SourceRegion": "us-east-1"},
			[]string{ec2arn("snapshot", "*")}},

		// Images.
		{"CreateImage", map[string]string{"InstanceId": "i-1"},
			[]string{ec2arn("image", "*"), ec2arn("instance", "i-1")}},
		{"RegisterImage", map[string]string{"Name": "img"}, []string{ec2arn("image", "*")}},
		{"CopyImage", map[string]string{"SourceImageId": "ami-1", "SourceRegion": "us-east-1"},
			[]string{ec2arn("image", "*")}},
		{"DeregisterImage", map[string]string{"ImageId": "ami-1"}, []string{ec2arn("image", "ami-1")}},
		{"ModifyImageAttribute", map[string]string{"ImageId": "ami-1"}, []string{ec2arn("image", "ami-1")}},
		{"ResetImageAttribute", map[string]string{"ImageId": "ami-1"}, []string{ec2arn("image", "ami-1")}},

		// Key pairs. Either parameter names the same resource, so neither
		// contributes a spurious "*" when the other is the one supplied.
		{"CreateKeyPair", map[string]string{"KeyName": "k1"}, []string{ec2arn("key-pair", "k1")}},
		{"ImportKeyPair", map[string]string{"KeyName": "k1"}, []string{ec2arn("key-pair", "k1")}},
		{"DeleteKeyPair", map[string]string{"KeyName": "k1"}, []string{ec2arn("key-pair", "k1")}},
		{"DeleteKeyPair", map[string]string{"KeyPairId": "key-1"}, []string{ec2arn("key-pair", "key-1")}},

		// VPCs and subnets.
		{"CreateVpc", map[string]string{"CidrBlock": "10.0.0.0/16"}, []string{ec2arn("vpc", "*")}},
		{"DeleteVpc", map[string]string{"VpcId": "vpc-1"}, []string{ec2arn("vpc", "vpc-1")}},
		{"ModifyVpcAttribute", map[string]string{"VpcId": "vpc-1"}, []string{ec2arn("vpc", "vpc-1")}},
		{"CreateSubnet", map[string]string{"VpcId": "vpc-1"},
			[]string{ec2arn("subnet", "*"), ec2arn("vpc", "vpc-1")}},
		{"DeleteSubnet", map[string]string{"SubnetId": "subnet-1"}, []string{ec2arn("subnet", "subnet-1")}},
		{"ModifySubnetAttribute", map[string]string{"SubnetId": "subnet-1"}, []string{ec2arn("subnet", "subnet-1")}},

		// Route tables.
		{"CreateRouteTable", map[string]string{"VpcId": "vpc-1"},
			[]string{ec2arn("route-table", "*"), ec2arn("vpc", "vpc-1")}},
		{"DeleteRouteTable", map[string]string{"RouteTableId": "rtb-1"}, []string{ec2arn("route-table", "rtb-1")}},
		{"DeleteRoute", map[string]string{"RouteTableId": "rtb-1"}, []string{ec2arn("route-table", "rtb-1")}},
		{"CreateRoute", map[string]string{"RouteTableId": "rtb-1", "GatewayId": "igw-1"},
			[]string{ec2arn("route-table", "rtb-1"), ec2arn("internet-gateway", "igw-1")}},
		{"CreateRoute", map[string]string{"RouteTableId": "rtb-1", "NatGatewayId": "nat-1"},
			[]string{ec2arn("route-table", "rtb-1")}},
		{"ReplaceRoute", map[string]string{"RouteTableId": "rtb-1", "GatewayId": "igw-1"},
			[]string{ec2arn("route-table", "rtb-1"), ec2arn("internet-gateway", "igw-1")}},
		{"AssociateRouteTable", map[string]string{"RouteTableId": "rtb-1", "SubnetId": "subnet-1"},
			[]string{ec2arn("route-table", "rtb-1"), ec2arn("subnet", "subnet-1")}},
		{"ReplaceRouteTableAssociation", map[string]string{"RouteTableId": "rtb-1", "AssociationId": "rtbassoc-1"},
			[]string{ec2arn("route-table", "rtb-1")}},
		{"DisassociateRouteTable", map[string]string{"AssociationId": "rtbassoc-1"}, []string{"*"}},

		// Gateways.
		{"CreateInternetGateway", map[string]string{}, []string{ec2arn("internet-gateway", "*")}},
		{"DeleteInternetGateway", map[string]string{"InternetGatewayId": "igw-1"},
			[]string{ec2arn("internet-gateway", "igw-1")}},
		{"AttachInternetGateway", map[string]string{"InternetGatewayId": "igw-1", "VpcId": "vpc-1"},
			[]string{ec2arn("internet-gateway", "igw-1"), ec2arn("vpc", "vpc-1")}},
		{"DetachInternetGateway", map[string]string{"InternetGatewayId": "igw-1", "VpcId": "vpc-1"},
			[]string{ec2arn("internet-gateway", "igw-1"), ec2arn("vpc", "vpc-1")}},
		{"CreateEgressOnlyInternetGateway", map[string]string{"VpcId": "vpc-1"},
			[]string{ec2arn("egress-only-internet-gateway", "*"), ec2arn("vpc", "vpc-1")}},
		{"DeleteEgressOnlyInternetGateway", map[string]string{"EgressOnlyInternetGatewayId": "eigw-1"},
			[]string{ec2arn("egress-only-internet-gateway", "eigw-1")}},
		{"CreateNatGateway", map[string]string{"SubnetId": "subnet-1", "AllocationId": "eipalloc-1"},
			[]string{ec2arn("natgateway", "*"), ec2arn("subnet", "subnet-1"), ec2arn("elastic-ip", "eipalloc-1")}},
		{"DeleteNatGateway", map[string]string{"NatGatewayId": "nat-1"}, []string{ec2arn("natgateway", "nat-1")}},

		// Network interfaces.
		{"CreateNetworkInterface", map[string]string{"SubnetId": "subnet-1", "SecurityGroupId.1": "sg-1"},
			[]string{ec2arn("network-interface", "*"), ec2arn("subnet", "subnet-1"), ec2arn("security-group", "sg-1")}},
		{"DeleteNetworkInterface", map[string]string{"NetworkInterfaceId": "eni-1"},
			[]string{ec2arn("network-interface", "eni-1")}},
		{"ModifyNetworkInterfaceAttribute", map[string]string{"NetworkInterfaceId": "eni-1"},
			[]string{ec2arn("network-interface", "eni-1")}},
		{"AttachNetworkInterface", map[string]string{"NetworkInterfaceId": "eni-1", "InstanceId": "i-1"},
			[]string{ec2arn("network-interface", "eni-1"), ec2arn("instance", "i-1")}},
		{"DetachNetworkInterface", map[string]string{"AttachmentId": "eni-attach-1"}, []string{"*"}},

		// Security groups.
		{"CreateSecurityGroup", map[string]string{"VpcId": "vpc-1"},
			[]string{ec2arn("security-group", "*"), ec2arn("vpc", "vpc-1")}},
		{"CreateSecurityGroup", map[string]string{"GroupName": "web"}, []string{ec2arn("security-group", "*")}},
		{"DeleteSecurityGroup", map[string]string{"GroupId": "sg-1"}, []string{ec2arn("security-group", "sg-1")}},
		{"AuthorizeSecurityGroupIngress", map[string]string{"GroupId": "sg-1"}, []string{ec2arn("security-group", "sg-1")}},
		{"AuthorizeSecurityGroupEgress", map[string]string{"GroupId": "sg-1"}, []string{ec2arn("security-group", "sg-1")}},
		{"RevokeSecurityGroupIngress", map[string]string{"GroupId": "sg-1"}, []string{ec2arn("security-group", "sg-1")}},
		{"RevokeSecurityGroupEgress", map[string]string{"GroupId": "sg-1"}, []string{ec2arn("security-group", "sg-1")}},

		// Addresses.
		{"AllocateAddress", map[string]string{"Domain": "vpc"}, []string{ec2arn("elastic-ip", "*")}},
		{"ReleaseAddress", map[string]string{"AllocationId": "eipalloc-1"}, []string{ec2arn("elastic-ip", "eipalloc-1")}},
		{"AssociateAddress", map[string]string{"AllocationId": "eipalloc-1", "InstanceId": "i-1"},
			[]string{ec2arn("elastic-ip", "eipalloc-1"), ec2arn("instance", "i-1")}},
		{"AssociateAddress", map[string]string{"AllocationId": "eipalloc-1", "NetworkInterfaceId": "eni-1"},
			[]string{ec2arn("elastic-ip", "eipalloc-1"), ec2arn("network-interface", "eni-1")}},
		{"DisassociateAddress", map[string]string{"AssociationId": "eipassoc-1"}, []string{"*"}},

		// Placement groups.
		{"CreatePlacementGroup", map[string]string{"GroupName": "pg1"}, []string{ec2arn("placement-group", "pg1")}},
		{"DeletePlacementGroup", map[string]string{"GroupName": "pg1"}, []string{ec2arn("placement-group", "pg1")}},

		// Launch templates.
		{"CreateLaunchTemplate", map[string]string{"LaunchTemplateName": "lt"},
			[]string{ec2arn("launch-template", "*")}},
		{"CreateLaunchTemplateVersion", map[string]string{"LaunchTemplateId": "lt-1"},
			[]string{ec2arn("launch-template", "lt-1")}},
		{"ModifyLaunchTemplate", map[string]string{"LaunchTemplateId": "lt-1"},
			[]string{ec2arn("launch-template", "lt-1")}},
		{"DeleteLaunchTemplate", map[string]string{"LaunchTemplateId": "lt-1"},
			[]string{ec2arn("launch-template", "lt-1")}},
		{"DeleteLaunchTemplateVersions", map[string]string{"LaunchTemplateId": "lt-1"},
			[]string{ec2arn("launch-template", "lt-1")}},

		// Capacity reservations and spot.
		{"CreateCapacityReservation", map[string]string{"InstanceType": "t3.micro"},
			[]string{ec2arn("capacity-reservation", "*")}},
		{"CancelCapacityReservation", map[string]string{"CapacityReservationId": "cr-1"},
			[]string{ec2arn("capacity-reservation", "cr-1")}},
		{"RequestSpotInstances", map[string]string{"SpotPrice": "0.01"},
			[]string{ec2arn("spot-instances-request", "*")}},
		{"CancelSpotInstanceRequests", map[string]string{"SpotInstanceRequestId.1": "sir-1"},
			[]string{ec2arn("spot-instances-request", "sir-1")}},

		// Tags: the type comes from each id's prefix, one member per id.
		{"CreateTags", map[string]string{"ResourceId.1": "i-1", "ResourceId.2": "vol-1"},
			[]string{ec2arn("instance", "i-1"), ec2arn("volume", "vol-1")}},
		{"DeleteTags", map[string]string{"ResourceId.1": "sg-1"}, []string{ec2arn("security-group", "sg-1")}},
		// An untypable id has no correct ARN, so the fence stays inert rather
		// than fencing a plausible-looking wrong object.
		{"CreateTags", map[string]string{"ResourceId.1": "i-1", "ResourceId.2": "nope-1"},
			[]string{ec2arn("instance", "i-1"), "*"}},

		// Account-level settings name no resource.
		{"EnableEbsEncryptionByDefault", map[string]string{}, []string{"*"}},
		{"DisableEbsEncryptionByDefault", map[string]string{}, []string{"*"}},
		{"GetEbsEncryptionByDefault", map[string]string{}, []string{"*"}},
		{"EnableSerialConsoleAccess", map[string]string{}, []string{"*"}},
		{"DisableSerialConsoleAccess", map[string]string{}, []string{"*"}},
		{"GetSerialConsoleAccessStatus", map[string]string{}, []string{"*"}},

		// Describes evaluate account-wide, as they do in AWS.
		{"DescribeInstances", map[string]string{"InstanceId.1": "i-1"}, []string{"*"}},
		{"DescribeVolumes", map[string]string{"VolumeId.1": "vol-1"}, []string{"*"}},
	}

	for _, tc := range tests {
		t.Run(tc.action, func(t *testing.T) {
			assert.Equal(t, tc.want, resolve(t, tc.action, tc.q))
		})
	}
}

// TestResourceARNsMissingIdentifier pins the property most easily lost: an
// absent identifier stays the handler's validation fault rather than becoming an
// authorization failure here.
func TestResourceARNsMissingIdentifier(t *testing.T) {
	assert.Equal(t, []string{"*"}, resolve(t, "TerminateInstances", map[string]string{}))
	assert.Equal(t, []string{"*"}, resolve(t, "DeleteVolume", map[string]string{}))
	// GroupName names a security group only in EC2-Classic, which has no ARN.
	assert.Equal(t, []string{"*"}, resolve(t, "DeleteSecurityGroup", map[string]string{"GroupName": "web"}))
	// A name-only launch template request leaves the id unresolved.
	assert.Equal(t, []string{"*"}, resolve(t, "DeleteLaunchTemplate", map[string]string{"LaunchTemplateName": "lt"}))
}

func TestResourceARNsUnknownAction(t *testing.T) {
	_, err := ResourceARNs("NotAnAction", testRegion, testAccountID, map[string]string{})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidAction, err.Error())
}

// TestCollectIndexed covers the list forms the query parser accepts and the
// two ways a naive collector loses resources.
func TestCollectIndexed(t *testing.T) {
	t.Run("member wrapper", func(t *testing.T) {
		assert.Equal(t, []string{ec2arn("instance", "i-1")},
			resolve(t, "TerminateInstances", map[string]string{"InstanceId.member.1": "i-1"}))
	})

	t.Run("field-name alias", func(t *testing.T) {
		assert.Equal(t, []string{ec2arn("instance", "i-1")},
			resolve(t, "TerminateInstances", map[string]string{"InstanceIds.1": "i-1"}))
	})

	t.Run("numeric order, not lexical", func(t *testing.T) {
		q := map[string]string{"InstanceId.1": "i-1", "InstanceId.2": "i-2", "InstanceId.10": "i-10"}
		assert.Equal(t, []string{ec2arn("instance", "i-1"), ec2arn("instance", "i-2"), ec2arn("instance", "i-10")},
			resolve(t, "TerminateInstances", q))
	})

	t.Run("gaps do not terminate the list", func(t *testing.T) {
		q := map[string]string{"InstanceId.1": "i-1", "InstanceId.3": "i-3"}
		assert.Equal(t, []string{ec2arn("instance", "i-1"), ec2arn("instance", "i-3")},
			resolve(t, "TerminateInstances", q))
	})

	t.Run("nested keys are not collected", func(t *testing.T) {
		q := map[string]string{"ResourceId.1": "i-1", "ResourceId.1.Value.1": "ignored"}
		assert.Equal(t, []string{ec2arn("instance", "i-1")}, resolve(t, "CreateTags", q))
	})
}

// TestResourceARNsIdentifierIsAValue covers the two shapes from the reverted STS
// work. Both build a value, not a pattern: an id containing / is not truncated,
// and an id that is literally * neither matches a scoped Deny nor widens a grant.
func TestResourceARNsIdentifierIsAValue(t *testing.T) {
	assert.Equal(t, []string{ec2arn("instance", "i-1/admin")},
		resolve(t, "TerminateInstances", map[string]string{"InstanceId.1": "i-1/admin"}))
	assert.Equal(t, []string{ec2arn("instance", "*")},
		resolve(t, "TerminateInstances", map[string]string{"InstanceId.1": "*"}))
}

// ec2Inputs is the input shape each scoped action's handler parses. It exists so
// the fidelity test below can prove the resolver reads a parameter the handler
// reads too, rather than checking the table against itself.
var ec2Inputs = map[string]any{
	"AllocateAddress":                      &ec2.AllocateAddressInput{},
	"AssociateAddress":                     &ec2.AssociateAddressInput{},
	"AssociateIamInstanceProfile":          &ec2.AssociateIamInstanceProfileInput{},
	"AssociateRouteTable":                  &ec2.AssociateRouteTableInput{},
	"AttachInternetGateway":                &ec2.AttachInternetGatewayInput{},
	"AttachNetworkInterface":               &ec2.AttachNetworkInterfaceInput{},
	"AttachVolume":                         &ec2.AttachVolumeInput{},
	"AuthorizeSecurityGroupEgress":         &ec2.AuthorizeSecurityGroupEgressInput{},
	"AuthorizeSecurityGroupIngress":        &ec2.AuthorizeSecurityGroupIngressInput{},
	"CancelCapacityReservation":            &ec2.CancelCapacityReservationInput{},
	"CancelSpotInstanceRequests":           &ec2.CancelSpotInstanceRequestsInput{},
	"CopyImage":                            &ec2.CopyImageInput{},
	"CopySnapshot":                         &ec2.CopySnapshotInput{},
	"CreateCapacityReservation":            &ec2.CreateCapacityReservationInput{},
	"CreateEgressOnlyInternetGateway":      &ec2.CreateEgressOnlyInternetGatewayInput{},
	"CreateImage":                          &ec2.CreateImageInput{},
	"CreateInternetGateway":                &ec2.CreateInternetGatewayInput{},
	"CreateKeyPair":                        &ec2.CreateKeyPairInput{},
	"CreateLaunchTemplate":                 &ec2.CreateLaunchTemplateInput{},
	"CreateLaunchTemplateVersion":          &ec2.CreateLaunchTemplateVersionInput{},
	"CreateNatGateway":                     &ec2.CreateNatGatewayInput{},
	"CreateNetworkInterface":               &ec2.CreateNetworkInterfaceInput{},
	"CreatePlacementGroup":                 &ec2.CreatePlacementGroupInput{},
	"CreateRoute":                          &ec2.CreateRouteInput{},
	"CreateRouteTable":                     &ec2.CreateRouteTableInput{},
	"CreateSecurityGroup":                  &ec2.CreateSecurityGroupInput{},
	"CreateSnapshot":                       &ec2.CreateSnapshotInput{},
	"CreateSubnet":                         &ec2.CreateSubnetInput{},
	"CreateTags":                           &ec2.CreateTagsInput{},
	"CreateVolume":                         &ec2.CreateVolumeInput{},
	"CreateVpc":                            &ec2.CreateVpcInput{},
	"DeleteEgressOnlyInternetGateway":      &ec2.DeleteEgressOnlyInternetGatewayInput{},
	"DeleteInternetGateway":                &ec2.DeleteInternetGatewayInput{},
	"DeleteKeyPair":                        &ec2.DeleteKeyPairInput{},
	"DeleteLaunchTemplate":                 &ec2.DeleteLaunchTemplateInput{},
	"DeleteLaunchTemplateVersions":         &ec2.DeleteLaunchTemplateVersionsInput{},
	"DeleteNatGateway":                     &ec2.DeleteNatGatewayInput{},
	"DeleteNetworkInterface":               &ec2.DeleteNetworkInterfaceInput{},
	"DeletePlacementGroup":                 &ec2.DeletePlacementGroupInput{},
	"DeleteRoute":                          &ec2.DeleteRouteInput{},
	"DeleteRouteTable":                     &ec2.DeleteRouteTableInput{},
	"DeleteSecurityGroup":                  &ec2.DeleteSecurityGroupInput{},
	"DeleteSnapshot":                       &ec2.DeleteSnapshotInput{},
	"DeleteSubnet":                         &ec2.DeleteSubnetInput{},
	"DeleteTags":                           &ec2.DeleteTagsInput{},
	"DeleteVolume":                         &ec2.DeleteVolumeInput{},
	"DeleteVpc":                            &ec2.DeleteVpcInput{},
	"DeregisterImage":                      &ec2.DeregisterImageInput{},
	"DetachInternetGateway":                &ec2.DetachInternetGatewayInput{},
	"DetachNetworkInterface":               &ec2.DetachNetworkInterfaceInput{},
	"DetachVolume":                         &ec2.DetachVolumeInput{},
	"DisassociateAddress":                  &ec2.DisassociateAddressInput{},
	"DisassociateIamInstanceProfile":       &ec2.DisassociateIamInstanceProfileInput{},
	"DisassociateRouteTable":               &ec2.DisassociateRouteTableInput{},
	"GetConsoleOutput":                     &ec2.GetConsoleOutputInput{},
	"GetPasswordData":                      &ec2.GetPasswordDataInput{},
	"ImportKeyPair":                        &ec2.ImportKeyPairInput{},
	"ModifyImageAttribute":                 &ec2.ModifyImageAttributeInput{},
	"ModifyInstanceAttribute":              &ec2.ModifyInstanceAttributeInput{},
	"ModifyInstanceMetadataOptions":        &ec2.ModifyInstanceMetadataOptionsInput{},
	"ModifyLaunchTemplate":                 &ec2.ModifyLaunchTemplateInput{},
	"ModifyNetworkInterfaceAttribute":      &ec2.ModifyNetworkInterfaceAttributeInput{},
	"ModifySubnetAttribute":                &ec2.ModifySubnetAttributeInput{},
	"ModifyVolume":                         &ec2.ModifyVolumeInput{},
	"ModifyVpcAttribute":                   &ec2.ModifyVpcAttributeInput{},
	"MonitorInstances":                     &ec2.MonitorInstancesInput{},
	"RebootInstances":                      &ec2.RebootInstancesInput{},
	"RegisterImage":                        &ec2.RegisterImageInput{},
	"ReleaseAddress":                       &ec2.ReleaseAddressInput{},
	"ReplaceIamInstanceProfileAssociation": &ec2.ReplaceIamInstanceProfileAssociationInput{},
	"ReplaceRoute":                         &ec2.ReplaceRouteInput{},
	"ReplaceRouteTableAssociation":         &ec2.ReplaceRouteTableAssociationInput{},
	"RequestSpotInstances":                 &ec2.RequestSpotInstancesInput{},
	"ResetImageAttribute":                  &ec2.ResetImageAttributeInput{},
	"RevokeSecurityGroupEgress":            &ec2.RevokeSecurityGroupEgressInput{},
	"RevokeSecurityGroupIngress":           &ec2.RevokeSecurityGroupIngressInput{},
	"RunInstances":                         &ec2.RunInstancesInput{},
	"StartInstances":                       &ec2.StartInstancesInput{},
	"StopInstances":                        &ec2.StopInstancesInput{},
	"TerminateInstances":                   &ec2.TerminateInstancesInput{},
	"UnmonitorInstances":                   &ec2.UnmonitorInstancesInput{},
}

// TestScopeParametersReachTheHandlerInput is the fidelity guard. A scope naming
// a parameter the handler never reads produces a gate that appears to work and
// authorizes against nothing, which is worse than "*": it fences the wrong
// object silently. Each scope must have at least one parameter the query parser
// binds, so the id the resolver reads is an id the handler acts on.
func TestScopeParametersReachTheHandlerInput(t *testing.T) {
	const sentinel = "sentinel-value"

	for action, scopes := range ec2Scopes {
		input, ok := ec2Inputs[action]
		if !ok {
			// Unscoped actions read no identifier, so there is nothing to bind.
			continue
		}
		for i, scope := range scopes {
			if len(scope.params) == 0 {
				continue
			}
			t.Run(action, func(t *testing.T) {
				var bound []string
				for _, param := range scope.params {
					key := param
					if scope.list {
						key = param + ".1"
					}
					fresh := reflect.New(reflect.TypeOf(input).Elem()).Interface()
					require.NoError(t, awsec2query.QueryParamsToStruct(map[string]string{key: sentinel}, fresh))
					if containsString(reflect.ValueOf(fresh), sentinel) {
						bound = append(bound, param)
					}
				}
				assert.NotEmpty(t, bound,
					"%s scope %d names %v, none of which the handler's input parses", action, i, scope.params)
			})
		}
	}
}

// containsString reports whether any string reachable from v equals want.
func containsString(v reflect.Value, want string) bool {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		return !v.IsNil() && containsString(v.Elem(), want)
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if containsString(v.Index(i), want) {
				return true
			}
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			if containsString(v.MapIndex(key), want) {
				return true
			}
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).PkgPath == "" && containsString(v.Field(i), want) {
				return true
			}
		}
	case reflect.String:
		return v.String() == want
	}
	return false
}
