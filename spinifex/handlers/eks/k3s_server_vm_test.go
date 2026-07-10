package handlers_eks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/handlers/sysinstance"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeK3sVPC struct {
	createCalls []*ec2.CreateNetworkInterfaceInput
	deleteCalls []*ec2.DeleteNetworkInterfaceInput
	detachCalls []string

	createOut *ec2.CreateNetworkInterfaceOutput
	createErr error
	deleteErr error
	detachErr error
	// inUseUntilDetached models real VPC semantics: DeleteNetworkInterface returns
	// InvalidNetworkInterface.InUse for these ENIs until DetachENI clears the
	// attachment. detached tracks which ENIs DetachENI has cleared.
	inUseUntilDetached map[string]bool
	detached           map[string]bool

	// describeByENI maps an ENI ID to the record DescribeNetworkInterfaces
	// returns; a missing ENI yields an empty result (gone). describeErr forces
	// a describe failure.
	describeByENI map[string]*ec2.NetworkInterface
	describeErr   error
}

var _ k3sVPCProvisioner = (*fakeK3sVPC)(nil)

func (f *fakeK3sVPC) CreateNetworkInterface(_ context.Context, input *ec2.CreateNetworkInterfaceInput, _ string) (*ec2.CreateNetworkInterfaceOutput, error) {
	f.createCalls = append(f.createCalls, input)
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createOut != nil {
		return f.createOut, nil
	}
	return &ec2.CreateNetworkInterfaceOutput{
		NetworkInterface: &ec2.NetworkInterface{
			NetworkInterfaceId: aws.String("eni-aaa111"),
			PrivateIpAddress:   aws.String("10.0.1.42"),
			SubnetId:           input.SubnetId,
		},
	}, nil
}

func (f *fakeK3sVPC) DeleteNetworkInterface(_ context.Context, input *ec2.DeleteNetworkInterfaceInput, _ string) (*ec2.DeleteNetworkInterfaceOutput, error) {
	f.deleteCalls = append(f.deleteCalls, input)
	if id := aws.StringValue(input.NetworkInterfaceId); f.inUseUntilDetached[id] && !f.detached[id] {
		return nil, errors.New(awserrors.ErrorInvalidNetworkInterfaceInUse)
	}
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &ec2.DeleteNetworkInterfaceOutput{}, nil
}

func (f *fakeK3sVPC) DetachENI(_ context.Context, _, eniID string) error {
	f.detachCalls = append(f.detachCalls, eniID)
	if f.detachErr != nil {
		return f.detachErr
	}
	if f.detached == nil {
		f.detached = map[string]bool{}
	}
	f.detached[eniID] = true
	return nil
}

func (f *fakeK3sVPC) DescribeNetworkInterfaces(_ context.Context, input *ec2.DescribeNetworkInterfacesInput, _ string) (*ec2.DescribeNetworkInterfacesOutput, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	out := &ec2.DescribeNetworkInterfacesOutput{}
	for _, id := range input.NetworkInterfaceIds {
		if eni, ok := f.describeByENI[aws.StringValue(id)]; ok {
			out.NetworkInterfaces = append(out.NetworkInterfaces, eni)
		}
	}
	return out, nil
}

type fakeK3sInst struct {
	launchCalls    []*sysinstance.SystemInstanceInput
	launchNodes    []string // TargetNodeID per launch (parallel to launchCalls)
	terminateCalls []string

	launchOut    *sysinstance.SystemInstanceOutput
	launchErr    error
	terminateErr error
}

var _ k3sInstanceLauncher = (*fakeK3sInst)(nil)

func (f *fakeK3sInst) LaunchSystemInstance(input *sysinstance.SystemInstanceInput) (*sysinstance.SystemInstanceOutput, error) {
	return f.LaunchSystemInstanceOnNode("", input)
}

func (f *fakeK3sInst) LaunchSystemInstanceOnNode(nodeID string, input *sysinstance.SystemInstanceInput) (*sysinstance.SystemInstanceOutput, error) {
	f.launchCalls = append(f.launchCalls, input)
	f.launchNodes = append(f.launchNodes, nodeID)
	if f.launchErr != nil {
		return nil, f.launchErr
	}
	if f.launchOut != nil {
		return f.launchOut, nil
	}
	return &sysinstance.SystemInstanceOutput{InstanceID: "i-aaa111"}, nil
}

func (f *fakeK3sInst) TerminateSystemInstance(instanceID string) error {
	f.terminateCalls = append(f.terminateCalls, instanceID)
	return f.terminateErr
}

type fakeK3sAMI struct {
	mu            sync.Mutex
	describeCalls []*ec2.DescribeImagesInput

	describeOut *ec2.DescribeImagesOutput
	describeErr error
}

var _ k3sAMIResolver = (*fakeK3sAMI)(nil)

