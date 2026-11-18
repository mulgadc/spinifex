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

// ipsecSAReadyTimeout covers one kernel acquire lifetime: strongSwan sets
// xfrm_acq_expires to 165 s, and a first initiation the peer rejects blocks
// any retry for that long.
const ipsecSAReadyTimeout = 200 * time.Second

// saWaitResult is the outcome of waitSAs: whether every leg came up, how long
// that took, and the legs still missing an SA when the budget ran out.
type saWaitResult struct {
	ready   bool
	elapsed time.Duration
	missing []string
}

// waitSAs polls each leg until all report an SA or budget runs out, calling
// trigger on the missing legs between polls. Each call gets the time left,
// capped at 10 s, so the wait overshoots its deadline by at most one probe.
func waitSAs[L fmt.Stringer](legs []L, budget time.Duration, ready func(L, time.Duration) bool,
	trigger func(L, time.Duration), now func() time.Time, pause func(time.Duration)) saWaitResult {
	start := now()
	deadline := start.Add(budget)
	limit := func() time.Duration {
		return min(10*time.Second, max(time.Second, deadline.Sub(now())))
	}
	for {
		var missing []L
		for _, l := range legs {
			if !ready(l, limit()) {
				missing = append(missing, l)
			}
		}
		if len(missing) == 0 {
			return saWaitResult{ready: true, elapsed: now().Sub(start)}
		}
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			names := make([]string, 0, len(missing))
			for _, l := range missing {
				names = append(names, l.String())
			}
			return saWaitResult{elapsed: now().Sub(start), missing: names}
		}
		for _, l := range missing {
			trigger(l, limit())
		}
		pause(min(2*time.Second, remaining))
	}
}

// geneveOutReqid returns the reqid of the outbound policy for the Geneve src
// sends to dst (udp dport 6081). The policy for replies from src's own 6081
// shares both endpoints, so it must not count.
func geneveOutReqid(policies, src, dst string) (string, bool) {
	match := false
	for line := range strings.SplitSeq(policies, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if f[0] == "src" {
			match = len(f) >= 8 && strings.TrimSuffix(f[1], "/32") == src &&
				f[2] == "dst" && strings.TrimSuffix(f[3], "/32") == dst &&
				f[4] == "proto" && f[5] == "udp" && f[6] == "dport" && f[7] == "6081"
			continue
		}
		if !match {
			continue
		}
		if f[0] == "dir" && len(f) > 1 && f[1] != "out" {
			match = false
			continue
		}
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "reqid" {
				return f[i+1], true
			}
		}
	}
	return "", false
}

// hasLiveSA reports whether states holds an ESP SA from src to dst with the
// given reqid and a non-zero SPI. A larval acquire state has SPI zero.
func hasLiveSA(states, src, dst, reqid string) bool {
	match := false
	for line := range strings.SplitSeq(states, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if f[0] == "src" {
			match = len(f) >= 4 && f[1] == src && f[2] == "dst" && f[3] == dst
			continue
		}
		if !match || len(f) < 6 || f[0] != "proto" || f[1] != "esp" || f[2] != "spi" || f[4] != "reqid" {
			continue
		}
		spi, _, _ := strings.Cut(f[3], "(")
		rid, _, _ := strings.Cut(f[5], "(")
		if rid == reqid && strings.Trim(strings.TrimPrefix(spi, "0x"), "0") != "" {
			return true
		}
	}
	return false
}

// xfrmStateMarker separates the policy and state listings in one probe.
const xfrmStateMarker = "=== xfrm state ==="

// legHasGeneveSA reports whether the leg's node holds a live outbound SA for
// the Geneve it sends to the peer. A probe error reads as not ready.
func legHasGeneveSA(t *testing.T, ssh *harness.PeerSSH, l ipsecLeg, limit time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	raw, err := ssh.Run(ctx, l.self.Addr, fmt.Sprintf(
		"sudo ip xfrm policy list src %[1]s dst %[2]s dir out && echo '%[3]s' && "+
			"sudo ip xfrm state list src %[1]s dst %[2]s proto esp nokeys",
		l.selfEncap, l.peerEncap, xfrmStateMarker))
	if err != nil {
		t.Logf("%s: xfrm probe: %v", l, err)
		return false
	}
	policies, states, _ := strings.Cut(string(raw), xfrmStateMarker)
	reqid, ok := geneveOutReqid(policies, l.selfEncap, l.peerEncap)
	return ok && hasLiveSA(states, l.selfEncap, l.peerEncap, reqid)
}

// triggerAcquire sends one UDP datagram from the node to the peer's Geneve
// port. It matches the same trap policy as Geneve, so the kernel raises an
// acquire without any guest traffic; the peer drops the payload.
func triggerAcquire(t *testing.T, ssh *harness.PeerSSH, l ipsecLeg, limit time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := fmt.Sprintf("bash -c 'echo x > /dev/udp/%s/6081'", l.peerEncap)
	if _, err := ssh.Run(ctx, l.self.Addr, cmd); err != nil {
		t.Logf("%s: acquire trigger: %v", l, err)
	}
}

// waitCrossHostSAs waits until each node of the pair holds the outbound SA for
// the Geneve it sends the other, so a later datapath budget times the datapath
// only. Returns the legs still missing an SA, or nil.
func waitCrossHostSAs(t *testing.T, src, dst harness.Node) []string {
	t.Helper()
	ssh := harness.NewPeerSSH()
	if !ipsecRequested(t, ssh, src) || !ipsecRequested(t, ssh, dst) {
		harness.Detail(t, "ipsec_sa_wait", "skipped")
		return nil
	}
	srcEncap, dstEncap := readEncapIP(t, ssh, src), readEncapIP(t, ssh, dst)
	legs := []ipsecLeg{
		{self: src, peer: dst, selfEncap: srcEncap, peerEncap: dstEncap},
		{self: dst, peer: src, selfEncap: dstEncap, peerEncap: srcEncap},
	}
	res := waitSAs(legs, ipsecSAReadyTimeout,
		func(l ipsecLeg, limit time.Duration) bool { return legHasGeneveSA(t, ssh, l, limit) },
		func(l ipsecLeg, limit time.Duration) { triggerAcquire(t, ssh, l, limit) },
		time.Now, time.Sleep)
	if !res.ready {
		harness.Detail(t, "ipsec_sa_wait", "expired", "ipsec_sa_missing", strings.Join(res.missing, "; "))
		return res.missing
	}
	harness.Detail(t, "ipsec_sa_wait", "ready", "ipsec_sa_ready_ms", res.elapsed.Milliseconds())
	return nil
}
