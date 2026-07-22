//go:build e2e

// Package natuplink is the routed-NAT (external_mode = "nat") single-node
// suite. It runs ON the spinifex node itself (like the single suite): the
// host-wiring and OVN phases shell out to ip/iptables/ovn-nbctl locally,
// and instance egress is proven via the serial console because nat mode
// has no inbound path to VMs (outbound-only by design).
//
// The node must be provisioned with `setup-ovn.sh --management --nat-uplink`
// followed by `spx admin init --external-mode=nat`; the suite skips unless
// spinifex.toml carries external_mode = "nat".
package natuplink

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	transitCIDR      = "100.127.0.0/24"
	transitGatewayIP = "100.127.0.1"
	transitHostEnd   = "spx-nat-host"
	transitOVSEnd    = "spx-nat-ovs"
	uplinkBridge     = "br-ext"

	egressOKMarker   = "NAT-E2E-EGRESS-OK"
	egressFailMarker = "NAT-E2E-EGRESS-FAIL"

	secondVPCCIDR    = "10.213.0.0/16"
	natExemptSetName = "spinifex_nat_exempt"
)

// egressUserData probes outbound WAN reachability from inside the guest and
// reports the verdict on the serial console — the only channel back to the
// test in nat mode. ping 8.8.8.8 is the DNS-free fallback; the curl targets
// additionally prove DHCP-delivered DNS works through the masquerade.
const egressUserData = `#!/bin/bash
for i in $(seq 1 36); do
  if curl -fsS -m 10 -o /dev/null http://connectivity-check.ubuntu.com/ \
     || curl -fsS -m 10 -o /dev/null http://www.google.com/generate_204 \
     || ping -c 1 -W 3 8.8.8.8 >/dev/null 2>&1; then
    echo "NAT-E2E-EGRESS-OK" | tee /dev/console
    exit 0
  fi
  sleep 5
done
echo "NAT-E2E-EGRESS-FAIL" | tee /dev/console
`

type fixture struct {
	env        *harness.Env
	aws        *harness.AWSClient
	harness    *harness.Fixture
	artifacts  string
	configTOML string
	// publicPool is true when spinifex.toml carries a non-transit pool —
	// the Tier 2 (EIP / public IP) lane is exercised only then.
	publicPool bool
}

// TestNATUplink runs the routed-NAT lane end to end. Phases are sequential:
// wiring must hold before config, config before API behaviour, and the OVN
// phases read state the earlier phases prove exists.
func TestNATUplink(t *testing.T) {
	env := harness.LoadEnv(t)
	if env.Mode != harness.ModeSingle {
		t.Skipf("natuplink suite is single-node only (mode=%s)", env.Mode)
	}
	cfgPath := configPath(env)
	if mode := readExternalMode(t, cfgPath); mode != "nat" {
		t.Skipf("natuplink suite requires external_mode=nat (got %q in %s)", mode, cfgPath)
	}
	harness.SkipIfNoOVN(t)

	awsCli := harness.NewAWSClient(t, env)
	hfx, err := harness.NewProcessFixture(awsCli)
	require.NoError(t, err, "harness fixture init")
	t.Cleanup(func() {
		if err := hfx.Close(); err != nil {
			t.Errorf("e2e teardown: %v", err)
		}
	})

	fix := &fixture{
		env:        env,
		aws:        awsCli,
		harness:    hfx,
		artifacts:  harness.ArtifactDir(t, env),
		configTOML: cfgPath,
		publicPool: hasPublicPool(t, cfgPath),
	}

	harness.Phase(t, "NAT Uplink — Phase 1: host wiring")
	phaseHostWiring(t)

	harness.Phase(t, "NAT Uplink — Phase 2: config")
	phaseConfig(t, fix)

	if fix.publicPool {
		harness.Phase(t, "NAT Uplink — Phase 3: EIP surface enabled (Tier 2)")
		phaseEIPEnabled(t, fix)
	} else {
		harness.Phase(t, "NAT Uplink — Phase 3: EIP surface disabled")
		phaseEIPDisabled(t, fix)
	}

	harness.Phase(t, "NAT Uplink — Phase 4: default subnet public IP mapping")
	def := phaseDefaultSubnet(t, fix)

	harness.Phase(t, "NAT Uplink — Phase 5: OVN gateway + SNAT on transit net")
	defaultGwIP := phaseOVNGateway(t, fix, def.VPCID)

	harness.Phase(t, "NAT Uplink — Phase 6: instance boots and reaches WAN outbound")
	probe := phaseInstanceEgress(t, fix, def)

	harness.Phase(t, "NAT Uplink — Phase 7: host reaches instance private IP (Tier 1 ingress)")
	phaseHostIngress(t, fix, def, probe)

	harness.Phase(t, "NAT Uplink — Phase 8: second VPC gets a unique transit gateway IP")
	phaseUniqueTransitIP(t, fix, defaultGwIP)

	if fix.publicPool {
		harness.Phase(t, "NAT Uplink — Phase 9: EIP ingress lifecycle (Tier 2)")
		phaseEIPIngress(t, fix, def, probe)
	} else {
		t.Log("Phase 9 (Tier 2 EIP ingress) skipped: no public pool in config")
	}
}

