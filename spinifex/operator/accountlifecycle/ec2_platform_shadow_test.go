package accountlifecycle

//test:in-package — the shadow comparison, the inventory binding it reads and
// its budget are unexported.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/accountinventory"
	natsmsg "github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var shadowScanTime = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

// fakeInventory answers with reply, or never when blocked, and records the
// correlation reference of every call.
type fakeInventory struct {
	reply   ec2v1.PlatformInventoryReply
	blocked chan struct{}

	mu   sync.Mutex
	refs []string
}

func (f *fakeInventory) PlatformInventory(_ context.Context, _, correlationRef string) ec2v1.PlatformInventoryReply {
	f.mu.Lock()
	f.refs = append(f.refs, correlationRef)
	f.mu.Unlock()
	if f.blocked != nil {
		<-f.blocked
	}
	return f.reply
}

func (f *fakeInventory) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.refs...)
}

// countingReaper counts listings, so a dry run can be shown to list once.
type countingReaper struct {
	Reaper

	lists int
}

func (r *countingReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	r.lists++
	return r.Reaper.List(ctx, accountID)
}

func platformKinds(status string) []ec2v1.PlatformKindReport {
	var kinds []ec2v1.PlatformKindReport
	for _, kind := range ec2v1.PlatformKinds() {
		kinds = append(kinds, ec2v1.PlatformKindReport{
			Kind: kind, Status: status, ScanStartedAt: shadowScanTime, ScanEndedAt: shadowScanTime.Add(time.Second),
		})
	}
	return kinds
}

func setKind(kinds []ec2v1.PlatformKindReport, kind, status string, reasons ...string) {
	for i := range kinds {
		if kinds[i].Kind == kind {
			kinds[i].Status, kinds[i].IncompleteReasons = status, reasons
		}
	}
}

func item(kind, id, disposition string) ec2v1.PlatformItem {
	return ec2v1.PlatformItem{ResourceID: id, Kind: kind, Disposition: disposition}
}

// legacyPlatform is a key pair, a placement group and a launch template as the
// legacy reapers list them: the first two by name, the third by ID.
func legacyPlatform() []Reaper {
	return []Reaper{
		&fakeReaper{kind: "instance", stage: StageCompute, resources: []Resource{{Kind: "instance", ID: "i-0123456789abcdef0"}}},
		&fakeReaper{kind: "key-pair", stage: StagePlatform, resources: []Resource{{Kind: "key-pair", ID: "deploy"}}},
		&fakeReaper{kind: "placement-group", stage: StagePlatform, resources: []Resource{{Kind: "placement-group", ID: "spread"}}},
		&fakeReaper{kind: "launch-template", stage: StagePlatform, resources: []Resource{{Kind: "launch-template", ID: "lt-0aaaaaaaaaaaaaaaa"}}},
	}
}

// matchingInventory reports exactly what legacyPlatform lists.
func matchingInventory() ec2v1.PlatformInventoryReply {
	return ec2v1.PlatformInventoryReply{
		AccountID: testAccountID,
		Kinds:     platformKinds(ec2v1.PlatformStatusRecordsFound),
		Items: []ec2v1.PlatformItem{
			item(ec2v1.PlatformKindKeyPair, "key-0aaaaaaaaaaaaaaaa", ec2v1.PlatformDispositionLive),
			item(ec2v1.PlatformKindPlacementGroup, "pg-0aaaaaaaaaaaaaaaa", ec2v1.PlatformDispositionLive),
			item(ec2v1.PlatformKindLaunchTemplate, "lt-0aaaaaaaaaaaaaaaa", ec2v1.PlatformDispositionLive),
		},
	}
}

// shadowEngine is a quiet engine whose log is captured as JSON in logs.
func shadowEngine(inventory ec2PlatformInventory, reapers ...Reaper) (*Engine, *bytes.Buffer) {
	engine := quietEngine(newFakeAccounts(), reapers...)
	logs := &bytes.Buffer{}
	engine.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	engine.ec2Inventory = inventory
	return engine, logs
}

// shadowRecords returns every shadow comparison record in logs.
func shadowRecords(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record))
		if record["msg"] == ec2PlatformShadowMessage {
			records = append(records, record)
		}
	}
	return records
}

func shadowRecord(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()
	records := shadowRecords(t, logs)
	require.Len(t, records, 1, "one shadow record per dry run")
	return records[0]
}

