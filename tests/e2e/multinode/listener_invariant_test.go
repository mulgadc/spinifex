//go:build e2e

package multinode

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	networkconnections "github.com/mulgadc/spinifex/docs/security/network-connections"
	"github.com/mulgadc/spinifex/spinifex/network/listenerinventory"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// runListenerInvariant is the runtime half of the listener invariant. The
// static half in spinifex/network/invariants reads install scripts and
// config templates; only this half reads what each node's kernel actually
// bound, which is what caught the original OVN NB/SB wildcard defect (spinifex
// #765) — that bug was found by hand on a live node with `ss -tulnp`, not by
// any static test.
//
// docs/security/network-connections/README.md "## 1. Inbound Listeners" is
// the same fixture the static half reads: it classifies each port's intended
// reach, and a row's own prose is the only thing allowed to excuse a
// wildcard bind. No port list is hardcoded here.
func runListenerInvariant(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Listener Invariant")

	table, err := listenerinventory.Parse(string(networkconnections.README()))
	if err != nil {
		t.Fatalf("parse inbound listener inventory: %v", err)
	}

	// SPINIFEX_WAN_IP is read directly (not via harness.Env.WANHost, which
	// falls back to the first NodeIPs entry — i.e. some node's own peer
	// address — when unset) so a WAN address is only ever trusted here when
	// it was positively supplied, never inferred.
	wanAddr := strings.TrimSpace(os.Getenv("SPINIFEX_WAN_IP"))
	if wanAddr == "" {
		harness.Detail(t, "wan_bind_check",
			"skipped for every node: SPINIFEX_WAN_IP is unset, so no address can be "+
				"distinguished from each node's cluster peer address")
	} else {
		harness.Detail(t, "wan_addr", wanAddr)
	}

	ssh := harness.NewPeerSSH()
	var violations []string
	for _, node := range fix.Cluster.Nodes {
		harness.Step(t, "ss -tulnp on %s", node.Name)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		out, runErr := ssh.Run(ctx, node.Addr, "sudo ss -tulnp")
		cancel()
		if runErr != nil {
			t.Fatalf("ss -tulnp on %s: %v", node.Name, runErr)
		}

		if wanAddr != "" && wanAddr == node.Addr {
			harness.Detail(t, "wan_bind_check", fmt.Sprintf(
				"skipped on %s: SPINIFEX_WAN_IP equals this node's peer address, so it is "+
					"single-homed for the purpose of this check", node.Name))
		}

		for _, sock := range parseSSOutput(string(out)) {
			for _, r := range table.RowsForPort(sock.port) {
				if r.Scope != listenerinventory.ScopeCluster && r.Scope != listenerinventory.ScopeEncap {
					continue
				}
				if v := checkSocketScope(node, sock, r, wanAddr); v != "" {
					violations = append(violations, v)
				}
			}
		}
	}

	if len(violations) > 0 {
		t.Fatalf("listener invariant violated (docs/security/network-connections/README.md"+
			" \"## 1. Inbound Listeners\" is the fixture; add or correct a row rather than"+
			" widening this test):\n  %s", strings.Join(violations, "\n  "))
	}
	harness.Detail(t, "nodes_checked", len(fix.Cluster.Nodes))
}

// checkSocketScope reports a violation message if sock is bound somewhere
// row's Scope does not permit, honoring row's own doc-declared wildcard
// exception. node.Addr is the cluster peer address (SSH target, not
// necessarily WAN-facing — a single-homed node has no other address).
// wanAddr is a positively-known WAN address, or "" when none is available;
// it is only meaningful when distinct from node.Addr, since on a
// single-homed node they are the same address by definition. Returns "" if
// the socket is clean for this row.
func checkSocketScope(node harness.Node, sock ssSocket, row listenerinventory.Row, wanAddr string) string {
	wildcard := listenerinventory.IsWildcardAddr(sock.addr)
	switch {
	case wildcard && row.WildcardOK():
		return ""
	case wildcard:
		return fmt.Sprintf(
			"%s: port %d (%s, %s scope) bound to the wildcard address %s; its inventory row does not declare a wildcard exception",
			node.Name, sock.port, sock.proto, row.Scope, sock.addr)
	case wanAddr != "" && wanAddr != node.Addr && sock.addr == wanAddr:
		return fmt.Sprintf(
			"%s: port %d (%s, %s scope) bound to %s, a WAN address distinct from the node's peer address %s",
			node.Name, sock.port, sock.proto, row.Scope, sock.addr, node.Addr)
	}
	return ""
}

// ssSocket is one row of `ss -tulnp` output: a listening TCP socket or an
// unconnected (server) UDP socket.
type ssSocket struct {
	proto string
	addr  string
	port  int
}

// parseSSOutput extracts every tcp/udp local bind from `ss -tulnp` output.
// Columns are whitespace-separated except the Process field, which is a
// single comma-joined token with no embedded space.
func parseSSOutput(out string) []ssSocket {
	var socks []ssSocket
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		proto := fields[0]
		if proto != "tcp" && proto != "udp" {
			continue // header row, or a protocol ss -tulnp does not report
		}
		addr, portStr := splitAddrPort(fields[4])
		port, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		socks = append(socks, ssSocket{proto: proto, addr: addr, port: port})
	}
	return socks
}

// splitAddrPort splits an ss(8) "Local Address:Port" token into address and
// port, handling the bracketed IPv6 form ("[::]:500") separately since an
// IPv6 address contains colons of its own.
func splitAddrPort(tok string) (addr, port string) {
	if strings.HasPrefix(tok, "[") {
		end := strings.Index(tok, "]")
		if end < 0 {
			return tok, ""
		}
		return tok[1:end], strings.TrimPrefix(tok[end+1:], ":")
	}
	i := strings.LastIndex(tok, ":")
	if i < 0 {
		return tok, ""
	}
	return tok[:i], tok[i+1:]
}