func (f *fakeK3sAMI) DescribeImages(_ context.Context, input *ec2.DescribeImagesInput, _ string) (*ec2.DescribeImagesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.describeCalls = append(f.describeCalls, input)
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	if f.describeOut != nil {
		return f.describeOut, nil
	}
	return &ec2.DescribeImagesOutput{
		Images: []*ec2.Image{{
			ImageId:      aws.String("ami-eks-server-001"),
			Name:         aws.String("spinifex-eks-server"),
			CreationDate: aws.String("2026-06-01T00:00:00.000Z"),
			Tags: []*ec2.Tag{
				{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByEKS)},
			},
		}},
	}, nil
}

func validK3sInput() K3sServerInput {
	return K3sServerInput{
		AccountID:         "000000000000",
		ClusterAccountID:  "111122223333",
		ClusterName:       "alpha",
		Region:            "us-east-1",
		SubnetID:          "subnet-aaa",
		VpcID:             "vpc-aaa",
		ControlPlaneSGID:  "sg-cp-aaa",
		NLBDNS:            "eks-alpha-lb-001.us-east-1.elb.spinifex.local",
		OIDCIssuer:        "https://oidc.spinifex.local/clusters/111122223333/alpha",
		OIDCPrivateKeyPEM: "-----BEGIN PRIVATE KEY-----\nFAKEKEY\n-----END PRIVATE KEY-----\n",
		OIDCPublicKeyPEM:  "-----BEGIN PUBLIC KEY-----\nFAKEPUB\n-----END PUBLIC KEY-----\n",
		GatewayURL:        "https://10.15.8.1:9999",
		AddonGatewayURL:   "https://192.168.1.33:9999",
		AccessKey:         "AKIAEXAMPLE",
		SecretKey:         "s3cr3t-key",
		GatewayCACert:     "-----BEGIN CERTIFICATE-----\nFAKECA\n-----END CERTIFICATE-----\n",
		JoinToken:         "clustertok-deadbeef",

		PredastoreEndpoint:  "https://10.15.8.1:8443",
		PredastoreAccessKey: "AKIAPREDASTORE",
		PredastoreSecretKey: "pred-s3cr3t-key",
	}
}

func TestLaunchK3sServerVM_EmptyInputsRejected(t *testing.T) {
	mk := func(mutate func(*K3sServerInput)) K3sServerInput {
		in := validK3sInput()
		mutate(&in)
		return in
	}
	cases := []struct {
		name string
		in   K3sServerInput
	}{
		{"empty AccountID", mk(func(in *K3sServerInput) { in.AccountID = "" })},
		{"empty ClusterAccountID", mk(func(in *K3sServerInput) { in.ClusterAccountID = "" })},
		{"empty ClusterName", mk(func(in *K3sServerInput) { in.ClusterName = "" })},
		{"empty SubnetID", mk(func(in *K3sServerInput) { in.SubnetID = "" })},
		{"empty ControlPlaneSGID", mk(func(in *K3sServerInput) { in.ControlPlaneSGID = "" })},
		{"empty NLBDNS", mk(func(in *K3sServerInput) { in.NLBDNS = "" })},
		{"empty OIDCIssuer", mk(func(in *K3sServerInput) { in.OIDCIssuer = "" })},
		{"empty OIDCPrivateKeyPEM", mk(func(in *K3sServerInput) { in.OIDCPrivateKeyPEM = "   \n " })},
		{"empty OIDCPublicKeyPEM", mk(func(in *K3sServerInput) { in.OIDCPublicKeyPEM = "   \n " })},
		{"empty GatewayURL", mk(func(in *K3sServerInput) { in.GatewayURL = "" })},
		{"empty AddonGatewayURL", mk(func(in *K3sServerInput) { in.AddonGatewayURL = "" })},
		{"empty AccessKey", mk(func(in *K3sServerInput) { in.AccessKey = "" })},
		{"empty SecretKey", mk(func(in *K3sServerInput) { in.SecretKey = "" })},
		{"empty GatewayCACert", mk(func(in *K3sServerInput) { in.GatewayCACert = "  \n" })},
		{"empty PredastoreEndpoint", mk(func(in *K3sServerInput) { in.PredastoreEndpoint = "" })},
		{"empty PredastoreAccessKey", mk(func(in *K3sServerInput) { in.PredastoreAccessKey = "" })},
		{"empty PredastoreSecretKey", mk(func(in *K3sServerInput) { in.PredastoreSecretKey = "" })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}
			_, err := LaunchK3sServerVM(context.Background(), vpc, inst, ami, tc.in)
			require.Error(t, err)
			assert.Empty(t, vpc.createCalls)
			assert.Empty(t, inst.launchCalls)
			assert.Empty(t, ami.describeCalls)
		})
	}
}