// --- Phase 1: host wiring --------------------------------------------------

func phaseHostWiring(t *testing.T) {
	t.Helper()

	harness.Step(t, "transit veth host end %s carries %s", transitHostEnd, transitGatewayIP)
	addrOut := hostCmd(t, "ip", "-o", "addr", "show", "dev", transitHostEnd)
	assert.Containsf(t, addrOut, transitGatewayIP+"/24",
		"%s must carry %s/24\n%s", transitHostEnd, transitGatewayIP, addrOut)

	harness.Step(t, "transit veth OVS end %s attached to %s", transitOVSEnd, uplinkBridge)
	br := harness.OvsVsctl(t, "port-to-br", transitOVSEnd)
	assert.Equalf(t, uplinkBridge, br, "%s must be a port on %s", transitOVSEnd, uplinkBridge)

	harness.Step(t, "ip_forward enabled")
	fwd, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	require.NoError(t, err, "read ip_forward")
	assert.Equal(t, "1", strings.TrimSpace(string(fwd)), "net.ipv4.ip_forward must be 1")

	harness.Step(t, "NAT egress iptables rules present")
	checks := [][]string{
		{"-t", "nat", "-C", "POSTROUTING", "-s", transitCIDR, "!", "-d", transitCIDR,
			"-m", "comment", "--comment", "spinifex-nat-egress", "-j", "MASQUERADE"},
		{"-t", "filter", "-C", "FORWARD", "-i", transitHostEnd, "-s", transitCIDR,
			"-m", "comment", "--comment", "spinifex-nat-egress", "-j", "ACCEPT"},
		{"-t", "filter", "-C", "FORWARD", "-o", transitHostEnd, "-m", "conntrack",
			"--ctstate", "RELATED,ESTABLISHED",
			"-m", "comment", "--comment", "spinifex-nat-egress", "-j", "ACCEPT"},
	}
	for _, args := range checks {
		out, err := exec.Command("sudo", append([]string{"-n", "iptables"}, args...)...).CombinedOutput()
		assert.NoErrorf(t, err, "iptables %s missing: %s", strings.Join(args, " "), string(out))
	}
}

// --- Phase 2: config ---------------------------------------------------------

func phaseConfig(t *testing.T, fix *fixture) {
	t.Helper()
	data, err := os.ReadFile(fix.configTOML)
	require.NoError(t, err, "read %s", fix.configTOML)
	content := string(data)

	assert.Contains(t, content, `external_mode = "nat"`)
	assert.Contains(t, content, `bridge_mode = "nat"`)
	assert.Containsf(t, content, "nat-transit", "transit pool block missing in %s", fix.configTOML)
	assert.Containsf(t, content, transitGatewayIP, "transit gateway IP missing in %s", fix.configTOML)
	if fix.publicPool {
		harness.Detail(t, "tier2", "public pool present — EIP lane active")
	} else {
		assert.NotContains(t, content, "range_start", "Tier-1-only nat mode must not carry a public IP range")
	}
}

// --- Phase 3: EIP surface ----------------------------------------------------

func phaseEIPDisabled(t *testing.T, fix *fixture) {
	t.Helper()

	harness.Step(t, "describe-addresses returns empty")
	out, err := fix.aws.EC2.DescribeAddresses(&ec2.DescribeAddressesInput{})
	require.NoError(t, err, "describe-addresses must succeed (empty), not error")
	assert.Emptyf(t, out.Addresses, "nat mode must report zero addresses, got %d", len(out.Addresses))

	harness.Step(t, "allocate-address returns UnsupportedOperation")
	harness.ExpectError(t, "UnsupportedOperation", func() error {
		// e2e:allow-create — negative-path call; nat mode must reject it, nothing is created.
		_, aerr := fix.aws.EC2.AllocateAddress(&ec2.AllocateAddressInput{Domain: aws.String("vpc")})
		return aerr
	})
}

