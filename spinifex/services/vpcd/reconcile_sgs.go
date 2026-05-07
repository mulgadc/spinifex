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
// converge drift. Chosen so that a vpcd that wins AcquireReconcileLeader
// (60s TTL) typically holds the bucket key across at least one cycle —
// re-acquire-each-tick keeps things simple at the cost of a possible single-
// tick gap when leadership flips.
const reconcileSGsLoopInterval = 30 * time.Second

// SGReconcileResult tracks what each periodic pass converged. Useful for tests
// and metrics; the loop logs it at info level.
type SGReconcileResult struct {
	VPCsTransitioned       int
	DefaultSGsCreated      int
	PortGroupsRecreated    int
	PortMembershipsSynced  int
	OrphanPortGroupsPruned int
}

// ReconcileSGsLoop runs the four SG/VPC convergence scans every 30s, gated on
// AcquireReconcileLeader so only one vpcd in the cluster runs them at a time.
// Returns when ctx is cancelled. Log-and-continue: a per-pass error never
// breaks the loop.
func ReconcileSGsLoop(ctx context.Context, nc *nats.Conn, topo *TopologyHandler) {
	holder, _ := os.Hostname()
	ticker := time.NewTicker(reconcileSGsLoopInterval)
	defer ticker.Stop()

	for {
		release, elected := AcquireReconcileLeader(nc, holder)
		if elected {
			result := ReconcileSGsOnce(ctx, nc, topo)
			if result.changed() {
				slog.Info("vpcd reconcile-sgs: converged drift",
					"vpcs_transitioned", result.VPCsTransitioned,
					"default_sgs_created", result.DefaultSGsCreated,
					"port_groups_recreated", result.PortGroupsRecreated,
					"port_memberships_synced", result.PortMembershipsSynced,
					"orphan_port_groups_pruned", result.OrphanPortGroupsPruned,
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

func (r SGReconcileResult) changed() bool {
	return r.VPCsTransitioned+r.DefaultSGsCreated+r.PortGroupsRecreated+r.PortMembershipsSynced+r.OrphanPortGroupsPruned > 0
}

// ReconcileSGsOnce runs a single pass of the four scans. Exported so tests can
// drive it directly without waiting for the timer.
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

	vpcKV, err := js.KeyValue(handlers_ec2_vpc.KVBucketVPCs)
	if err != nil {
		slog.Debug("vpcd reconcile-sgs: VPC bucket not available", "err", err)
		// vpcKV may legitimately not exist on first boot — keep going so the
		// other scans can still report no-op for empty buckets.
		vpcKV = nil
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

	// Scan 1: pending VPCs → ensure default SG, flip to available.
	if vpcKV != nil && sgKV != nil {
		n1, n2 := scanPendingVPCs(nc, vpcKV, sgKV)
		result.DefaultSGsCreated = n1
		result.VPCsTransitioned = n2
	}

	// Scans 2+4 walk both SG records and OVN port groups; collect both once.
	var sgRecords []handlers_ec2_vpc.SecurityGroupRecord
	if sgKV != nil {
		sgRecords = listSGRecords(sgKV)
	}

	// Scan 2: SG records without an OVN port group → recreate.
	if sgKV != nil {
		result.PortGroupsRecreated = scanMissingPortGroups(ctx, topo, sgRecords)
	}

	// Scan 3: ENIs with SecurityGroupIds → reconcile membership.
	if eniKV != nil {
		result.PortMembershipsSynced = scanENIPortMembership(ctx, topo, eniKV)
	}

	// Scan 4: OVN port groups without an SG record → prune.
	result.OrphanPortGroupsPruned = scanOrphanPortGroups(ctx, topo, sgRecords)

	return result
}

// scanPendingVPCs walks the VPC bucket; for each VPC stuck in a non-available
// state it ensures a default SG exists then flips the VPC to "available".
// Returns (defaultSGsCreated, vpcsTransitioned).
func scanPendingVPCs(nc *nats.Conn, vpcKV, sgKV nats.KeyValue) (int, int) {
	created, transitioned := 0, 0

	keys, err := vpcKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		slog.Warn("vpcd reconcile-sgs: failed to list VPC keys", "err", err)
		return 0, 0
	}
	for _, key := range keys {
		if key == utils.VersionKey {
			continue
		}
		entry, err := vpcKV.Get(key)
		if err != nil {
			continue
		}
		var rec handlers_ec2_vpc.VPCRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			continue
		}
		if rec.State == "available" {
			continue
		}
		accountID, ok := accountIDFromKey(key, rec.VpcId)
		if !ok {
			continue
		}

		existing, err := handlers_ec2_vpc.FindDefaultSGForVPCKV(sgKV, accountID, rec.VpcId)
		if err != nil {
			slog.Warn("vpcd reconcile-sgs: lookup default SG failed", "vpc", rec.VpcId, "err", err)
			continue
		}
		if existing == "" {
			sgId, err := handlers_ec2_vpc.CreateDefaultSecurityGroupKV(sgKV, nc, accountID, rec.VpcId)
			if err != nil {
				slog.Warn("vpcd reconcile-sgs: create default SG failed", "vpc", rec.VpcId, "err", err)
				continue
			}
			slog.Info("vpcd reconcile-sgs: provisioned missing default SG", "vpc", rec.VpcId, "sg", sgId)
			created++
		}
		if err := handlers_ec2_vpc.SetVPCStateKV(vpcKV, accountID, rec.VpcId, "available"); err != nil {
			slog.Warn("vpcd reconcile-sgs: VPC state transition failed", "vpc", rec.VpcId, "err", err)
			continue
		}
		slog.Info("vpcd reconcile-sgs: transitioned VPC to available", "vpc", rec.VpcId)
		transitioned++
	}
	return created, transitioned
}

// listSGRecords reads every SG record from the bucket. Skips _version and
// malformed entries silently — same defensive shape as ReconcileFromKV.
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
			continue
		}
		var rec handlers_ec2_vpc.SecurityGroupRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
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
// SecurityGroupIds list. Idempotent: a no-drift ENI produces no OVN ops.
// Returns the count of ENIs successfully reconciled.
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
			continue
		}
		var rec handlers_ec2_vpc.ENIRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			continue
		}
		if len(rec.SecurityGroupIds) == 0 {
			continue
		}
		portName := "port-" + rec.NetworkInterfaceId
		if err := topo.reconcilePortSGs(ctx, portName, rec.PrivateIpAddress, rec.SecurityGroupIds); err != nil {
			// Most often: the LSP itself doesn't exist yet (ReconcileFromKV
			// runs once at startup, this loop runs forever — so the LSP may
			// catch up on the next pass). Warn-and-continue.
			slog.Warn("vpcd reconcile-sgs: port SG reconcile failed", "eni", rec.NetworkInterfaceId, "err", err)
			continue
		}
		synced++
	}
	return synced
}

