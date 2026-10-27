package accountlifecycle

import (
	"context"
	"log/slog"
	"regexp"
	"slices"
	"time"

	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
)

// ec2PlatformShadowBudget caps how long a dry run waits for EC2's platform
// inventory after its own listings, never past the caller's deadline. A later
// answer is scan-incomplete and the dry run returns exactly as it would without.
const ec2PlatformShadowBudget = 10 * time.Second

// ec2PlatformShadowMessage is the one operator audit record a dry run writes.
const ec2PlatformShadowMessage = "Account teardown dry-run EC2 platform shadow comparison"

// Shadow outcomes, per kind and for the whole record. Only agree means the
// legacy listing and the inventory matched and nothing hidden was reported.
const (
	shadowAgree          = "agree"
	shadowDiffer         = "differ"
	shadowScanIncomplete = "scan-incomplete"
)

// Bases a kind's visible resources are compared on.
const (
	shadowBasisResourceID = "resource-id"
	shadowBasisCount      = "count"
)

// shadowKind is one compared kind. The key-pair and placement-group reapers
// list by name, which the inventory never carries, so those compare by count.
type shadowKind struct {
	kind   string
	field  string
	byName bool
}

var ec2ShadowKinds = []shadowKind{
	{kind: ec2v1.PlatformKindKeyPair, field: "keyPair", byName: true},
	{kind: ec2v1.PlatformKindPlacementGroup, field: "placementGroup", byName: true},
	{kind: ec2v1.PlatformKindLaunchTemplate, field: "launchTemplate"},
}

// loggableResourceID admits only the AWS ID shape of the three kinds, so a
// reply that put stored content where an ID belongs never reaches the log.
var loggableResourceID = regexp.MustCompile(`^(key|pg|lt)-[0-9a-f]{1,32}$`)

var knownShadowReasons = []string{
	ec2v1.PlatformReasonStoreListFailed, ec2v1.PlatformReasonRecordReadFailed,
	ec2v1.PlatformReasonOwnerRecordsUnresolved, ec2v1.PlatformReasonNoResponder,
	ec2v1.PlatformReasonTimeout, ec2v1.PlatformReasonTransportFailed,
	ec2v1.PlatformReasonErrorReply, ec2v1.PlatformReasonInvalidReply,
	ec2v1.PlatformReasonKindNotReported,
}

var knownShadowClasses = []string{
	ec2v1.PlatformClassPublicKeyWithoutMetadata, ec2v1.PlatformClassPublicKeyUnresolved,
	ec2v1.PlatformClassUnreadableRecord, ec2v1.PlatformClassVersionBodyWithoutHeader,
	ec2v1.PlatformClassNameIndexWithoutHeader, ec2v1.PlatformClassUnrecognisedTemplateEntry,
}

// shadowUnknownClass stands in for a count class this consumer does not know.
const shadowUnknownClass = "unrecognised-class"

func (e *Engine) ec2ShadowBudget() time.Duration {
	if e.ec2PlatformShadowBudget > 0 {
		return e.ec2PlatformShadowBudget
	}
	return ec2PlatformShadowBudget
}

// shadowEC2Platform compares EC2's platform inventory with the legacy listings
// in result and logs the comparison. It reads result and never changes it.
func (e *Engine) shadowEC2Platform(ctx context.Context, result *Result, correlationRef string) {
	start := time.Now()
	reply := e.awaitEC2PlatformInventory(ctx, result.AccountID, correlationRef)

	reports := make(map[string]ec2v1.PlatformKindReport, len(reply.Kinds))
	for _, report := range reply.Kinds {
		reports[report.Kind] = report
	}

	outcome := shadowAgree
	attrs := []slog.Attr{
		slog.String("accountID", result.AccountID),
		slog.String("correlationRef", correlationRef),
	}
	for _, kind := range ec2ShadowKinds {
		attr, kindOutcome := compareShadowKind(kind, legacyIDs(result, kind.kind), reports[kind.kind], reply)
		attrs = append(attrs, attr)
		outcome = worseShadowOutcome(outcome, kindOutcome)
	}
	tagAttr, tagOutcome := shadowTagIndex(reports[ec2v1.PlatformKindTagIndex], reply)
	attrs = append(attrs, tagAttr)
	outcome = worseShadowOutcome(outcome, tagOutcome)

	attrs = append(attrs, slog.String("outcome", outcome), slog.Int64("duration_ms", otelsetup.Millis(time.Since(start))))
	e.logger().LogAttrs(ctx, slog.LevelInfo, ec2PlatformShadowMessage, attrs...)
}

// awaitEC2PlatformInventory bounds the inventory call by the shadow budget even
// if the implementation ignores its context, and distrusts the reply's shape.
func (e *Engine) awaitEC2PlatformInventory(ctx context.Context, accountID, correlationRef string) ec2v1.PlatformInventoryReply {
	ctx, cancel := context.WithTimeout(ctx, e.ec2ShadowBudget())
	defer cancel()

	replies := make(chan ec2v1.PlatformInventoryReply, 1)
	go func() { replies <- e.ec2Inventory.PlatformInventory(ctx, accountID, correlationRef) }()
	select {
	case reply := <-replies:
		return normalisePlatformInventory(reply, accountID, correlationRef)
	case <-ctx.Done():
		return ec2v1.IncompletePlatformInventory(accountID, correlationRef, ec2v1.PlatformReasonTimeout)
	}
}