// phaseEIPEnabled proves nat mode with a public pool exposes the same EIP
// surface as pool mode: allocate works, the address shows in describe, and
// release returns it to the pool. The full ingress lifecycle runs in Phase 9.
func phaseEIPEnabled(t *testing.T, fix *fixture) {
	t.Helper()

	harness.Step(t, "allocate-address succeeds from public pool")
	// e2e:allow-create — scratch EIP, released at the end of this phase.
	alloc, err := fix.aws.EC2.AllocateAddress(&ec2.AllocateAddressInput{Domain: aws.String("vpc")})
	require.NoError(t, err, "allocate-address must succeed with a public pool configured")
	allocID := aws.StringValue(alloc.AllocationId)
	eip := aws.StringValue(alloc.PublicIp)
	require.NotEmpty(t, allocID, "allocate-address returned no AllocationId")
	require.NotEmpty(t, eip, "allocate-address returned no PublicIp")
	harness.Detail(t, "allocation", allocID, "eip", eip)

	harness.Step(t, "describe-addresses lists the allocation")
	out, err := fix.aws.EC2.DescribeAddresses(&ec2.DescribeAddressesInput{
		AllocationIds: []*string{aws.String(allocID)},
	})
	require.NoError(t, err, "describe-addresses %s", allocID)
	require.Len(t, out.Addresses, 1, "allocation %s missing from describe-addresses", allocID)
	assert.Equal(t, eip, aws.StringValue(out.Addresses[0].PublicIp))

	harness.Step(t, "release-address returns it to the pool")
	_, err = fix.aws.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{AllocationId: aws.String(allocID)})
	require.NoError(t, err, "release-address %s", allocID)
}

// --- Phase 4: default subnet -------------------------------------------------

func phaseDefaultSubnet(t *testing.T, fix *fixture) harness.VPCInfo {
	t.Helper()
	def := harness.EnsureDefaultVPC(t, fix.harness)
	harness.Detail(t, "vpc", def.VPCID, "subnet", def.SubnetID)

	out, err := fix.aws.EC2.DescribeSubnets(&ec2.DescribeSubnetsInput{
		SubnetIds: []*string{aws.String(def.SubnetID)},
	})
	require.NoError(t, err, "describe-subnets %s", def.SubnetID)
	require.NotEmpty(t, out.Subnets, "default subnet %s not found", def.SubnetID)
	if fix.publicPool {
		assert.Truef(t, aws.BoolValue(out.Subnets[0].MapPublicIpOnLaunch),
			"default subnet %s must have MapPublicIpOnLaunch=true with a public pool (bridge parity)", def.SubnetID)
	} else {
		assert.Falsef(t, aws.BoolValue(out.Subnets[0].MapPublicIpOnLaunch),
			"default subnet %s must have MapPublicIpOnLaunch=false in Tier-1-only nat mode", def.SubnetID)
	}
	return def
}

// --- Phase 5: OVN gateway + SNAT ----------------------------------------------

// phaseOVNGateway asserts the default VPC's gateway LRP sits on the transit
// /24 and the router SNATs the VPC CIDR to that LRP IP. Returns the gateway
// LRP IP for the uniqueness check in Phase 7.
func phaseOVNGateway(t *testing.T, fix *fixture, vpcID string) string {
	t.Helper()

	gwIP := gatewayLRPIP(t, vpcID)
	require.NotEmptyf(t, gwIP, "gateway LRP gw-%s has no transit IP", vpcID)
	harness.Detail(t, "gw_lrp_ip", gwIP)
	assert.Truef(t, strings.HasPrefix(gwIP, "100.127.0."),
		"gateway LRP IP %s must be on %s", gwIP, transitCIDR)
	assert.NotEqualf(t, transitGatewayIP, gwIP,
		"gateway LRP must not squat the host transit gateway IP")

	vpcCIDR := describeVPCCIDR(t, fix, vpcID)
	natList := harness.OvnNbctl(t, "lr-nat-list", "vpc-"+vpcID)
	harness.Detail(t, "lr_nat_list", natList)
	assert.Containsf(t, natList, "snat", "router vpc-%s missing snat rule\n%s", vpcID, natList)
	assert.Containsf(t, natList, gwIP, "snat external IP should be gateway LRP IP %s\n%s", gwIP, natList)
	assert.Containsf(t, natList, vpcCIDR, "snat logical IP should be VPC CIDR %s\n%s", vpcCIDR, natList)
	return gwIP
}

// --- Phase 6: instance egress ---------------------------------------------------

// egressProbe carries the phase-6 instance into the ingress phases.
type egressProbe struct {
	instanceID string
	privateIP  string
	publicIP   string
	sgID       string
}