func TestValidateK3sServerInput_ProfileOnlyAccepted(t *testing.T) {
	in := validK3sInput()
	in.AccessKey = ""
	in.SecretKey = ""
	in.IamInstanceProfileArn = "arn:aws:iam::000000000000:instance-profile/spinifex-eks-server"
	require.NoError(t, validateK3sServerInput(in))
}

func TestValidateK3sServerInput_NoCredsNoProfileRejected(t *testing.T) {
	in := validK3sInput()
	in.AccessKey = ""
	in.SecretKey = ""
	require.Error(t, validateK3sServerInput(in))
}

func TestBuildK3sUserData_StaticCredsBakeKeys(t *testing.T) {
	ud := buildK3sUserData(validK3sInput())
	assert.Contains(t, ud, "EKS_ACCESS_KEY=AKIAEXAMPLE")
	assert.Contains(t, ud, "EKS_SECRET_KEY=s3cr3t-key")
}

func TestBuildK3sUserData_WritesEtcdSnapshotEnv(t *testing.T) {
	ud := buildK3sUserData(validK3sInput())
	require.Contains(t, ud, "path: "+k3sSnapshotEnvPath)
	assert.Contains(t, ud, "EKS_ACCOUNT_ID=111122223333")
	assert.Contains(t, ud, "EKS_CLUSTER_NAME=alpha")
	assert.Contains(t, ud, "SPINIFEX_PREDASTORE_ENDPOINT=https://10.15.8.1:8443")
	assert.Contains(t, ud, "SPINIFEX_PREDASTORE_AKID=AKIAPREDASTORE")
	assert.Contains(t, ud, "SPINIFEX_PREDASTORE_SECRET=pred-s3cr3t-key")
}

func TestBuildK3sUserData_ProfileModeOmitsKeys(t *testing.T) {
	in := validK3sInput()
	in.AccessKey = ""
	in.SecretKey = ""
	in.IamInstanceProfileArn = "arn:aws:iam::000000000000:instance-profile/spinifex-eks-server"
	ud := buildK3sUserData(in)
	assert.NotContains(t, ud, "EKS_ACCESS_KEY=")
	assert.NotContains(t, ud, "EKS_SECRET_KEY=")
	// Non-credential env still present.
	assert.Contains(t, ud, "EKS_CLUSTER_NAME=alpha")
}

func TestLaunchK3sServerVM_AMINotFound(t *testing.T) {
	vpc, inst := &fakeK3sVPC{}, &fakeK3sInst{}
	ami := &fakeK3sAMI{describeOut: &ec2.DescribeImagesOutput{}}

	_, err := LaunchK3sServerVM(context.Background(), vpc, inst, ami, validK3sInput())
	require.ErrorIs(t, err, ErrEKSServerAMINotFound)
	assert.Contains(t, err.Error(), "spinifex:managed-by=eks")
	assert.Empty(t, vpc.createCalls, "no ENI created when AMI lookup fails")
	assert.Empty(t, inst.launchCalls)
}

func TestLaunchK3sServerVM_AMILookupErrorPropagated(t *testing.T) {
	vpc, inst := &fakeK3sVPC{}, &fakeK3sInst{}
	ami := &fakeK3sAMI{describeErr: errors.New("DescribeImages backend down")}

	_, err := LaunchK3sServerVM(context.Background(), vpc, inst, ami, validK3sInput())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "describe eks AMI")
	assert.Empty(t, vpc.createCalls)
	assert.Empty(t, inst.launchCalls)
}

func TestLaunchK3sServerVM_ENICreateFailureNoRunInstances(t *testing.T) {
	vpc := &fakeK3sVPC{createErr: errors.New("InsufficientFreeAddressesInSubnet")}
	inst, ami := &fakeK3sInst{}, &fakeK3sAMI{}

	_, err := LaunchK3sServerVM(context.Background(), vpc, inst, ami, validK3sInput())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create K3s ENI in subnet subnet-aaa")
	assert.Empty(t, inst.launchCalls)
	assert.Empty(t, vpc.deleteCalls, "no ENI to roll back when create itself failed")
}

func TestLaunchK3sServerVM_RunInstancesFailureRollsBackENI(t *testing.T) {
	vpc := &fakeK3sVPC{}
	inst := &fakeK3sInst{launchErr: errors.New("InsufficientInstanceCapacity")}
	ami := &fakeK3sAMI{}

	_, err := LaunchK3sServerVM(context.Background(), vpc, inst, ami, validK3sInput())
	require.Error(t, err)
	require.Len(t, vpc.createCalls, 1)
	require.Len(t, vpc.deleteCalls, 1, "ENI must roll back when RunInstances fails")
	assert.Equal(t, "eni-aaa111", aws.StringValue(vpc.deleteCalls[0].NetworkInterfaceId))
}

