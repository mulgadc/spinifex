package handlers_eks

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/handlers/sysinstance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClusterJoinEndpoint_PrivateAccessPrefersSetA(t *testing.T) {
	// public+private: published endpoint is public, but workers join via Set A.
	meta := &ClusterMeta{
		Endpoint:          "https://203.0.113.9:443",
		PrivateEndpointIP: "10.20.0.5",
		ResourcesVpcConfig: &ClusterVpcConfig{
			EndpointPublicAccess:  true,
			EndpointPrivateAccess: true,
		},
	}
	assert.Equal(t, "https://10.20.0.5:443", clusterJoinEndpoint(meta))
}

func TestClusterJoinEndpoint_PrivateOnlyMatchesPublished(t *testing.T) {
	// private-only: meta.Endpoint already equals the Set A endpoint.
	meta := &ClusterMeta{
		Endpoint:          "https://10.20.0.5:443",
		PrivateEndpointIP: "10.20.0.5",
		ResourcesVpcConfig: &ClusterVpcConfig{
			EndpointPublicAccess:  false,
			EndpointPrivateAccess: true,
		},
	}
	assert.Equal(t, "https://10.20.0.5:443", clusterJoinEndpoint(meta))
}

func TestClusterJoinEndpoint_PublicOnlyUsesPublished(t *testing.T) {
	meta := &ClusterMeta{
		Endpoint: "https://203.0.113.9:443",
		ResourcesVpcConfig: &ClusterVpcConfig{
			EndpointPublicAccess:  true,
			EndpointPrivateAccess: false,
		},
	}
	assert.Equal(t, "https://203.0.113.9:443", clusterJoinEndpoint(meta))
}

func TestEnsurePrivateEndpointSG_AuthorizesVPCCIDROnAPIServerPorts(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.createIDs = []string{"sg-pe-001"}

	sgID, err := EnsurePrivateEndpointSG(sgp, "111122223333", "alpha", "vpc-aaa", "10.0.0.0/16")
	require.NoError(t, err)
	assert.Equal(t, "sg-pe-001", sgID)

	// :443 for kubectl/SDK; :6443 for worker pods hitting the in-cluster kubernetes Endpoints.
	require.Len(t, sgp.authorizeCalls, 2)
	ports := map[int64]bool{}
	for _, in := range sgp.authorizeCalls {
		assert.Equal(t, "sg-pe-001", aws.StringValue(in.GroupId))
		require.Len(t, in.IpPermissions, 1)
		perm := in.IpPermissions[0]
		assert.Equal(t, "tcp", aws.StringValue(perm.IpProtocol))
		assert.Equal(t, aws.Int64Value(perm.FromPort), aws.Int64Value(perm.ToPort))
		require.Len(t, perm.IpRanges, 1)
		assert.Equal(t, "10.0.0.0/16", aws.StringValue(perm.IpRanges[0].CidrIp))
		ports[aws.Int64Value(perm.FromPort)] = true
	}
	assert.True(t, ports[clusterNLBListenPort], "admits :443")
	assert.True(t, ports[k3sAPIServerPort], "admits :6443")
}

func TestEnsurePrivateEndpointSG_EmptyInputsRejected(t *testing.T) {
	sgp := newFakeSGProvisioner()
	_, err := EnsurePrivateEndpointSG(sgp, "111122223333", "", "vpc-aaa", "10.0.0.0/16")
	require.Error(t, err)
	_, err = EnsurePrivateEndpointSG(sgp, "111122223333", "alpha", "", "10.0.0.0/16")
	require.Error(t, err)
	_, err = EnsurePrivateEndpointSG(sgp, "111122223333", "alpha", "vpc-aaa", "")
	require.Error(t, err)
	assert.Empty(t, sgp.createCalls)
}

func TestEnsurePrivateEndpointSG_DuplicateIngressTolerated(t *testing.T) {
	sgp := newFakeSGProvisioner()
	sgp.authorizeErr = errors.New(awserrors.ErrorInvalidPermissionDuplicate)

	_, err := EnsurePrivateEndpointSG(sgp, "111122223333", "alpha", "vpc-aaa", "10.0.0.0/16")
	require.NoError(t, err, "a duplicate :443 rule on re-run must be treated as success")
}

