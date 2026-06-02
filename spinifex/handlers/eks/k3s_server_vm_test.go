package handlers_eks

import (
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/handlers/sysinstance"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeK3sVPC struct {
	createCalls []*ec2.CreateNetworkInterfaceInput
	deleteCalls []*ec2.DeleteNetworkInterfaceInput

	createOut *ec2.CreateNetworkInterfaceOutput
	createErr error
	deleteErr error
}

var _ k3sVPCProvisioner = (*fakeK3sVPC)(nil)

func (f *fakeK3sVPC) CreateNetworkInterface(input *ec2.CreateNetworkInterfaceInput, _ string) (*ec2.CreateNetworkInterfaceOutput, error) {
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

func (f *fakeK3sVPC) DeleteNetworkInterface(input *ec2.DeleteNetworkInterfaceInput, _ string) (*ec2.DeleteNetworkInterfaceOutput, error) {
	f.deleteCalls = append(f.deleteCalls, input)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &ec2.DeleteNetworkInterfaceOutput{}, nil
}

type fakeK3sInst struct {
	launchCalls    []*sysinstance.SystemInstanceInput
	terminateCalls []string

	launchOut    *sysinstance.SystemInstanceOutput
	launchErr    error
	terminateErr error
}

var _ k3sInstanceLauncher = (*fakeK3sInst)(nil)

func (f *fakeK3sInst) LaunchSystemInstance(input *sysinstance.SystemInstanceInput) (*sysinstance.SystemInstanceOutput, error) {
	f.launchCalls = append(f.launchCalls, input)
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
	describeCalls []*ec2.DescribeImagesInput

	describeOut *ec2.DescribeImagesOutput
	describeErr error
}

var _ k3sAMIResolver = (*fakeK3sAMI)(nil)

func (f *fakeK3sAMI) DescribeImages(input *ec2.DescribeImagesInput, _ string) (*ec2.DescribeImagesOutput, error) {
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
		AccountID:         "111122223333",
		ClusterName:       "alpha",
		Region:            "us-east-1",
		SubnetID:          "subnet-aaa",
		ControlPlaneSGID:  "sg-cp-aaa",
		NLBDNS:            "eks-alpha-lb-001.us-east-1.elb.spinifex.local",
		OIDCIssuer:        "https://oidc.spinifex.local/clusters/111122223333/alpha",
		OIDCPrivateKeyPEM: "-----BEGIN EC PRIVATE KEY-----\nFAKEKEY\n-----END EC PRIVATE KEY-----\n",
		NATSURL:           "nats://localhost:4222",
		NATSToken:         "s3cr3t-token",
		NATSCACert:        "-----BEGIN CERTIFICATE-----\nFAKECA\n-----END CERTIFICATE-----\n",
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
		{"empty ClusterName", mk(func(in *K3sServerInput) { in.ClusterName = "" })},
		{"empty SubnetID", mk(func(in *K3sServerInput) { in.SubnetID = "" })},
		{"empty ControlPlaneSGID", mk(func(in *K3sServerInput) { in.ControlPlaneSGID = "" })},
		{"empty NLBDNS", mk(func(in *K3sServerInput) { in.NLBDNS = "" })},
		{"empty OIDCIssuer", mk(func(in *K3sServerInput) { in.OIDCIssuer = "" })},
		{"empty OIDCPrivateKeyPEM", mk(func(in *K3sServerInput) { in.OIDCPrivateKeyPEM = "   \n " })},
		{"empty NATSURL", mk(func(in *K3sServerInput) { in.NATSURL = "" })},
		{"empty NATSToken", mk(func(in *K3sServerInput) { in.NATSToken = "" })},
		{"empty NATSCACert", mk(func(in *K3sServerInput) { in.NATSCACert = "  \n" })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}
			_, err := LaunchK3sServerVM(vpc, inst, ami, tc.in)
			require.Error(t, err)
			assert.Empty(t, vpc.createCalls)
			assert.Empty(t, inst.launchCalls)
			assert.Empty(t, ami.describeCalls)
		})
	}
}

