//go:build e2e

// Package natuplink is the routed-NAT (external_mode = "nat") suite. It runs
// ON the spinifex node itself (like the single suite): the host-wiring and OVN
// phases shell out to ip/iptables/ovn-nbctl locally, and default egress is
// proven via the serial console since an instance with no public IP has no
// inbound path. Routed mode does carry public IPs when a public pool is
// configured, and phase 9 walks that ingress path.
//
// On a multi-node cluster phase 9 becomes the distributed lane instead: one
// guest per node, each public IP delivered by the node running its guest and by
// no other. The two phases that need host-to-guest delivery over the transit
// veth run only where that is possible, since ext-shared is a localnet and the
// segment does not leave the chassis holding the VPC's gateway router port.
//
// Every node must be provisioned with `setup-ovn.sh --nat-uplink` followed by
// `spx admin init --external-mode=nat`; the suite skips unless spinifex.toml
// carries external_mode = "nat".
package natuplink

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/network/host"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	transitCIDR      = "100.127.0.0/24"
	transitGatewayIP = "100.127.0.1"
	uplinkBridge     = "br-ext"

	// Taken from the package the daemon uses, not copied: the transit MAC in
	// particular is one address cluster-wide, and a stale duplicate here would
	// assert the wrong one everywhere at once.
	transitHostEnd = host.NATTransitHostEnd
	transitOVSEnd  = host.NATTransitOVSEnd
	transitHostMAC = host.NATTransitHostMAC

	egressOKMarker   = "NAT-E2E-EGRESS-OK"
	egressFailMarker = "NAT-E2E-EGRESS-FAIL"

	secondVPCCIDR    = "10.213.0.0/16"
	natExemptSetName = "spinifex_nat_exempt"
)

// A failing iteration costs two 10 s curl timeouts, a 3 s ping and a 5 s
// sleep — ~28 s measured on the console clock. The verdict budget must cover
// the whole loop plus boot and cloud-init, or the fail marker never prints
// inside the window the test watches.
const (
	egressProbeIterations = 10
	egressIterationBudget = 30 * time.Second
	egressBootBudget      = 3 * time.Minute
	egressVerdictBudget   = egressProbeIterations*egressIterationBudget + egressBootBudget
)

// egressUserData probes outbound WAN reachability from inside the guest and
// reports the verdict on the serial console — the only channel back to the
// test in nat mode. ping 8.8.8.8 is the DNS-free fallback; the curl targets
// additionally prove DHCP-delivered DNS works through the masquerade.
var egressUserData = fmt.Sprintf(`#!/bin/bash
for i in $(seq 1 %d); do
  if curl -fsS -m 10 -o /dev/null http://connectivity-check.ubuntu.com/ \
     || curl -fsS -m 10 -o /dev/null http://www.google.com/generate_204 \
     || ping -c 1 -W 3 8.8.8.8 >/dev/null 2>&1; then
    echo "%s" | tee /dev/console
    exit 0
  fi
  sleep 5
done
echo "%s" | tee /dev/console
`, egressProbeIterations, egressOKMarker, egressFailMarker)

type fixture struct {
	env        *harness.Env
	aws        *harness.AWSClient
	harness    *harness.Fixture
	artifacts  string
	configTOML string
	// publicPool is true when spinifex.toml carries a non-transit pool —
	// the Tier 2 (EIP / public IP) lane is exercised only then.
	publicPool bool
	// cluster is nil on a single node and is what distinguishes the two
	// shapes of phase 9: the lifecycle on one host, or delivery across all.
	cluster *harness.Cluster
}

