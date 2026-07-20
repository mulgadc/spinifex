//go:build e2e

package single

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	handlers_imds "github.com/mulgadc/spinifex/spinifex/handlers/imds"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sgReachabilityEgressRounds returns how many revoke/restore round-trips the
// RevokeEgress/RestoreEgress stages repeat, via SPINIFEX_SGREACHABILITY_EGRESS_ROUNDS
// (default 1). Only the naturally-idempotent revoke/restore cycle is repeatable
// this way — Deny/Authorize/Resolver/WANEgress each assert a one-shot state
// transition that would need new reset behavior to repeat safely.
func sgReachabilityEgressRounds() int {
	return envPositiveIntOr("SPINIFEX_SGREACHABILITY_EGRESS_ROUNDS", 1)
}

// runSGReachabilityPolicy merges three checks that used to boot their own
// instance apiece — default-SG deny/authorize, guest DNS resolution, and
// security-group egress enforcement — around one shared guest, cutting three
// boots to one.
//
// Stage order and gating:
//   - Setup (dedicated SG + one instance) is an unconditional prerequisite for
//     everything below it; failure here is fatal in the ordinary Go-test sense.
//   - Deny probes the pre-authorize state. Authorize doesn't need Deny to have
//     passed to be meaningful on its own, so there is no gate between them.
//   - Authorize is the hard dependency for every later stage: Resolver,
//     WANEgress, and the egress round-trip all need a working SSH session, so
//     Authorize's failure aborts the rest of the scenario rather than let four
//     more stages time out for the same reason.
//   - Resolver and WANEgress are independent DNS-path signals (local resolver
//     config + internal-name resolution vs. public-hostname resolution over
//     the northstar path) — one failing doesn't make the other meaningless, so
//     neither gates the other.
//   - The egress round-trip mutates this scenario's OWN dedicated SG (not the
//     shared default SG the rest of the suite relies on) — a deliberate
//     isolation improvement over the original, which mutated-and-restored the
//     shared default SG. A gateway-discovery + baseline-ICMP probe gates entry
//     into the round loop entirely (skip, not fail: an environment that never
//     carried the probe can't distinguish enforcement from a broken network).
//     Within each round, RestoreEgress only depends on the revoke API call
//     having actually removed the rule (tracked separately from the
//     ICMP-drop assertion) — a flaky drop-detection shouldn't stop the
//     restore half from being independently verified.
func runSGReachabilityPolicy(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Single — SG reachability policy: deny, authorize, resolve, and egress-enforce around one guest")

	vpcID, _, subnetID := harness.DiscoverDefaultVPC(t, fix.AWS)
	instType, _ := needInstanceTypeArch(t, fix)
	keyName, keyPath := needKeyPair(t, fix)
	ami := needAMI(t, fix)

	// Dedicated SG, mutated by this scenario alone (its own egress is
	// revoked/restored below) rather than the shared default SG every other
	// test relies on.
	sgID := harness.EnsureSG(t, fix.Harness, vpcID, "sgreachability-sg")
	harness.Detail(t, "vpc", vpcID, "subnet", subnetID, "sg", sgID)
	t.Cleanup(func() {
		if err := authorizeAllowAllEgress(fix.AWS, sgID); err != nil &&
			!harness.ErrorCodeIs(err, "InvalidPermission.Duplicate") {
			t.Logf("WARNING: cleanup failed to restore allow-all egress on %s: %v", sgID, err)
		}
	})

	instanceID := launchBaselineInstance(t, fix, ami, instType, keyName, subnetID, []string{sgID})
	pubIP := instancePublicIP(t, fix, instanceID)
	privIP := instancePrivateIP(t, fix, instanceID)
	harness.Detail(t, "instance", instanceID, "public_ip", pubIP, "private_ip", privIP)

	t.Run("Deny", func(t *testing.T) {
		// No ingress rules yet; egress is allow-all by default so only inbound
		// is gated. Probe a short window to confirm the default-deny ACL is
		// applied and stable. This overlaps guest boot, and Authorize below
		// still pays the full boot wait via trySSHReady, so a longer window
		// buys little extra coverage.
		harness.Step(t, "asserting tcp/22 stays blocked under default-deny SG")
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			require.Falsef(t, tcpReachable(pubIP, 22, 3*time.Second),
				"tcp/22 to %s connected with NO ingress rule — default SG must deny external traffic", pubIP)
			time.Sleep(3 * time.Second)
		}
	})

	authorizeOK := t.Run("Authorize", func(t *testing.T) {
		harness.Step(t, "authorizing tcp/22 ingress, expecting reachability")
		harness.AuthorizeSSHIngress(t, fix.AWS, sgID)
		require.Truef(t, trySSHReady(pubIP, 22, keyPath, sshReadyBudget),
			"tcp/22 to %s never became reachable after authorizing ingress — "+
				"default subnet egress/IGW datapath is broken", pubIP)

		tgt := harness.SSHTarget{User: "ubuntu", Host: pubIP, Port: 22, KeyPath: keyPath}
		idOut := runSSH(t, tgt, "id")
		assert.Containsf(t, idOut, "ubuntu", "ssh id after authorize\n%s", idOut)
	})
	if !authorizeOK {
		t.Fatalf("Authorize stage failed; skipping every later stage that depends on tcp/22 ingress being open")
	}

	tgt := harness.SSHTarget{User: "ubuntu", Host: pubIP, Port: 22, KeyPath: keyPath}

	// Resolver exercises the guest resolver datapath: DHCP pointed the guest
	// at the VPC resolver, the local ICMP datapath works before DNS is
	// exercised, and the instance's own internal EC2 name resolves to its
	// private IP.
	t.Run("Resolver", func(t *testing.T) {
		harness.RequireDNSEnabled(t, fix.Env)
		internalDomain := harness.NorthstarInternalDomain(fix.Env)
		require.NotEmpty(t, internalDomain, "fixture requires Northstar's internal DNS domain")
		region := aws.StringValue(fix.AWS.EC2Conf.Config.Region)
		require.NotEmpty(t, region, "AWS region is required to build the internal EC2 name")

		harness.Step(t, "assert guest uses the VPC resolver %s", handlers_imds.VPCDNSServerIP)
		harness.AssertGuestResolver(t, tgt)

		// Ping the instance's own private IP — local datapath sanity, no DNS
		// involved. (Own public IP is unreachable from inside via gateway
		// NAT, and the public inbound path is already proven by Authorize.)
		harness.Step(t, "ping own private IP %s from guest", privIP)
		out, ok := pingConverged(tgt, privIP, 30*time.Second)
		require.Truef(t, ok,
			"guest ping to own private IP %s never reached 0%% loss within 30s\n%s",
			privIP, out)

		privateName := handlers_dns.EC2PrivateName(privIP, region, internalDomain)
		harness.Step(t, "resolve internal EC2 name %s via guest resolver", privateName)
		internalResult, err := sshCapture(tgt, "getent ahostsv4 "+privateName)
		require.NoErrorf(t, err, "guest failed to resolve internal name %s\n%s", privateName, internalResult)
		require.Containsf(t, strings.Fields(internalResult), privIP,
			"internal name %s did not resolve to private IP %s\n%s", privateName, privIP, internalResult)
	})

	// WANEgress resolves public hostnames through the guest resolver (the
	// northstar path) and then pings them. getent hosts is the DNS
	// assertion; the ping only adds egress coverage — splitting them
	// isolates a resolver/northstar failure from a WAN-egress failure. Kept
	// as its own stage rather than gated on Resolver: it is an independent
	// signal about the northstar path either way.
	t.Run("WANEgress", func(t *testing.T) {
		harness.RequireDNSEnabled(t, fix.Env)
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
	})

	// The egress round-trip below flips this scenario's own dedicated SG's
	// allow-all egress rule and verifies vpcd programs OVN ACLs that
	// actually drop traffic. Egress is tested because in dev_networking mode
	// ingress SSH bypasses OVN via QEMU hostfwd — only egress hits the ACL.
	harness.Step(t, "discover default gateway inside VM")
	gwOut, gwErr := runSSHCombined(tgt, `ip route show default | awk '{print $3}' | head -1`)
	gw := strings.TrimSpace(strings.Map(func(r rune) rune {
		// Strip all whitespace, matching `tr -d '[:space:]'` from bash.
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, gwOut))
	if gwErr != nil || gw == "" {
		t.Skipf("could not discover default gateway inside VM (err=%v, out=%q)", gwErr, gwOut)
	}
	harness.Detail(t, "probe_gateway", gw)

	probeICMP := func() string {
		out, _ := runSSHCombined(tgt, fmt.Sprintf("ping -c 3 -W 2 %s", gw))
		return out
	}

	// Baseline — allow-all egress should let ICMP through. If the environment
	// blocks ICMP between the VM and gateway regardless of SG, skip the rest:
	// we can't distinguish enforcement from a network that never carried the
	// probe in the first place.
	baseline := probeICMP()
	if !pingReceivedRE.MatchString(baseline) {
		t.Skipf("baseline ICMP did not succeed; env may block ICMP regardless of SG\nOutput:\n%s", baseline)
	}
	harness.Detail(t, "baseline", "icmp_ok")

	rounds := sgReachabilityEgressRounds()
	for round := 1; round <= rounds; round++ {
		t.Run(fmt.Sprintf("Round%d", round), func(t *testing.T) {
			revokeMutationOK := true
			t.Run("RevokeEgress", func(t *testing.T) {
				_, err := fix.AWS.EC2.RevokeSecurityGroupEgress(&ec2.RevokeSecurityGroupEgressInput{
					GroupId:       aws.String(sgID),
					IpPermissions: []*ec2.IpPermission{allowAllEgressPermission()},
				})
				if err != nil {
					revokeMutationOK = false
				}
				require.NoError(t, err, "revoke-security-group-egress")

				// ACL propagation: poll the probe instead of a flat sleep so a
				// slow OVN flow install still gets bounded, fast environments
				// don't waste the full budget.
				var lastRevoke string
				harness.EventuallyErr(t, func() error {
					lastRevoke = probeICMP()
					if pingDroppedRE.MatchString(lastRevoke) {
						return nil
					}
					return fmt.Errorf("ICMP still succeeding after revoke; output:\n%s", lastRevoke)
				}, 30*time.Second, 1*time.Second)
			})
			if !revokeMutationOK {
				t.Fatalf("revoke-security-group-egress mutation failed; skipping re-authorize since the rule was never removed")
			}

			t.Run("RestoreEgress", func(t *testing.T) {
				err := authorizeAllowAllEgress(fix.AWS, sgID)
				require.NoError(t, err, "authorize-security-group-egress")

				var lastRestore string
				harness.EventuallyErr(t, func() error {
					lastRestore = probeICMP()
					if pingReceivedRE.MatchString(lastRestore) {
						return nil
					}
					return fmt.Errorf("ICMP still dropped after re-authorize; output:\n%s", lastRestore)
				}, 30*time.Second, 1*time.Second)
			})
		})
	}
}