func phaseInstanceEgress(t *testing.T, fix *fixture, def harness.VPCInfo) egressProbe {
	t.Helper()

	instType, arch := harness.DiscoverNanoInstanceType(t, fix.harness)
	amiID := harness.DiscoverUbuntuAMI(t, fix.harness, arch)
	keyName, _ := harness.EnsureKeyPair(t, fix.harness, fix.artifacts)
	harness.Detail(t, "type", instType, "ami", amiID, "key", keyName)

	harness.Step(t, "run-instances (default subnet, egress-probe user-data)")
	// e2e:allow-create — throwaway probe VM; the egress-probe user-data makes it unshareable.
	runOut, err := fix.aws.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String(amiID),
		InstanceType: aws.String(instType),
		KeyName:      aws.String(keyName),
		SubnetId:     aws.String(def.SubnetID),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		UserData:     aws.String(base64.StdEncoding.EncodeToString([]byte(egressUserData))),
	})
	require.NoError(t, err, "run-instances")
	require.NotEmpty(t, runOut.Instances, "run-instances returned no Instances")
	instanceID := aws.StringValue(runOut.Instances[0].InstanceId)
	t.Cleanup(func() {
		_, _ = fix.aws.EC2.TerminateInstances(&ec2.TerminateInstancesInput{
			InstanceIds: []*string{aws.String(instanceID)},
		})
		harness.WaitForInstanceTerminated(t, fix.aws, []string{instanceID}, 5*time.Minute)
	})
	harness.Detail(t, "instance", instanceID)

	inst := harness.WaitForInstanceState(t, fix.aws, instanceID, "running")

	if fix.publicPool {
		harness.Step(t, "instance auto-assigned a public IP (MapPublicIpOnLaunch)")
		assert.NotEmptyf(t, aws.StringValue(inst.PublicIpAddress),
			"instance %s must auto-assign a public IP with a public pool configured", instanceID)
		harness.Detail(t, "public_ip", aws.StringValue(inst.PublicIpAddress))
	} else {
		harness.Step(t, "instance has no public IP")
		assert.Emptyf(t, aws.StringValue(inst.PublicIpAddress),
			"Tier-1-only nat mode instance %s must not get a public IP (got %q)",
			instanceID, aws.StringValue(inst.PublicIpAddress))
	}
	assert.NotEmptyf(t, aws.StringValue(inst.PrivateIpAddress),
		"instance %s missing private IP", instanceID)

	harness.Step(t, "wait for egress verdict on serial console")
	var console string
	harness.EventuallyErr(t, func() error {
		console = consoleOutput(t, fix, instanceID)
		if strings.Contains(console, egressOKMarker) || strings.Contains(console, egressFailMarker) {
			return nil
		}
		return fmt.Errorf("no egress marker on console yet (%d bytes)", len(console))
	}, 8*time.Minute, 10*time.Second)

	if strings.Contains(console, egressFailMarker) {
		harness.DumpFile(t, fix.artifacts, "egress-console.log", []byte(console))
		t.Fatalf("guest reported %s — outbound WAN unreachable through routed NAT (console saved to artifacts)", egressFailMarker)
	}
	harness.Step(t, "guest reported %s", egressOKMarker)

	var sgID string
	if len(inst.SecurityGroups) > 0 {
		sgID = aws.StringValue(inst.SecurityGroups[0].GroupId)
	}
	return egressProbe{
		instanceID: instanceID,
		privateIP:  aws.StringValue(inst.PrivateIpAddress),
		publicIP:   aws.StringValue(inst.PublicIpAddress),
		sgID:       sgID,
	}
}

// --- Phase 7: Tier 1 host ingress -----------------------------------------------