// TestNATUplink runs the routed-NAT lane end to end. Phases are sequential:
// wiring must hold before config, config before API behaviour, and the OVN
// phases read state the earlier phases prove exists.
func TestNATUplink(t *testing.T) {
	env := harness.LoadEnv(t)
	if env.Mode != harness.ModeSingle && env.Mode != harness.ModeMultinode {
		t.Skipf("natuplink suite runs on single and multinode clusters (mode=%s)", env.Mode)
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
	if env.Mode == harness.ModeMultinode {
		cluster, cerr := harness.ClusterFromEnv()
		require.NoError(t, cerr, "multinode run needs SPINIFEX_NODES/SSH_USER/SSH_KEY")
		fix.cluster = cluster
		harness.Detail(t, "nodes", len(cluster.Nodes))
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

	// ext-shared is a localnet, so the transit segment never leaves the node:
	// only the chassis holding the VPC's gateway router port can reach the
	// gateway LRP. On one node that is always this host.
	gwLocal := fix.cluster == nil || localIsGatewayChassis(t, def.VPCID)

	harness.Phase(t, "NAT Uplink — Phase 6: instance boots and reaches WAN outbound")
	probe := phaseInstanceEgress(t, fix, def)

	// Tier 1 ingress needs both ends here: the gateway LRP is reachable from
	// its own chassis alone, and the guest has to be one this host is running.
	switch {
	case gwLocal && (fix.cluster == nil || hostsInstanceLocally(t, fix, probe.instanceID)):
		harness.Phase(t, "NAT Uplink — Phase 7: host reaches instance private IP (Tier 1 ingress)")
		phaseHostIngress(t, fix, def, probe)
	case gwLocal:
		t.Logf("Phase 7 (Tier 1 host ingress) skipped: %s is running on another node", probe.instanceID)
	default:
		t.Log("Phase 7 (Tier 1 host ingress) skipped: the gateway router port for this VPC is on another chassis")
	}

	harness.Phase(t, "NAT Uplink — Phase 8: second VPC gets a unique transit gateway IP")
	phaseUniqueTransitIP(t, fix, defaultGwIP)

	switch {
	case !fix.publicPool:
		t.Log("Phase 9 (Tier 2 EIP ingress) skipped: no public pool in config")
	case fix.cluster != nil:
		harness.Phase(t, "NAT Uplink — Phase 9: every node delivers its own guests' public IPs")
		phaseDistributedEIPs(t, fix, def)
	default:
		harness.Phase(t, "NAT Uplink — Phase 9: EIP ingress lifecycle (Tier 2)")
		phaseEIPIngress(t, fix, def, probe)
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

	// -C says the rule exists, never where. Ubuntu ships a catch-all REJECT in
	// FORWARD, so a present-but-appended ACCEPT is dead and guests lose egress
	// with every rule reporting healthy.
	harness.Step(t, "NAT egress FORWARD rules precede any catch-all reject")
	fwdOut, err := exec.Command("sudo", "-n", "iptables", "-t", "filter", "-S", "FORWARD").CombinedOutput()
	require.NoErrorf(t, err, "iptables -S FORWARD: %s", string(fwdOut))
	firstDeny, lastAccept := -1, -1
	for i, line := range strings.Split(strings.TrimSpace(string(fwdOut)), "\n") {
		switch {
		case strings.HasPrefix(line, "-A FORWARD") && strings.Contains(line, "spinifex-nat-egress"):
			lastAccept = i
		case strings.HasPrefix(line, "-A FORWARD") && !strings.Contains(line, "-j LIBVIRT") &&
			(strings.Contains(line, "-j REJECT") || strings.Contains(line, "-j DROP")):
			if firstDeny < 0 {
				firstDeny = i
			}
		}
	}
	if firstDeny >= 0 {
		assert.Lessf(t, lastAccept, firstDeny,
			"spinifex-nat-egress ACCEPT rules must sit above the first REJECT/DROP in FORWARD\n%s", string(fwdOut))
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

	// Whether the gateway router resolved the transit nexthop is the decisive
	// question when egress fails, and nothing else in the bundle answers it.
	harness.OnFailure(t, func() { dumpOVNRouting(t, fix.artifacts, def.VPCID) })

	instType, arch := harness.DiscoverNanoInstanceType(t, fix.harness)
	amiID := harness.DiscoverUbuntuAMI(t, fix.harness, arch)
	keyName, _ := harness.EnsureKeyPair(t, fix.harness)
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

	awaitEgressVerdict(t, fix, instanceID)

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

	// Every node installs this route, not only the gateway chassis: the nexthop
	// is the same transit address everywhere, and a node that never held the
	// gateway port still has to reach the VPC the day it takes it over.
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

	harness.Step(t, "auto-assigned public IP %s has host delivery plumbing", probe.publicIP)
	require.NotEmpty(t, probe.publicIP, "phase 6 probe carries no public IP")
	harness.EventuallyErr(t, func() error {
		return eipHostPlumbing(probe.publicIP)
	}, 2*time.Minute, 3*time.Second)
	assertDNATExempt(t, probe.publicIP)
	assertDistributedEIP(t, probe.publicIP)

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
		return eipHostPlumbing(eip)
	}, 2*time.Minute, 3*time.Second)
	assertDNATExempt(t, eip)
	assertDistributedEIP(t, eip)

	// EC2 releases an instance's auto-assigned public IPv4 when an EIP is
	// associated: "that public IPv4 address is released back into Amazon's pool
	// ... You cannot reuse the public IPv4 address previously associated".
	harness.Step(t, "associating the EIP released the auto-assigned %s", probe.publicIP)
	harness.EventuallyErr(t, func() error {
		return eipHostPlumbingGone(probe.publicIP)
	}, 2*time.Minute, 3*time.Second)

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
			return eipHostPlumbing(eip)
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

	// The auto-assigned address went back to the pool at associate time, so
	// disassociating leaves the instance with no public delivery at all. It does
	// not come back — matching EC2, where a stop/start is what issues a new one.
	harness.Step(t, "no public delivery remains after the EIP teardown")
	require.NoError(t, eipHostPlumbingGone(probe.publicIP))
}

// --- Phase 9 (multi-node): every node delivers its own guests ----------------

// spreadGuest is one phase-9 guest and the node its QEMU process runs on.
type spreadGuest struct {
	instanceID string
	publicIP   string
	node       harness.Node
}

// phaseDistributedEIPs proves what a single-node run cannot see: with a guest
// on every node, each public IP is delivered by the node running that guest and
// by no other. It guards the two defects that made routed NAT work on one node
// only — a per-node transit MAC against OVN's single cluster-wide binding for
// the nexthop, and host EIP plumbing that ran on the reconcile leader alone.
func phaseDistributedEIPs(t *testing.T, fix *fixture, def harness.VPCInfo) {
	t.Helper()
	nodes := fix.cluster.Nodes
	ssh := harness.NewPeerSSH()

	harness.Step(t, "every node's transit veth carries the shared MAC %s", transitHostMAC)
	for _, n := range nodes {
		link := nodeCmd(t, ssh, n, "ip -o link show dev "+transitHostEnd)
		assert.Containsf(t, strings.ToLower(link), transitHostMAC,
			"%s: OVN holds one MAC binding for %s cluster-wide, so a node keeping its own address receives none of the egress that binding points at\n%s",
			n.Name, transitGatewayIP, link)
	}

	for _, g := range launchSpreadGuests(t, fix, def, len(nodes)) {
		harness.Step(t, "%s on %s carries public IP %s", g.instanceID, g.node.Name, g.publicIP)
		assertDistributedEIP(t, g.publicIP)
		assertSoleEIPHolder(t, ssh, nodes, g)

		harness.Step(t, "TCP handshake to %s:22 — inbound over %s", g.publicIP, g.node.Name)
		harness.EventuallyErr(t, func() error {
			return sshHandshake(g.publicIP)
		}, 2*time.Minute, 5*time.Second)

		awaitEgressVerdict(t, fix, g.instanceID)
	}
}

// launchSpreadGuests boots one egress-probing guest per node in a spread
// placement group and returns each with the node running it. Colocation fails
// the phase: two guests on one node prove nothing about the second node's
// datapath, which is the whole subject here.
func launchSpreadGuests(t *testing.T, fix *fixture, def harness.VPCInfo, count int) []spreadGuest {
	t.Helper()

	instType, arch := harness.DiscoverNanoInstanceType(t, fix.harness)
	amiID := harness.DiscoverUbuntuAMI(t, fix.harness, arch)
	keyName, _ := harness.EnsureKeyPair(t, fix.harness)
	harness.Detail(t, "type", instType, "ami", amiID, "key", keyName, "count", count)

	harness.Step(t, "create spread placement group + %d egress-probe guests", count)
	pgName := "natuplink-spread"
	// e2e:allow-create — scratch placement group owned end to end by this phase.
	_, err := fix.aws.EC2.CreatePlacementGroup(&ec2.CreatePlacementGroupInput{
		GroupName: aws.String(pgName), Strategy: aws.String("spread"),
	})
	require.NoError(t, err, "create-placement-group %s", pgName)
	t.Cleanup(func() {
		_, _ = fix.aws.EC2.DeletePlacementGroup(&ec2.DeletePlacementGroupInput{GroupName: aws.String(pgName)})
	})

	// e2e:allow-create — throwaway probe VMs; the egress-probe user-data makes them unshareable.
	runOut, err := fix.aws.EC2.RunInstances(&ec2.RunInstancesInput{
		ImageId:      aws.String(amiID),
		InstanceType: aws.String(instType),
		KeyName:      aws.String(keyName),
		SubnetId:     aws.String(def.SubnetID),
		MinCount:     aws.Int64(int64(count)),
		MaxCount:     aws.Int64(int64(count)),
		Placement:    &ec2.Placement{GroupName: aws.String(pgName)},
		UserData:     aws.String(base64.StdEncoding.EncodeToString([]byte(egressUserData))),
	})
	require.NoError(t, err, "run-instances x%d", count)
	require.Lenf(t, runOut.Instances, count, "run-instances returned %d instances", len(runOut.Instances))

	ids := make([]string, 0, count)
	for _, inst := range runOut.Instances {
		ids = append(ids, aws.StringValue(inst.InstanceId))
	}
	t.Cleanup(func() {
		_, _ = fix.aws.EC2.TerminateInstances(&ec2.TerminateInstancesInput{InstanceIds: aws.StringSlice(ids)})
		harness.WaitForInstanceTerminated(t, fix.aws, ids, 5*time.Minute)
	})

	guests := make([]spreadGuest, 0, count)
	owner := make(map[string]string, count)
	sgIDs := make(map[string]struct{}, 1)
	for _, id := range ids {
		inst := harness.WaitForInstanceState(t, fix.aws, id, "running")
		publicIP := aws.StringValue(inst.PublicIpAddress)
		require.NotEmptyf(t, publicIP, "instance %s must auto-assign a public IP (MapPublicIpOnLaunch)", id)
		for _, sg := range inst.SecurityGroups {
			sgIDs[aws.StringValue(sg.GroupId)] = struct{}{}
		}
		node := harness.InstanceHostingNode(t, fix.cluster, id)
		require.NotNilf(t, node, "no node runs a QEMU process for %s", id)
		require.Emptyf(t, owner[node.Name],
			"spread placement failed: %s and %s both landed on %s", id, owner[node.Name], node.Name)
		owner[node.Name] = id
		harness.Detail(t, "instance", id, "node", node.Name, "public_ip", publicIP)
		guests = append(guests, spreadGuest{instanceID: id, publicIP: publicIP, node: *node})
	}

	harness.Step(t, "open SSH from anywhere on %d security group(s)", len(sgIDs))
	perms := []*ec2.IpPermission{{
		IpProtocol: aws.String("tcp"), FromPort: aws.Int64(22), ToPort: aws.Int64(22),
		IpRanges: []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
	}}
	for sgID := range sgIDs {
		_, err := fix.aws.EC2.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: aws.String(sgID), IpPermissions: perms,
		})
		require.NoError(t, err, "authorize-security-group-ingress %s", sgID)
		t.Cleanup(func() {
			_, _ = fix.aws.EC2.RevokeSecurityGroupIngress(&ec2.RevokeSecurityGroupIngressInput{
				GroupId: aws.String(sgID), IpPermissions: perms,
			})
		})
	}
	return guests
}

