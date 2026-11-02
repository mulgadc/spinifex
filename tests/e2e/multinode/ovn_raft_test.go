//go:build e2e

package multinode

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

const (
	ovnNBSock = "/var/run/ovn/ovnnb_db.ctl"
	ovnSBSock = "/var/run/ovn/ovnsb_db.ctl"
)

// ovnCentralUnits expands, on the node, to debianUnits where ovn-central.service
// exists. EL has no such unit: ovn-northd.service runs both databases too.
func ovnCentralUnits(debianUnits string) string {
	return "$(systemctl cat ovn-central.service >/dev/null 2>&1 && echo " + debianUnits + " || echo ovn-northd)"
}

// runOVNRaft asserts the clustered OVN control plane: every DB node's NB and SB
// RAFT cluster reports the full quorum with exactly one leader, survives a
// rolling restart of each node's OVN central, then kills the NB leader's
// ovn-central and proves NB writes still land via a surviving endpoint
// (libovsdb-style multi-endpoint failover, here through ovn-nbctl).
func runOVNRaft(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — OVN OVSDB RAFT")

	require.GreaterOrEqualf(t, len(fix.Cluster.Nodes), 3,
		"OVN RAFT quorum requires a 3-node cluster, have %d", len(fix.Cluster.Nodes))
	// The first three nodes host the clustered NB/SB DBs (mirrors
	// formation.BuildOVNDBAddrs); in a 3-node cluster that is every node.
	dbNodes := fix.Cluster.Nodes[:3]
	ssh := harness.NewPeerSSH()

	// 1. Quorum health: 3 servers + exactly one leader, per schema, per node.
	assertOVNQuorum(t, ssh, dbNodes)

	// 2. Rolling restart, as setup-ovn.sh reruns do it: each node restarts both
	// of its members at once, on EL through the one ovn-northd unit, and the
	// next node waits for them to rejoin. NB writes must land after every step.
	dbList := nbEndpointList(dbNodes)
	for i, n := range dbNodes {
		harness.Step(t, "restart OVN central on %s (%s)", n.Name, n.Addr)
		restartCtx, restartCancel := context.WithTimeout(context.Background(), 60*time.Second)
		_, err := ssh.Run(restartCtx, n.Addr, "sudo systemctl restart "+ovnCentralUnits("ovn-ovsdb-server-nb ovn-ovsdb-server-sb ovn-northd"))
		restartCancel()
		require.NoErrorf(t, err, "restart OVN central on %s", n.Name)

		for _, db := range ovnRaftDBs {
			waitOVNRaftMember(t, ssh, n, db.sock, db.schema, 60*time.Second)
		}

		sw := fmt.Sprintf("e2e-raft-rolling-%d-%d", time.Now().Unix(), i)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_, _ = ssh.Run(ctx, n.Addr,
				fmt.Sprintf("sudo ovn-nbctl --db=%s --timeout=10 --if-exists ls-del %s", dbList, sw))
		})
		harness.Step(t, "NB write (ls-add %s) after %s rejoined", sw, n.Name)
		writeCtx, writeCancel := context.WithTimeout(context.Background(), 25*time.Second)
		_, err = ssh.Run(writeCtx, n.Addr, fmt.Sprintf("sudo ovn-nbctl --db=%s --timeout=20 ls-add %s", dbList, sw))
		writeCancel()
		require.NoErrorf(t, err, "NB ls-add after restarting OVN central on %s", n.Name)
	}
	assertOVNQuorum(t, ssh, dbNodes)

	// 3. Failover: kill the NB leader and confirm an NB write still succeeds.
	var leader harness.Node
	var survivors []harness.Node
	for _, n := range dbNodes {
		if _, role := ovnClusterStatus(t, ssh, n, ovnNBSock, host.OVNNBSchema); role == "leader" {
			leader = n
		} else {
			survivors = append(survivors, n)
		}
	}
	require.NotEmptyf(t, leader.Name, "no NB leader found across quorum")
	require.NotEmptyf(t, survivors, "no surviving DB node to write through")

	harness.Step(t, "stop ovn-central on NB leader %s (%s)", leader.Name, leader.Addr)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	_, err := ssh.Run(stopCtx, leader.Addr, "sudo systemctl stop "+ovnCentralUnits("ovn-central ovn-ovsdb-server-nb ovn-ovsdb-server-sb ovn-northd"))
	require.NoErrorf(t, err, "stop ovn-central on %s", leader.Name)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, e := ssh.Run(ctx, leader.Addr, "sudo systemctl start "+ovnCentralUnits("ovn-ovsdb-server-nb ovn-ovsdb-server-sb ovn-northd")); e != nil {
			t.Logf("cleanup: restart ovn-central on %s: %v", leader.Name, e)
		}
	})

	// The --db list includes the dead leader so the write must fail over past it.
	sw := fmt.Sprintf("e2e-raft-failover-%d", time.Now().Unix())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = ssh.Run(ctx, survivors[0].Addr,
			fmt.Sprintf("sudo ovn-nbctl --db=%s --timeout=10 --if-exists ls-del %s", dbList, sw))
	})

	harness.Step(t, "NB write (ls-add %s) via %s after leader loss", sw, survivors[0].Name)
	// RAFT re-election after the leader drops takes a beat; retry the write
	// until a new leader accepts it or the deadline passes.
	var writeErr error
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		_, writeErr = ssh.Run(ctx, survivors[0].Addr,
			fmt.Sprintf("sudo ovn-nbctl --db=%s --timeout=20 ls-add %s", dbList, sw))
		cancel()
		if writeErr == nil {
			break
		}
		time.Sleep(3 * time.Second)
	}
	require.NoErrorf(t, writeErr, "NB ls-add via surviving endpoint after leader %s loss", leader.Name)

	harness.Step(t, "confirm %s is present in NB via surviving endpoint", sw)
	readCtx, readCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer readCancel()
	out, err := ssh.Run(readCtx, survivors[0].Addr,
		fmt.Sprintf("sudo ovn-nbctl --db=%s --timeout=15 ls-list", dbList))
	require.NoError(t, err, "NB ls-list via surviving endpoint")
	require.Containsf(t, string(out), sw, "switch %s not visible after failover write", sw)
}

