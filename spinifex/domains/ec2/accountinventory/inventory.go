// Package accountinventory reports one account's EC2 key pairs, placement
// groups and launch templates from their owners' stores. It composes the
// resource packages' read-only scans and owns the dependency knowledge
// between them; it never writes to any store.
package accountinventory

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	ec2launchtemplate "github.com/mulgadc/spinifex/spinifex/domains/ec2/launchtemplate"
	ec2placementgroup "github.com/mulgadc/spinifex/spinifex/domains/ec2/placementgroup"
	ec2tags "github.com/mulgadc/spinifex/spinifex/domains/ec2/tags"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
)

// KeyPairScanner reads one account's key-pair objects.
type KeyPairScanner interface {
	ScanAccount(ctx context.Context, accountID string) (*ec2key.AccountScan, error)
}

// PlacementGroupScanner reads one account's placement-group records.
type PlacementGroupScanner interface {
	ScanAccount(ctx context.Context, accountID string) (*ec2placementgroup.AccountScan, error)
}

// LaunchTemplateScanner reads one account's launch-template records.
type LaunchTemplateScanner interface {
	ScanAccount(ctx context.Context, accountID string) (*ec2launchtemplate.AccountScan, error)
}

// TagIndexScanner lists one account's tagged resource IDs by prefix.
type TagIndexScanner interface {
	ScanAccountResourceIDs(ctx context.Context, accountID string, prefixes ...string) ([]string, error)
}

var (
	_ KeyPairScanner        = (*ec2key.KeyServiceImpl)(nil)
	_ PlacementGroupScanner = (*ec2placementgroup.PlacementGroupServiceImpl)(nil)
	_ LaunchTemplateScanner = (*ec2launchtemplate.LaunchTemplateServiceImpl)(nil)
	_ TagIndexScanner       = (*ec2tags.TagsServiceImpl)(nil)
)

// Resource-ID prefixes of the three kinds in the tag index.
const (
	keyPairIDPrefix        = "key-"
	placementGroupIDPrefix = "pg-"
	launchTemplateIDPrefix = "lt-"
)

// Inventory answers platform-inventory requests from the owner stores.
type Inventory struct {
	keys      KeyPairScanner
	groups    PlacementGroupScanner
	templates LaunchTemplateScanner
	tags      TagIndexScanner
	now       func() time.Time
}

// New builds an inventory over the four owner scans.
func New(keys KeyPairScanner, groups PlacementGroupScanner, templates LaunchTemplateScanner, tags TagIndexScanner) *Inventory {
	return &Inventory{keys: keys, groups: groups, templates: templates, tags: tags, now: time.Now}
}

// owned is the set of IDs whose owner record exists, live or unreadable, for
// one kind; resolved is false when an owner record exists whose ID is unknown.
type owned struct {
	ids      map[string]bool
	resolved bool
}

// kindScan accumulates one kind's report while its stores are read.
type kindScan struct {
	report ec2v1.PlatformKindReport
	found  bool
}

func (i *Inventory) startKind(kind string) *kindScan {
	return &kindScan{report: ec2v1.PlatformKindReport{Kind: kind, ScanStartedAt: i.now().UTC()}}
}

func (k *kindScan) incomplete(reason string) {
	if !slices.Contains(k.report.IncompleteReasons, reason) {
		k.report.IncompleteReasons = append(k.report.IncompleteReasons, reason)
	}
}

func (i *Inventory) finishKind(k *kindScan) ec2v1.PlatformKindReport {
	k.report.ScanEndedAt = i.now().UTC()
	switch {
	case len(k.report.IncompleteReasons) > 0:
		k.report.Status = ec2v1.PlatformStatusScanIncomplete
	case k.found:
		k.report.Status = ec2v1.PlatformStatusRecordsFound
	default:
		k.report.Status = ec2v1.PlatformStatusObservedClean
	}
	return k.report
}

