package handlers_elbv2

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// extractAgentEnv parses the cloud-config user-data into the env-var map that
// cloud-init will write to /etc/conf.d/lb-agent. We don't pull in a YAML
// dependency just for tests — the format is stable enough that line-based
// parsing catches the things substring matching misses (misplaced keys,
// broken indentation, content that escaped the agent-conf block).
//
// Returns the parsed env map plus the `runcmd:` section's body (raw lines)
// so callers can assert on the launch command shape.
func extractAgentEnv(t *testing.T, ud string) (envs map[string]string, runcmd []string) {
	t.Helper()
	require.True(t, strings.HasPrefix(ud, "#cloud-config\n"),
		"user-data must start with #cloud-config directive")

	envs = map[string]string{}
	const agentPathMarker = "  - path: /etc/conf.d/lb-agent"
	const contentMarker = "    content: |"
	const envIndent = "      " // 6 spaces under `content: |`

	lines := strings.Split(ud, "\n")
	inAgentEnv := false
	inRuncmd := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		switch {
		case line == agentPathMarker:
			require.Less(t, i+1, len(lines), "agent path entry truncated")
			require.Equal(t, contentMarker, lines[i+1],
				"agent path must be followed by `content: |` block")
			inAgentEnv = true
			i++ // skip the content marker
		case inAgentEnv:
			if !strings.HasPrefix(line, envIndent) {
				inAgentEnv = false
				i-- // re-process this line in case it starts another section
				continue
			}
			kv := strings.TrimPrefix(line, envIndent)
			k, v, ok := strings.Cut(kv, "=")
			require.True(t, ok, "agent env line missing '=': %q", line)
			envs[k] = v
		case line == "runcmd:":
			inRuncmd = true
		case inRuncmd:
			if !strings.HasPrefix(line, "  ") {
				inRuncmd = false
				continue
			}
			runcmd = append(runcmd, line)
		}
	}
	return envs, runcmd
}

func TestBuildLBArn(t *testing.T) {
	arn := buildLBArn("us-east-1", "123456789012", "my-alb", "50dc6c495c0c9188", LoadBalancerTypeApplication)
	assert.Equal(t, "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/my-alb/50dc6c495c0c9188", arn)
}

func TestBuildLBArn_Network(t *testing.T) {
	arn := buildLBArn("us-east-1", "123456789012", "my-nlb", "50dc6c495c0c9188", LoadBalancerTypeNetwork)
	assert.Equal(t, "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/net/my-nlb/50dc6c495c0c9188", arn)
}

func TestBuildTGArn(t *testing.T) {
	arn := buildTGArn("us-west-2", "111222333444", "my-tg", "deadbeef")
	assert.Equal(t, "arn:aws:elasticloadbalancing:us-west-2:111222333444:targetgroup/my-tg/deadbeef", arn)
}

func TestBuildListenerArn(t *testing.T) {
	arn := buildListenerArn("eu-west-1", "999888777666", "my-alb", "lbid123", "listener456", LoadBalancerTypeApplication)
	assert.Equal(t, "arn:aws:elasticloadbalancing:eu-west-1:999888777666:listener/app/my-alb/lbid123/listener456", arn)
}

func TestBuildListenerArn_NLB(t *testing.T) {
	arn := buildListenerArn("eu-west-1", "999888777666", "my-nlb", "lbid123", "listener456", LoadBalancerTypeNetwork)
	assert.Equal(t, "arn:aws:elasticloadbalancing:eu-west-1:999888777666:listener/net/my-nlb/lbid123/listener456", arn)
}

