package handlers_eks

import (
	"errors"
	"fmt"
	"strings"
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
		OIDCPrivateKeyPEM: "-----BEGIN PRIVATE KEY-----\nFAKEKEY\n-----END PRIVATE KEY-----\n",
		OIDCPublicKeyPEM:  "-----BEGIN PUBLIC KEY-----\nFAKEPUB\n-----END PUBLIC KEY-----\n",
		GatewayURL:        "https://10.15.8.1:9999",
		AccessKey:         "AKIAEXAMPLE",
		SecretKey:         "s3cr3t-key",
		GatewayCACert:     "-----BEGIN CERTIFICATE-----\nFAKECA\n-----END CERTIFICATE-----\n",
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
		{"empty OIDCPublicKeyPEM", mk(func(in *K3sServerInput) { in.OIDCPublicKeyPEM = "   \n " })},
		{"empty GatewayURL", mk(func(in *K3sServerInput) { in.GatewayURL = "" })},
		{"empty AccessKey", mk(func(in *K3sServerInput) { in.AccessKey = "" })},
		{"empty SecretKey", mk(func(in *K3sServerInput) { in.SecretKey = "" })},
		{"empty GatewayCACert", mk(func(in *K3sServerInput) { in.GatewayCACert = "  \n" })},
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
	assert.Contains(t, udata, "EKS_GATEWAY_CA="+k3sGatewayCAPath)
	assert.Contains(t, udata, "EKS_ACCESS_KEY=AKIAEXAMPLE")
	assert.Contains(t, udata, "EKS_SECRET_KEY=s3cr3t-key")
	assert.Contains(t, udata, "EKS_REGION=us-east-1")
	assert.Contains(t, udata, "EKS_ACCOUNT_ID=111122223333")
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
	// Parity default (BuiltinIngress=false): K3s' bundled traefik + servicelb are
	// disabled; Service type=LoadBalancer / Ingress are the AWS LB Controller's job.
	// With built-in ingress off there is nothing to defer, so EKS_DEFER_TRAEFIK=0.
	assert.Contains(t, udata, "disable:")
	assert.Contains(t, udata, "  - traefik")
	assert.Contains(t, udata, "  - servicelb")
	assert.Contains(t, udata, "EKS_DEFER_TRAEFIK=0")
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

	// IMDS on-link route delivered out-of-band (Alpine's eni renderer can't emit
	// the gateway-less route in network-config). It rides this payload's own
	// write_files/runcmd, enabled via the OpenRC local service.
	assert.Contains(t, udata, "path: /etc/local.d/imds-onlink-route.start")
	assert.Contains(t, udata, "ip route show default")
	assert.Contains(t, udata, `ip route replace 169.254.169.254/32 dev "$dev" scope link`)
	assert.Contains(t, udata, "runcmd:")
	assert.Contains(t, udata, "[ rc-update, add, local, default ]")

	// Exactly one write_files / runcmd key — a second would collide and silently
	// drop a block under yaml.safe_load (last key wins).
	assert.Equal(t, 1, strings.Count(udata, "\nwrite_files:"))
	assert.Equal(t, 1, strings.Count(udata, "\nruncmd:"))
}

func TestLaunchK3sServerVM_BuiltinIngressOptInDefersTraefik(t *testing.T) {
	vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}

	in := validK3sInput()
	in.BuiltinIngress = true
	_, err := LaunchK3sServerVM(vpc, inst, ami, in)
	require.NoError(t, err)
	require.Len(t, inst.launchCalls, 1)

	udata := inst.launchCalls[0].UserData
	// Opted in: traefik stays ENABLED in config so k3s writes traefik.yaml, but
	// it is deferred — k3s.initd stages a .skip marker and the state-reporter
	// removes it once the apiserver is stable, gated by EKS_DEFER_TRAEFIK=1.
	// Disabling traefik would leave no manifest to un-skip, so the config must
	// carry no disable block at all. servicelb is lazy and likewise enabled.
	assert.NotContains(t, udata, "disable:")
	assert.NotContains(t, udata, "  - traefik")
	assert.NotContains(t, udata, "  - servicelb")
	assert.Contains(t, udata, "EKS_DEFER_TRAEFIK=1")
}

func TestLaunchK3sServerVM_UsesEmbeddedEtcd(t *testing.T) {
	vpc, inst, ami := &fakeK3sVPC{}, &fakeK3sInst{}, &fakeK3sAMI{}

	_, err := LaunchK3sServerVM(vpc, inst, ami, validK3sInput())
	require.NoError(t, err)
	require.Len(t, inst.launchCalls, 1)

	udata := inst.launchCalls[0].UserData
	// cluster-init selects the embedded etcd datastore (not SQLite/kine);
	// etcd-expose-metrics surfaces wal_fsync/backend_commit on 127.0.0.1:2381.
	assert.Contains(t, udata, "cluster-init: true")
	assert.Contains(t, udata, "etcd-expose-metrics: true")
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

func TestTerminateK3sServerVM_ENINotFoundIsIdempotent(t *testing.T) {
	// A retried delete-cluster after the instance-terminate cascade already
	// removed the ENI: DeleteNetworkInterface returns NotFound. This must be
	// treated as success so the SG + KV sweep downstream is not blocked.
	vpc := &fakeK3sVPC{deleteErr: errors.New(awserrors.ErrorInvalidNetworkInterfaceIDNotFound)}
	inst := &fakeK3sInst{}

	err := TerminateK3sServerVM(vpc, inst, "111122223333", "i-aaa111", "eni-aaa111")
	require.NoError(t, err, "ENI already gone must be idempotent success, not a blocking error")
	require.Len(t, vpc.deleteCalls, 1)
}

func TestTerminateK3sServerVM_ENIInUseIsRetryable(t *testing.T) {
	// The VM is still terminating async and holds the ENI. InUse must surface
	// as an error so the cluster stays DELETING and the reconciler retries.
	vpc := &fakeK3sVPC{deleteErr: errors.New(awserrors.ErrorInvalidNetworkInterfaceInUse)}
	inst := &fakeK3sInst{}

	err := TerminateK3sServerVM(vpc, inst, "111122223333", "i-aaa111", "eni-aaa111")
	require.Error(t, err, "ENI still in use must be retryable, not swallowed")
	assert.Contains(t, err.Error(), "delete ENI eni-aaa111")
}

func TestTerminateK3sServerVM_InstanceAlreadyGoneIsIdempotent(t *testing.T) {
	// On a reconciler retry the VM already drained, so TerminateSystemInstance
	// returns ErrSystemInstanceNotFound. This must not block teardown; the ENI
	// delete still runs.
	vpc := &fakeK3sVPC{}
	inst := &fakeK3sInst{terminateErr: fmt.Errorf("%w: i-aaa111", sysinstance.ErrSystemInstanceNotFound)}

	err := TerminateK3sServerVM(vpc, inst, "111122223333", "i-aaa111", "eni-aaa111")
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