// PlatformInventory scans the account's tag index first and then each owner
// store, so a resource created mid-scan shows as live rather than as a
// tag-index entry with no owner record.
func (i *Inventory) PlatformInventory(ctx context.Context, req *ec2v1.PlatformInventoryRequest) (*ec2v1.PlatformInventoryReply, error) {
	if req == nil || req.AccountID == "" {
		return nil, awserrors.Errorf(awserrors.ErrorMissingParameter, "The request must specify an account ID.")
	}
	if !awsidentifiers.IsAccountID(req.AccountID) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "The account ID must be 12 digits.")
	}
	start := time.Now()
	reply := &ec2v1.PlatformInventoryReply{AccountID: req.AccountID, CorrelationRef: req.CorrelationRef}

	tagScan := i.startKind(ec2v1.PlatformKindTagIndex)
	tagged, tagErr := i.tags.ScanAccountResourceIDs(ctx, req.AccountID, keyPairIDPrefix, placementGroupIDPrefix, launchTemplateIDPrefix)
	if tagErr != nil {
		slog.WarnContext(ctx, "Platform inventory: tag index listing failed", "err", tagErr)
		tagScan.incomplete(ec2v1.PlatformReasonStoreListFailed)
	}
	tagScan.found = len(tagged) > 0

	keyReport, keyOwned := i.scanKeyPairs(ctx, req.AccountID, reply)
	groupReport, groupOwned := i.scanPlacementGroups(ctx, req.AccountID, reply)
	templateReport, templateOwned := i.scanLaunchTemplates(ctx, req.AccountID, reply)

	byPrefix := map[string]owned{
		keyPairIDPrefix:        keyOwned,
		placementGroupIDPrefix: groupOwned,
		launchTemplateIDPrefix: templateOwned,
	}
	for _, id := range tagged {
		parent := byPrefix[idPrefix(id)]
		switch {
		case parent.ids[id]:
		case !parent.resolved:
			tagScan.incomplete(ec2v1.PlatformReasonOwnerRecordsUnresolved)
		default:
			reply.Items = append(reply.Items, ec2v1.PlatformItem{
				ResourceID: id, Kind: ec2v1.PlatformKindTagIndex, Disposition: ec2v1.PlatformDispositionResidue,
			})
		}
	}
	tagReport := i.finishKind(tagScan)

	reply.Kinds = []ec2v1.PlatformKindReport{keyReport, groupReport, templateReport, tagReport}
	slog.InfoContext(ctx, "Platform inventory completed", "accountID", req.AccountID,
		"correlationRef", req.CorrelationRef, "items", len(reply.Items), "duration_ms", otelsetup.Millis(time.Since(start)))
	return reply, nil
}

func idPrefix(id string) string {
	for _, prefix := range []string{keyPairIDPrefix, placementGroupIDPrefix, launchTemplateIDPrefix} {
		if strings.HasPrefix(id, prefix) {
			return prefix
		}
	}
	return ""
}

func (i *Inventory) scanKeyPairs(ctx context.Context, accountID string, reply *ec2v1.PlatformInventoryReply) (ec2v1.PlatformKindReport, owned) {
	kind := i.startKind(ec2v1.PlatformKindKeyPair)
	scan, err := i.keys.ScanAccount(ctx, accountID)
	if err != nil {
		slog.WarnContext(ctx, "Platform inventory: key-pair listing failed", "err", err)
		kind.incomplete(ec2v1.PlatformReasonStoreListFailed)
		return i.finishKind(kind), owned{}
	}
	ids := make(map[string]bool)
	for _, id := range scan.LiveKeyPairIDs {
		ids[id] = true
		addItem(reply, kind, id, ec2v1.PlatformDispositionLive)
	}
	for _, id := range scan.UnreadableKeyPairIDs {
		ids[id] = true
	}
	addCount(reply, kind, ec2v1.PlatformDispositionUnreadable, ec2v1.PlatformClassUnreadableRecord, len(scan.UnreadableKeyPairIDs))
	addCount(reply, kind, ec2v1.PlatformDispositionResidue, ec2v1.PlatformClassPublicKeyWithoutMetadata, scan.OrphanPublicKeys)
	addCount(reply, kind, ec2v1.PlatformDispositionUnreadable, ec2v1.PlatformClassPublicKeyUnresolved, scan.UnresolvedPublicKeys)
	if scan.ReadFailures > 0 {
		kind.incomplete(ec2v1.PlatformReasonRecordReadFailed)
	}
	return i.finishKind(kind), owned{ids: ids, resolved: true}
}