// phaseHostIngress proves the automatic routed-NAT ingress tier: the host has
// a route to the VPC CIDR via the gateway LRP transit IP, the SNAT rule
// carries the exempt Address_Set ref (so VM replies to host-initiated flows
// are not SNATted), and — after opening the SG like on AWS — the host can
// ping the instance's private IP and complete a TCP handshake to sshd.
func phaseHostIngress(t *testing.T, fix *fixture, def harness.VPCInfo, probe egressProbe) {
	t.Helper()

	gwIP := gatewayLRPIP(t, def.VPCID)
	require.NotEmpty(t, gwIP, "gateway LRP IP")
	vpcCIDR := describeVPCCIDR(t, fix, def.VPCID)

	harness.Step(t, "host route %s via %s dev %s", vpcCIDR, gwIP, transitHostEnd)
	routeOut := hostCmd(t, "ip", "route", "show", vpcCIDR)
	assert.Containsf(t, routeOut, "via "+gwIP, "VPC ingress route must go via gateway LRP IP\n%s", routeOut)
	assert.Containsf(t, routeOut, "dev "+transitHostEnd, "VPC ingress route must use %s\n%s", transitHostEnd, routeOut)

	harness.Step(t, "SNAT rule carries exempted_ext_ips -> %s", natExemptSetName)
	exemptRef := strings.TrimSpace(harness.OvnNbctl(t, "--bare", "--columns=exempted_ext_ips",
		"find", "nat", "type=snat", "logical_ip="+vpcCIDR))
	require.NotEmptyf(t, exemptRef, "snat rule for %s has no exempted_ext_ips ref", vpcCIDR)
	setAddrs := harness.OvnNbctl(t, "--bare", "--columns=addresses",
		"find", "address_set", "name="+natExemptSetName)
	assert.Containsf(t, setAddrs, transitCIDR,
		"%s must contain the transit CIDR %s\n%s", natExemptSetName, transitCIDR, setAddrs)

	harness.Step(t, "open SG for ICMP + SSH from transit net (default-closed, AWS parity)")
	require.NotEmpty(t, probe.sgID, "probe instance has no security group")
	perms := []*ec2.IpPermission{
		{
			IpProtocol: aws.String("icmp"), FromPort: aws.Int64(-1), ToPort: aws.Int64(-1),
			IpRanges: []*ec2.IpRange{{CidrIp: aws.String(transitCIDR)}},
		},
		{
			IpProtocol: aws.String("tcp"), FromPort: aws.Int64(22), ToPort: aws.Int64(22),
			IpRanges: []*ec2.IpRange{{CidrIp: aws.String(transitCIDR)}},
		},
	}
	_, err := fix.aws.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(probe.sgID), IpPermissions: perms,
	})
	require.NoError(t, err, "authorize-security-group-ingress")
	t.Cleanup(func() {
		_, _ = fix.aws.EC2.RevokeSecurityGroupIngress(&ec2.RevokeSecurityGroupIngressInput{
			GroupId: aws.String(probe.sgID), IpPermissions: perms,
		})
	})

	harness.Step(t, "ping instance private IP %s from host", probe.privateIP)
	harness.EventuallyErr(t, func() error {
		out, perr := exec.Command("ping", "-c", "1", "-W", "3", probe.privateIP).CombinedOutput()
		if perr != nil {
			return fmt.Errorf("ping %s: %v: %s", probe.privateIP, perr, string(out))
		}
		return nil
	}, 2*time.Minute, 5*time.Second)

	harness.Step(t, "TCP handshake to sshd on %s:22 from host", probe.privateIP)
	harness.EventuallyErr(t, func() error {
		return sshHandshake(probe.privateIP)
	}, 2*time.Minute, 5*time.Second)
	harness.Step(t, "host->instance return path works without SNAT mangling")
}

// --- Phase 7: unique transit IP per VPC -----------------------------------------