// TestLbVMUserData_Structural parses the generated cloud-config and asserts
// the structure: agent env vars land in /etc/conf.d/lb-agent, runcmd starts
// lb-agent via OpenRC (not the bare binary), the CA cert section — owned by
// the instance service's cloud-init template — is absent, and no bootcmd is
// emitted in the single-node default.
func TestLbVMUserData_Structural(t *testing.T) {
	svc := &ELBv2ServiceImpl{
		GatewayURL:      "https://192.168.1.33:9999",
		SystemAccessKey: "AKIAIOSFODNN7EXAMPLE",
		SystemSecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		region:          "ap-southeast-2",
	}
	ud, err := svc.lbVMUserData("lb-abc123", SchemeInternetFacing)
	require.NoError(t, err)

	envs, runcmd := extractAgentEnv(t, ud)
	assert.Equal(t, map[string]string{
		"LB_LB_ID":       "lb-abc123",
		"LB_GATEWAY_URL": "https://192.168.1.33:9999",
		"LB_ACCESS_KEY":  "AKIAIOSFODNN7EXAMPLE",
		"LB_SECRET_KEY":  "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"LB_REGION":      "ap-southeast-2",
	}, envs)
	for k := range envs {
		assert.NotContains(t, k, "NATS", "NATS URL must not leak into agent config")
	}

	// Agent is launched via OpenRC, never as a bare binary path.
	require.Len(t, runcmd, 1)
	assert.Contains(t, runcmd[0], `[ "rc-service", "lb-agent", "start" ]`)
	assert.NotContains(t, ud, "/usr/local/bin/lb-agent")

	// CA cert is injected upstream by the instance service's template.
	assert.NotContains(t, ud, "ca_certs:")

	// Single-node default: no MgmtRoute set means no bootcmd.
	assert.NotContains(t, ud, "bootcmd:")
}

// TestBuildLBAgentEnv verifies that buildLBAgentEnv produces a KEY=value blob
// containing all five env vars with the correct values.
func TestBuildLBAgentEnv(t *testing.T) {
	svc := &ELBv2ServiceImpl{
		GatewayURL:      "https://10.0.0.1:9999",
		SystemAccessKey: "AKIAIOSFODNN7EXAMPLE",
		SystemSecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		region:          "ap-southeast-2",
	}
	env := svc.buildLBAgentEnv("lb-abc123")

	lines := strings.Split(strings.TrimRight(env, "\n"), "\n")
	kvs := make(map[string]string, len(lines))
	for _, l := range lines {
		k, v, ok := strings.Cut(l, "=")
		require.True(t, ok, "env line missing '=': %q", l)
		kvs[k] = v
	}

	assert.Equal(t, "lb-abc123", kvs["LB_LB_ID"])
	assert.Equal(t, "https://10.0.0.1:9999", kvs["LB_GATEWAY_URL"])
	assert.Equal(t, "AKIAIOSFODNN7EXAMPLE", kvs["LB_ACCESS_KEY"])
	assert.Equal(t, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", kvs["LB_SECRET_KEY"])
	assert.Equal(t, "ap-southeast-2", kvs["LB_REGION"])
}

// TestSubnetCIDRForIP and TestSubnetGatewayIP cover the CIDR helpers.
func TestSubnetCIDRForIP(t *testing.T) {
	assert.Equal(t, "10.0.1.5/24", subnetCIDRForIP("10.0.1.5", "10.0.1.0/24"))
	assert.Equal(t, "172.16.2.10/20", subnetCIDRForIP("172.16.2.10", "172.16.0.0/20"))
	assert.Equal(t, "", subnetCIDRForIP("10.0.1.5", "bad-cidr"))
}

func TestSubnetGatewayIP(t *testing.T) {
	assert.Equal(t, "10.0.1.1", subnetGatewayIP("10.0.1.0/24"))
	assert.Equal(t, "172.16.0.1", subnetGatewayIP("172.16.0.0/20"))
	assert.Equal(t, "", subnetGatewayIP("not-a-cidr"))
}

// setupMicrovmTestService creates an ELBv2 service with ELBv2Enabled=true wired
// to a real VPC service. The VPC has one subnet (10.0.1.0/24) pre-created.
func setupMicrovmTestService(t *testing.T) (*ELBv2ServiceImpl, *handlers_ec2_vpc.VPCServiceImpl, string) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	testutil.StubVpcdSGResponder(t, nc)

	vpcSvc, err := handlers_ec2_vpc.NewVPCServiceImplWithNATS(nil, nc)
	require.NoError(t, err)

	cfg := &config.Config{
		Daemon: config.DaemonConfig{
			DevNetworking: true,
			Microvm:       config.MicrovmConfig{ELBv2Enabled: true},
		},
	}
	elbv2Svc, err := NewELBv2ServiceImplWithNATS(cfg, nc)
	require.NoError(t, err)
	elbv2Svc.VPCService = vpcSvc
	elbv2Svc.GatewayURL = "https://10.20.0.5:9999"
	elbv2Svc.SystemAccessKey = "AKID"
	elbv2Svc.SystemSecretKey = "SECRET"
	elbv2Svc.region = "us-east-1"
	elbv2Svc.CACert = "-----BEGIN CERTIFICATE-----\nMOCK\n-----END CERTIFICATE-----\n"

	vpcOut, err := vpcSvc.CreateVpc(&ec2.CreateVpcInput{
		CidrBlock: aws.String("10.0.0.0/16"),
	}, testAccountID)
	require.NoError(t, err)
	vpcID := *vpcOut.Vpc.VpcId

	subnetOut, err := vpcSvc.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:            aws.String(vpcID),
		CidrBlock:        aws.String("10.0.1.0/24"),
		AvailabilityZone: aws.String("us-east-1a"),
	}, testAccountID)
	require.NoError(t, err)
	subnetID := *subnetOut.Subnet.SubnetId

	return elbv2Svc, vpcSvc, subnetID
}