func group(t *testing.T, record map[string]any, name string) map[string]any {
	t.Helper()
	g, ok := record[name].(map[string]any)
	require.True(t, ok, "record has a %s group", name)
	return g
}

func strs(values ...string) []any {
	out := []any{}
	for _, v := range values {
		out = append(out, v)
	}
	return out
}

func resultJSON(t *testing.T, result *Result) string {
	t.Helper()
	data, err := json.Marshal(result)
	require.NoError(t, err)
	return string(data)
}

// noShadowResult is what the same dry run returns with no inventory bound.
func noShadowResult(t *testing.T, reapers ...Reaper) string {
	t.Helper()
	result, err := quietEngine(newFakeAccounts(), reapers...).Inventory(testCtx(t), testAccountID)
	require.NoError(t, err)
	return resultJSON(t, result)
}

// Visible resources that match on both sides agree. The legacy key-pair and
// placement-group reapers list names, so those kinds compare by count.
func TestDryRunShadowAgreesWhenBothSidesListTheSameResources(t *testing.T) {
	reapers := legacyPlatform()
	counted := make([]Reaper, 0, len(reapers))
	for _, reaper := range reapers {
		counted = append(counted, &countingReaper{Reaper: reaper})
	}
	inventory := &fakeInventory{reply: matchingInventory()}
	engine, logs := shadowEngine(inventory, counted...)

	result, err := engine.DryRun(testCtx(t), testAccountID, "req-agree")
	require.NoError(t, err)

	assert.Equal(t, noShadowResult(t, legacyPlatform()...), resultJSON(t, result))
	assert.Equal(t, []string{"req-agree"}, inventory.calls())
	for _, reaper := range counted {
		assert.Equal(t, 1, reaper.(*countingReaper).lists, "%s listed once: the shadow reuses the dry run's listing", reaper.Kind())
	}

	record := shadowRecord(t, logs)
	assert.Equal(t, "INFO", record["level"])
	assert.Equal(t, testAccountID, record["accountID"])
	assert.Equal(t, "req-agree", record["correlationRef"])
	assert.Equal(t, shadowAgree, record["outcome"])
	assert.Contains(t, record, "duration_ms")

	keyPair := group(t, record, "keyPair")
	assert.Equal(t, ec2v1.PlatformStatusRecordsFound, keyPair["status"])
	assert.Equal(t, shadowAgree, keyPair["comparison"])
	assert.Equal(t, shadowBasisCount, keyPair["matchBasis"])
	assert.InDelta(t, 1, keyPair["legacyCount"], 0)
	assert.InDelta(t, 1, keyPair["inventoryVisibleCount"], 0)
	assert.Equal(t, shadowScanTime.Format(time.RFC3339), keyPair["scanStartedAt"])

	launchTemplate := group(t, record, "launchTemplate")
	assert.Equal(t, shadowAgree, launchTemplate["comparison"])
	assert.Equal(t, shadowBasisResourceID, launchTemplate["matchBasis"])
	assert.Equal(t, strs(), launchTemplate["onlyInLegacy"])
	assert.Equal(t, strs(), launchTemplate["onlyInInventory"])

	tagIndex := group(t, record, "tagIndex")
	assert.Equal(t, ec2v1.PlatformStatusRecordsFound, tagIndex["status"])
	assert.InDelta(t, 0, tagIndex["residueCount"], 0)
}