// phaseUniqueTransitIP creates a second VPC and attaches an IGW, then asserts
// its gateway LRP gets a transit IP distinct from the default VPC's — the
// regression guard for the duplicate-gateway-IP failure in field report #471.
func phaseUniqueTransitIP(t *testing.T, fix *fixture, defaultGwIP string) {
	t.Helper()

	harness.Step(t, "create-vpc %s", secondVPCCIDR)
	// e2e:allow-create — scratch VPC owned end to end by this uniqueness check.
	vpcOut, err := fix.aws.EC2.CreateVpc(&ec2.CreateVpcInput{CidrBlock: aws.String(secondVPCCIDR)})
	require.NoError(t, err, "create-vpc")
	vpcID := aws.StringValue(vpcOut.Vpc.VpcId)
	t.Cleanup(func() {
		_, _ = fix.aws.EC2.DeleteVpc(&ec2.DeleteVpcInput{VpcId: aws.String(vpcID)})
	})

	harness.Step(t, "create + attach internet gateway")
	// e2e:allow-create — scratch IGW owned by this uniqueness check.
	igwOut, err := fix.aws.EC2.CreateInternetGateway(&ec2.CreateInternetGatewayInput{})
	require.NoError(t, err, "create-internet-gateway")
	igwID := aws.StringValue(igwOut.InternetGateway.InternetGatewayId)
	t.Cleanup(func() {
		_, _ = fix.aws.EC2.DetachInternetGateway(&ec2.DetachInternetGatewayInput{
			InternetGatewayId: aws.String(igwID), VpcId: aws.String(vpcID),
		})
		_, _ = fix.aws.EC2.DeleteInternetGateway(&ec2.DeleteInternetGatewayInput{
			InternetGatewayId: aws.String(igwID),
		})
	})
	_, err = fix.aws.EC2.AttachInternetGateway(&ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID), VpcId: aws.String(vpcID),
	})
	require.NoError(t, err, "attach-internet-gateway")

	harness.Step(t, "wait for gateway LRP on transit net")
	var gwIP string
	harness.EventuallyErr(t, func() error {
		gwIP = gatewayLRPIP(t, vpcID)
		if gwIP == "" {
			return fmt.Errorf("gw-%s has no networks yet", vpcID)
		}
		return nil
	}, 2*time.Minute, 3*time.Second)

	harness.Detail(t, "second_gw_lrp_ip", gwIP, "default_gw_lrp_ip", defaultGwIP)
	assert.Truef(t, strings.HasPrefix(gwIP, "100.127.0."),
		"second VPC gateway LRP IP %s must be on %s", gwIP, transitCIDR)
	assert.NotEqualf(t, defaultGwIP, gwIP,
		"each VPC gateway LRP must get a unique transit IP (both got %s)", gwIP)

	// The IGW default snat commits a beat after the gateway LRP appears, so
	// poll lr-nat-list instead of reading it once — a single-shot check races
	// the OVN commit and intermittently sees an empty list.
	harness.Step(t, "second VPC transit snat programmed")
	harness.EventuallyErr(t, func() error {
		natList := harness.OvnNbctl(t, "lr-nat-list", "vpc-"+vpcID)
		if !strings.Contains(natList, secondVPCCIDR) {
			return fmt.Errorf("second VPC router missing snat for %s\n%s", secondVPCCIDR, natList)
		}
		return nil
	}, 2*time.Minute, 3*time.Second)

	harness.Step(t, "host ingress route for second VPC installed on attach")
	harness.EventuallyErr(t, func() error {
		out, _ := exec.Command("ip", "route", "show", secondVPCCIDR).CombinedOutput()
		if !strings.Contains(string(out), "via "+gwIP) {
			return fmt.Errorf("route %s via %s not present yet: %s", secondVPCCIDR, gwIP, string(out))
		}
		return nil
	}, 2*time.Minute, 3*time.Second)
}

// --- Phase 9: Tier 2 EIP ingress lifecycle -----------------------------------