func TestLaunchK3sServerVM_HappyPath(t *testing.T) {
	vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}

	out, err := LaunchK3sServerVM(context.Background(), vpc, inst, ami, validK3sInput())
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "i-aaa111", out.InstanceID)
	assert.Equal(t, "eni-aaa111", out.ENIID)
	assert.Equal(t, "10.0.1.42", out.ENIIP)

	require.Len(t, vpc.createCalls, 1)
	eniIn := vpc.createCalls[0]
	assert.Equal(t, "subnet-aaa", aws.StringValue(eniIn.SubnetId))
	assert.Equal(t, []string{"sg-cp-aaa"}, aws.StringValueSlice(eniIn.Groups))
	require.Len(t, eniIn.TagSpecifications, 1)
	assertEC2TaggedAsEKSControlPlane(t, eniIn.TagSpecifications[0].Tags, "alpha")

	require.Len(t, inst.launchCalls, 1)
	runIn := inst.launchCalls[0]
	assert.Equal(t, sysinstance.BootAMI, runIn.BootMode)
	assert.Equal(t, tags.ManagedByEKS, runIn.ManagedBy)
	assert.Equal(t, "ami-eks-server-001", runIn.ImageID)
	assert.Equal(t, defaultK3sServerInstanceType, runIn.InstanceType)
	assert.Equal(t, "000000000000", runIn.AccountID, "VM launches under the infra (system) account")
	assert.Equal(t, "eni-aaa111", runIn.ENIID)
	assert.Equal(t, "10.0.1.42", runIn.ENIIP)
	// The subnet must reach the launch input so the daemon can build the per-tap
	// IMDS datapath; without it the primary tap is stranded on br-imds (no patch).
	assert.Equal(t, "subnet-aaa", runIn.SubnetID)

	assert.Empty(t, vpc.deleteCalls, "no rollback on success")
}

func TestLaunchK3sServerVM_HonorsCustomInstanceType(t *testing.T) {
	vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}
	in := validK3sInput()
	in.InstanceType = "t3.large"

	_, err := LaunchK3sServerVM(context.Background(), vpc, inst, ami, in)
	require.NoError(t, err)
	require.Len(t, inst.launchCalls, 1)
	assert.Equal(t, "t3.large", inst.launchCalls[0].InstanceType)
}

func TestLaunchK3sServerVM_DefaultsToSystemInstanceType(t *testing.T) {
	// The control-plane VM defaults to a sys.* type so the daemon registers the
	// node-targeted system.LaunchInstance subject the HA spread path depends on.
	assert.Equal(t, "sys.medium", defaultK3sServerInstanceType)

	vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}
	_, err := LaunchK3sServerVM(context.Background(), vpc, inst, ami, validK3sInput())
	require.NoError(t, err)
	require.Len(t, inst.launchCalls, 1)
	assert.Equal(t, "sys.medium", inst.launchCalls[0].InstanceType)
}

func TestLaunchK3sServerVM_NoTargetNodeLaunchesLocal(t *testing.T) {
	vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}

	_, err := LaunchK3sServerVM(context.Background(), vpc, inst, ami, validK3sInput())
	require.NoError(t, err)
	require.Len(t, inst.launchNodes, 1)
	assert.Empty(t, inst.launchNodes[0], "empty TargetNodeID launches on the local node")
}

func TestLaunchK3sServerVM_TargetNodeIDRoutedToLauncher(t *testing.T) {
	vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}
	in := validK3sInput()
	in.TargetNodeID = "node-c"

	_, err := LaunchK3sServerVM(context.Background(), vpc, inst, ami, in)
	require.NoError(t, err)
	require.Len(t, inst.launchNodes, 1)
	assert.Equal(t, "node-c", inst.launchNodes[0], "TargetNodeID pins placement to a specific host")
}

func TestBuildK3sUserData_TLSSANIncludesPrivateEndpointIP(t *testing.T) {
	in := validK3sInput()
	in.EndpointIP = "203.0.113.9"
	in.PrivateEndpointIP = "10.20.0.5"

	ud := buildK3sUserData(in)
	assert.Contains(t, ud, "  - "+in.NLBDNS)
	assert.Contains(t, ud, "  - 203.0.113.9")
	assert.Contains(t, ud, "  - 10.20.0.5", "Set A private-endpoint IP must be a cert SAN")
}

func TestBuildK3sUserData_TLSSANDedupsWhenPrivateEqualsEndpoint(t *testing.T) {
	in := validK3sInput()
	in.EndpointIP = "10.20.0.5"
	in.PrivateEndpointIP = "10.20.0.5"

	ud := buildK3sUserData(in)
	assert.Equal(t, 1, strings.Count(ud, "  - 10.20.0.5"), "must not emit a duplicate SAN")
}