// assertSoleEIPHolder proves g's public IP is plumbed on g's node and nowhere
// else. Two nodes holding the same address answer ARP for it from two places,
// which delivers the guest's traffic to the wrong host about half the time.
func assertSoleEIPHolder(t *testing.T, ssh *harness.PeerSSH, nodes []harness.Node, g spreadGuest) {
	t.Helper()
	routes := make(map[string]string, len(nodes))
	harness.EventuallyErr(t, func() error {
		holders := make([]string, 0, 1)
		for _, n := range nodes {
			route := strings.TrimSpace(nodeCmd(t, ssh, n, "ip route show "+g.publicIP+"/32"))
			routes[n.Name] = route
			if route != "" {
				holders = append(holders, n.Name)
			}
		}
		if len(holders) != 1 || holders[0] != g.node.Name {
			return fmt.Errorf("public IP %s plumbed by %v, want [%s] alone", g.publicIP, holders, g.node.Name)
		}
		return nil
	}, 2*time.Minute, 5*time.Second)

	route := routes[g.node.Name]
	assert.Containsf(t, route, "dev "+transitHostEnd,
		"%s: EIP route must point into OVN\n%s", g.node.Name, route)
	assert.NotContainsf(t, route, "via ",
		"%s: a distributed EIP is answered on-link by its own node, not forwarded to a gateway LRP\n%s",
		g.node.Name, route)

	proxy := nodeCmd(t, ssh, g.node, "ip neigh show proxy")
	assert.Containsf(t, proxy, g.publicIP,
		"%s must answer ARP for %s on its uplink\n%s", g.node.Name, g.publicIP, proxy)
}

