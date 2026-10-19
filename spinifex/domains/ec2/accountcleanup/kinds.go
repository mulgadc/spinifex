package accountcleanup

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"time"

	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	ec2launchtemplate "github.com/mulgadc/spinifex/spinifex/domains/ec2/launchtemplate"
	ec2placementgroup "github.com/mulgadc/spinifex/spinifex/domains/ec2/placementgroup"
)

// Each kind runs scan, owner cleanup, then re-scan. The first scan only bounds
// how much nameless residue a run can claim to have removed.

type keyPairRun struct {
	kind *kindRun
	// unchecked: the reference check failed, so nothing was attempted.
	unchecked bool
	before    *ec2key.AccountScan
	cleaned   *ec2key.AccountCleanup
	did       attempts
}

// cleanKeyPairs deletes only when the reference check completed; otherwise
// every key pair and orphan public key is kept and the kind is incomplete.
func (c *Cleanup) cleanKeyPairs(ctx context.Context, acct string, settledBefore time.Time, refs referenceCheck) *keyPairRun {
	run := &keyPairRun{kind: c.startKind(ec2v1.PlatformKindKeyPair), did: newAttempts()}
	if before, err := c.keys.ScanAccount(ctx, acct); err == nil {
		run.before = before
	}
	if !refs.complete {
		run.unchecked = true
		run.kind.incomplete(ReasonReferenceCheckFailed)
		return run
	}
	cleaned, err := c.keys.CleanupAccount(ctx, acct, settledBefore, refs.keyReason)
	if err != nil {
		slog.WarnContext(ctx, "Platform cleanup: key-pair cleanup failed", "err", err)
		run.kind.incomplete(ec2v1.PlatformReasonStoreListFailed)
		return run
	}
	run.cleaned = cleaned
	markAll(run.did.deleted, cleaned.DeletedKeyPairIDs)
	markAll(run.did.failed, cleaned.FailedKeyPairIDs)
	maps.Copy(run.did.kept, cleaned.Referenced)
	return run
}

func (c *Cleanup) finishKeyPairs(ctx context.Context, acct string, run *keyPairRun, res *Result) ownedIDs {
	defer c.finishKind(run.kind)
	after, err := c.keys.ScanAccount(ctx, acct)
	if err != nil {
		slog.WarnContext(ctx, "Platform cleanup: key-pair re-scan failed", "err", err)
		run.kind.incomplete(ec2v1.PlatformReasonStoreListFailed)
		after = nil
	}
	present := map[string]bool{}
	if after != nil {
		markAll(present, after.LiveKeyPairIDs)
		markAll(present, after.UnreadableKeyPairIDs)
		if after.ReadFailures > 0 {
			run.kind.incomplete(ec2v1.PlatformReasonRecordReadFailed)
		}
	}
	var beforeLive, afterLive, deleted, failed []string
	if run.before != nil {
		beforeLive = run.before.LiveKeyPairIDs
	}
	if after != nil {
		afterLive = after.LiveKeyPairIDs
	}
	if run.cleaned != nil {
		deleted, failed = run.cleaned.DeletedKeyPairIDs, run.cleaned.FailedKeyPairIDs
	}
	if run.unchecked {
		keepAll(run.did.kept, beforeLive, ReasonReferenceCheckFailed)
		keepAll(run.did.kept, afterLive, ReasonReferenceCheckFailed)
	}
	for _, id := range sortedUnion(beforeLive, deleted, failed, afterLive) {
		disposition, reason := itemDisposition(id, run.did, present[id], after != nil)
		res.addItem(run.kind, id, disposition, reason, 0)
	}
	if after == nil {
		return ownedIDs{}
	}
	res.addCount(run.kind, ec2v1.PlatformClassUnreadableRecord, DispositionResidue, ReasonUnreadableRecord, len(after.UnreadableKeyPairIDs))
	res.addCount(run.kind, ec2v1.PlatformClassPublicKeyUnresolved, DispositionResidue, ReasonOwnerRecordsUnresolved, after.UnresolvedPublicKeys)
	var before, removed, failedN, unsettled int
	kept := map[string]int{}
	if run.before != nil {
		before = run.before.OrphanPublicKeys
	}
	if run.cleaned != nil {
		removed, failedN, unsettled = run.cleaned.OrphansRemoved, run.cleaned.OrphansFailed, run.cleaned.OrphansUnsettled
		kept = run.cleaned.OrphansReferenced
	} else if run.unchecked {
		kept[ReasonReferenceCheckFailed] = after.OrphanPublicKeys
	}
	res.residueCounts(run.kind, ec2v1.PlatformClassPublicKeyWithoutMetadata, before, after.OrphanPublicKeys, removed, failedN, unsettled, kept)
	return ownedIDs{ids: present, resolved: after.ReadFailures == 0 && len(after.UnreadableKeyPairIDs) == 0}
}

