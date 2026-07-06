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
//  1. pings the instance's own private and public IPs (from DescribeInstances)
//     to prove the local ICMP datapath before DNS is exercised, and
//  2. pings public hostnames (google.com, cloudflare.com), forcing guest-side
//     name resolution (resolver -> northstar recursion) followed by ICMP egress.
//
// Public-hostname pings validate that the DHCP-advertised resolver
// (169.254.169.253, served by the per-tap shim once P7 is deployed) returns a
// routable answer and that egress works. The instance's own IPs isolate a DNS
// failure from a plain datapath failure when the hostname pings fail.
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

	tgt := harness.SSHTarget{User: "ec2-user", Host: pubIP, Port: 22, KeyPath: keyPath}

	// Step 1: ping the instance's own IPs — datapath sanity, no DNS involved.
	for _, self := range []struct{ label, dst string }{
		{"private", privIP},
		{"public", pubIP},
	} {
		harness.Step(t, "ping own %s IP %s from guest", self.label, self.dst)
		out, ok := pingConverged(tgt, self.dst, 30*time.Second)
		require.Truef(t, ok,
			"guest ping to own %s IP %s never reached 0%% loss within 30s\n%s",
			self.label, self.dst, out)
	}

	// Step 2: ping public hostnames — forces DNS resolution then ICMP egress. A
	// resolver failure surfaces as a name-resolution error (never 0% loss), so a
	// converged ping proves the full resolve -> egress chain.
	for _, host := range []string{"google.com", "cloudflare.com"} {
		harness.Step(t, "ping public hostname %s (DNS resolve + egress)", host)
		out, ok := pingConverged(tgt, host, 45*time.Second)
		require.Truef(t, ok,
			"guest ping to %s never reached 0%% loss within 45s — DNS resolution "+
				"or WAN egress is broken\n%s", host, out)
	}
}
