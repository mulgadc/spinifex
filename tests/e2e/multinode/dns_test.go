//go:build e2e

package multinode

import (
	"context"
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	nsconfig "github.com/mulgadc/northstar/pkg/config"
	spinconfig "github.com/mulgadc/spinifex/spinifex/config"
	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

const (
	northstarUnit       = "spinifex-northstar.service"
	northstarConfigPath = "/etc/spinifex/northstar/northstar.toml"
)

type dnsGuest struct {
	ID        string
	PrivateIP string
	PublicIP  string
	Node      harness.Node
	SSH       harness.SSHTarget
}

// runMultinodeDNS proves every node serves one converged Northstar topology and
// guests retain DNS when the first resolver backend is unavailable.
func runMultinodeDNS(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Northstar DNS")
	require.Len(t, fix.Cluster.Nodes, 3, "DNS cross-node coverage requires the three-node fixture")

	harness.Step(t, "D1: assert Northstar is active on every node")
	for _, node := range fix.Cluster.Nodes {
		harness.AssertUnitActive(t, node, northstarUnit)
	}

	harness.Step(t, "D2: assert every node carries the cluster-wide Northstar configuration")
	configs := make(map[string]*spinconfig.ClusterConfig, len(fix.Cluster.Nodes))
	for _, node := range fix.Cluster.Nodes {
		cc := harness.PeerClusterConfig(t, node)
		configs[node.Name] = cc
		require.Equalf(t, node.Name, cc.Node, "local node identity in %s config", node.Name)
		for name, cfg := range cc.Nodes {
			require.Equalf(t, northstarConfigPath, cfg.Northstar.ConfigPath,
				"Northstar config path for %s as viewed from %s", name, node.Name)
		}
		stat := runPeerCommand(t, node, "sudo stat -c '%a %U' -- "+northstarConfigPath)
		require.Equalf(t, "600 spinifex-northstar", strings.TrimSpace(stat),
			"Northstar credential file metadata on %s", node.Name)
	}

	firstNode := fix.Cluster.Nodes[0]
	baseDomain, internalDomain := harness.PeerNorthstarDomains(t, firstNode)
	firstConfig := configs[firstNode.Name]
	expectedSeeds := make([]nsconfig.NameserverSeed, 0, len(fix.Cluster.Nodes))
	expectedResolvers := make([]string, 0, len(fix.Cluster.Nodes))
	for i, node := range fix.Cluster.Nodes {
		ip := net.ParseIP(addressHost(node.Addr))
		require.NotNilf(t, ip, "cluster node %s address %q is not an IP", node.Name, node.Addr)
		require.Falsef(t, ip.IsLoopback() || ip.IsUnspecified(), "cluster node %s has unusable DNS address %s", node.Name, ip)
		expectedResolvers = append(expectedResolvers, ip.String())
		expectedSeeds = append(expectedSeeds, nsconfig.NameserverSeed{Host: fmt.Sprintf("ns%d", i+1), IP: ip.String()})
	}
	expectedTopology := seedTopology(baseDomain, expectedSeeds)

	harness.Step(t, "D3: query identical base-zone NS topology from every node")
	for _, node := range fix.Cluster.Nodes {
		peerBase, peerInternal := harness.PeerNorthstarDomains(t, node)
		require.Equalf(t, baseDomain, peerBase, "base domain differs on %s", node.Name)
		require.Equalf(t, internalDomain, peerInternal, "internal domain differs on %s", node.Name)
		require.Equalf(t, expectedSeeds, handlers_dns.NameserverSeeds(configs[node.Name]),
			"nameserver seeds differ on %s", node.Name)
		require.Equalf(t, expectedResolvers, handlers_dns.ResolverNameserverIPs(configs[node.Name]),
			"resolver backends differ on %s", node.Name)

		for _, port := range []string{"53", "5300"} {
			var topology []string
			harness.EventuallyErr(t, func() error {
				var err error
				topology, err = queryNSTopology(node.Addr, port, baseDomain)
				if err != nil {
					return err
				}
				if !slices.Equal(topology, expectedTopology) {
					return fmt.Errorf("%s:%s topology %v, want %v", node.Name, port, topology, expectedTopology)
				}
				return nil
			}, 90*time.Second, 3*time.Second)
			harness.Detail(t, "node", node.Name, "port", port, "topology", topology)
		}
	}

	harness.Step(t, "launch one DNS test guest on each of node1, node2, and node3")
	guests := launchDNSGuests(t, fix)
	source := guests[fix.Cluster.Nodes[1].Name]
	target := guests[fix.Cluster.Nodes[2].Name]
	failoverRecord := guests[fix.Cluster.Nodes[0].Name]

	region := strings.TrimSpace(firstConfig.AWS.Region)
	require.NotEmpty(t, region, "cluster AWS region is required to build internal DNS names")

	harness.Step(t, "D4: resolve internal and recursive names from a non-init-node guest")
	harness.AssertGuestResolver(t, source.SSH)
	sourceName := handlers_dns.EC2PrivateName(source.PrivateIP, region, internalDomain)
	assertGuestIPv4(t, source.SSH, sourceName, source.PrivateIP)
	assertGuestIPv4(t, source.SSH, "google.com", "")

	harness.Step(t, "D5: resolve a node3 guest record from the node2 guest and every backend")
	targetName := handlers_dns.EC2PrivateName(target.PrivateIP, region, internalDomain)
	assertGuestIPv4(t, source.SSH, targetName, target.PrivateIP)
	for _, node := range fix.Cluster.Nodes {
		assertNodeIPv4(t, node, targetName, target.PrivateIP)
	}

	harness.Step(t, "D6: stop the first resolver backend and retain guest DNS")
	firstBackend := &fix.Cluster.Nodes[0]
	restored := false
	t.Cleanup(func() {
		if restored {
			return
		}
		if _, err := peerCommand(*firstBackend, "sudo systemctl start "+northstarUnit); err != nil {
			t.Errorf("restore %s on %s: %v", northstarUnit, firstBackend.Name, err)
			return
		}
		state, err := harness.NodeUnitState(*firstBackend, northstarUnit)
		if err != nil || state != "active" {
			t.Errorf("restore %s on %s: state=%q err=%v", northstarUnit, firstBackend.Name, state, err)
		}
	})

	// Converge a fresh record on every backend before stopping one. The guest has
	// not queried this name, so its first lookup cannot be satisfied from cache.
	failoverName := handlers_dns.EC2PrivateName(failoverRecord.PrivateIP, region, internalDomain)
	for _, node := range fix.Cluster.Nodes {
		assertNodeIPv4(t, node, failoverName, failoverRecord.PrivateIP)
	}
	publicFailoverName := handlers_dns.EC2PublicName(source.PublicIP, region, baseDomain)
	for _, node := range fix.Cluster.Nodes {
		assertNodeIPv4(t, node, publicFailoverName, source.PublicIP)
	}

	runPeerCommand(t, *firstBackend, "sudo systemctl stop "+northstarUnit)
	state, _ := harness.NodeUnitState(*firstBackend, northstarUnit)
	require.Equalf(t, "inactive", state, "%s did not stop on %s", northstarUnit, firstBackend.Name)
	require.Error(t, queryDNS(*firstBackend, "5300", failoverName, dns.TypeA),
		"stopped backend still answered on the guest-forwarder port")

	out, err := guestIPv4(source.SSH, failoverName, failoverRecord.PrivateIP)
	require.NoErrorf(t, err, "first guest lookup did not fail over after stopping %s: %s", firstBackend.Name, out)
	elapsed, out, err := timedGuestIPv4(source.SSH, publicFailoverName, source.PublicIP)
	require.NoErrorf(t, err, "second guest lookup failed during backend cooldown: %s", out)
	require.Lessf(t, elapsed, 1500*time.Millisecond,
		"down backend was not deprioritised; second lookup took %s: %s", elapsed, out)

	runPeerCommand(t, *firstBackend, "sudo systemctl start "+northstarUnit)
	harness.AssertUnitActive(t, *firstBackend, northstarUnit)
	restored = true
}

func seedTopology(domain string, seeds []nsconfig.NameserverSeed) []string {
	fqdn := dns.Fqdn(domain)
	topology := make([]string, 0, len(seeds)*2)
	for _, seed := range seeds {
		host := dns.Fqdn(seed.Host + "." + fqdn)
		topology = append(topology, "NS "+host, "A "+host+"="+seed.IP)
	}
	sort.Strings(topology)
	return topology
}

func queryNSTopology(host, port, domain string) ([]string, error) {
	query := new(dns.Msg)
	query.SetQuestion(dns.Fqdn(domain), dns.TypeNS)
	response, _, err := (&dns.Client{Timeout: 3 * time.Second}).Exchange(query, net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("query NS %s via %s: %w", domain, host, err)
	}
	if response.Rcode != dns.RcodeSuccess {
		return nil, fmt.Errorf("query NS %s via %s: rcode %s", domain, host, dns.RcodeToString[response.Rcode])
	}
	if !response.Authoritative {
		return nil, fmt.Errorf("query NS %s via %s: response is not authoritative", domain, host)
	}

	topology := make([]string, 0, len(response.Answer)+len(response.Extra))
	for _, record := range response.Answer {
		if ns, ok := record.(*dns.NS); ok {
			topology = append(topology, "NS "+dns.Fqdn(ns.Ns))
		}
	}
	for _, record := range response.Extra {
		if a, ok := record.(*dns.A); ok {
			topology = append(topology, "A "+dns.Fqdn(a.Hdr.Name)+"="+a.A.String())
		}
	}
	sort.Strings(topology)
	return topology, nil
}

func launchDNSGuests(t *testing.T, fix *Fixture) map[string]dnsGuest {
	t.Helper()
	_, defaultSG, subnetID := harness.DiscoverDefaultVPC(t, fix.AWS)
	harness.AuthorizeSSHIngress(t, fix.AWS, defaultSG)
	instanceType, arch := needInstanceTypeArch(t, fix)
	amiID := needAMI(t, fix, arch)
	keyName, keyPath := needKeyPair(t, fix)

	wanted := make(map[string]harness.Node, 3)
	for _, node := range fix.Cluster.Nodes[:3] {
		wanted[node.Name] = node
	}
	guests := make(map[string]dnsGuest, len(wanted))
	for attempt := 1; attempt <= 9 && len(guests) < len(wanted); attempt++ {
		id := baselineLaunch(t, fix, amiID, instanceType, keyName, subnetID, []string{defaultSG})
		host := harness.InstanceHostingNode(t, fix.Cluster, id)
		if host == nil {
			t.Logf("DNS guest %s hosting node was not discoverable", id)
			continue
		}
		if _, needed := wanted[host.Name]; !needed {
			continue
		}
		if _, exists := guests[host.Name]; exists {
			t.Logf("DNS guest %s colocated on already-covered %s", id, host.Name)
			continue
		}
		guests[host.Name] = dnsGuest{
			ID:        id,
			PrivateIP: instancePrivateIP(t, fix, id),
			Node:      *host,
		}
	}
	for name := range wanted {
		require.Containsf(t, guests, name, "could not place a DNS guest on %s after bounded rescans", name)
	}

	source := guests[fix.Cluster.Nodes[1].Name]
	host, port := harness.GuestSSHEndpoint(t, fix.AWS, fix.Cluster, source.ID)
	harness.GuestSSHReady(t, host, port, "ubuntu", keyPath,
		harness.WithTimeout(3*time.Minute), harness.WithPoll(3*time.Second))
	source.PublicIP = host
	source.SSH = harness.SSHTarget{User: "ubuntu", Host: host, Port: port, KeyPath: keyPath}
	guests[source.Node.Name] = source
	return guests
}

func assertGuestIPv4(t *testing.T, target harness.SSHTarget, name, expectedIP string) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		_, err := guestIPv4(target, name, expectedIP)
		return err
	}, 2*time.Minute, 3*time.Second)
}