func TestBuildK3sUserData_AdvertiseAddressPrefersPrivateEndpoint(t *testing.T) {
	in := validK3sInput()
	in.EndpointIP = "203.0.113.9"
	in.PrivateEndpointIP = "10.20.0.5"

	ud := buildK3sUserData(in)
	assert.Contains(t, ud, "advertise-address: 10.20.0.5",
		"apiserver must advertise the worker-reachable Set A NLB front-end, not the CP node-ip")
	assert.NotContains(t, ud, "advertise-address: 203.0.113.9")
}

func TestBuildK3sUserData_AdvertiseAddressFallsBackToEndpoint(t *testing.T) {
	in := validK3sInput()
	in.EndpointIP = "203.0.113.9"
	in.PrivateEndpointIP = ""

	ud := buildK3sUserData(in)
	assert.Contains(t, ud, "advertise-address: 203.0.113.9")
}

func TestBuildK3sUserData_EgressSelectorDisabledWithKonnConfig(t *testing.T) {
	ud := buildK3sUserData(validK3sInput())
	assert.Contains(t, ud, "egress-selector-mode: disabled",
		"k3s remotedialer is off; the apiserver egress rides upstream konnectivity")
	assert.NotContains(t, ud, "egress-selector-mode: cluster")
	assert.Contains(t, ud, "egress-selector-config-file="+k3sEgressSelectorConfigPath,
		"apiserver cluster egress must point at the konnectivity UDS")
	assert.Contains(t, ud, "udsName: "+konnectivityUDSPath,
		"the EgressSelectorConfiguration file routes the cluster egress to the konn socket")
}

func TestBuildK3sUserData_KonnectivityEnv(t *testing.T) {
	in := validK3sInput()
	in.PrivateEndpointIP = "10.32.100.4"
	in.EndpointIP = "203.0.113.9"
	in.KonnServerCount = 3

	ud := buildK3sUserData(in)
	assert.Contains(t, ud, "EKS_KONNECTIVITY_HOST=10.32.100.4",
		"agents dial the private-endpoint IP to reach the konnectivity-server")
	assert.Contains(t, ud, "EKS_KONNECTIVITY_SERVER_COUNT=3",
		"agents must learn the apiserver replica count to tunnel to every replica")
	assert.Contains(t, ud, "EKS_KONNECTIVITY_SANS=10.32.100.4,203.0.113.9,"+in.NLBDNS)
}

func TestBuildK3sUserData_KonnServerCountDefaultsToOne(t *testing.T) {
	in := validK3sInput()
	in.KonnServerCount = 0
	ud := buildK3sUserData(in)
	assert.Contains(t, ud, "EKS_KONNECTIVITY_SERVER_COUNT=1",
		"a zero/unset count means a single apiserver")
}

func TestLaunchK3sServerVM_SingleControlPlaneENI(t *testing.T) {
	vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}

	_, err := LaunchK3sServerVM(context.Background(), vpc, inst, ami, validK3sInput())
	require.NoError(t, err)
	require.Len(t, vpc.createCalls, 1, "only the primary CP ENI; konnectivity needs no extra NIC")
	require.Len(t, inst.launchCalls, 1)
	assert.Empty(t, inst.launchCalls[0].ExtraENIs)
}

