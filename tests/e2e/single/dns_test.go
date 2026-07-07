//go:build e2e

package single

import (
	"time"

	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// runGuestDNSResolution exercises the guest resolver datapath end-to-end from
// inside a running instance. It launches one instance, SSHes in over its public
// IP, and over that session:
//
//  1. pings the instance's own private IP (from DescribeInstances) to prove the
//     local ICMP datapath before DNS is exercised, and
//  2. resolves public hostnames (google.com, cloudflare.com) through the guest
//     resolver, then pings them.
//
// Resolution is asserted separately from the ping via `getent ahostsv4` (AF_INET
// so it forces an A-record answer, not the AAAA the resolver may otherwise
// prefer), which drives the same NSS -> /etc/resolv.conf (169.254.169.253,
// served by the per-tap shim once P7 is deployed) -> northstar recursion path
// guest apps use and is always present (nslookup/dig are not on the minimal
// cloud image). The
// getent step is the northstar signal; the follow-on ping only adds ICMP-egress
// coverage — so a resolver/northstar failure is isolated from a WAN-egress
// failure. The own private IP ping isolates both from a plain local-datapath
// failure. The public inbound path is already proven by the SSH over the public
// IP, and a guest cannot ping its own public IP (gateway NAT, no hairpin — AWS
// behaves the same), so no own-public-IP ping is attempted.
func runGuestDNSResolution(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — Guest DNS end-to-end: resolver + ICMP egress")

	vpcID, _, subnetID := harness.DiscoverDefaultVPC(t, fix.AWS)
	instType, _ := needInstanceTypeArch(t, fix)
	keyName, keyPath := needKeyPair(t, fix)
	ami := needAMI(t, fix)

	// Dedicated SG admitting tcp/22 from the runner so we can SSH in; egress is
	// allow-all by default so the guest reaches its resolver and the WAN.
	sgID := harness.EnsureSG(t, fix.Harness, vpcID, "dns-e2e-sg")
	harness.AuthorizeSSHIngress(t, fix.AWS, sgID)
	harness.Detail(t, "vpc", vpcID, "subnet", subnetID, "sg", sgID)

	instanceID := launchBaselineInstance(t, fix, ami, instType, keyName, subnetID, []string{sgID})

	pubIP := instancePublicIP(t, fix, instanceID)
	privIP := instancePrivateIP(t, fix, instanceID)
	harness.Detail(t, "instance", instanceID, "public_ip", pubIP, "private_ip", privIP)

	require.Truef(t, trySSHReady(pubIP, 22, keyPath, sshReadyBudget),
		"tcp/22 to %s never became reachable after authorizing ingress", pubIP)

	tgt := harness.SSHTarget{User: "ubuntu", Host: pubIP, Port: 22, KeyPath: keyPath}

	// Step 1: ping the instance's own private IP — local datapath sanity, no DNS
	// involved. (Own public IP is unreachable from inside via gateway NAT, and the
	// public inbound path is already proven by the SSH above.)
	harness.Step(t, "ping own private IP %s from guest", privIP)
	out, ok := pingConverged(tgt, privIP, 30*time.Second)
	require.Truef(t, ok,
		"guest ping to own private IP %s never reached 0%% loss within 30s\n%s",
		privIP, out)

	// Step 2: resolve each public hostname through the guest resolver (the
	// northstar path), then ping it. getent hosts is the DNS assertion; the ping
	// only adds egress coverage — splitting them isolates a resolver/northstar
	// failure from a WAN-egress failure.
	for _, host := range []string{"google.com", "cloudflare.com"} {
		harness.Step(t, "resolve %s via guest resolver (northstar path)", host)
		res, err := sshCapture(tgt, "getent ahostsv4 "+host)
		require.NoErrorf(t, err,
			"guest failed to resolve %s — DNS path (resolver -> northstar) is broken\n%s",
			host, res)
		require.Regexpf(t, `\d{1,3}(\.\d{1,3}){3}`, res,
			"resolve %s returned no IPv4 address — northstar answered without an A record\n%s",
			host, res)

		harness.Step(t, "ping %s (ICMP egress after resolution)", host)
		out, ok := pingConverged(tgt, host, 45*time.Second)
		require.Truef(t, ok,
			"guest ping to %s never reached 0%% loss within 45s — WAN egress is broken\n%s",
			host, out)
	}
}
