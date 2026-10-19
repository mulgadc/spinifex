// Package accountcleanup deletes the proven-safe subset of one account's EC2
// platform resources through the owners' own delete paths: placement groups
// with no members that no instance, spot request or live launch-template
// version names; key pairs none of those name; and launch-template residue no
// header owns. Live launch templates are never deleted: an EKS nodegroup may
// name one and EC2 cannot see it. A reference check that cannot complete
// deletes nothing of the kinds it guards. Every disposition is proved by an
// owner-store re-scan, never by Describe.
//
// It adds no caller, checks no authority and holds no store or transport of
// its own: the owners and reference readers are injected. Whoever binds it
// decides who may invoke it. It is not exposed on any NATS subject.
package accountcleanup

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

// PlatformCleaner deletes one account's EC2 platform resources and reports
// what the owner stores hold afterwards. It is destructive.
type PlatformCleaner interface {
	CleanupPlatform(ctx context.Context, req *Request) (*Result, error)
}

var _ PlatformCleaner = (*Cleanup)(nil)

// KeyPairOwner is the key-pair owner's account scan and cleanup.
type KeyPairOwner interface {
	ScanAccount(ctx context.Context, accountID string) (*ec2key.AccountScan, error)
	CleanupAccount(ctx context.Context, accountID string, settledBefore time.Time, referenced func(keyName string) string) (*ec2key.AccountCleanup, error)
}

// PlacementGroupOwner is the placement-group owner's account scan and cleanup.
type PlacementGroupOwner interface {
	ScanAccount(ctx context.Context, accountID string) (*ec2placementgroup.AccountScan, error)
	CleanupAccount(ctx context.Context, accountID string, referenced func(groupID, groupName string) string) (*ec2placementgroup.AccountCleanup, error)
}

// LaunchTemplateOwner is the launch-template owner's account scan and residue
// cleanup; live templates are never deleted by account cleanup.
type LaunchTemplateOwner interface {
	ScanAccount(ctx context.Context, accountID string) (*ec2launchtemplate.AccountScan, error)
	CleanupAccountResidue(ctx context.Context, accountID string, settledBefore time.Time) (*ec2launchtemplate.AccountCleanup, error)
}

// TagIndexOwner lists and clears the account's tag-index entries.
type TagIndexOwner interface {
	ScanAccountResourceIDs(ctx context.Context, accountID string, prefixes ...string) ([]string, error)
	DeleteAllTags(ctx context.Context, accountID, resourceID string) error
}

var (
	_ KeyPairOwner        = (*ec2key.KeyServiceImpl)(nil)
	_ PlacementGroupOwner = (*ec2placementgroup.PlacementGroupServiceImpl)(nil)
	_ LaunchTemplateOwner = (*ec2launchtemplate.LaunchTemplateServiceImpl)(nil)
	_ TagIndexOwner       = (*ec2tags.TagsServiceImpl)(nil)
)

// DefaultSettleWindow is how old headerless residue must be before it is
// removed; anything younger may be a create still in flight.
const DefaultSettleWindow = 10 * time.Minute

// Cleanup composes the owner cleanups and re-scans.
type Cleanup struct {
	keys      KeyPairOwner
	groups    PlacementGroupOwner
	templates LaunchTemplateOwner
	tags      TagIndexOwner
	refs      ReferenceReaders

	// SettleWindow overrides DefaultSettleWindow when positive.
	SettleWindow time.Duration
	now          func() time.Time
}

// New builds a cleanup over the four owners and the reference readers. A nil
// reader counts as a failed check, so the kinds it guards delete nothing.
func New(keys KeyPairOwner, groups PlacementGroupOwner, templates LaunchTemplateOwner, tags TagIndexOwner, refs ReferenceReaders) *Cleanup {
	return &Cleanup{keys: keys, groups: groups, templates: templates, tags: tags, refs: refs, now: time.Now}
}

// Resource-ID prefixes of the three kinds in the tag index.
const (
	keyPairIDPrefix        = "key-"
	placementGroupIDPrefix = "pg-"
	launchTemplateIDPrefix = "lt-"
)

// CleanupPlatform deletes the account's key pairs, empty placement groups and
// launch templates, removes settled residue and orphaned tag-index entries,
// then re-scans every owner store to build the result. It is safe to re-run.
func (c *Cleanup) CleanupPlatform(ctx context.Context, req *Request) (*Result, error) {
	if req == nil || req.AccountID == "" {
		return nil, awserrors.Errorf(awserrors.ErrorMissingParameter, "The request must specify an account ID.")
	}
	if !awsidentifiers.IsAccountID(req.AccountID) {
		return nil, awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "The account ID must be 12 digits.")
	}
	start := time.Now()
	window := c.SettleWindow
	if window <= 0 {
		window = DefaultSettleWindow
	}
	settledBefore := c.now().Add(-window)
	acct := req.AccountID
	res := &Result{AccountID: acct, CorrelationRef: req.CorrelationRef}

	refs := c.readReferences(ctx, acct)
	keyRun := c.cleanKeyPairs(ctx, acct, settledBefore, refs)
	groupRun := c.cleanPlacementGroups(ctx, acct, refs)
	templateRun := c.cleanLaunchTemplates(ctx, acct, settledBefore)

	// Tags are listed before the owner re-scans, so a resource created in
	// between is seen as owned and keeps its tags.
	tagKind := c.startKind(ec2v1.PlatformKindTagIndex)
	tagged, tagErr := c.tags.ScanAccountResourceIDs(ctx, acct, keyPairIDPrefix, placementGroupIDPrefix, launchTemplateIDPrefix)
	if tagErr != nil {
		slog.WarnContext(ctx, "Platform cleanup: tag index listing failed", "err", tagErr)
		tagKind.incomplete(ec2v1.PlatformReasonStoreListFailed)
	}

	owners := map[string]ownedIDs{
		keyPairIDPrefix:        c.finishKeyPairs(ctx, acct, keyRun, res),
		placementGroupIDPrefix: c.finishPlacementGroups(ctx, acct, groupRun, res),
		launchTemplateIDPrefix: c.finishLaunchTemplates(ctx, acct, templateRun, res),
	}
	res.Kinds = []KindReport{keyRun.kind.report, groupRun.kind.report, templateRun.kind.report}
	res.Kinds = append(res.Kinds, c.cleanTagIndex(ctx, acct, tagKind, tagged, tagErr == nil, owners, res))

	slog.InfoContext(ctx, "Platform cleanup completed", "accountID", acct, "correlationRef", req.CorrelationRef,
		"items", len(res.Items), "duration_ms", otelsetup.Millis(time.Since(start)))
	return res, nil
}

