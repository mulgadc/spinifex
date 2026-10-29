//go:build e2e

package multinode

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// runIPSec verifies OVN native IPsec is live: every node carries cert/key/CA pointers
// and AES-GCM SAs, and guest traffic between two nodes leaves each as ESP only.
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

	assertGuestTrafficEncrypted(t, fix, ssh)
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

	// The flag is asserted only once every node has published its own readiness,
	// so it lags the per-node config by up to a reconcile interval. Polling for it
	// is the difference between testing the invariant and testing the clock.
	var asserted string
	harness.EventuallyErr(t, func() error {
		var unreadable []string
		for _, n := range fix.Cluster.Nodes {
			value, err := readNBGlobalIPSec(t, ssh, n)
			if err != nil {
				unreadable = append(unreadable, fmt.Sprintf("%s: %v", n.Name, err))
				continue
			}
			harness.Detail(t, "node", n.Name, "nb_global_ipsec", value)
			if value == "true" {
				asserted = n.Name
				return nil
			}
		}
		if len(unreadable) == len(fix.Cluster.Nodes) {
			return fmt.Errorf("no node could read NB_Global: %s", strings.Join(unreadable, "; "))
		}
		return errors.New("no node reports NB_Global ipsec=true — ovn-controller adds no options:remote_name, so intra-AZ Geneve is plaintext")
	}, 120*time.Second, 5*time.Second)

	if len(incomplete) > 0 {
		t.Fatalf("NB_Global ipsec=true (read on %s) while %d chassis are unconfigured: %v — cross-chassis guest traffic is black-holing",
			asserted, len(incomplete), incomplete)
	}
}

// readNBGlobalIPSec returns the flag as the node reports it. A node with no
// local NB DB returns an error rather than "false": conflating the two is the
// defect the production change exists to remove, and the test must not reinstate
// it by discarding the exit status.
func readNBGlobalIPSec(t *testing.T, ssh *harness.PeerSSH, n harness.Node) (string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := ssh.Run(ctx, n.Addr, "sudo ovn-nbctl --timeout=5 get NB_Global . ipsec")
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(raw)))
	}
	return strings.Trim(strings.TrimSpace(string(raw)), `"`), nil
}

// Guest traffic sent in each direction of the pair during the capture window.
// Each ping's ICMP payload alone is a lower bound on the bytes its SA carries.
const (
	ipsecPingCount     = 200
	ipsecPingSize      = 1000
	ipsecCaptureWindow = 30 * time.Second
)

// ipsecLeg is one direction of the pair: what self sends towards peer's tunnel
// endpoint, through the interface that routes there.
type ipsecLeg struct {
	self, peer           harness.Node
	selfEncap, peerEncap string
	dev                  string
}

func (l ipsecLeg) String() string {
	return fmt.Sprintf("%s (%s) -> %s (%s)", l.self.Name, l.selfEncap, l.peer.Name, l.peerEncap)
}

