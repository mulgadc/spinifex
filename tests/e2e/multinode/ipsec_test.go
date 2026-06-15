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

// runIPSec verifies OVN native IPsec is live on every node: OVS DB carries cert/key/CA
// pointers, xfrm shows AES-GCM SAs, and ESP traffic is observed (best-effort tcpdump).
// Skips if the cluster was bootstrapped with --ipsec=false.
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
			// Kernel renders GCM AEAD as rfc4106(gcm(aes)) or gcm(aes); accept either.
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