type placementGroupRun struct {
	kind      *kindRun
	unchecked bool
	before    *ec2placementgroup.AccountScan
	cleaned   *ec2placementgroup.AccountCleanup
	did       attempts
}

// cleanPlacementGroups deletes only when the reference check completed.
func (c *Cleanup) cleanPlacementGroups(ctx context.Context, acct string, refs referenceCheck) *placementGroupRun {
	run := &placementGroupRun{kind: c.startKind(ec2v1.PlatformKindPlacementGroup), did: newAttempts()}
	if before, err := c.groups.ScanAccount(ctx, acct); err == nil {
		run.before = before
	}
	if !refs.complete {
		run.unchecked = true
		run.kind.incomplete(ReasonReferenceCheckFailed)
		return run
	}
	cleaned, err := c.groups.CleanupAccount(ctx, acct, refs.groupReason)
	if err != nil {
		slog.WarnContext(ctx, "Platform cleanup: placement-group cleanup failed", "err", err)
		run.kind.incomplete(ec2v1.PlatformReasonStoreListFailed)
		return run
	}
	run.cleaned = cleaned
	markAll(run.did.deleted, cleaned.DeletedGroupIDs)
	markAll(run.did.failed, cleaned.FailedGroupIDs)
	for _, b := range cleaned.Blocked {
		run.did.blocked[b.GroupID] = b.MemberCount
	}
	maps.Copy(run.did.kept, cleaned.Referenced)
	return run
}

// finishPlacementGroups reports a group that still names instances as
// blocked with its member count, from the re-scan when it can.
func (c *Cleanup) finishPlacementGroups(ctx context.Context, acct string, run *placementGroupRun, res *Result) ownedIDs {
	defer c.finishKind(run.kind)
	after, err := c.groups.ScanAccount(ctx, acct)
	if err != nil {
		slog.WarnContext(ctx, "Platform cleanup: placement-group re-scan failed", "err", err)
		run.kind.incomplete(ec2v1.PlatformReasonStoreListFailed)
		after = nil
	}
	present := map[string]bool{}
	members := map[string]int{}
	var ids []string
	if run.before != nil {
		for _, g := range run.before.Groups {
			ids = append(ids, g.GroupID)
		}
	}
	if run.cleaned != nil {
		ids = append(ids, run.cleaned.DeletedGroupIDs...)
		ids = append(ids, run.cleaned.FailedGroupIDs...)
		for _, b := range run.cleaned.Blocked {
			ids = append(ids, b.GroupID)
		}
		for id := range run.cleaned.Referenced {
			ids = append(ids, id)
		}
	}
	if after != nil {
		for _, g := range after.Groups {
			present[g.GroupID] = true
			members[g.GroupID] = g.MemberCount
			ids = append(ids, g.GroupID)
		}
		if after.ReadFailures > 0 {
			run.kind.incomplete(ec2v1.PlatformReasonRecordReadFailed)
		}
	}
	if run.unchecked {
		keepAll(run.did.kept, ids, ReasonReferenceCheckFailed)
	}
	for _, id := range sortedUnion(ids) {
		if present[id] && (members[id] > 0 || run.did.blocked[id] > 0) {
			count := members[id]
			if count == 0 {
				count = run.did.blocked[id]
			}
			res.addItem(run.kind, id, DispositionBlocked, ReasonInstanceMembers, count)
			continue
		}
		disposition, reason := itemDisposition(id, run.did, present[id], after != nil)
		res.addItem(run.kind, id, disposition, reason, 0)
	}
	if after == nil {
		return ownedIDs{}
	}
	res.addCount(run.kind, ec2v1.PlatformClassUnreadableRecord, DispositionResidue, ReasonUnreadableRecord, after.Unreadable)
	return ownedIDs{ids: present, resolved: after.ReadFailures == 0 && after.Unreadable == 0}
}

type launchTemplateRun struct {
	kind    *kindRun
	before  *ec2launchtemplate.AccountScan
	cleaned *ec2launchtemplate.AccountCleanup
	did     attempts
}