func TestLaunchK3sServerVM_AMINotFound(t *testing.T) {
	vpc, inst := &fakeK3sVPC{}, &fakeK3sInst{}
	ami := &fakeK3sAMI{describeOut: &ec2.DescribeImagesOutput{}}

	_, err := LaunchK3sServerVM(vpc, inst, ami, validK3sInput())
	require.ErrorIs(t, err, ErrEKSServerAMINotFound)
	assert.Contains(t, err.Error(), "spinifex:managed-by=eks")
	assert.Empty(t, vpc.createCalls, "no ENI created when AMI lookup fails")
	assert.Empty(t, inst.launchCalls)
}

func TestLaunchK3sServerVM_AMILookupErrorPropagated(t *testing.T) {
	vpc, inst := &fakeK3sVPC{}, &fakeK3sInst{}
	ami := &fakeK3sAMI{describeErr: errors.New("DescribeImages backend down")}

	_, err := LaunchK3sServerVM(vpc, inst, ami, validK3sInput())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "describe eks AMI")
	assert.Empty(t, vpc.createCalls)
	assert.Empty(t, inst.launchCalls)
}

func TestLaunchK3sServerVM_ENICreateFailureNoRunInstances(t *testing.T) {
	vpc := &fakeK3sVPC{createErr: errors.New("InsufficientFreeAddressesInSubnet")}
	inst, ami := &fakeK3sInst{}, &fakeK3sAMI{}

	_, err := LaunchK3sServerVM(vpc, inst, ami, validK3sInput())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create K3s ENI in subnet subnet-aaa")
	assert.Empty(t, inst.launchCalls)
	assert.Empty(t, vpc.deleteCalls, "no ENI to roll back when create itself failed")
}

func TestLaunchK3sServerVM_RunInstancesFailureRollsBackENI(t *testing.T) {
	vpc := &fakeK3sVPC{}
	inst := &fakeK3sInst{launchErr: errors.New("InsufficientInstanceCapacity")}
	ami := &fakeK3sAMI{}

	_, err := LaunchK3sServerVM(vpc, inst, ami, validK3sInput())
	require.Error(t, err)
	require.Len(t, vpc.createCalls, 1)
	require.Len(t, vpc.deleteCalls, 1, "ENI must roll back when RunInstances fails")
	assert.Equal(t, "eni-aaa111", aws.StringValue(vpc.deleteCalls[0].NetworkInterfaceId))
}

func TestLaunchK3sServerVM_HappyPath(t *testing.T) {
	vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}

	out, err := LaunchK3sServerVM(vpc, inst, ami, validK3sInput())
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
	assert.Equal(t, "111122223333", runIn.AccountID)
	assert.Equal(t, "eni-aaa111", runIn.ENIID)
	assert.Equal(t, "10.0.1.42", runIn.ENIIP)

	assert.Empty(t, vpc.deleteCalls, "no rollback on success")
}

func TestLaunchK3sServerVM_HonorsCustomInstanceType(t *testing.T) {
	vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}
	in := validK3sInput()
	in.InstanceType = "t3.large"

	_, err := LaunchK3sServerVM(vpc, inst, ami, in)
	require.NoError(t, err)
	require.Len(t, inst.launchCalls, 1)
	assert.Equal(t, "t3.large", inst.launchCalls[0].InstanceType)
}