func TestLaunchK3sServerVM_UserDataContainsAllArtifacts(t *testing.T) {
	vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}

	_, err := LaunchK3sServerVM(context.Background(), vpc, inst, ami, validK3sInput())
	require.NoError(t, err)
	require.Len(t, inst.launchCalls, 1)

	udata := inst.launchCalls[0].UserData

	assert.True(t, strings.HasPrefix(udata, "#cloud-config\n"))
	assert.Contains(t, udata, "write_files:")

	// Static resolver via bootcmd (NOT write_files — /etc/resolv.conf is a
	// dangling symlink on the AMI; write_files would follow it, fail, and abort
	// the whole block). Without it containerd image pulls cannot resolve.
	assert.Contains(t, udata, "bootcmd:")
	assert.Contains(t, udata, "rm -f "+k3sResolvConfPath)
	assert.Contains(t, udata, "nameserver 1.1.1.1")
	// Resolver must NOT be a write_files entry (that path is what broke seeding).
	assert.NotContains(t, udata, "path: "+k3sResolvConfPath)

	assert.Contains(t, udata, "path: "+k3sFirstBootEnvPath)
	// The eks-node-role first-boot selector keys off this to start control-plane
	// services; without it the unified AMI boots into no role.
	assert.Contains(t, udata, "SPINIFEX_K3S_ROLE=server")
	assert.Contains(t, udata, "EKS_GATEWAY_URL=https://10.15.8.1:9999")
	assert.Contains(t, udata, "EKS_ADDON_GATEWAY_URL=https://192.168.1.33:9999")
	assert.Contains(t, udata, "EKS_GATEWAY_CA="+k3sGatewayCAPath)
	assert.Contains(t, udata, "EKS_ACCESS_KEY=AKIAEXAMPLE")
	assert.Contains(t, udata, "EKS_SECRET_KEY=s3cr3t-key")
	assert.Contains(t, udata, "EKS_REGION=us-east-1")
	assert.Contains(t, udata, "EKS_VPC_ID=vpc-aaa")
	// EKS_ACCOUNT_ID is the cluster-OWNER account (ClusterAccountID), not the
	// infra account the VM is launched under — the on-VM agents namespace their
	// bootstrap publish / state report / add-on fetch by it, so it must reach the
	// customer cluster, not the system account that owns the CP VPC + VM.
	assert.Contains(t, udata, "EKS_ACCOUNT_ID=111122223333")
	assert.NotContains(t, udata, "EKS_ACCOUNT_ID=000000000000")
	assert.Contains(t, udata, "EKS_CLUSTER_NAME=alpha")
	assert.Contains(t, udata, "EKS_NLB_ENDPOINT=https://eks-alpha-lb-001.us-east-1.elb.spinifex.local:443")
	assert.Contains(t, udata, "EKS_OIDC_ISSUER=https://oidc.spinifex.local/clusters/111122223333/alpha")

	assert.Contains(t, udata, "path: "+k3sOIDCSigningKeyPath)
	assert.Contains(t, udata, "permissions: '0600'")
	assert.Contains(t, udata, "-----BEGIN PRIVATE KEY-----")
	assert.Contains(t, udata, "FAKEKEY")

	assert.Contains(t, udata, "path: "+k3sOIDCPublicKeyPath)
	assert.Contains(t, udata, "-----BEGIN PUBLIC KEY-----")
	assert.Contains(t, udata, "FAKEPUB")

	assert.Contains(t, udata, "path: "+k3sConfigPath)
	// The control plane is tainted so user workloads never schedule on it (EKS
	// parity, and it keeps image pulls off the etcd disk).
	assert.Contains(t, udata, "node-taint:")
	assert.Contains(t, udata, "  - CriticalAddonsOnly=true:NoExecute")
	// AWS parity: K3s' bundled traefik + servicelb are always disabled; Service
	// type=LoadBalancer / Ingress are the AWS LB Controller's job. local-storage is
	// disabled too so its local-path provisioner does not add a second default
	// StorageClass racing the EBS CSI one.
	assert.Contains(t, udata, "disable:")
	assert.Contains(t, udata, "  - traefik")
	assert.Contains(t, udata, "  - servicelb")
	assert.Contains(t, udata, "  - local-storage")
	assert.NotContains(t, udata, "EKS_DEFER_TRAEFIK")
	assert.Contains(t, udata, "tls-san:")
	assert.Contains(t, udata, "  - eks-alpha-lb-001.us-east-1.elb.spinifex.local")
	// service-account-key-file must point at the PUBLIC key; the signing key
	// (private) is a separate file — pointing key-file at the private key
	// crash-loops kube-apiserver.
	assert.Contains(t, udata, "service-account-key-file="+k3sOIDCPublicKeyPath)
	assert.Contains(t, udata, "service-account-signing-key-file="+k3sOIDCSigningKeyPath)
	assert.NotContains(t, udata, "service-account-key-file="+k3sOIDCSigningKeyPath)
	assert.Contains(t, udata, "service-account-issuer=https://oidc.spinifex.local/clusters/111122223333/alpha")
	assert.Contains(t, udata, "api-audiences=sts.amazonaws.com")
	// IAM bearer-token auth: the generated config overrides the AMI skel, so the
	// token-webhook arg MUST be emitted here or `aws eks get-token` 401s with the
	// webhook never invoked.
	assert.Contains(t, udata, "authentication-token-webhook-config-file="+k3sTokenWebhookKubeconfigPath)

	assert.Contains(t, udata, "path: "+k3sGatewayCAPath)
	assert.Contains(t, udata, "-----BEGIN CERTIFICATE-----")
	assert.Contains(t, udata, "FAKECA")

	// IMDS is served at the host tap, so no in-guest on-link route is emitted —
	// no local.d route script and no runcmd block to enable/run it.
	assert.NotContains(t, udata, "169.254.169.254")
	assert.NotContains(t, udata, "/etc/local.d/imds-onlink-route.start")
	assert.NotContains(t, udata, "runcmd:")

	// Exactly one write_files key — a second would collide and silently
	// drop a block under yaml.safe_load (last key wins).
	assert.Equal(t, 1, strings.Count(udata, "\nwrite_files:"))
}

func TestLaunchK3sServerVM_UsesEmbeddedEtcd(t *testing.T) {
	vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}

	_, err := LaunchK3sServerVM(context.Background(), vpc, inst, ami, validK3sInput())
	require.NoError(t, err)
	require.Len(t, inst.launchCalls, 1)

	udata := inst.launchCalls[0].UserData
	// cluster-init selects the embedded etcd datastore (not SQLite/kine);
	// etcd-expose-metrics surfaces wal_fsync/backend_commit on 127.0.0.1:2381.
	assert.Contains(t, udata, "cluster-init: true")
	assert.Contains(t, udata, "etcd-expose-metrics: true")
}