func (c *Cleanup) cleanLaunchTemplates(ctx context.Context, acct string, settledBefore time.Time) *launchTemplateRun {
	run := &launchTemplateRun{kind: c.startKind(ec2v1.PlatformKindLaunchTemplate), did: newAttempts()}
	if before, err := c.templates.ScanAccount(ctx, acct); err == nil {
		run.before = before
	}
	cleaned, err := c.templates.CleanupAccountResidue(ctx, acct, settledBefore)
	if err != nil {
		slog.WarnContext(ctx, "Platform cleanup: launch-template residue cleanup failed", "err", err)
		run.kind.incomplete(ec2v1.PlatformReasonStoreListFailed)
		return run
	}
	run.cleaned = cleaned
	keepAll(run.did.kept, cleaned.KeptTemplateIDs, ReasonReferenceUnknown)
	return run
}

func (c *Cleanup) finishLaunchTemplates(ctx context.Context, acct string, run *launchTemplateRun, res *Result) ownedIDs {
	defer c.finishKind(run.kind)
	after, err := c.templates.ScanAccount(ctx, acct)
	if err != nil {
		slog.WarnContext(ctx, "Platform cleanup: launch-template re-scan failed", "err", err)
		run.kind.incomplete(ec2v1.PlatformReasonStoreListFailed)
		after = nil
	}
	present := map[string]bool{}
	if after != nil {
		markAll(present, after.LiveTemplateIDs)
		markAll(present, after.UnreadableTemplateIDs)
		if after.ReadFailures > 0 {
			run.kind.incomplete(ec2v1.PlatformReasonRecordReadFailed)
		}
	}
	var beforeLive, afterLive, kept []string
	if run.before != nil {
		beforeLive = run.before.LiveTemplateIDs
	}
	if after != nil {
		afterLive = after.LiveTemplateIDs
	}
	if run.cleaned != nil {
		kept = run.cleaned.KeptTemplateIDs
	}
	// Live templates are never deleted here, so every one still present is
	// blocked on the unknown EKS nodegroup reference, whenever it appeared.
	for _, id := range afterLive {
		run.did.kept[id] = ReasonReferenceUnknown
	}
	for _, id := range sortedUnion(beforeLive, kept, afterLive) {
		disposition, reason := itemDisposition(id, run.did, present[id], after != nil)
		res.addItem(run.kind, id, disposition, reason, 0)
	}
	if after == nil {
		return ownedIDs{}
	}
	res.addCount(run.kind, ec2v1.PlatformClassUnreadableRecord, DispositionResidue, ReasonUnreadableRecord,
		len(after.UnreadableTemplateIDs)+after.UnreadableNameIndexEntries)
	res.addCount(run.kind, ec2v1.PlatformClassUnrecognisedTemplateEntry, DispositionResidue, ReasonUnrecognisedRecord, after.UnrecognisedEntries)
	var c0 ec2launchtemplate.AccountCleanup
	if run.cleaned != nil {
		c0 = *run.cleaned
	}
	var beforeBodies, beforeNames int
	if run.before != nil {
		beforeBodies, beforeNames = run.before.OrphanVersionBodies, run.before.OrphanNameIndexEntries
	}
	res.residueCounts(run.kind, ec2v1.PlatformClassVersionBodyWithoutHeader, beforeBodies, after.OrphanVersionBodies,
		c0.OrphanBodiesRemoved, c0.OrphanBodiesFailed, c0.OrphanBodiesUnsettled, nil)
	res.residueCounts(run.kind, ec2v1.PlatformClassNameIndexWithoutHeader, beforeNames, after.OrphanNameIndexEntries,
		c0.OrphanNamesRemoved, c0.OrphanNamesFailed, c0.OrphanNamesUnsettled, nil)
	return ownedIDs{ids: present, resolved: after.ReadFailures == 0 && len(after.UnreadableTemplateIDs) == 0 &&
		after.UnreadableNameIndexEntries == 0 && after.UnrecognisedEntries == 0}
}

// cleanTagIndex clears entries whose owner re-scan resolved and holds no
// record for the ID, then re-lists the index to prove each clear. Entries of
// resources that still exist are theirs, not residue, and are left alone.
func (c *Cleanup) cleanTagIndex(ctx context.Context, acct string, kind *kindRun, tagged []string, listed bool, owners map[string]ownedIDs, res *Result) KindReport {
	if listed {
		c.clearOrphanTags(ctx, acct, kind, tagged, owners, res)
	}
	c.finishKind(kind)
	return kind.report
}