func guestIPv4(target harness.SSHTarget, name, expectedIP string) ([]byte, error) {
	out, err := runGuestCommand(target, "getent ahostsv4 "+shellQuoteDNS(name))
	if err != nil {
		return out, err
	}
	if outputContainsIPv4(out, expectedIP) {
		return out, nil
	}
	return out, fmt.Errorf("getent ahostsv4 %s did not return %q: %s", name, expectedIP, out)
}

func timedGuestIPv4(target harness.SSHTarget, name, expectedIP string) (time.Duration, []byte, error) {
	command := "start=$(date +%s%N); getent ahostsv4 " + shellQuoteDNS(name) +
		"; status=$?; end=$(date +%s%N); echo __elapsed_ms=$(((end-start)/1000000)); exit $status"
	out, err := runGuestCommand(target, command)
	if err != nil {
		return 0, out, err
	}
	if !outputContainsIPv4(out, expectedIP) {
		return 0, out, fmt.Errorf("getent ahostsv4 %s did not return %q: %s", name, expectedIP, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "__elapsed_ms=") {
			continue
		}
		var milliseconds int64
		if _, err := fmt.Sscanf(line, "__elapsed_ms=%d", &milliseconds); err != nil {
			return 0, out, fmt.Errorf("parse DNS lookup duration %q: %w", line, err)
		}
		return time.Duration(milliseconds) * time.Millisecond, out, nil
	}
	return 0, out, fmt.Errorf("DNS lookup output omitted elapsed time: %s", out)
}