// scanOrphanPortGroups deletes OVN port groups (and their address sets and
// ACLs) that have no matching SG record in KV. Restricts deletion to port
// groups whose name starts with the SG prefix so non-SG port groups (none
// today, but defensive) are never touched.
func scanOrphanPortGroups(ctx context.Context, topo *TopologyHandler, sgs []handlers_ec2_vpc.SecurityGroupRecord) int {
	pgs, err := topo.ovn.ListPortGroups(ctx)
	if err != nil {
		slog.Warn("vpcd reconcile-sgs: failed to list port groups", "err", err)
		return 0
	}

	expected := make(map[string]struct{}, len(sgs))
	for _, sg := range sgs {
		expected[portGroupName(sg.GroupId)] = struct{}{}
	}

	pruned := 0
	for _, pg := range pgs {
		if !strings.HasPrefix(pg.Name, "sg_") {
			continue
		}
		if _, ok := expected[pg.Name]; ok {
			continue
		}
		// Clear ACLs first; mock and live both reject deletion of a row whose
		// referenced children would dangle, and DeletePortGroup on the live
		// client uses Delete (not destroy-cascade).
		if err := topo.ovn.ClearACLs(ctx, pg.Name); err != nil {
			slog.Warn("vpcd reconcile-sgs: clear orphan ACLs failed", "pg", pg.Name, "err", err)
		}
		if err := topo.ovn.DeletePortGroup(ctx, pg.Name); err != nil {
			slog.Warn("vpcd reconcile-sgs: delete orphan port group failed", "pg", pg.Name, "err", err)
			continue
		}
		if err := topo.ovn.DeleteAddressSet(ctx, addressSetName(pg.Name)); err != nil {
			slog.Debug("vpcd reconcile-sgs: delete orphan address set failed (likely never existed)", "as", addressSetName(pg.Name), "err", err)
		}
		slog.Info("vpcd reconcile-sgs: pruned orphan port group", "pg", pg.Name)
		pruned++
	}
	return pruned
}

// accountIDFromKey extracts the account ID from a NATS KV key shaped like
// "<accountID>.<resourceID>". Returns false if the key doesn't match the
// expected resource ID at the tail.
func accountIDFromKey(key, resourceID string) (string, bool) {
	suffix := "." + resourceID
	if !strings.HasSuffix(key, suffix) {
		return "", false
	}
	accountID := strings.TrimSuffix(key, suffix)
	if accountID == "" {
		return "", false
	}
	return accountID, true
}

// sgRulesToACLRules converts handler-side SGRules to vpcd-side SGRuleForACL.
// Identical fields; the conversion is here to keep the package boundary clean.
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