func TestLaunchK3sServerVM_FirstServerTokenNoServerURL(t *testing.T) {
	vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}

	in := validK3sInput()
	in.JoinToken = "sharedtok123"
	_, err := LaunchK3sServerVM(context.Background(), vpc, inst, ami, in)
	require.NoError(t, err)
	require.Len(t, inst.launchCalls, 1)

	udata := inst.launchCalls[0].UserData
	// First server: cluster-inits the datastore, carries the shared token so
	// servers 2..N and workers join, and boots the full server role.
	assert.Contains(t, udata, "cluster-init: true")
	assert.Contains(t, udata, "token: sharedtok123")
	assert.NotContains(t, udata, "server: https://")
	assert.Contains(t, udata, "SPINIFEX_K3S_ROLE=server\n")
}

func TestLaunchK3sServerVM_JoinServerRendersServerAndJoinRole(t *testing.T) {
	vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}

	in := validK3sInput()
	in.JoinToken = "sharedtok123"
	in.ServerURL = "https://10.0.1.7:6443"
	_, err := LaunchK3sServerVM(context.Background(), vpc, inst, ami, in)
	require.NoError(t, err)
	require.Len(t, inst.launchCalls, 1)

	udata := inst.launchCalls[0].UserData
	// Join server: registers at the first server's endpoint with the shared
	// token, WITHOUT cluster-init, and boots the join role (no bootstrap publish).
	assert.Contains(t, udata, "server: https://10.0.1.7:6443")
	assert.Contains(t, udata, "token: sharedtok123")
	assert.NotContains(t, udata, "cluster-init: true")
	assert.Contains(t, udata, "SPINIFEX_K3S_ROLE=server-join")
	assert.NotContains(t, udata, "SPINIFEX_K3S_ROLE=server\n")
}

func TestLaunchK3sServerVM_JoinServerRequiresToken(t *testing.T) {
	vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}

	in := validK3sInput()
	in.JoinToken = ""
	in.ServerURL = "https://10.0.1.7:6443" // ServerURL set, token cleared
	_, err := LaunchK3sServerVM(context.Background(), vpc, inst, ami, in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JoinToken")
	assert.Empty(t, inst.launchCalls)
}

func TestGenerateK3sClusterToken_UniqueHex(t *testing.T) {
	a, err := GenerateK3sClusterToken()
	require.NoError(t, err)
	b, err := GenerateK3sClusterToken()
	require.NoError(t, err)
	assert.Len(t, a, 64) // 32 bytes hex
	assert.NotEqual(t, a, b)
}

func TestK3sServerJoinURL(t *testing.T) {
	assert.Equal(t, "https://10.0.1.7:6443", k3sServerJoinURL("10.0.1.7"))
}

func TestLaunchK3sServerVM_RunInstancesEmptyReservationRollsBack(t *testing.T) {
	vpc, ami := &fakeK3sVPC{}, &fakeK3sAMI{}
	inst := &fakeK3sInst{launchOut: &sysinstance.SystemInstanceOutput{}}

	_, err := LaunchK3sServerVM(context.Background(), vpc, inst, ami, validK3sInput())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LaunchSystemInstance returned no instance")
	require.Len(t, vpc.deleteCalls, 1, "must roll back ENI when reservation empty")
}

func TestLaunchK3sServerVM_AMIFilterShape(t *testing.T) {
	vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}

	_, err := LaunchK3sServerVM(context.Background(), vpc, inst, ami, validK3sInput())
	require.NoError(t, err)
	require.Len(t, ami.describeCalls, 1)
	filters := ami.describeCalls[0].Filters
	require.Len(t, filters, 1)
	assert.Equal(t, "tag:"+tags.ManagedByKey, aws.StringValue(filters[0].Name))
	assert.Equal(t, []string{tags.ManagedByEKS}, aws.StringValueSlice(filters[0].Values))
}

func TestTerminateK3sServerVM_BothNoopOnEmpty(t *testing.T) {
	vpc, inst := &fakeK3sVPC{}, &fakeK3sInst{}

	require.NoError(t, TerminateK3sServerVM(context.Background(), vpc, inst, "111122223333", "", ""))
	assert.Empty(t, inst.terminateCalls)
	assert.Empty(t, vpc.deleteCalls)
}