func TestLaunchK3sServerVM_UserDataContainsAllArtifacts(t *testing.T) {
	vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}

	_, err := LaunchK3sServerVM(vpc, inst, ami, validK3sInput())
	require.NoError(t, err)
	require.Len(t, inst.launchCalls, 1)

	udata := inst.launchCalls[0].UserData

	assert.True(t, strings.HasPrefix(udata, "#cloud-config\n"))
	assert.Contains(t, udata, "write_files:")

	assert.Contains(t, udata, "path: "+k3sFirstBootEnvPath)
	assert.Contains(t, udata, "SPINIFEX_NATS_URL=nats://localhost:4222")
	assert.Contains(t, udata, "SPINIFEX_NATS_TOKEN=s3cr3t-token")
	assert.Contains(t, udata, "SPINIFEX_NATS_CA="+k3sNATSCAPath)
	assert.Contains(t, udata, "EKS_ACCOUNT_ID=111122223333")
	assert.Contains(t, udata, "EKS_CLUSTER_NAME=alpha")
	assert.Contains(t, udata, "EKS_NLB_ENDPOINT=https://eks-alpha-lb-001.us-east-1.elb.spinifex.local:443")
	assert.Contains(t, udata, "EKS_OIDC_ISSUER=https://oidc.spinifex.local/clusters/111122223333/alpha")

	assert.Contains(t, udata, "path: "+k3sOIDCSigningKeyPath)
	assert.Contains(t, udata, "permissions: '0600'")
	assert.Contains(t, udata, "-----BEGIN EC PRIVATE KEY-----")
	assert.Contains(t, udata, "FAKEKEY")

	assert.Contains(t, udata, "path: "+k3sConfigPath)
	assert.Contains(t, udata, "tls-san:")
	assert.Contains(t, udata, "  - eks-alpha-lb-001.us-east-1.elb.spinifex.local")
	assert.Contains(t, udata, "service-account-key-file="+k3sOIDCSigningKeyPath)
	assert.Contains(t, udata, "service-account-issuer=https://oidc.spinifex.local/clusters/111122223333/alpha")
	assert.Contains(t, udata, "api-audiences=sts.amazonaws.com")

	assert.Contains(t, udata, "path: "+k3sNATSCAPath)
	assert.Contains(t, udata, "-----BEGIN CERTIFICATE-----")
	assert.Contains(t, udata, "FAKECA")
}

func TestLaunchK3sServerVM_RunInstancesEmptyReservationRollsBack(t *testing.T) {
	vpc, ami := &fakeK3sVPC{}, &fakeK3sAMI{}
	inst := &fakeK3sInst{launchOut: &sysinstance.SystemInstanceOutput{}}

	_, err := LaunchK3sServerVM(vpc, inst, ami, validK3sInput())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "LaunchSystemInstance returned no instance")
	require.Len(t, vpc.deleteCalls, 1, "must roll back ENI when reservation empty")
}

func TestLaunchK3sServerVM_AMIFilterShape(t *testing.T) {
	vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}

	_, err := LaunchK3sServerVM(vpc, inst, ami, validK3sInput())
	require.NoError(t, err)
	require.Len(t, ami.describeCalls, 1)
	filters := ami.describeCalls[0].Filters
	require.Len(t, filters, 1)
	assert.Equal(t, "tag:"+tags.ManagedByKey, aws.StringValue(filters[0].Name))
	assert.Equal(t, []string{tags.ManagedByEKS}, aws.StringValueSlice(filters[0].Values))
}

func TestTerminateK3sServerVM_BothNoopOnEmpty(t *testing.T) {
	vpc, inst := &fakeK3sVPC{}, &fakeK3sInst{}

	require.NoError(t, TerminateK3sServerVM(vpc, inst, "111122223333", "", ""))
	assert.Empty(t, inst.terminateCalls)
	assert.Empty(t, vpc.deleteCalls)
}

func TestTerminateK3sServerVM_TerminatesInstanceAndDeletesENI(t *testing.T) {
	vpc, inst := &fakeK3sVPC{}, &fakeK3sInst{}

	require.NoError(t, TerminateK3sServerVM(vpc, inst, "111122223333", "i-aaa111", "eni-aaa111"))
	require.Len(t, inst.terminateCalls, 1)
	assert.Equal(t, "i-aaa111", inst.terminateCalls[0])
	require.Len(t, vpc.deleteCalls, 1)
	assert.Equal(t, "eni-aaa111", aws.StringValue(vpc.deleteCalls[0].NetworkInterfaceId))
}

func TestTerminateK3sServerVM_InstanceErrorReturnedENIStillDeleted(t *testing.T) {
	vpc := &fakeK3sVPC{}
	inst := &fakeK3sInst{terminateErr: errors.New("IncorrectInstanceState")}

	err := TerminateK3sServerVM(vpc, inst, "111122223333", "i-aaa111", "eni-aaa111")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "terminate instance i-aaa111")
	require.Len(t, vpc.deleteCalls, 1, "ENI delete should still run after instance terminate fails")
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