// phaseEIPIngress proves EIP parity with pool mode on a routed-NAT node: the
// auto-assigned public IP and a freshly associated EIP both get the host
// delivery trio (/32 route into OVN, proxy-ARP on the uplink, per-EIP FORWARD
// accepts) plus an exempt-stamped dnat_and_snat row; a TCP handshake to the
// EIP proves the DNAT path end to end; a vpcd restart must replay the host
// bindings; disassociating must tear them down.
func phaseEIPIngress(t *testing.T, fix *fixture, def harness.VPCInfo, probe egressProbe) {
	t.Helper()

	gwIP := gatewayLRPIP(t, def.VPCID)
	require.NotEmpty(t, gwIP, "gateway LRP IP")

	harness.Step(t, "auto-assigned public IP %s has host delivery plumbing", probe.publicIP)
	require.NotEmpty(t, probe.publicIP, "phase 6 probe carries no public IP")
	harness.EventuallyErr(t, func() error {
		return eipHostPlumbing(probe.publicIP, gwIP)
	}, 2*time.Minute, 3*time.Second)
	assertDNATExempt(t, probe.publicIP)

	harness.Step(t, "allocate + associate a fresh EIP")
	// e2e:allow-create — scratch EIP owned end to end by this phase.
	alloc, err := fix.aws.EC2.AllocateAddress(&ec2.AllocateAddressInput{Domain: aws.String("vpc")})
	require.NoError(t, err, "allocate-address")
	allocID := aws.StringValue(alloc.AllocationId)
	eip := aws.StringValue(alloc.PublicIp)
	require.NotEmpty(t, eip, "allocate-address returned no PublicIp")
	t.Cleanup(func() {
		_, _ = fix.aws.EC2.ReleaseAddress(&ec2.ReleaseAddressInput{AllocationId: aws.String(allocID)})
	})
	harness.Detail(t, "allocation", allocID, "eip", eip)

	assocOut, err := fix.aws.EC2.AssociateAddress(&ec2.AssociateAddressInput{
		AllocationId: aws.String(allocID),
		InstanceId:   aws.String(probe.instanceID),
	})
	require.NoError(t, err, "associate-address %s -> %s", allocID, probe.instanceID)
	assocID := aws.StringValue(assocOut.AssociationId)

	harness.Step(t, "host delivery plumbing lands for %s", eip)
	harness.EventuallyErr(t, func() error {
		return eipHostPlumbing(eip, gwIP)
	}, 2*time.Minute, 3*time.Second)
	assertDNATExempt(t, eip)

	harness.Step(t, "open SG for SSH from anywhere, TCP handshake to EIP %s:22", eip)
	perms := []*ec2.IpPermission{{
		IpProtocol: aws.String("tcp"), FromPort: aws.Int64(22), ToPort: aws.Int64(22),
		IpRanges: []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
	}}
	_, err = fix.aws.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(probe.sgID), IpPermissions: perms,
	})
	require.NoError(t, err, "authorize-security-group-ingress")
	t.Cleanup(func() {
		_, _ = fix.aws.EC2.RevokeSecurityGroupIngress(&ec2.RevokeSecurityGroupIngressInput{
			GroupId: aws.String(probe.sgID), IpPermissions: perms,
		})
	})
	harness.EventuallyErr(t, func() error {
		return sshHandshake(eip)
	}, 2*time.Minute, 5*time.Second)

	harness.Step(t, "vpcd restart replays host EIP bindings (reconcile)")
	if out, rerr := exec.Command("sudo", "-n", "ip", "route", "del", eip+"/32",
		"dev", transitHostEnd).CombinedOutput(); rerr != nil {
		t.Logf("route del before restart failed (continuing): %s", string(out))
	}
	if out, rerr := exec.Command("sudo", "-n", "systemctl", "restart",
		"spinifex-vpcd").CombinedOutput(); rerr != nil {
		t.Logf("skipping reconcile-replay check — cannot restart spinifex-vpcd: %s", string(out))
	} else {
		harness.EventuallyErr(t, func() error {
			return eipHostPlumbing(eip, gwIP)
		}, 3*time.Minute, 5*time.Second)
	}

	harness.Step(t, "disassociate tears down host delivery for %s", eip)
	_, err = fix.aws.EC2.DisassociateAddress(&ec2.DisassociateAddressInput{
		AssociationId: aws.String(assocID),
	})
	require.NoError(t, err, "disassociate-address %s", assocID)
	harness.EventuallyErr(t, func() error {
		return eipHostPlumbingGone(eip)
	}, 2*time.Minute, 3*time.Second)

	harness.Step(t, "auto-assigned public IP %s still plumbed after EIP teardown", probe.publicIP)
	require.NoError(t, eipHostPlumbing(probe.publicIP, gwIP))
}

// eipHostPlumbing returns nil when the full Tier 2 host state for eip is in
// place: the /32 route into OVN via the gateway LRP, a proxy-ARP neighbor on
// the uplink, and both per-EIP FORWARD accepts.
func eipHostPlumbing(eip, gwIP string) error {
	out, _ := exec.Command("ip", "route", "show", eip+"/32").CombinedOutput()
	route := string(out)
	if !strings.Contains(route, "via "+gwIP) || !strings.Contains(route, "dev "+transitHostEnd) {
		return fmt.Errorf("EIP route for %s missing (want via %s dev %s): %q",
			eip, gwIP, transitHostEnd, strings.TrimSpace(route))
	}
	out, _ = exec.Command("ip", "neigh", "show", "proxy").CombinedOutput()
	if !strings.Contains(string(out), eip) {
		return fmt.Errorf("proxy-ARP entry for %s missing:\n%s", eip, string(out))
	}
	for _, args := range eipForwardChecks(eip) {
		if out, err := exec.Command("sudo", append([]string{"-n", "iptables"}, args...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("iptables %s missing: %s", strings.Join(args, " "), string(out))
		}
	}
	return nil
}

// eipHostPlumbingGone returns nil when no host delivery state remains for eip.
func eipHostPlumbingGone(eip string) error {
	out, _ := exec.Command("ip", "route", "show", eip+"/32").CombinedOutput()
	if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("EIP route for %s still present: %q", eip, strings.TrimSpace(string(out)))
	}
	out, _ = exec.Command("ip", "neigh", "show", "proxy").CombinedOutput()
	if strings.Contains(string(out), eip) {
		return fmt.Errorf("proxy-ARP entry for %s still present", eip)
	}
	for _, args := range eipForwardChecks(eip) {
		if _, err := exec.Command("sudo", append([]string{"-n", "iptables"}, args...)...).CombinedOutput(); err == nil {
			return fmt.Errorf("iptables rule still present: %s", strings.Join(args, " "))
		}
	}
	return nil
}