func legacyIDs(result *Result, kind string) []string {
	var ids []string
	for _, stage := range result.Stages {
		for _, resource := range stage.Deleted {
			if resource.Kind == kind {
				ids = append(ids, resource.ID)
			}
		}
	}
	return ids
}

// compareShadowKind compares one kind's legacy listing with the inventory's
// live and dependency-held items, and reports its hidden records as counts.
func compareShadowKind(kind shadowKind, legacy []string, report ec2v1.PlatformKindReport, reply ec2v1.PlatformInventoryReply) (slog.Attr, string) {
	var visible, held []string
	hidden := map[string]int{}
	hiddenTotal := 0
	for _, item := range reply.Items {
		if item.Kind != kind.kind {
			continue
		}
		switch item.Disposition {
		case ec2v1.PlatformDispositionLive:
			visible = append(visible, item.ResourceID)
		case ec2v1.PlatformDispositionDependencyHeld:
			visible = append(visible, item.ResourceID)
			held = append(held, item.ResourceID)
		default:
			hidden[shadowUnknownClass]++
			hiddenTotal++
		}
	}
	for _, count := range reply.Counts {
		if count.Kind != kind.kind || count.Count <= 0 {
			continue
		}
		class := count.Class
		if !slices.Contains(knownShadowClasses, class) {
			class = shadowUnknownClass
		}
		hidden[class] += count.Count
		hiddenTotal += count.Count
	}

	basis := shadowBasisResourceID
	var onlyLegacy, onlyInventory []string
	agree := len(legacy) == len(visible)
	if kind.byName {
		basis = shadowBasisCount
	} else {
		onlyLegacy, onlyInventory = setDifference(legacy, visible), setDifference(visible, legacy)
		agree = len(onlyLegacy) == 0 && len(onlyInventory) == 0
	}

	comparison := shadowDiffer
	if agree {
		comparison = shadowAgree
	}
	outcome := comparison
	if hiddenTotal > 0 {
		outcome = shadowDiffer
	}
	if report.Status != ec2v1.PlatformStatusObservedClean && report.Status != ec2v1.PlatformStatusRecordsFound {
		comparison, outcome = shadowScanIncomplete, shadowScanIncomplete
	}

	loggableOnlyLegacy, droppedLegacy := loggableIDs(onlyLegacy)
	loggableOnlyInventory, droppedInventory := loggableIDs(onlyInventory)
	loggableHeld, droppedHeld := loggableIDs(held)
	return slog.Group(kind.field,
		slog.String("status", report.Status),
		slog.Any("incompleteReasons", fixedReasons(report.IncompleteReasons)),
		slog.Time("scanStartedAt", report.ScanStartedAt),
		slog.Time("scanEndedAt", report.ScanEndedAt),
		slog.String("comparison", comparison),
		slog.String("matchBasis", basis),
		slog.Int("legacyCount", len(legacy)),
		slog.Int("inventoryVisibleCount", len(visible)),
		slog.Any("onlyInLegacy", loggableOnlyLegacy),
		slog.Any("onlyInInventory", loggableOnlyInventory),
		slog.Any("dependencyHeld", loggableHeld),
		slog.Int("hiddenRecordCount", hiddenTotal),
		slog.Any("hiddenRecords", hidden),
		slog.Int("unloggableIDCount", droppedLegacy+droppedInventory+droppedHeld),
	), outcome
}

// shadowTagIndex reports tag-index entries whose resource has no owner record,
// which no legacy reaper lists.
func shadowTagIndex(report ec2v1.PlatformKindReport, reply ec2v1.PlatformInventoryReply) (slog.Attr, string) {
	var residue []string
	for _, item := range reply.Items {
		if item.Kind == ec2v1.PlatformKindTagIndex {
			residue = append(residue, item.ResourceID)
		}
	}
	outcome := shadowAgree
	if len(residue) > 0 {
		outcome = shadowDiffer
	}
	if report.Status != ec2v1.PlatformStatusObservedClean && report.Status != ec2v1.PlatformStatusRecordsFound {
		outcome = shadowScanIncomplete
	}
	loggable, dropped := loggableIDs(residue)
	return slog.Group("tagIndex",
		slog.String("status", report.Status),
		slog.Any("incompleteReasons", fixedReasons(report.IncompleteReasons)),
		slog.Time("scanStartedAt", report.ScanStartedAt),
		slog.Time("scanEndedAt", report.ScanEndedAt),
		slog.Int("residueCount", len(residue)),
		slog.Any("residue", loggable),
		slog.Int("unloggableIDCount", dropped),
	), outcome
}

func worseShadowOutcome(a, b string) string {
	rank := map[string]int{shadowAgree: 0, shadowDiffer: 1, shadowScanIncomplete: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// setDifference returns the sorted members of a absent from b.
func setDifference(a, b []string) []string {
	out := []string{}
	for _, id := range a {
		if !slices.Contains(b, id) && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func loggableIDs(ids []string) ([]string, int) {
	out := []string{}
	dropped := 0
	for _, id := range ids {
		if loggableResourceID.MatchString(id) {
			out = append(out, id)
			continue
		}
		dropped++
	}
	slices.Sort(out)
	return out, dropped
}

// fixedReasons keeps only the contract's reason categories; anything else a
// reply carried is reported as an invalid reply rather than logged.
func fixedReasons(reasons []string) []string {
	out := []string{}
	for _, reason := range reasons {
		if !slices.Contains(knownShadowReasons, reason) {
			reason = ec2v1.PlatformReasonInvalidReply
		}
		if !slices.Contains(out, reason) {
			out = append(out, reason)
		}
	}
	return out
}
