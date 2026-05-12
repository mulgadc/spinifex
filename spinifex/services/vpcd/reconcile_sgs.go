package vpcd

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// reconcileSGsLoopInterval is how often the reconciler walks KV + OVN to
// converge drift. Chosen so a vpcd that wins AcquireReconcileLeader (60s TTL)
// typically holds the bucket key across at least one cycle.
const reconcileSGsLoopInterval = 30 * time.Second

// SGReconcileResult tracks what each periodic pass converged.
type SGReconcileResult struct {
	PortGroupsRecreated     int
	PortMembershipsSynced   int
	OrphanPortGroupsRemoved int
}

// ReconcileSGsLoop runs the SG/ENI convergence scans every 30s, gated on
// AcquireReconcileLeader so only one vpcd in the cluster runs them at a time.
// Returns when ctx is cancelled.
func ReconcileSGsLoop(ctx context.Context, nc *nats.Conn, topo *TopologyHandler) {
	holder, _ := os.Hostname()
	ticker := time.NewTicker(reconcileSGsLoopInterval)
	defer ticker.Stop()

	for {
		release, elected := AcquireReconcileLeader(nc, holder)
		if elected {
			result := ReconcileSGsOnce(ctx, nc, topo)
			if result.PortGroupsRecreated+result.PortMembershipsSynced+result.OrphanPortGroupsRemoved > 0 {
				slog.Info("vpcd reconcile-sgs: converged drift",
					"port_groups_recreated", result.PortGroupsRecreated,
					"port_memberships_synced", result.PortMembershipsSynced,
					"orphan_port_groups_removed", result.OrphanPortGroupsRemoved,
				)
			}
			release()
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ReconcileSGsOnce runs a single pass. Exported so tests can drive it directly.
func ReconcileSGsOnce(ctx context.Context, nc *nats.Conn, topo *TopologyHandler) SGReconcileResult {
	var result SGReconcileResult
	if topo == nil || topo.ovn == nil {
		slog.Warn("vpcd reconcile-sgs: skipped — topology handler not connected")
		return result
	}

	js, err := nc.JetStream()
	if err != nil {
		slog.Warn("vpcd reconcile-sgs: failed to get JetStream context", "err", err)
		return result
	}

	sgKV, err := js.KeyValue(handlers_ec2_vpc.KVBucketSecurityGroups)
	if err != nil {
		slog.Debug("vpcd reconcile-sgs: SG bucket not available", "err", err)
		sgKV = nil
	}
	eniKV, err := js.KeyValue(handlers_ec2_vpc.KVBucketENIs)
	if err != nil {
		slog.Debug("vpcd reconcile-sgs: ENI bucket not available", "err", err)
		eniKV = nil
	}

	var sgs []handlers_ec2_vpc.SecurityGroupRecord
	if sgKV != nil {
		sgs = listSGRecords(sgKV)
	}

	// SG records without an OVN port group → recreate.
	if sgKV != nil {
		result.PortGroupsRecreated = scanMissingPortGroups(ctx, topo, sgs)
	}

	// OVN port groups with no matching SG record → tear down. Re-snapshot KV
	// right before the orphan check: scanMissingPortGroups may have taken
	// seconds on a large cluster, and any CreateSecurityGroup that landed in
	// the meantime would otherwise look like an orphan against the stale
	// snapshot and get silently deleted.
	if sgKV != nil {
		sgs = listSGRecords(sgKV)
	}
	result.OrphanPortGroupsRemoved = scanOrphanPortGroups(ctx, topo, sgs)

	// ENIs with SecurityGroupIds → reconcile membership.
	if eniKV != nil {
		result.PortMembershipsSynced = scanENIPortMembership(ctx, topo, eniKV)
	}

	return result
}

// listSGRecords reads every SG record from the bucket. Logs and skips
// malformed entries; the orphan-SG case can't occur on a healthy cluster.
func listSGRecords(sgKV nats.KeyValue) []handlers_ec2_vpc.SecurityGroupRecord {
	keys, err := sgKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		slog.Warn("vpcd reconcile-sgs: failed to list SG keys", "err", err)
		return nil
	}
	var out []handlers_ec2_vpc.SecurityGroupRecord
	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		entry, err := sgKV.Get(key)
		if err != nil {
			slog.Warn("vpcd reconcile-sgs: SG read failed", "key", key, "err", err)
			continue
		}
		var rec handlers_ec2_vpc.SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Warn("vpcd reconcile-sgs: SG unmarshal failed", "key", key, "err", err)
			continue
		}
		out = append(out, rec)
	}
	return out
}