// TestCreateLoadBalancer_Microvm_DirectBoot asserts the microvm launch path
// sets DirectBoot=true, leaves ImageID empty, and delivers lb-agent env vars.
func TestCreateLoadBalancer_Microvm_DirectBoot(t *testing.T) {
	svc, _, subnetID := setupMicrovmTestService(t)

	mock := &mockSystemInstanceLauncher{
		launchResult: &SystemInstanceOutput{
			InstanceID: "i-microvm-test",
			PrivateIP:  "10.0.1.5",
		},
	}
	svc.InstanceLauncher = mock

	_, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:    aws.String("mv-alb"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)

	require.Len(t, mock.launchCalls, 1)
	inp := mock.launchCalls[0]

	assert.True(t, inp.DirectBoot, "DirectBoot must be true in microvm mode")
	assert.Equal(t, "", inp.ImageID, "ImageID must be empty in microvm mode")
	assert.Equal(t, "-----BEGIN CERTIFICATE-----\nMOCK\n-----END CERTIFICATE-----\n", inp.CACert)
}

// TestCreateLoadBalancer_Microvm_LBAgentEnv asserts all five lb-agent env vars
// appear in LBAgentEnv with correct values.
func TestCreateLoadBalancer_Microvm_LBAgentEnv(t *testing.T) {
	svc, _, subnetID := setupMicrovmTestService(t)

	mock := &mockSystemInstanceLauncher{
		launchResult: &SystemInstanceOutput{InstanceID: "i-env-test", PrivateIP: "10.0.1.6"},
	}
	svc.InstanceLauncher = mock

	_, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:    aws.String("env-alb"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, mock.launchCalls, 1)

	envBlob := mock.launchCalls[0].LBAgentEnv
	kvs := parseEnvBlob(t, envBlob)

	assert.NotEmpty(t, kvs["LB_LB_ID"])
	assert.Equal(t, "https://10.20.0.5:9999", kvs["LB_GATEWAY_URL"])
	assert.Equal(t, "AKID", kvs["LB_ACCESS_KEY"])
	assert.Equal(t, "SECRET", kvs["LB_SECRET_KEY"])
	assert.Equal(t, "us-east-1", kvs["LB_REGION"])
}

// TestCreateLoadBalancer_Microvm_NICs asserts NIC[0].IsDefault=true and
// NIC[1].IsDefault=false, and that CIDR/Gateway are derived from the subnet.
func TestCreateLoadBalancer_Microvm_NICs(t *testing.T) {
	svc, _, subnetID := setupMicrovmTestService(t)

	mock := &mockSystemInstanceLauncher{
		launchResult: &SystemInstanceOutput{InstanceID: "i-nic-test", PrivateIP: "10.0.1.7"},
	}
	svc.InstanceLauncher = mock

	_, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:    aws.String("nic-alb"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, mock.launchCalls, 1)

	nics := mock.launchCalls[0].NICs
	require.GreaterOrEqual(t, len(nics), 2, "must have at least primary ENI NIC and mgmt NIC")

	// NIC[0]: primary VPC ENI — must be the default route owner.
	assert.True(t, nics[0].IsDefault, "NIC[0] must have IsDefault=true")
	assert.NotEmpty(t, nics[0].MAC, "NIC[0] MAC must be populated from ENI")
	assert.Contains(t, nics[0].CIDR, "/24", "NIC[0] CIDR must include prefix from subnet")
	assert.Equal(t, "10.0.1.1", nics[0].Gateway, "NIC[0] gateway must be subnet .1 address")

	// NIC[1]: management NIC — daemon allocates IP/MAC.
	assert.False(t, nics[1].IsDefault, "NIC[1] must have IsDefault=false")
}