func TestTerminateK3sServerVM_TerminatesInstanceAndDeletesENI(t *testing.T) {
	vpc, inst := &fakeK3sVPC{}, &fakeK3sInst{}

	require.NoError(t, TerminateK3sServerVM(context.Background(), vpc, inst, "111122223333", "i-aaa111", "eni-aaa111"))
	require.Len(t, inst.terminateCalls, 1)
	assert.Equal(t, "i-aaa111", inst.terminateCalls[0])
	// Teardown owns its CP ENI: detach first to clear any stale attachment, then
	// delete. The detach precedes the delete so the delete passes the in-use guard.
	require.Equal(t, []string{"eni-aaa111"}, vpc.detachCalls)
	require.Len(t, vpc.deleteCalls, 1)
	assert.Equal(t, "eni-aaa111", aws.StringValue(vpc.deleteCalls[0].NetworkInterfaceId))
}

func TestTerminateK3sServerVM_InstanceErrorReturnedENIStillDeleted(t *testing.T) {
	vpc := &fakeK3sVPC{}
	inst := &fakeK3sInst{terminateErr: errors.New("IncorrectInstanceState")}

	err := TerminateK3sServerVM(context.Background(), vpc, inst, "111122223333", "i-aaa111", "eni-aaa111")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "terminate instance i-aaa111")
	require.Len(t, vpc.deleteCalls, 1, "ENI delete should still run after instance terminate fails")
}

func TestTerminateK3sServerVM_ENINotFoundIsIdempotent(t *testing.T) {
	// A retried delete-cluster after the instance-terminate cascade already
	// removed the ENI: DeleteNetworkInterface returns NotFound. This must be
	// treated as success so the SG + KV sweep downstream is not blocked.
	vpc := &fakeK3sVPC{deleteErr: errors.New(awserrors.ErrorInvalidNetworkInterfaceIDNotFound)}
	inst := &fakeK3sInst{}

	err := TerminateK3sServerVM(context.Background(), vpc, inst, "111122223333", "i-aaa111", "eni-aaa111")
	require.NoError(t, err, "ENI already gone must be idempotent success, not a blocking error")
	require.Len(t, vpc.deleteCalls, 1)
}

func TestTerminateK3sServerVM_ENIInUseDetachedThenDeletes(t *testing.T) {
	// The ENI record still shows the attachment (VM gone but
	// fields never cleared), so a plain force=false delete returns InUse forever
	// and wedges EKSDeletingReaper. Teardown owns the ENI: detach clears the
	// stale attachment, then the delete succeeds — no retry loop.
	vpc := &fakeK3sVPC{inUseUntilDetached: map[string]bool{"eni-aaa111": true}}
	inst := &fakeK3sInst{}

	err := TerminateK3sServerVM(context.Background(), vpc, inst, "111122223333", "i-aaa111", "eni-aaa111")
	require.NoError(t, err, "detach-then-delete must clear the stale attachment, not surface InUse")
	require.Equal(t, []string{"eni-aaa111"}, vpc.detachCalls, "must detach before deleting")
	require.Len(t, vpc.deleteCalls, 1, "single delete after detach; no InUse-retry loop")
}

func TestTerminateK3sServerVM_ENIDeleteErrorSurfaces(t *testing.T) {
	// A real delete failure (not NotFound) must surface so the teardown backstop
	// retries rather than silently stranding the ENI.
	vpc := &fakeK3sVPC{deleteErr: errors.New(awserrors.ErrorInvalidNetworkInterfaceInUse)}
	inst := &fakeK3sInst{}

	err := TerminateK3sServerVM(context.Background(), vpc, inst, "111122223333", "i-aaa111", "eni-aaa111")
	require.Error(t, err, "a non-NotFound delete error must surface")
	assert.Contains(t, err.Error(), "delete ENI eni-aaa111")
}

func TestTerminateK3sServerVM_InstanceAlreadyGoneIsIdempotent(t *testing.T) {
	// On a reconciler retry the VM already drained, so TerminateSystemInstance
	// returns ErrSystemInstanceNotFound. This must not block teardown; the ENI
	// delete still runs.
	vpc := &fakeK3sVPC{}
	inst := &fakeK3sInst{terminateErr: fmt.Errorf("%w: i-aaa111", sysinstance.ErrSystemInstanceNotFound)}

	err := TerminateK3sServerVM(context.Background(), vpc, inst, "111122223333", "i-aaa111", "eni-aaa111")
	require.NoError(t, err, "instance already gone must be idempotent success")
	require.Len(t, vpc.deleteCalls, 1, "ENI delete still runs after a tolerated instance-gone")
}

func assertEC2TaggedAsEKSControlPlane(t *testing.T, ec2Tags []*ec2.Tag, clusterName string) {
	t.Helper()
	got := map[string]string{}
	for _, tg := range ec2Tags {
		if tg == nil || tg.Key == nil || tg.Value == nil {
			continue
		}
		got[*tg.Key] = *tg.Value
	}
	assert.Equal(t, tags.ManagedByEKS, got[tags.ManagedByKey])
	assert.Equal(t, clusterName, got[clusterEKSClusterTagKey])
	assert.Equal(t, clusterEKSRoleControlPlane, got[clusterEKSRoleTagKey])
}