// scanPlacementGroups reports a group whose member list still names instances
// as dependency-held: it cannot be deleted until those instances leave it.
func (i *Inventory) scanPlacementGroups(ctx context.Context, accountID string, reply *ec2v1.PlatformInventoryReply) (ec2v1.PlatformKindReport, owned) {
	kind := i.startKind(ec2v1.PlatformKindPlacementGroup)
	scan, err := i.groups.ScanAccount(ctx, accountID)
	if err != nil {
		slog.WarnContext(ctx, "Platform inventory: placement-group listing failed", "err", err)
		kind.incomplete(ec2v1.PlatformReasonStoreListFailed)
		return i.finishKind(kind), owned{}
	}
	ids := make(map[string]bool)
	for _, group := range scan.Groups {
		ids[group.GroupID] = true
		if group.MemberCount > 0 {
			addItem(reply, kind, group.GroupID, ec2v1.PlatformDispositionDependencyHeld)
			continue
		}
		addItem(reply, kind, group.GroupID, ec2v1.PlatformDispositionLive)
	}
	addCount(reply, kind, ec2v1.PlatformDispositionUnreadable, ec2v1.PlatformClassUnreadableRecord, scan.Unreadable)
	if scan.ReadFailures > 0 {
		kind.incomplete(ec2v1.PlatformReasonRecordReadFailed)
	}
	return i.finishKind(kind), owned{ids: ids, resolved: scan.Unreadable == 0}
}

func (i *Inventory) scanLaunchTemplates(ctx context.Context, accountID string, reply *ec2v1.PlatformInventoryReply) (ec2v1.PlatformKindReport, owned) {
	kind := i.startKind(ec2v1.PlatformKindLaunchTemplate)
	scan, err := i.templates.ScanAccount(ctx, accountID)
	if err != nil {
		slog.WarnContext(ctx, "Platform inventory: launch-template listing failed", "err", err)
		kind.incomplete(ec2v1.PlatformReasonStoreListFailed)
		return i.finishKind(kind), owned{}
	}
	ids := make(map[string]bool)
	for _, id := range scan.LiveTemplateIDs {
		ids[id] = true
		addItem(reply, kind, id, ec2v1.PlatformDispositionLive)
	}
	for _, id := range scan.UnreadableTemplateIDs {
		ids[id] = true
	}
	addCount(reply, kind, ec2v1.PlatformDispositionUnreadable, ec2v1.PlatformClassUnreadableRecord, len(scan.UnreadableTemplateIDs)+scan.UnreadableNameIndexEntries)
	addCount(reply, kind, ec2v1.PlatformDispositionUnreadable, ec2v1.PlatformClassUnrecognisedTemplateEntry, scan.UnrecognisedEntries)
	addCount(reply, kind, ec2v1.PlatformDispositionResidue, ec2v1.PlatformClassVersionBodyWithoutHeader, scan.OrphanVersionBodies)
	addCount(reply, kind, ec2v1.PlatformDispositionResidue, ec2v1.PlatformClassNameIndexWithoutHeader, scan.OrphanNameIndexEntries)
	if scan.ReadFailures > 0 {
		kind.incomplete(ec2v1.PlatformReasonRecordReadFailed)
	}
	return i.finishKind(kind), owned{ids: ids, resolved: true}
}

func addItem(reply *ec2v1.PlatformInventoryReply, kind *kindScan, id, disposition string) {
	kind.found = true
	reply.Items = append(reply.Items, ec2v1.PlatformItem{
		ResourceID: id, Kind: kind.report.Kind, Disposition: disposition,
	})
}

func addCount(reply *ec2v1.PlatformInventoryReply, kind *kindScan, disposition, class string, n int) {
	if n == 0 {
		return
	}
	kind.found = true
	reply.Counts = append(reply.Counts, ec2v1.PlatformRecordCount{
		Kind: kind.report.Kind, Disposition: disposition, Class: class, Count: n,
	})
}
