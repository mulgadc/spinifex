//go:build e2e

package single

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wanBlockedSMTPPorts mirrors config.DefaultBlockedWANPorts: the outbound
// SMTP ports the platform egress ACL drops to public destinations, matching
// AWS's default outbound-25 block. Kept as a literal here so the e2e asserts
// the deployed default rather than importing and trusting the same constant.
var wanBlockedSMTPPorts = []int{25, 465, 587}

// guestConnectProbe returns a guest-side shell one-liner that attempts a TCP
// connect to host:port with a hard timeout and prints OPEN or BLOCKED. A
// dropped SYN (the ACL's behaviour) makes the connect hang until timeout kills
// it; a RST would fail fast — either way the port is not OPEN. The timeout is
// the caller's budget: the user asked for a 5s SMTP probe.
func guestConnectProbe(host string, port, timeoutSec int) string {
	return fmt.Sprintf(
		"timeout %d bash -c 'exec 3<>/dev/tcp/%s/%d' >/dev/null 2>&1 && echo OPEN || echo BLOCKED",
		timeoutSec, host, port)
}

// runWANEgressSMTPBlock boots one public guest and proves the platform egress
// ACL drops outbound SMTP (25/465/587) to a public destination while ordinary
// WAN egress still works — the AWS-parity default. It resolves gmail.com's MX
// the way the user asked ("dig gmail.com mx; connect to one of the servers via
// smtp, 5 sec timeout, confirm it does not work"), then probes each blocked
// port against that real mail exchanger.
//
// Stage order and gating:
//   - Setup (dedicated SG + one public guest + SSH ingress) is an
//     unconditional prerequisite; failure is fatal in the ordinary sense.
//   - Control proves the guest has working WAN egress at all: a resolvable
//     public name and an open connect on a non-blocked port (443). Without it
//     a total egress outage would masquerade as a working SMTP block, so its
//     failure aborts the SMTP assertions rather than let them pass vacuously.
//   - SMTPBlocked is the assertion under test and depends on Control having
//     proven egress is otherwise alive.
func runWANEgressSMTPBlock(t *testing.T, fix *Fixture) {
	if !fix.PoolMode {
		t.Skip("WAN egress SMTP block scenario requires pool-mode networking (real WAN egress)")
	}
	harness.Phase(t, "Single — Platform egress ACL blocks outbound SMTP (25/465/587) to public destinations, AWS parity")

	vpcID, _, subnetID := harness.DiscoverDefaultVPC(t, fix.AWS)
	instType, _ := needInstanceTypeArch(t, fix)
	keyName, keyPath := needKeyPair(t, fix)
	ami := needAMI(t, fix)

	sgID := harness.EnsureSG(t, fix.Harness, vpcID, "smtpblock-sg")
	harness.Detail(t, "vpc", vpcID, "subnet", subnetID, "sg", sgID)

	instanceID := launchBaselineInstance(t, fix, ami, instType, keyName, subnetID, []string{sgID})
	pubIP := instancePublicIP(t, fix, instanceID)
	harness.Detail(t, "instance", instanceID, "public_ip", pubIP)

	harness.Step(t, "authorizing tcp/22 ingress, expecting reachability")
	harness.AuthorizeSSHIngress(t, fix.AWS, sgID)
	require.Truef(t, trySSHReady(pubIP, 22, keyPath, sshReadyBudget),
		"tcp/22 to %s never became reachable after authorizing ingress", pubIP)
	tgt := harness.SSHTarget{User: "ubuntu", Host: pubIP, Port: 22, KeyPath: keyPath}

	// Resolve gmail.com's MX from the guest, exactly the "dig gmail.com mx"
	// step, and fall back to the well-known exchanger if the guest image
	// carries no dig. The ACL matches on destination port, not host, so any
	// public IP would do; a real MX just makes the probe honest.
	var mxHost string
	harness.Step(t, "dig gmail.com MX from guest")
	harness.EventuallyErr(t, func() error {
		out, err := sshCapture(tgt, "dig +short gmail.com MX 2>/dev/null | sort -n | "+
			"awk '{print $2}' | sed 's/\\.$//' | head -1")
		if err != nil {
			return fmt.Errorf("ssh dig: %w (out=%q)", err, out)
		}
		if h := strings.TrimSpace(out); h != "" {
			mxHost = h
			return nil
		}
		return fmt.Errorf("dig returned no MX (out=%q)", strings.TrimSpace(out))
	}, 90*time.Second, 5*time.Second)
	if mxHost == "" {
		mxHost = "gmail-smtp-in.l.google.com"
	}
	harness.Detail(t, "gmail_mx", mxHost)

	// Control: egress is alive. The guest must resolve a public name and open
	// a non-blocked port (443) — otherwise a broken WAN path, not the ACL,
	// explains the SMTP failures below.
	controlOK := t.Run("Control", func(t *testing.T) {
		harness.RequireDNSEnabled(t, fix.Env)
		harness.Step(t, "resolve gmail.com A via guest resolver")
		res, err := sshCapture(tgt, "getent ahostsv4 gmail.com")
		require.NoErrorf(t, err, "guest failed to resolve gmail.com — WAN DNS path is broken\n%s", res)
		require.Regexpf(t, `\d{1,3}(\.\d{1,3}){3}`, res,
			"resolve gmail.com returned no IPv4 — resolver path is broken\n%s", res)

		harness.Step(t, "connect gmail.com:443 (non-blocked port must be OPEN)")
		out, err := sshCapture(tgt, guestConnectProbe("gmail.com", 443, 10))
		require.NoErrorf(t, err, "ssh connect probe 443\n%s", out)
		require.Equalf(t, "OPEN", strings.TrimSpace(out),
			"gmail.com:443 was not reachable — WAN egress is broken, cannot attribute an SMTP failure to the ACL\n%s", out)
	})
	if !controlOK {
		t.Fatalf("Control stage failed; skipping SMTP assertions that would pass vacuously under a dead WAN path")
	}

	t.Run("SMTPBlocked", func(t *testing.T) {
		for _, port := range wanBlockedSMTPPorts {
			harness.Step(t, "connect %s:%d (blocked SMTP port, 5s timeout, expect BLOCKED)", mxHost, port)
			out, err := sshCapture(tgt, guestConnectProbe(mxHost, port, 5))
			require.NoErrorf(t, err, "ssh connect probe %s:%d\n%s", mxHost, port, out)
			assert.Equalf(t, "BLOCKED", strings.TrimSpace(out),
				"outbound SMTP to %s:%d was OPEN — the platform egress ACL is not dropping it\n%s",
				mxHost, port, out)
		}
	})
}