var ovnRaftDBs = []struct{ name, sock, schema string }{
	{"NB", ovnNBSock, host.OVNNBSchema},
	{"SB", ovnSBSock, host.OVNSBSchema},
}

// assertOVNQuorum checks every DB node lists 3 servers per schema and that
// exactly one of them leads each.
func assertOVNQuorum(t *testing.T, ssh *harness.PeerSSH, dbNodes []harness.Node) {
	t.Helper()
	for _, db := range ovnRaftDBs {
		leaders := 0
		for _, n := range dbNodes {
			harness.Step(t, "%s cluster/status on %s (%s)", db.name, n.Name, n.Addr)
			servers, role := ovnClusterStatus(t, ssh, n, db.sock, db.schema)
			harness.Detail(t, fmt.Sprintf("%s_%s_servers", db.name, n.Name), servers)
			harness.Detail(t, fmt.Sprintf("%s_%s_role", db.name, n.Name), role)
			require.Equalf(t, 3, servers, "%s on %s: want 3 servers in quorum", db.name, n.Name)
			if role == "leader" {
				leaders++
			}
		}
		require.Equalf(t, 1, leaders, "%s: want exactly one leader across the quorum", db.name)
	}
}

// waitOVNRaftMember polls until the node's server for schema is a cluster
// member that knows its leader, the condition setup-ovn.sh waits on.
func waitOVNRaftMember(t *testing.T, ssh *harness.PeerSSH, n harness.Node, sock, schema string, timeout time.Duration) {
	t.Helper()
	var last string
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		out, err := ssh.Run(ctx, n.Addr, fmt.Sprintf("sudo ovn-appctl -t %s cluster/status %s", sock, schema))
		cancel()
		last = string(out)
		if err == nil && ovnRaftMemberRejoined(last) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("%s on %s did not rejoin its RAFT cluster within %s; last cluster/status:\n%s", schema, n.Name, timeout, last)
}

// ovnRaftMemberRejoined reports whether cluster/status shows a cluster member
// following a known leader, or leading itself.
func ovnRaftMemberRejoined(out string) bool {
	var member, leader bool
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "Status: cluster member" {
			member = true
		}
		if rest, ok := strings.CutPrefix(line, "Leader:"); ok {
			v := strings.TrimSpace(rest)
			leader = v != "" && v != "unknown"
		}
	}
	return member && leader
}

// ovnClusterStatus runs cluster/status for one schema on a node and returns the
// reported server count and the queried server's role.
func ovnClusterStatus(t *testing.T, ssh *harness.PeerSSH, n harness.Node, sock, schema string) (servers int, role string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := ssh.Run(ctx, n.Addr, fmt.Sprintf("sudo ovn-appctl -t %s cluster/status %s", sock, schema))
	require.NoErrorf(t, err, "cluster/status %s on %s", schema, n.Name)
	return host.ParseClusterStatus(string(out))
}

// nbEndpointList builds the comma-separated NB client endpoint list used by
// ovn-nbctl --db across the given DB nodes.
func nbEndpointList(nodes []harness.Node) string {
	eps := make([]string, len(nodes))
	for i, n := range nodes {
		eps[i] = fmt.Sprintf("tcp:%s:6641", n.Addr)
	}
	return strings.Join(eps, ",")
}
