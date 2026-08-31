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
// Skips only when the node config disables IPsec; a cluster that asked for it fails.
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
		// An empty ipsec_encapsulation alone cannot distinguish "deliberately
		// disabled" from "enable failed", so cross-check the config's intent.
		// Skipping on both let ubuntu-24.04 cells run plaintext and report green.
		if ipsecRequested(t, ssh, first) {
			t.Fatalf("%s requests network.ipsec_enabled but reports ipsec_encapsulation=%q — intra-AZ Geneve is plaintext; check openvswitch-ipsec.service",
				first.Name, strings.TrimSpace(string(out)))
		}
		t.Skipf("IPsec disabled in node config on %s (network.ipsec_enabled=false): skip", first.Name)
	}

	harness.Step(t, "OVS DB carries cert pointers + ipsec_encapsulation=true on every node")
	required := []string{"certificate=", "private_key=", "ca_cert=", "ipsec_encapsulation=\"true\""}
	incomplete := map[string]string{}
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
				incomplete[n.Name] = s
				t.Errorf("%s OVS other_config missing %q: %s", n.Name, key, s)
			}
		}
		harness.Detail(t, "node", n.Name, "other_config", s)
	}

	assertNBGlobalMatchesChassis(t, fix, ssh, incomplete)

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

// nodeConfigPath is the daemon config the cluster is provisioned with.
const nodeConfigPath = "/etc/spinifex/spinifex.toml"

// ipsecRequested reports whether the node was configured to run IPsec.
// network.ipsec_enabled defaults to true, so an absent or unreadable key means
// requested; only an explicit false opts out.
func ipsecRequested(t *testing.T, ssh *harness.PeerSSH, n harness.Node) bool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := ssh.Run(ctx, n.Addr,
		"sudo grep -E '^[[:space:]]*ipsec_enabled' "+nodeConfigPath+" 2>/dev/null || true",
	)
	if err != nil {
		t.Logf("WARN: %s read ipsec_enabled from %s: %v — assuming requested (config default)", n.Name, nodeConfigPath, err)
		return true
	}

	// Keep the first token after "=" so a trailing comment cannot be mistaken
	// for the value. An absent key leaves this empty, which reads as requested.
	value := strings.TrimSpace(string(raw))
	if _, after, ok := strings.Cut(value, "="); ok {
		value = strings.TrimSpace(after)
	}
	if fields := strings.Fields(value); len(fields) > 0 {
		value = fields[0]
	}

	harness.Detail(t, "node", n.Name, "ipsec_enabled", value)
	return value != "false"
}

// assertNBGlobalMatchesChassis checks the invariant a partial IPsec enable
// breaks: NB_Global.ipsec is cluster-wide, so asserting it while any chassis is
// still unconfigured black-holes every guest that crosses chassis, with no
// control-plane signal that anything is wrong.
func assertNBGlobalMatchesChassis(t *testing.T, fix *Fixture, ssh *harness.PeerSSH, incomplete map[string]string) {
	harness.Step(t, "NB_Global ipsec is not asserted over an unconfigured chassis")

	var asserted string
	for _, n := range fix.Cluster.Nodes {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		raw, err := ssh.Run(c, n.Addr, "sudo ovn-nbctl --timeout=5 get NB_Global . ipsec 2>/dev/null || true")
		cancel()
		if err != nil {
			t.Logf("WARN: %s ovn-nbctl get NB_Global ipsec: %v", n.Name, err)
			continue
		}
		value := strings.Trim(strings.TrimSpace(string(raw)), `"`)
		harness.Detail(t, "node", n.Name, "nb_global_ipsec", value)
		if value == "true" {
			asserted = n.Name
		}
	}

	if asserted == "" {
		t.Fatal("no node reports NB_Global ipsec=true — ovn-controller adds no options:remote_name, so intra-AZ Geneve is plaintext")
	}
	if len(incomplete) > 0 {
		t.Fatalf("NB_Global ipsec=true (read on %s) while %d chassis are unconfigured: %v — cross-chassis guest traffic is black-holing",
			asserted, len(incomplete), incomplete)
	}
}
