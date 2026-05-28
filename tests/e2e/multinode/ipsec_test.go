//go:build e2e

package multinode

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// runIPSec verifies OVN native IPsec is live on every node. Three checks:
//
//  1. OVS DB on each node carries the cert/key/CA pointers plus
//     other_config:ipsec_encapsulation=true. This is what
//     daemon.enableOVNIPSec writes; if any node lost it (rolling restart,
//     manual ovs-vsctl unset, missing peer cert) the SAs below would
//     silently drop to plaintext.
//
//  2. `ip xfrm state` on each node shows at least one AEAD SA negotiated
//     in AES-GCM mode. ovs-monitor-ipsec drives strongSwan to bring SAs up
//     against every peer ovn-controller has a Geneve tunnel to, so on a
//     3-node mesh we expect 2N SAs per node (in+out per peer) — but the
//     test only asserts >=1 to stay tolerant of ovs-monitor-ipsec startup
//     ordering quirks.
//
//  3. Best-effort tcpdump for ESP (proto 50) traffic on the underlay.
//     Logged only — geneve_sys_6081 may be idle during the capture window
//     if no overlay VMs are running yet.
//
// Skips cleanly if the cluster was bootstrapped with --ipsec=false (no
// other_config:ipsec_encapsulation key on the first node).
func runIPSec(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — OVN Native IPsec")

	ssh := harness.NewPeerSSH()
	first := fix.Cluster.Nodes[0]

	probeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := ssh.Run(probeCtx,
		first.Addr,
		"sudo ovs-vsctl --if-exists get Open_vSwitch . other_config:ipsec_encapsulation 2>/dev/null || true",
	)
	if err != nil {
		t.Fatalf("probe ovs-vsctl on %s: %v", first.Name, err)
	}
	if !strings.Contains(string(out), "true") {
		t.Skipf("IPsec not enabled on cluster (%s reports %q): skip", first.Name, strings.TrimSpace(string(out)))
	}

	harness.Step(t, "OVS DB carries cert pointers + ipsec_encapsulation=true on every node")
	required := []string{"certificate=", "private_key=", "ca_cert=", "ipsec_encapsulation=\"true\""}
	for _, n := range fix.Cluster.Nodes {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		raw, err := ssh.Run(c, n.Addr, "sudo ovs-vsctl get Open_vSwitch . other_config")
		cancel()
		if err != nil {
			t.Fatalf("%s ovs-vsctl get other_config: %v", n.Name, err)
		}
		s := strings.TrimSpace(string(raw))
		for _, key := range required {
			if !strings.Contains(s, key) {
				t.Fatalf("%s OVS other_config missing %q: %s", n.Name, key, s)
			}
		}
		harness.Detail(t, "node", n.Name, "other_config", s)
	}

	harness.Step(t, "xfrm SAs with AES-GCM established on every node")
	harness.EventuallyErr(t, func() error {
		for _, n := range fix.Cluster.Nodes {
			c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			raw, err := ssh.Run(c, n.Addr, "sudo ip xfrm state")
			cancel()
			if err != nil {
				return fmt.Errorf("%s ip xfrm state: %w", n.Name, err)
			}
			s := string(raw)
			if !strings.Contains(s, "aead") {
				return fmt.Errorf("%s xfrm has no AEAD SAs:\n%s", n.Name, strings.TrimSpace(s))
			}
			// Kernel renders the GCM AEAD as either `rfc4106(gcm(aes))`
			// (RFC 4106 ESP AES-GCM, what strongSwan negotiates by default)
			// or the bare `gcm(aes)` template name. Accept either.
			if !strings.Contains(s, "gcm(aes)") {
				return fmt.Errorf("%s xfrm SAs not AES-GCM:\n%s", n.Name, strings.TrimSpace(s))
			}
		}
		return nil
	}, 90*time.Second, 5*time.Second)

	harness.Step(t, "tcpdump ESP traffic on underlay (best-effort)")
	for _, n := range fix.Cluster.Nodes {
		c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		raw, err := ssh.Run(c, n.Addr,
			"sudo timeout 5 tcpdump -i any -nn -c 5 'ip proto 50' 2>&1 || true",
		)
		cancel()
		if err != nil {
			t.Logf("WARN: %s tcpdump ESP capture failed: %v", n.Name, err)
			continue
		}
		s := strings.TrimSpace(string(raw))
		if strings.Contains(s, "ESP") {
			harness.Detail(t, "node", n.Name, "esp_capture", "observed")
		} else {
			t.Logf("WARN: %s tcpdump saw no ESP traffic in 5s window (geneve may be idle):\n%s", n.Name, s)
		}
	}
}