// Visible resources one side has and the other does not are named by ID where
// the legacy reaper lists IDs, and counted where it lists names.
func TestDryRunShadowReportsVisibleResourcesOnlyOneSideSees(t *testing.T) {
	reapers := legacyPlatform()
	reapers[1].(*fakeReaper).resources = append(reapers[1].(*fakeReaper).resources, Resource{Kind: "key-pair", ID: "ci"})
	reapers[3].(*fakeReaper).resources = []Resource{
		{Kind: "launch-template", ID: "lt-0aaaaaaaaaaaaaaaa"}, {Kind: "launch-template", ID: "lt-0bbbbbbbbbbbbbbbb"},
	}
	reply := matchingInventory()
	reply.Items = append(reply.Items, item(ec2v1.PlatformKindLaunchTemplate, "lt-0cccccccccccccccc", ec2v1.PlatformDispositionLive))
	engine, logs := shadowEngine(&fakeInventory{reply: reply}, reapers...)

	_, err := engine.DryRun(testCtx(t), testAccountID, "req-differ")
	require.NoError(t, err)

	record := shadowRecord(t, logs)
	assert.Equal(t, shadowDiffer, record["outcome"])
	keyPair := group(t, record, "keyPair")
	assert.Equal(t, shadowDiffer, keyPair["comparison"])
	assert.InDelta(t, 2, keyPair["legacyCount"], 0)
	assert.InDelta(t, 1, keyPair["inventoryVisibleCount"], 0)
	assert.Equal(t, strs(), keyPair["onlyInLegacy"], "legacy key-pair names are never logged")

	launchTemplate := group(t, record, "launchTemplate")
	assert.Equal(t, shadowDiffer, launchTemplate["comparison"])
	assert.Equal(t, strs("lt-0bbbbbbbbbbbbbbbb"), launchTemplate["onlyInLegacy"])
	assert.Equal(t, strs("lt-0cccccccccccccccc"), launchTemplate["onlyInInventory"])
	assert.Equal(t, shadowAgree, group(t, record, "placementGroup")["comparison"])
}

// Residue the legacy reapers cannot see makes the record differ even when the
// visible resources agree: as counts per class, and tag-index entries by ID.
func TestDryRunShadowReportsHiddenResidueTheLegacyReapersMiss(t *testing.T) {
	reply := matchingInventory()
	reply.Counts = []ec2v1.PlatformRecordCount{
		{Kind: ec2v1.PlatformKindKeyPair, Disposition: ec2v1.PlatformDispositionResidue, Class: ec2v1.PlatformClassPublicKeyWithoutMetadata, Count: 1},
		{Kind: ec2v1.PlatformKindLaunchTemplate, Disposition: ec2v1.PlatformDispositionResidue, Class: ec2v1.PlatformClassVersionBodyWithoutHeader, Count: 2},
		{Kind: ec2v1.PlatformKindLaunchTemplate, Disposition: ec2v1.PlatformDispositionUnreadable, Class: ec2v1.PlatformClassUnreadableRecord, Count: 1},
	}
	reply.Items = append(reply.Items, item(ec2v1.PlatformKindTagIndex, "pg-0dddddddddddddddd", ec2v1.PlatformDispositionResidue))
	engine, logs := shadowEngine(&fakeInventory{reply: reply}, legacyPlatform()...)

	_, err := engine.DryRun(testCtx(t), testAccountID, "req-residue")
	require.NoError(t, err)

	record := shadowRecord(t, logs)
	assert.Equal(t, shadowDiffer, record["outcome"])
	keyPair := group(t, record, "keyPair")
	assert.Equal(t, shadowAgree, keyPair["comparison"], "the visible key pairs still agree")
	assert.InDelta(t, 1, keyPair["hiddenRecordCount"], 0)
	assert.Equal(t, map[string]any{ec2v1.PlatformClassPublicKeyWithoutMetadata: float64(1)}, keyPair["hiddenRecords"])
	launchTemplate := group(t, record, "launchTemplate")
	assert.InDelta(t, 3, launchTemplate["hiddenRecordCount"], 0)
	assert.Equal(t, map[string]any{
		ec2v1.PlatformClassVersionBodyWithoutHeader: float64(2),
		ec2v1.PlatformClassUnreadableRecord:         float64(1),
	}, launchTemplate["hiddenRecords"])
	tagIndex := group(t, record, "tagIndex")
	assert.InDelta(t, 1, tagIndex["residueCount"], 0)
	assert.Equal(t, strs("pg-0dddddddddddddddd"), tagIndex["residue"])
}

// A placement group still holding instances is a plain listed resource to the
// legacy dry run, which learns otherwise only when its delete fails.
func TestDryRunShadowReportsADependencyHeldPlacementGroup(t *testing.T) {
	reply := matchingInventory()
	reply.Items[1].Disposition = ec2v1.PlatformDispositionDependencyHeld
	engine, logs := shadowEngine(&fakeInventory{reply: reply}, legacyPlatform()...)

	_, err := engine.DryRun(testCtx(t), testAccountID, "req-held")
	require.NoError(t, err)

	placementGroup := group(t, shadowRecord(t, logs), "placementGroup")
	assert.Equal(t, shadowAgree, placementGroup["comparison"])
	assert.InDelta(t, 1, placementGroup["inventoryVisibleCount"], 0)
	assert.Equal(t, strs("pg-0aaaaaaaaaaaaaaaa"), placementGroup["dependencyHeld"])
}