func outputContainsIPv4(out []byte, expectedIP string) bool {
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		ip := net.ParseIP(fields[0])
		if ip != nil && ip.To4() != nil && (expectedIP == "" || fields[0] == expectedIP) {
			return true
		}
	}
	return false
}

func assertNodeIPv4(t *testing.T, node harness.Node, name, expectedIP string) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		query := new(dns.Msg)
		query.SetQuestion(dns.Fqdn(name), dns.TypeA)
		response, _, err := (&dns.Client{Timeout: 3 * time.Second}).Exchange(query, net.JoinHostPort(node.Addr, "5300"))
		if err != nil {
			return fmt.Errorf("query A %s via %s: %w", name, node.Name, err)
		}
		if response.Rcode != dns.RcodeSuccess {
			return fmt.Errorf("query A %s via %s: rcode %s", name, node.Name, dns.RcodeToString[response.Rcode])
		}
		for _, record := range response.Answer {
			if a, ok := record.(*dns.A); ok && a.A.String() == expectedIP {
				return nil
			}
		}
		return fmt.Errorf("query A %s via %s did not return %s: %v", name, node.Name, expectedIP, response.Answer)
	}, 2*time.Minute, 3*time.Second)
}

func runGuestCommand(target harness.SSHTarget, command string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return harness.RunGuestSSH(ctx, target, command)
}

func runPeerCommand(t *testing.T, node harness.Node, command string) string {
	t.Helper()
	out, err := peerCommand(node, command)
	require.NoErrorf(t, err, "%s on %s: %s", command, node.Name, out)
	return string(out)
}

func peerCommand(node harness.Node, command string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return harness.NewPeerSSH().Run(ctx, node.Addr, command)
}

func queryDNS(node harness.Node, port, name string, queryType uint16) error {
	query := new(dns.Msg)
	query.SetQuestion(dns.Fqdn(name), queryType)
	_, _, err := (&dns.Client{Timeout: 3 * time.Second}).Exchange(query, net.JoinHostPort(node.Addr, port))
	return err
}

func addressHost(address string) string {
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return strings.Trim(address, "[]")
}

func shellQuoteDNS(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