func eipForwardChecks(eip string) [][]string {
	return [][]string{
		{"-C", "FORWARD", "-i", transitHostEnd, "-s", eip + "/32",
			"-m", "comment", "--comment", "spinifex-eip-ingress", "-j", "ACCEPT"},
		{"-C", "FORWARD", "-o", transitHostEnd, "-d", eip + "/32",
			"-m", "comment", "--comment", "spinifex-eip-ingress", "-j", "ACCEPT"},
	}
}

// assertDNATExempt checks the dnat_and_snat row for eip exists and carries
// the exempt Address_Set ref (so Tier 1 host->private-IP flows keep working
// for EIP-holding instances).
func assertDNATExempt(t *testing.T, eip string) {
	t.Helper()
	exemptRef := strings.TrimSpace(harness.OvnNbctl(t, "--bare", "--columns=exempted_ext_ips",
		"find", "nat", "type=dnat_and_snat", "external_ip="+eip))
	require.NotEmptyf(t, exemptRef, "dnat_and_snat for %s missing or has no exempted_ext_ips ref", eip)
}

// sshHandshake dials hostPort:22 and reads the SSH banner prefix.
func sshHandshake(host string) error {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "22"), 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s:22: %w", host, err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	banner := make([]byte, 4)
	if _, err := io.ReadFull(conn, banner); err != nil {
		return fmt.Errorf("read ssh banner: %w", err)
	}
	if string(banner) != "SSH-" {
		return fmt.Errorf("unexpected banner prefix %q", string(banner))
	}
	return nil
}

// --- Helpers ---------------------------------------------------------------

// hostCmd runs a local (non-sudo) command and fails the test on error.
func hostCmd(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	require.NoErrorf(t, err, "%s %s: %s", name, strings.Join(args, " "), string(out))
	return string(out)
}

func configPath(env *harness.Env) string {
	if env.ConfigDir != "" {
		return filepath.Join(env.ConfigDir, "spinifex.toml")
	}
	return os.ExpandEnv("$HOME/spinifex/config/spinifex.toml")
}

// readExternalMode extracts external_mode from the [network] block of
// spinifex.toml. Returns "" when the file or key is absent.
func readExternalMode(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	inNetwork := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			inNetwork = line == "[network]"
			continue
		}
		if !inNetwork || !strings.HasPrefix(line, "external_mode") {
			continue
		}
		if i := strings.IndexByte(line, '='); i >= 0 {
			return strings.Trim(strings.TrimSpace(line[i+1:]), "\"'")
		}
	}
	return ""
}

// hasPublicPool reports whether spinifex.toml carries an external pool other
// than the transit pool — the marker that the Tier 2 (EIP / public IP) lane
// is configured on this node.
func hasPublicPool(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	inPool := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			inPool = line == "[[network.external_pools]]"
			continue
		}
		if !inPool || !strings.HasPrefix(line, "name") {
			continue
		}
		if i := strings.IndexByte(line, '='); i >= 0 {
			name := strings.Trim(strings.TrimSpace(line[i+1:]), "\"'")
			if name != "nat-transit" {
				return true
			}
		}
	}
	return false
}

// gatewayLRPIP returns the IP (sans prefix) of gw-<vpcID>'s first network,
// or "" when the LRP does not exist yet.
func gatewayLRPIP(t *testing.T, vpcID string) string {
	t.Helper()
	out := harness.OvnNbctl(t, "--bare", "--columns=networks",
		"find", "logical_router_port", "name=gw-"+vpcID)
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return ""
	}
	ip, _, ok := strings.Cut(fields[0], "/")
	if !ok {
		return fields[0]
	}
	return ip
}

func describeVPCCIDR(t *testing.T, fix *fixture, vpcID string) string {
	t.Helper()
	out, err := fix.aws.EC2.DescribeVpcs(&ec2.DescribeVpcsInput{
		VpcIds: []*string{aws.String(vpcID)},
	})
	require.NoError(t, err, "describe-vpcs %s", vpcID)
	require.NotEmpty(t, out.Vpcs, "vpc %s not found", vpcID)
	return aws.StringValue(out.Vpcs[0].CidrBlock)
}

func consoleOutput(t *testing.T, fix *fixture, instanceID string) string {
	t.Helper()
	out, err := fix.aws.EC2.GetConsoleOutput(&ec2.GetConsoleOutputInput{
		InstanceId: aws.String(instanceID),
	})
	if err != nil {
		return ""
	}
	encoded := aws.StringValue(out.Output)
	if encoded == "" {
		return ""
	}
	raw, derr := base64.StdEncoding.DecodeString(encoded)
	if derr != nil {
		return encoded
	}
	return string(raw)
}