// scanMissingPortGroups recreates the OVN port group + ACLs for any SG record
// whose port group has gone missing in OVN NB. Returns the count recreated.
func scanMissingPortGroups(ctx context.Context, topo *TopologyHandler, sgs []handlers_ec2_vpc.SecurityGroupRecord) int {
	recreated := 0
	for _, sg := range sgs {
		pgName := portGroupName(sg.GroupId)
		if _, err := topo.ovn.GetPortGroup(ctx, pgName); err == nil {
			continue
		}
		if err := topo.provisionSG(ctx, sg.GroupId, sgRulesToACLRules(sg.IngressRules), sgRulesToACLRules(sg.EgressRules)); err != nil {
			slog.Warn("vpcd reconcile-sgs: failed to recreate port group for SG", "sg", sg.GroupId, "err", err)
			continue
		}
		slog.Info("vpcd reconcile-sgs: recreated missing port group", "sg", sg.GroupId, "pg", pgName)
		recreated++
	}
	return recreated
}

// scanENIPortMembership runs reconcilePortSGs for every ENI with a non-empty
// SecurityGroupIds list. Idempotent. Counts only ENIs whose membership
// actually changed — a converged ENI scanned with no drift is a no-op and
// must not appear in the converged-drift log line.
func scanENIPortMembership(ctx context.Context, topo *TopologyHandler, eniKV nats.KeyValue) int {
	keys, err := eniKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		slog.Warn("vpcd reconcile-sgs: failed to list ENI keys", "err", err)
		return 0
	}
	synced := 0
	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		entry, err := eniKV.Get(key)
		if err != nil {
			slog.Warn("vpcd reconcile-sgs: ENI read failed", "key", key, "err", err)
			continue
		}
		var rec handlers_ec2_vpc.ENIRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Warn("vpcd reconcile-sgs: ENI unmarshal failed", "key", key, "err", err)
			continue
		}
		if len(rec.SecurityGroupIds) == 0 {
			continue
		}
		portName := "port-" + rec.NetworkInterfaceId
		changed, err := topo.reconcilePortSGs(ctx, portName, rec.SecurityGroupIds)
		if err != nil {
			// Most often: the LSP itself doesn't exist yet. Warn-and-continue.
			slog.Warn("vpcd reconcile-sgs: port SG reconcile failed", "eni", rec.NetworkInterfaceId, "err", err)
			continue
		}
		if changed {
			synced++
		}
	}
	return synced
}

// scanOrphanPortGroups removes any spinifex-managed port group (`sg_*`) in OVN
// that has no matching SG record in KV. Mirrors handleDeleteSG's teardown
// (ClearACLs → DeletePortGroup). The matching `<pg>_ip4` / `<pg>_ip6`
// Address_Set rows in SB are auto-derived from port group membership by
// ovn-northd, so removing the port group also removes its address sets.
// Non-`sg_*` port groups (e.g. third-party usage) are left alone. Returns
// the count removed.
func scanOrphanPortGroups(ctx context.Context, topo *TopologyHandler, sgs []handlers_ec2_vpc.SecurityGroupRecord) int {
	pgs, err := topo.ovn.ListPortGroups(ctx)
	if err != nil {
		slog.Warn("vpcd reconcile-sgs: list port groups failed", "err", err)
		return 0
	}

	expected := make(map[string]struct{}, len(sgs))
	for _, sg := range sgs {
		expected[portGroupName(sg.GroupId)] = struct{}{}
	}

	removed := 0
	for _, pg := range pgs {
		if !strings.HasPrefix(pg.Name, "sg_") {
			continue
		}
		if _, ok := expected[pg.Name]; ok {
			continue
		}
		if err := topo.ovn.ClearACLs(ctx, pg.Name); err != nil {
			slog.Warn("vpcd reconcile-sgs: orphan ClearACLs failed", "pg", pg.Name, "err", err)
			continue
		}
		if err := topo.ovn.DeletePortGroup(ctx, pg.Name); err != nil {
			slog.Warn("vpcd reconcile-sgs: orphan DeletePortGroup failed", "pg", pg.Name, "err", err)
			continue
		}
		slog.Info("vpcd reconcile-sgs: removed orphan port group", "pg", pg.Name)
		removed++
	}
	return removed
}

// sgRulesToACLRules converts handler-side SGRules to vpcd-side SGRuleForACL.
// The duplication keeps vpcd from importing the handler package.
func sgRulesToACLRules(rules []handlers_ec2_vpc.SGRule) []SGRuleForACL {
	out := make([]SGRuleForACL, len(rules))
	for i, r := range rules {
		out[i] = SGRuleForACL{
			IpProtocol: r.IpProtocol,
			FromPort:   r.FromPort,
			ToPort:     r.ToPort,
			CidrIp:     r.CidrIp,
			SourceSG:   r.SourceSG,
		}
	}
	return out
}