// An owner store that could not be read is scan-incomplete, never agreement,
// even where the IDs it did report match.
func TestDryRunShadowNeverReportsAnIncompleteScanAsAgreement(t *testing.T) {
	reply := matchingInventory()
	setKind(reply.Kinds, ec2v1.PlatformKindLaunchTemplate, ec2v1.PlatformStatusScanIncomplete, ec2v1.PlatformReasonRecordReadFailed)
	engine, logs := shadowEngine(&fakeInventory{reply: reply}, legacyPlatform()...)

	result, err := engine.DryRun(testCtx(t), testAccountID, "req-store")
	require.NoError(t, err)
	assert.Equal(t, noShadowResult(t, legacyPlatform()...), resultJSON(t, result))

	record := shadowRecord(t, logs)
	assert.Equal(t, shadowScanIncomplete, record["outcome"])
	launchTemplate := group(t, record, "launchTemplate")
	assert.Equal(t, shadowScanIncomplete, launchTemplate["comparison"])
	assert.Equal(t, strs(ec2v1.PlatformReasonRecordReadFailed), launchTemplate["incompleteReasons"])
	assert.Equal(t, shadowAgree, group(t, record, "keyPair")["comparison"])
}

// However the inventory fails, the dry run returns what it returns without it
// and the record says scan-incomplete with the fixed reason.
func TestDryRunShadowFailureLeavesTheDryRunUnchanged(t *testing.T) {
	want := noShadowResult(t, legacyPlatform()...)
	for _, reason := range []string{
		ec2v1.PlatformReasonNoResponder, ec2v1.PlatformReasonTimeout,
		ec2v1.PlatformReasonTransportFailed, ec2v1.PlatformReasonErrorReply,
	} {
		t.Run(reason, func(t *testing.T) {
			engine, logs := shadowEngine(&fakeInventory{reply: ec2v1.IncompletePlatformInventory(testAccountID, "req-fail", reason)}, legacyPlatform()...)
			result, err := engine.DryRun(testCtx(t), testAccountID, "req-fail")
			require.NoError(t, err)
			assert.Equal(t, want, resultJSON(t, result))

			record := shadowRecord(t, logs)
			assert.Equal(t, shadowScanIncomplete, record["outcome"])
			for _, name := range []string{"keyPair", "placementGroup", "launchTemplate", "tagIndex"} {
				g := group(t, record, name)
				assert.Equal(t, ec2v1.PlatformStatusScanIncomplete, g["status"], name)
				assert.Equal(t, strs(reason), g["incompleteReasons"], name)
			}
		})
	}

	t.Run("reply for another account", func(t *testing.T) {
		reply := matchingInventory()
		reply.AccountID = "000000000099"
		engine, logs := shadowEngine(&fakeInventory{reply: reply}, legacyPlatform()...)
		result, err := engine.DryRun(testCtx(t), testAccountID, "req-fail")
		require.NoError(t, err)
		assert.Equal(t, want, resultJSON(t, result))
		record := shadowRecord(t, logs)
		assert.Equal(t, shadowScanIncomplete, record["outcome"])
		assert.Equal(t, strs(ec2v1.PlatformReasonInvalidReply), group(t, record, "keyPair")["incompleteReasons"])
	})

	t.Run("no daemon serves the subject", func(t *testing.T) {
		_, nc, _ := testutil.StartTestJetStream(t)
		engine, logs := shadowEngine(newEC2PlatformInventoryClient(nc), legacyPlatform()...)
		result, err := engine.DryRun(testCtx(t), testAccountID, "req-fail")
		require.NoError(t, err)
		assert.Equal(t, want, resultJSON(t, result))
		assert.Equal(t, strs(ec2v1.PlatformReasonNoResponder), group(t, shadowRecord(t, logs), "tagIndex")["incompleteReasons"])
	})
}