// TestCreateLoadBalancer_Microvm_MissingCredentials_SetsStateFailed verifies
// that missing system credentials on the microvm path result in StateFailed.
func TestCreateLoadBalancer_Microvm_MissingCredentials_SetsStateFailed(t *testing.T) {
	svc, _, subnetID := setupMicrovmTestService(t)
	// Clear credentials to trigger the failure path.
	svc.GatewayURL = ""

	mock := &mockSystemInstanceLauncher{
		launchResult: &SystemInstanceOutput{InstanceID: "i-should-not-launch"},
	}
	svc.InstanceLauncher = mock

	out, err := svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:    aws.String("mv-nocreds-alb"),
		Subnets: []*string{aws.String(subnetID)},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, StateFailed, *out.LoadBalancers[0].State.Code)
	assert.Empty(t, mock.launchCalls, "launcher must not be invoked when credentials are missing")
}

// TestCreateLoadBalancer_PCMachine_Unchanged verifies that when ELBv2Enabled=false
// the existing PC-machine cloud-config path is used and DirectBoot is not set.
func TestCreateLoadBalancer_PCMachine_Unchanged(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	testutil.StubVpcdSGResponder(t, nc)

	vpcSvc, err := handlers_ec2_vpc.NewVPCServiceImplWithNATS(nil, nc)
	require.NoError(t, err)

	cfg := &config.Config{
		Daemon: config.DaemonConfig{
			DevNetworking: true,
			Microvm:       config.MicrovmConfig{ELBv2Enabled: false},
		},
	}
	svc, err := NewELBv2ServiceImplWithNATS(cfg, nc)
	require.NoError(t, err)
	svc.VPCService = vpcSvc
	svc.GatewayURL = "https://10.0.0.1:9999"
	svc.SystemAccessKey = "AKID"
	svc.SystemSecretKey = "SECRET"
	svc.SetSystemAMIFunc(func() (string, error) { return "ami-lb-test", nil })

	vpcOut, err := vpcSvc.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String("10.0.0.0/16")}, testAccountID)
	require.NoError(t, err)
	subnetOut, err := vpcSvc.CreateSubnet(&ec2.CreateSubnetInput{
		VpcId:            vpcOut.Vpc.VpcId,
		CidrBlock:        aws.String("10.0.2.0/24"),
		AvailabilityZone: aws.String("us-east-1a"),
	}, testAccountID)
	require.NoError(t, err)

	mock := &mockSystemInstanceLauncher{
		launchResult: &SystemInstanceOutput{InstanceID: "i-pc-test", PrivateIP: "10.0.2.5"},
	}
	svc.InstanceLauncher = mock

	_, err = svc.CreateLoadBalancer(&elbv2.CreateLoadBalancerInput{
		Name:    aws.String("pc-alb"),
		Subnets: []*string{subnetOut.Subnet.SubnetId},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, mock.launchCalls, 1)

	inp := mock.launchCalls[0]
	assert.False(t, inp.DirectBoot, "DirectBoot must be false on PC-machine path")
	assert.Equal(t, "ami-lb-test", inp.ImageID, "ImageID must be set on PC-machine path")
	assert.NotEmpty(t, inp.UserData, "UserData (cloud-config) must be set on PC-machine path")
	assert.True(t, strings.HasPrefix(inp.UserData, "#cloud-config\n"),
		"UserData must be cloud-config on PC-machine path")
}

// parseEnvBlob parses a KEY=value newline-separated blob into a map.
func parseEnvBlob(t *testing.T, blob string) map[string]string {
	t.Helper()
	kvs := make(map[string]string)
	for line := range strings.SplitSeq(strings.TrimRight(blob, "\n"), "\n") {
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		require.True(t, ok, "env blob line missing '=': %q", line)
		kvs[k] = v
	}
	return kvs
}

// TestLbVMUserData_MissingCredentials covers the three required-field error
// paths (gateway URL, access key, secret key) in one table-driven test.
func TestLbVMUserData_MissingCredentials(t *testing.T) {
	tests := []struct {
		name string
		svc  *ELBv2ServiceImpl
	}{
		{
			name: "missing GatewayURL",
			svc:  &ELBv2ServiceImpl{SystemAccessKey: "AKID", SystemSecretKey: "SECRET"},
		},
		{
			name: "missing SystemAccessKey",
			svc:  &ELBv2ServiceImpl{GatewayURL: "https://10.0.0.1:9999", SystemSecretKey: "SECRET"},
		},
		{
			name: "missing SystemSecretKey",
			svc:  &ELBv2ServiceImpl{GatewayURL: "https://10.0.0.1:9999", SystemAccessKey: "AKID"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.svc.lbVMUserData("lb-test", SchemeInternetFacing)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "missing system credentials")
		})
	}
}