func TestEnsurePrivateEndpointENI_HappyPath(t *testing.T) {
	vpcSvc := &fakeK3sVPC{
		createOut: &ec2.CreateNetworkInterfaceOutput{
			NetworkInterface: &ec2.NetworkInterface{
				NetworkInterfaceId: aws.String("eni-pe-001"),
				PrivateIpAddress:   aws.String("10.0.1.50"),
				MacAddress:         aws.String("02:0a:01:23:45:67"),
				SubnetId:           aws.String("subnet-aaa"),
			},
		},
	}
	sgp := newFakeSGProvisioner()
	sgp.createIDs = []string{"sg-pe-001"}

	pe, err := EnsurePrivateEndpointENI(vpcSvc, sgp, fakeSubnetResolver{}, "111122223333", "alpha", "subnet-aaa", "vpc-aaa")
	require.NoError(t, err)
	assert.Equal(t, "eni-pe-001", pe.ENIID)
	assert.Equal(t, "10.0.1.50", pe.ENIIP)
	assert.Equal(t, "02:0a:01:23:45:67", pe.ENIMac)
	assert.Equal(t, "sg-pe-001", pe.SGID)

	// ENI must be created in the customer subnet, bound to the private-endpoint SG.
	require.Len(t, vpcSvc.createCalls, 1)
	createIn := vpcSvc.createCalls[0]
	assert.Equal(t, "subnet-aaa", aws.StringValue(createIn.SubnetId))
	require.Len(t, createIn.Groups, 1)
	assert.Equal(t, "sg-pe-001", aws.StringValue(createIn.Groups[0]))
}

func TestEnsurePrivateEndpointENI_EmptyInputsRejected(t *testing.T) {
	vpcSvc := &fakeK3sVPC{}
	sgp := newFakeSGProvisioner()
	_, err := EnsurePrivateEndpointENI(vpcSvc, sgp, fakeSubnetResolver{}, "", "alpha", "subnet-aaa", "vpc-aaa")
	require.Error(t, err)
	_, err = EnsurePrivateEndpointENI(vpcSvc, sgp, fakeSubnetResolver{}, "111122223333", "alpha", "", "vpc-aaa")
	require.Error(t, err)
	_, err = EnsurePrivateEndpointENI(vpcSvc, sgp, fakeSubnetResolver{}, "111122223333", "alpha", "subnet-aaa", "")
	require.Error(t, err)
	assert.Empty(t, vpcSvc.createCalls)
}

func TestEnsureClusterNLB_ThreadsCrossAccountENIToSyncCreate(t *testing.T) {
	nlbp := newFakeNLBProvisioner()
	extras := []sysinstance.ExtraENIInput{{
		ENIID:     "eni-pe-001",
		ENIMac:    "02:0a:01:23:45:67",
		ENIIP:     "10.0.1.50",
		SubnetID:  "subnet-aaa",
		AccountID: "111122223333",
	}}

	_, err := EnsureClusterNLB(nlbp, "000000000000", "alpha", []string{"subnet-cp"}, false, nil, extras)
	require.NoError(t, err)

	require.Len(t, nlbp.createClusterNLBExtras, 1, "extras present must route through CreateClusterNLBSync")
	require.Len(t, nlbp.createClusterNLBExtras[0], 1)
	assert.Equal(t, "eni-pe-001", nlbp.createClusterNLBExtras[0][0].ENIID)
	assert.Equal(t, "111122223333", nlbp.createClusterNLBExtras[0][0].AccountID)
}

func TestEnsureClusterNLB_NoExtrasUsesPlainSync(t *testing.T) {
	nlbp := newFakeNLBProvisioner()

	_, err := EnsureClusterNLB(nlbp, "000000000000", "alpha", []string{"subnet-cp"}, false, nil, nil)
	require.NoError(t, err)

	assert.Empty(t, nlbp.createClusterNLBExtras, "no extras must use the plain sync create path")
	assert.Len(t, nlbp.createLBSyncCalls, 1)
}