// An inventory that never answers, even one ignoring its context, holds the
// dry run for the shadow budget at most and is logged as a timeout.
func TestDryRunShadowIsBoundedByItsBudget(t *testing.T) {
	inventory := &fakeInventory{reply: matchingInventory(), blocked: make(chan struct{})}
	t.Cleanup(func() { close(inventory.blocked) })
	engine, logs := shadowEngine(inventory, legacyPlatform()...)
	engine.ec2PlatformShadowBudget = 20 * time.Millisecond

	started := time.Now()
	result, err := engine.DryRun(testCtx(t), testAccountID, "req-slow")
	require.NoError(t, err)
	assert.Less(t, time.Since(started), 5*time.Second)
	assert.Equal(t, noShadowResult(t, legacyPlatform()...), resultJSON(t, result))
	assert.Equal(t, strs(ec2v1.PlatformReasonTimeout), group(t, shadowRecord(t, logs), "launchTemplate")["incompleteReasons"])
}

// The caller's deadline wins over the budget, so a dry run never outlasts the
// bound its caller already set.
func TestDryRunShadowStopsAtTheCallersDeadline(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	serveInventory(t, nc, func() []byte { return nil })
	engine, logs := shadowEngine(newEC2PlatformInventoryClient(nc), legacyPlatform()...)
	require.Greater(t, engine.ec2ShadowBudget(), 5*time.Second)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := engine.DryRun(ctx, testAccountID, "req-deadline")
	require.NoError(t, err)
	assert.Less(t, time.Since(started), 5*time.Second)
	assert.Equal(t, noShadowResult(t, legacyPlatform()...), resultJSON(t, result))
	assert.Equal(t, strs(ec2v1.PlatformReasonTimeout), group(t, shadowRecord(t, logs), "keyPair")["incompleteReasons"])
}

// A dry run whose own listing fails returns that error as before, and nothing
// is compared against a listing that does not exist.
func TestDryRunWithAFailedListingDoesNotCallTheInventory(t *testing.T) {
	reapers := legacyPlatform()
	reapers[2].(*fakeReaper).listErr = errors.New("describe failed")
	inventory := &fakeInventory{reply: matchingInventory()}
	engine, logs := shadowEngine(inventory, reapers...)

	_, err := engine.DryRun(testCtx(t), testAccountID, "req-list")
	require.ErrorContains(t, err, "inventory placement-group")
	assert.Empty(t, inventory.calls())
	assert.Empty(t, shadowRecords(t, logs))
}

// A reply that puts anything but the contract's fixed vocabulary where a
// reason, class or ID belongs never reaches the log; the IDs are counted.
func TestDryRunShadowLogsOnlyTheFixedVocabulary(t *testing.T) {
	reply := matchingInventory()
	setKind(reply.Kinds, ec2v1.PlatformKindKeyPair, ec2v1.PlatformStatusScanIncomplete, "read keys/000000000042/secret-key-name: denied")
	reply.Counts = []ec2v1.PlatformRecordCount{{Kind: ec2v1.PlatformKindLaunchTemplate, Disposition: ec2v1.PlatformDispositionResidue, Class: "secret-template-name", Count: 1}}
	reply.Items = append(reply.Items,
		item(ec2v1.PlatformKindLaunchTemplate, "lt-secret-template-name", ec2v1.PlatformDispositionLive),
		item(ec2v1.PlatformKindTagIndex, "pg-team=secret-tag-value", ec2v1.PlatformDispositionResidue),
	)
	reapers := legacyPlatform()
	reapers[1].(*fakeReaper).resources = []Resource{{Kind: "key-pair", ID: "secret-key-name"}}
	engine, logs := shadowEngine(&fakeInventory{reply: reply}, reapers...)

	_, err := engine.DryRun(testCtx(t), testAccountID, "req-disclosure")
	require.NoError(t, err)

	assert.NotContains(t, logs.String(), "secret")
	record := shadowRecord(t, logs)
	assert.Equal(t, strs(ec2v1.PlatformReasonInvalidReply), group(t, record, "keyPair")["incompleteReasons"])
	launchTemplate := group(t, record, "launchTemplate")
	assert.Equal(t, map[string]any{shadowUnknownClass: float64(1)}, launchTemplate["hiddenRecords"])
	assert.InDelta(t, 1, launchTemplate["unloggableIDCount"], 0)
	assert.InDelta(t, 1, group(t, record, "tagIndex")["unloggableIDCount"], 0)
}