// assertGuestTrafficEncrypted drives guest pings across a distinct-node pair and
// fails unless, in each direction, the outbound SA counters rose by the payload,
// the underlay carried ESP, and no plaintext Geneve left for the peer.
func assertGuestTrafficEncrypted(t *testing.T, fix *Fixture, ssh *harness.PeerSSH) {
	harness.Step(t, "cross-node guest traffic leaves each node of the pair as ESP, never plaintext Geneve")

	pair := launchCrossNodePair(t, fix, "ipsec-runnersg")
	srcEncap := readEncapIP(t, ssh, pair.src.node)
	dstEncap := readEncapIP(t, ssh, pair.dst.node)
	legs := []ipsecLeg{
		{self: pair.src.node, peer: pair.dst.node, selfEncap: srcEncap, peerEncap: dstEncap},
		{self: pair.dst.node, peer: pair.src.node, selfEncap: dstEncap, peerEncap: srcEncap},
	}
	for i := range legs {
		legs[i].dev = routeDevTo(t, ssh, legs[i].self, legs[i].peerEncap)
		harness.Detail(t, "leg", legs[i].String(), "dev", legs[i].dev)
	}

	// Converge first so the measured window is not spent on address-set
	// propagation, which would read as packet loss rather than as plaintext.
	out, converged, attempts, _ := pingConvergedCrossHost(pair.keyPath, pair.sshHost, pair.sshPort,
		pair.dstPrivateIP, sameSGPingTimeout, nil)
	require.Truef(t, converged, "cross-host ping %s -> %s never converged within %s (%d attempts)\n%s",
		pair.src.id, pair.dst.id, sameSGPingTimeout, attempts, out)

	before := make([]map[string]saCounters, len(legs))
	for i, l := range legs {
		before[i] = readOutboundSAs(t, ssh, l)
	}

	captures := make([]capture, len(legs))
	var wg sync.WaitGroup
	for i, l := range legs {
		captures[i].done = make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(captures[i].done)
			captures[i].out, captures[i].err = captureOutbound(ssh, l)
		}()
	}
	waitForCaptures(t, ssh, legs, captures)

	pingCmd := fmt.Sprintf("sudo -n ping -q -c %d -i 0.02 -s %d -W 2 %s", ipsecPingCount, ipsecPingSize, pair.dstPrivateIP)
	pingOut, pingErr := sshCapture(pair.keyPath, "ubuntu", pair.sshHost, pair.sshPort, pingCmd)
	wg.Wait()
	if pingErr != nil {
		t.Errorf("guest ping %s -> %s during capture: %v\n%s", pair.src.id, pair.dstPrivateIP, pingErr, pingOut)
	}
	harness.Detail(t, "guest_ping", strings.TrimSpace(pingOut))

	payload := uint64(ipsecPingCount * ipsecPingSize)
	for i, l := range legs {
		after := readOutboundSAs(t, ssh, l)
		bytes, packets := saDelta(before[i], after)
		if captures[i].err != nil {
			t.Errorf("%s: outbound capture on %s failed: %v", l, l.dev, captures[i].err)
			continue
		}
		c := classifyUnderlay(captures[i].out)
		harness.Detail(t, "leg", l.String(), "sa_bytes_delta", bytes, "sa_packets_delta", packets,
			"esp_packets", c.esp, "geneve_packets", c.geneve, "other_packets", c.other)

		if bytes < payload {
			t.Errorf("%s: outbound SA bytes rose by %d during the window, under the %d bytes of guest payload sent — guest traffic bypassed the SA",
				l, bytes, payload)
		}
		if c.esp == 0 {
			t.Errorf("%s: no outbound ESP or UDP/4500 ESP on %s during guest traffic\n%s", l, l.dev, captures[i].out)
		}
		if c.geneve > 0 {
			t.Errorf("%s: %d plaintext Geneve (UDP/6081) packets left %s for the peer — guest traffic is unencrypted on the underlay\n%s",
				l, c.geneve, l.dev, captures[i].out)
		}
	}
}

// readEncapIP returns the node's Geneve tunnel endpoint, which is also the
// address its IPsec SAs are keyed on.
func readEncapIP(t *testing.T, ssh *harness.PeerSSH, n harness.Node) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := ssh.Run(ctx, n.Addr, "sudo ovs-vsctl get Open_vSwitch . external_ids:ovn-encap-ip")
	if err != nil {
		t.Fatalf("%s read ovn-encap-ip: %v", n.Name, err)
	}
	ip := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if net.ParseIP(ip) == nil {
		t.Fatalf("%s ovn-encap-ip %q is not an IP address", n.Name, ip)
	}
	return ip
}

// routeDevTo returns the interface self routes peerIP through, where outbound
// ESP is emitted and where plaintext Geneve would be if the SA were bypassed.
func routeDevTo(t *testing.T, ssh *harness.PeerSSH, self harness.Node, peerIP string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := ssh.Run(ctx, self.Addr, "ip route get "+peerIP)
	if err != nil {
		t.Fatalf("%s ip route get %s: %v", self.Name, peerIP, err)
	}
	dev, ok := routeDev(string(raw))
	if !ok {
		t.Fatalf("%s ip route get %s names no device:\n%s", self.Name, peerIP, raw)
	}
	return dev
}

// routeDev extracts the device from `ip route get` output.
func routeDev(out string) (string, bool) {
	f := strings.Fields(out)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "dev" {
			return f[i+1], true
		}
	}
	return "", false
}

// saCounters is the "lifetime current" usage of one SA.
type saCounters struct {
	bytes, packets uint64
}

// readOutboundSAs returns the ESP SAs from self to peer, keyed by SPI.
func readOutboundSAs(t *testing.T, ssh *harness.PeerSSH, l ipsecLeg) map[string]saCounters {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := ssh.Run(ctx, l.self.Addr,
		fmt.Sprintf("sudo ip -s xfrm state list src %s dst %s proto esp", l.selfEncap, l.peerEncap))
	if err != nil {
		t.Fatalf("%s: ip -s xfrm state: %v", l, err)
	}
	sas, err := parseXfrmCounters(string(raw))
	if err != nil {
		t.Fatalf("%s: %v\n%s", l, err, raw)
	}
	if len(sas) == 0 {
		t.Fatalf("%s: no outbound ESP SA to the peer's tunnel endpoint", l)
	}
	return sas
}