// eipHostPlumbing returns nil when the full Tier 2 host state for eip is in
// place: the /32 route into OVN, a proxy-ARP neighbor on the uplink, and both
// per-EIP FORWARD accepts. The route is on-link — a distributed EIP is answered
// by this node's own transit veth, so no gateway LRP is in the path.
func eipHostPlumbing(eip string) error {
	out, _ := exec.Command("ip", "route", "show", eip+"/32").CombinedOutput()
	route := strings.TrimSpace(string(out))
	if !strings.Contains(route, "dev "+transitHostEnd) {
		return fmt.Errorf("EIP route for %s missing (want dev %s): %q", eip, transitHostEnd, route)
	}
	if strings.Contains(route, "via ") {
		return fmt.Errorf("EIP route for %s is forwarded to a gateway LRP, not answered on-link: %q", eip, route)
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

// assertDistributedEIP checks the NAT row for eip is the distributed kind.
// external_mac and logical_port are what let the chassis running the guest
// answer for the address itself; without them the row is centralised and only
// the gateway chassis can deliver it.
func assertDistributedEIP(t *testing.T, eip string) {
	t.Helper()
	var row string
	harness.EventuallyErr(t, func() error {
		row = harness.OvnNbctl(t, "--format=csv", "--no-headings", "--data=bare",
			"--columns=external_mac,logical_port", "find", "nat",
			"type=dnat_and_snat", "external_ip="+eip)
		if row == "" {
			return fmt.Errorf("no dnat_and_snat row for %s yet", eip)
		}
		if lines := strings.Split(row, "\n"); len(lines) != 1 {
			return fmt.Errorf("%d dnat_and_snat rows for %s, want 1:\n%s", len(lines), eip, row)
		}
		mac, port, _ := strings.Cut(row, ",")
		if strings.TrimSpace(mac) == "" || strings.TrimSpace(port) == "" {
			return fmt.Errorf("dnat_and_snat for %s is centralised (external_mac=%q logical_port=%q)",
				eip, strings.TrimSpace(mac), strings.TrimSpace(port))
		}
		return nil
	}, time.Minute, 3*time.Second)
	harness.Detail(t, "eip", eip, "external_mac,logical_port", row)
}

// localIsGatewayChassis reports whether this host holds the VPC's active
// gateway router port. Every node carries a gateway_chassis row for every VPC,
// so the row alone means nothing — the highest priority is the one OVN uses,
// and only that chassis reaches the gateway LRP over the transit veth, since
// ext-shared is a localnet and the segment stops at the node.
func localIsGatewayChassis(t *testing.T, vpcID string) bool {
	t.Helper()
	rows := harness.OvnNbctl(t, "--format=csv", "--no-headings", "--data=bare",
		"--columns=name,chassis_name,priority", "list", "gateway_chassis")
	best, gwChassis := -1, ""
	for _, row := range strings.Split(rows, "\n") {
		fields := strings.Split(strings.TrimSpace(row), ",")
		if len(fields) != 3 || !strings.Contains(fields[0], vpcID) {
			continue
		}
		prio, err := strconv.Atoi(fields[2])
		if err != nil || prio <= best {
			continue
		}
		best, gwChassis = prio, fields[1]
	}
	local := localChassis(t)
	harness.Detail(t, "vpc", vpcID, "gateway_chassis", gwChassis, "local_chassis", local)
	return gwChassis != "" && gwChassis == local
}

// localChassis is this host's OVN chassis name, which the multi-node bootstrap
// pins to the cluster node label so it compares with harness.Node.Name.
func localChassis(t *testing.T) string {
	t.Helper()
	return strings.Trim(harness.OvsVsctl(t, "get", "Open_vSwitch", ".", "external_ids:system-id"), `"`)
}

// hostsInstanceLocally reports whether this host runs the instance's QEMU.
func hostsInstanceLocally(t *testing.T, fix *fixture, instanceID string) bool {
	t.Helper()
	node := harness.InstanceHostingNode(t, fix.cluster, instanceID)
	if node == nil {
		return false
	}
	harness.Detail(t, "instance", instanceID, "node", node.Name, "local_chassis", localChassis(t))
	return node.Name == localChassis(t)
}

// awaitEgressVerdict blocks until the guest prints its egress marker on the
// serial console — the only channel back from a routed-NAT guest — and fails on
// the negative one. The console is saved either way a failure arrives.
func awaitEgressVerdict(t *testing.T, fix *fixture, instanceID string) {
	t.Helper()
	harness.Step(t, "wait for egress verdict from %s on the serial console", instanceID)
	var console string
	// Registered before the wait so a budget overrun — which Fatals inside
	// EventuallyErr — still leaves the last console read behind.
	harness.OnFailure(t, func() {
		harness.DumpFile(t, fix.artifacts, "egress-console-"+instanceID+".log", []byte(console))
	})
	harness.EventuallyErr(t, func() error {
		console = consoleOutput(t, fix, instanceID)
		if strings.Contains(console, egressOKMarker) || strings.Contains(console, egressFailMarker) {
			return nil
		}
		return fmt.Errorf("no egress marker on console yet (%d bytes)", len(console))
	}, egressVerdictBudget, 10*time.Second)

	if strings.Contains(console, egressFailMarker) {
		t.Fatalf("guest %s reported %s — outbound WAN unreachable through routed NAT (console saved to artifacts)",
			instanceID, egressFailMarker)
	}
	harness.Step(t, "guest %s reported %s", instanceID, egressOKMarker)
}

// nodeCmd runs cmd on a cluster node over SSH and fails the test on error.
func nodeCmd(t *testing.T, ssh *harness.PeerSSH, n harness.Node, cmd string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := ssh.Run(ctx, n.Addr, cmd)
	require.NoErrorf(t, err, "%s: %s", n.Name, cmd)
	return string(out)
}

// sshHandshake dials addr:22 and reads the SSH banner prefix.
func sshHandshake(addr string) error {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(addr, "22"), 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial %s:22: %w", addr, err)
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

// dumpOVNRouting writes the gateway router's nexthop and routing state to the
// artifact bundle. Best-effort: DumpCmd records a non-zero exit rather than
// failing, so a half-built router still yields whatever OVN has.
func dumpOVNRouting(t *testing.T, dir, vpcID string) {
	t.Helper()
	harness.DumpCmd(t, dir, "ovn-static-mac-binding-list.txt",
		"sudo", "-n", "ovn-nbctl", "static-mac-binding-list")
	harness.DumpCmd(t, dir, "ovn-sb-mac-binding.txt",
		"sudo", "-n", "ovn-sbctl", "list", "MAC_Binding")
	harness.DumpCmd(t, dir, "ovn-lr-route-list.txt",
		"sudo", "-n", "ovn-nbctl", "lr-route-list", "vpc-"+vpcID)
}

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