// End to end over the real owner stores and the NATS adapter: the record names
// the residue each reaper misses and nothing a customer named or stored.
func TestDryRunShadowOverTheOwnerStores(t *testing.T) {
	o := newPlatformOwners(t)
	ctx := testCtx(t)
	inv := accountinventory.New(o.keys, o.groups, o.templates, o.tags)
	sub, err := o.nc.QueueSubscribe(ec2v1.PlatformInventorySubject, "spinifex-workers", func(msg *nats.Msg) {
		natsmsg.ServeNATSRequestCtx(msg, inv.PlatformInventory)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	material := []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl")
	for _, name := range []string{"secret-key-name", "secret-orphan-key"} {
		_, err = o.keys.ImportKeyPair(ctx, &ec2.ImportKeyPairInput{KeyName: aws.String(name), PublicKeyMaterial: material}, testAccountID)
		require.NoError(t, err)
	}
	_, err = o.keys.DeleteKeyPair(ctx, &ec2.DeleteKeyPairInput{KeyName: aws.String("secret-orphan-key")}, testAccountID)
	require.NoError(t, err)
	// Residue an older release's failed deletes left behind.
	_, err = o.store.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(platformBucket), Key: aws.String("keys/" + testAccountID + "/secret-orphan-key"), Body: bytes.NewReader(material),
	})
	require.NoError(t, err)

	_, err = o.groups.CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{
		GroupName: aws.String("secret-group-name"), Strategy: aws.String(ec2.PlacementStrategySpread),
	}, testAccountID)
	require.NoError(t, err)
	doomed, err := o.groups.CreatePlacementGroup(ctx, &ec2.CreatePlacementGroupInput{
		GroupName: aws.String("secret-doomed-group"), Strategy: aws.String(ec2.PlacementStrategySpread),
	}, testAccountID)
	require.NoError(t, err)
	lt, err := o.templates.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{
		LaunchTemplateName: aws.String("secret-template-name"),
		LaunchTemplateData: &ec2.RequestLaunchTemplateData{UserData: aws.String(base64.StdEncoding.EncodeToString([]byte("secret-user-data")))},
	}, testAccountID)
	require.NoError(t, err)
	doomedID, ltID := aws.StringValue(doomed.PlacementGroup.GroupId), aws.StringValue(lt.LaunchTemplate.LaunchTemplateId)
	_, err = o.tags.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []*string{aws.String(doomedID), aws.String(ltID)},
		Tags:      []*ec2.Tag{{Key: aws.String("secret-tag-key"), Value: aws.String("secret-tag-value")}},
	}, testAccountID)
	require.NoError(t, err)
	_, err = o.groups.DeletePlacementGroup(ctx, &ec2.DeletePlacementGroupInput{GroupName: aws.String("secret-doomed-group")}, testAccountID)
	require.NoError(t, err)
	_, err = o.tags.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []*string{aws.String(doomedID)},
		Tags:      []*ec2.Tag{{Key: aws.String("secret-tag-key"), Value: aws.String("secret-tag-value")}},
	}, testAccountID)
	require.NoError(t, err)

	engine, logs := shadowEngine(newEC2PlatformInventoryClient(o.nc), platformReapers(o.nc)...)
	result, err := engine.DryRun(ctx, testAccountID, "req-owners")
	require.NoError(t, err)
	plain, err := quietEngine(newFakeAccounts(), platformReapers(o.nc)...).Inventory(ctx, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, resultJSON(t, plain), resultJSON(t, result))

	assert.NotContains(t, logs.String(), "secret")
	assert.NotContains(t, logs.String(), base64.StdEncoding.EncodeToString([]byte("secret-user-data")))
	record := shadowRecord(t, logs)
	assert.Equal(t, shadowDiffer, record["outcome"])
	keyPair := group(t, record, "keyPair")
	assert.Equal(t, shadowAgree, keyPair["comparison"])
	assert.Equal(t, map[string]any{ec2v1.PlatformClassPublicKeyWithoutMetadata: float64(1)}, keyPair["hiddenRecords"])
	assert.Equal(t, shadowAgree, group(t, record, "placementGroup")["comparison"])
	launchTemplate := group(t, record, "launchTemplate")
	assert.Equal(t, shadowAgree, launchTemplate["comparison"])
	assert.Equal(t, shadowBasisResourceID, launchTemplate["matchBasis"])
	assert.Equal(t, strs(doomedID), group(t, record, "tagIndex")["residue"])
}