// parseXfrmCounters reads `ip -s xfrm state` output into per-SPI usage counters.
func parseXfrmCounters(out string) (map[string]saCounters, error) {
	sas := map[string]saCounters{}
	spi := ""
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		f := strings.Fields(line)
		if len(f) >= 4 && f[0] == "proto" && f[2] == "spi" {
			spi = f[3]
			continue
		}
		if strings.TrimSpace(line) != "lifetime current:" {
			continue
		}
		if spi == "" {
			return nil, errors.New("xfrm lifetime counters precede any SPI")
		}
		if i+1 >= len(lines) {
			return nil, fmt.Errorf("SA %s: lifetime current has no counter line", spi)
		}
		var c saCounters
		if _, err := fmt.Sscanf(strings.TrimSpace(lines[i+1]), "%d(bytes), %d(packets)", &c.bytes, &c.packets); err != nil {
			return nil, fmt.Errorf("SA %s: parse counters %q: %w", spi, lines[i+1], err)
		}
		sas[spi] = c
		spi = ""
	}
	return sas, nil
}

// saDelta sums what each SA carried between two snapshots. An SA that appeared
// in between counts in full and one that expired counts nothing, so a rekey
// mid-window can only understate the delta, never inflate it.
func saDelta(before, after map[string]saCounters) (bytes, packets uint64) {
	for spi, a := range after {
		b := before[spi]
		if a.bytes >= b.bytes {
			bytes += a.bytes - b.bytes
		}
		if a.packets >= b.packets {
			packets += a.packets - b.packets
		}
	}
	return bytes, packets
}

// captureOutbound runs a fixed-window, outbound-only capture of what self sends
// the peer's tunnel endpoint. Inbound ESP is decrypted and seen again as Geneve
// on the same interface, so only outbound packets are evidence.
func captureOutbound(ssh *harness.PeerSSH, l ipsecLeg) (string, error) {
	secs := int(ipsecCaptureWindow.Seconds())
	ctx, cancel := context.WithTimeout(context.Background(), ipsecCaptureWindow+15*time.Second)
	defer cancel()
	// timeout(1) exits 124 when the window elapses, which is the normal end.
	cmd := fmt.Sprintf("sudo timeout %d tcpdump -Q out -t -nn -l -i %s "+
		"'dst host %s and (ip proto 50 or udp port 4500 or udp port 6081)' 2>&1; "+
		"rc=$?; [ $rc -eq 124 ] && exit 0; exit $rc",
		secs, l.dev, l.peerEncap)
	raw, err := ssh.Run(ctx, l.self.Addr, cmd)
	return string(raw), err
}

// capture is one leg's tcpdump output; done closes once out and err are set.
type capture struct {
	out  string
	err  error
	done chan struct{}
}

// waitForCaptures returns once tcpdump is running on every leg, so no guest
// packet goes out before the capture that should see it. The anchored pattern
// matches tcpdump itself, not the sudo and timeout processes that launch it.
func waitForCaptures(t *testing.T, ssh *harness.PeerSSH, legs []ipsecLeg, captures []capture) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		for i, l := range legs {
			select {
			case <-captures[i].done:
				return fmt.Errorf("%s: capture exited before guest traffic: %v\n%s", l, captures[i].err, captures[i].out)
			default:
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := ssh.Run(ctx, l.self.Addr, "pgrep -f '^tcpdump -Q out .* -i "+l.dev+" '")
			cancel()
			if err != nil {
				return fmt.Errorf("%s: capture not running yet: %w", l, err)
			}
		}
		return nil
	}, 15*time.Second, 500*time.Millisecond)
}

// underlayCapture counts captured packets by what they reveal on the wire.
type underlayCapture struct {
	esp    int // ESP, raw or UDP-encapsulated
	geneve int // plaintext Geneve, UDP/6081
	other  int // anything else the filter admitted, such as IKE on UDP/4500
}

// classifyUnderlay classifies `tcpdump -t -nn` lines. UDP-encapsulated ESP
// prints as "UDP-encap: ESP(spi=...)", so one marker covers both encodings.
func classifyUnderlay(out string) underlayCapture {
	var c underlayCapture
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || (f[0] != "IP" && f[0] != "IP6") || f[2] != ">" {
			continue
		}
		switch {
		case strings.Contains(line, "ESP(spi="):
			c.esp++
		case strings.HasSuffix(f[1], ".6081") || strings.HasSuffix(f[3], ".6081:"):
			c.geneve++
		default:
			c.other++
		}
	}
	return c
}