// ownedIDs is the set of IDs whose owner record a re-scan found, live or
// unreadable; resolved is false unless the re-scan listed and read every
// record, so an absent ID is not proof its tags are orphaned.
type ownedIDs struct {
	ids      map[string]bool
	resolved bool
}

// kindRun accumulates one kind's report across its cleanup and re-scan.
type kindRun struct {
	report KindReport
	found  bool
}

func (c *Cleanup) startKind(kind string) *kindRun {
	return &kindRun{report: KindReport{Kind: kind, StartedAt: c.now().UTC()}}
}

func (k *kindRun) incomplete(reason string) {
	if !slices.Contains(k.report.IncompleteReasons, reason) {
		k.report.IncompleteReasons = append(k.report.IncompleteReasons, reason)
	}
}

func (c *Cleanup) finishKind(k *kindRun) {
	k.report.EndedAt = c.now().UTC()
	switch {
	case len(k.report.IncompleteReasons) > 0:
		k.report.Status = ec2v1.PlatformStatusScanIncomplete
	case k.found:
		k.report.Status = ec2v1.PlatformStatusRecordsFound
	default:
		k.report.Status = ec2v1.PlatformStatusObservedClean
	}
}

// attempts is what an owner said it did, by resource ID. kept holds items
// left on purpose, with the fixed reason.
type attempts struct {
	deleted, failed map[string]bool
	blocked         map[string]int
	kept            map[string]string
}

func newAttempts() attempts {
	return attempts{deleted: map[string]bool{}, failed: map[string]bool{}, blocked: map[string]int{}, kept: map[string]string{}}
}

// itemDisposition decides one ID from what the owner did and whether the
// re-scan still found it. Only a complete re-scan that misses an ID is deleted;
// an item kept on purpose stays blocked unless that re-scan shows it gone.
func itemDisposition(id string, did attempts, present, rescanned bool) (string, string) {
	switch {
	case did.kept[id] != "" && (present || !rescanned):
		return DispositionBlocked, did.kept[id]
	case !rescanned:
		return DispositionFailed, ReasonAbsenceUnproven
	case !present:
		return DispositionDeleted, ""
	case did.blocked[id] > 0:
		return DispositionBlocked, ReasonInstanceMembers
	case did.failed[id]:
		return DispositionFailed, ReasonOwnerDeleteFailed
	case did.deleted[id]:
		return DispositionFailed, ReasonStillPresent
	default:
		return DispositionResidue, ReasonAppearedDuringCleanup
	}
}

func (r *Result) addItem(k *kindRun, id, disposition, reason string, members int) {
	if disposition != DispositionDeleted {
		k.found = true
	}
	r.Items = append(r.Items, Item{ResourceID: id, Kind: k.report.Kind, Disposition: disposition, Reason: reason, MemberCount: members})
}

func (r *Result) addCount(k *kindRun, class, disposition, reason string, n int) {
	if n <= 0 {
		return
	}
	if disposition != DispositionDeleted {
		k.found = true
	}
	r.Counts = append(r.Counts, Count{Kind: k.report.Kind, Class: class, Disposition: disposition, Reason: reason, Count: n})
}

// residueCounts reports one headerless class: the proven decrease as deleted
// (never more than the owner removed), and what remains split by cause, kept
// records first, by reason in a fixed order.
func (r *Result) residueCounts(k *kindRun, class string, before, after, removed, failed, unsettled int, kept map[string]int) {
	r.addCount(k, class, DispositionDeleted, "", min(removed, max(0, before-after)))
	left := after
	for _, reason := range keptReasons {
		n := min(left, kept[reason])
		r.addCount(k, class, DispositionBlocked, reason, n)
		left -= n
	}
	failedLeft := min(left, failed)
	unsettledLeft := min(left-failedLeft, unsettled)
	r.addCount(k, class, DispositionFailed, ReasonOwnerDeleteFailed, failedLeft)
	r.addCount(k, class, DispositionResidue, ReasonRecentlyWritten, unsettledLeft)
	r.addCount(k, class, DispositionFailed, ReasonStillPresent, left-failedLeft-unsettledLeft)
}

// keptReasons are the reasons an owner may keep a record, in report order.
var keptReasons = []string{ReasonReferenceCheckFailed, ReasonInstanceReference, ReasonSpotRequestReference, ReasonLaunchTemplateReference}

// sortedUnion returns the distinct IDs of all lists, sorted.
func sortedUnion(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		for _, id := range l {
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	slices.Sort(out)
	return out
}

func idPrefix(id string) string {
	for _, prefix := range []string{keyPairIDPrefix, placementGroupIDPrefix, launchTemplateIDPrefix} {
		if strings.HasPrefix(id, prefix) {
			return prefix
		}
	}
	return ""
}