func (c *Cleanup) clearOrphanTags(ctx context.Context, acct string, kind *kindRun, tagged []string, owners map[string]ownedIDs, res *Result) {
	// A resource this run reports as not deleted owns its tags even when its
	// kind's re-scan failed.
	reported := map[string]bool{}
	for _, it := range res.Items {
		if it.Disposition != DispositionDeleted {
			reported[it.ResourceID] = true
		}
	}
	var cleared []string
	for _, id := range tagged {
		owner := owners[idPrefix(id)]
		switch {
		case owner.ids[id], reported[id]:
			kind.found = true
		case !owner.resolved:
			kind.incomplete(ec2v1.PlatformReasonOwnerRecordsUnresolved)
			res.addItem(kind, id, DispositionResidue, ReasonOwnerRecordsUnresolved, 0)
		default:
			if err := c.tags.DeleteAllTags(ctx, acct, id); err != nil {
				slog.WarnContext(ctx, "Platform cleanup: tag-index clear failed", "resourceId", id, "err", err)
				res.addItem(kind, id, DispositionFailed, ReasonOwnerDeleteFailed, 0)
				continue
			}
			cleared = append(cleared, id)
		}
	}
	after, err := c.tags.ScanAccountResourceIDs(ctx, acct, keyPairIDPrefix, placementGroupIDPrefix, launchTemplateIDPrefix)
	if err != nil {
		slog.WarnContext(ctx, "Platform cleanup: tag index re-scan failed", "err", err)
		kind.incomplete(ec2v1.PlatformReasonStoreListFailed)
	}
	for _, id := range cleared {
		disposition, reason := itemDisposition(id, attempts{deleted: map[string]bool{id: true}}, slices.Contains(after, id), err == nil)
		res.addItem(kind, id, disposition, reason, 0)
	}
	if len(after) > 0 {
		kind.found = true
	}
}

func keepAll(set map[string]string, ids []string, reason string) {
	for _, id := range ids {
		set[id] = reason
	}
}

// referenceCheck is the outcome of reading what instance records, spot
// requests and launch-template versions name. complete is false when any
// reader failed or is absent.
type referenceCheck struct {
	complete                  bool
	instances, spot, template *References
}

// readReferences runs every reference check. A check that cannot complete is
// never read as "no references".
func (c *Cleanup) readReferences(ctx context.Context, acct string) referenceCheck {
	r := c.refs
	if r.Instances == nil || r.Spot == nil || r.Templates == nil {
		slog.WarnContext(ctx, "Platform cleanup: reference reader not configured")
		return referenceCheck{}
	}
	instances, err := r.Instances.AccountInstanceReferences(ctx, acct)
	if err != nil || instances == nil {
		slog.WarnContext(ctx, "Platform cleanup: instance reference check failed", "err", err)
		return referenceCheck{}
	}
	launch, err := r.Spot.AccountLaunchReferences(ctx, acct)
	if err != nil || launch == nil {
		slog.WarnContext(ctx, "Platform cleanup: spot reference check failed", "err", err)
		return referenceCheck{}
	}
	tmpl, err := r.Templates.AccountTemplateReferences(ctx, acct)
	if err != nil || tmpl == nil {
		slog.WarnContext(ctx, "Platform cleanup: launch-template reference check failed", "err", err)
		return referenceCheck{}
	}
	inst := newReferences()
	inst.merge(instances)
	spot := newReferences()
	spot.addKeys(launch.KeyNames...)
	spot.addGroups(launch.PlacementGroupNames...)
	template := newReferences()
	template.addKeys(tmpl.KeyNames...)
	template.addGroups(tmpl.PlacementGroupNames...)
	template.addGroupIDs(tmpl.PlacementGroupIDs...)
	return referenceCheck{complete: true, instances: inst, spot: spot, template: template}
}

func (r referenceCheck) keyReason(name string) string {
	switch {
	case r.instances.KeyNames[name]:
		return ReasonInstanceReference
	case r.spot.KeyNames[name]:
		return ReasonSpotRequestReference
	case r.template.KeyNames[name]:
		return ReasonLaunchTemplateReference
	}
	return ""
}

func (r referenceCheck) groupReason(id, name string) string {
	switch {
	case r.instances.hasGroup(id, name):
		return ReasonInstanceReference
	case r.spot.hasGroup(id, name):
		return ReasonSpotRequestReference
	case r.template.hasGroup(id, name):
		return ReasonLaunchTemplateReference
	}
	return ""
}

func markAll(set map[string]bool, ids []string) {
	for _, id := range ids {
		set[id] = true
	}
}