// TestBuildMicrovmNICs_NilVPC verifies buildMicrovmNICs works when VPCService
// is nil — CIDR and gateway remain empty, NIC structure is still correct.
func TestBuildMicrovmNICs_NilVPC(t *testing.T) {
	svc := &ELBv2ServiceImpl{VPCService: nil}
	nics := svc.buildMicrovmNICs("10.0.1.5", "02:aa:bb:cc:dd:01", "subnet-abc", "eni-abc", SchemeInternal, nil, testAccountID)
	require.Len(t, nics, 2, "primary ENI NIC + mgmt NIC")
	assert.True(t, nics[0].IsDefault)
	assert.Equal(t, "02:aa:bb:cc:dd:01", nics[0].MAC)
	assert.Empty(t, nics[0].CIDR, "CIDR unknown when VPC service unavailable")
	assert.False(t, nics[1].IsDefault)
}

// TestBuildMicrovmNICs_ExtraENIs verifies NIC[2+] are appended for multi-subnet ALBs.
func TestBuildMicrovmNICs_ExtraENIs(t *testing.T) {
	svc := &ELBv2ServiceImpl{VPCService: nil}
	extras := []ExtraENIInput{
		{ENIID: "eni-extra1", ENIMac: "02:aa:bb:cc:dd:02", ENIIP: "10.0.2.5", SubnetID: "subnet-extra1"},
	}
	nics := svc.buildMicrovmNICs("10.0.1.5", "02:aa:bb:cc:dd:01", "subnet-abc", "eni-abc", SchemeInternal, extras, testAccountID)
	require.Len(t, nics, 3, "primary ENI + mgmt + 1 extra")
	assert.Equal(t, "02:aa:bb:cc:dd:02", nics[2].MAC)
	assert.False(t, nics[2].IsDefault)
}

// TestBuildMicrovmNICs_MgmtRoute covers the same single-node/multi-node matrix
// as TestLBVMUserData_InternalSingleNodeFallback: internet-facing single-node
// must NOT emit a /32 via mgmt to AdvertiseIP because that steals the host's
// WAN return path for reply traffic, breaking same-chassis ingress.
func TestBuildMicrovmNICs_MgmtRoute(t *testing.T) {
	mk := func() *ELBv2ServiceImpl {
		return &ELBv2ServiceImpl{
			VPCService:   nil,
			MgmtBridgeIP: "10.15.8.1",
			AdvertiseIP:  "192.168.1.33",
		}
	}

	internal := mk().buildMicrovmNICs("10.0.1.5", "02:aa:bb:cc:dd:01", "subnet-abc", "eni-abc", SchemeInternal, nil, testAccountID)
	require.Len(t, internal, 2)
	assert.Equal(t, "192.168.1.33", internal[1].RouteDst, "internal single-node forces /32 via mgmt")
	assert.Equal(t, "10.15.8.1", internal[1].RouteVia)

	inet := mk().buildMicrovmNICs("10.0.1.5", "02:aa:bb:cc:dd:01", "subnet-abc", "eni-abc", SchemeInternetFacing, nil, testAccountID)
	require.Len(t, inet, 2)
	assert.Empty(t, inet[1].RouteDst, "internet-facing single-node must not /32 AdvertiseIP via mgmt")
	assert.Empty(t, inet[1].RouteVia)

	multi := mk()
	multi.MgmtRouteGateway = "10.15.8.1"
	multi.MgmtRouteTarget = "10.15.8.100"
	nics := multi.buildMicrovmNICs("10.0.1.5", "02:aa:bb:cc:dd:01", "subnet-abc", "eni-abc", SchemeInternetFacing, nil, testAccountID)
	require.Len(t, nics, 2)
	assert.Equal(t, "10.15.8.100", nics[1].RouteDst, "multi-node uses explicit MgmtRouteTarget")
	assert.Equal(t, "10.15.8.1", nics[1].RouteVia)
}
