package accountinventory

//test:in-package — the scan clock is injected to pin the per-kind timestamps.

import (
	"context"
	"errors"
	"testing"
	"time"

	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	ec2launchtemplate "github.com/mulgadc/spinifex/spinifex/domains/ec2/launchtemplate"
	ec2placementgroup "github.com/mulgadc/spinifex/spinifex/domains/ec2/placementgroup"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccount = "111122223333"

var errList = errors.New("injected listing failure")

type fakeKeys struct {
	scan *ec2key.AccountScan
	err  error
}

func (f fakeKeys) ScanAccount(context.Context, string) (*ec2key.AccountScan, error) {
	return f.scan, f.err
}

type fakeGroups struct {
	scan *ec2placementgroup.AccountScan
	err  error
}

func (f fakeGroups) ScanAccount(context.Context, string) (*ec2placementgroup.AccountScan, error) {
	return f.scan, f.err
}

type fakeTemplates struct {
	scan *ec2launchtemplate.AccountScan
	err  error
}

func (f fakeTemplates) ScanAccount(context.Context, string) (*ec2launchtemplate.AccountScan, error) {
	return f.scan, f.err
}

type fakeTags struct {
	ids      []string
	err      error
	prefixes []string
}

func (f *fakeTags) ScanAccountResourceIDs(_ context.Context, _ string, prefixes ...string) ([]string, error) {
	f.prefixes = prefixes
	return f.ids, f.err
}

// fixture is an account holding every class each scan can report.
type fixture struct {
	keys      fakeKeys
	groups    fakeGroups
	templates fakeTemplates
	tags      *fakeTags
}

func emptyFixture() fixture {
	return fixture{
		keys:      fakeKeys{scan: &ec2key.AccountScan{}},
		groups:    fakeGroups{scan: &ec2placementgroup.AccountScan{}},
		templates: fakeTemplates{scan: &ec2launchtemplate.AccountScan{}},
		tags:      &fakeTags{},
	}
}

func (f fixture) run(t *testing.T) *ec2v1.PlatformInventoryReply {
	t.Helper()
	inv := New(f.keys, f.groups, f.templates, f.tags)
	tick := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	inv.now = func() time.Time { tick = tick.Add(time.Second); return tick }
	reply, err := inv.PlatformInventory(context.Background(), &ec2v1.PlatformInventoryRequest{AccountID: testAccount, CorrelationRef: "req-1"})
	require.NoError(t, err)
	return reply
}

func kindsByName(reply *ec2v1.PlatformInventoryReply) map[string]ec2v1.PlatformKindReport {
	out := make(map[string]ec2v1.PlatformKindReport)
	for _, k := range reply.Kinds {
		out[k.Kind] = k
	}
	return out
}

// An account with nothing in any store is observed clean for every kind, each
// with its own scan window, and asks the tag index for the three kinds only.
func TestEmptyAccountIsObservedCleanForEveryKind(t *testing.T) {
	f := emptyFixture()
	reply := f.run(t)

	assert.Equal(t, testAccount, reply.AccountID)
	assert.Equal(t, "req-1", reply.CorrelationRef)
	assert.Empty(t, reply.Items)
	assert.Empty(t, reply.Counts)
	require.Len(t, reply.Kinds, 4)
	for i, kind := range ec2v1.PlatformKinds() {
		report := reply.Kinds[i]
		assert.Equal(t, kind, report.Kind)
		assert.Equal(t, ec2v1.PlatformStatusObservedClean, report.Status, kind)
		assert.Empty(t, report.IncompleteReasons, kind)
		assert.True(t, report.ScanEndedAt.After(report.ScanStartedAt), kind)
	}
	assert.ElementsMatch(t, []string{"key-", "pg-", "lt-"}, f.tags.prefixes)
}

// Live and dependency-held items carry IDs; owner-store residue and unreadable
// records are counts; a tag-index entry is residue only when its owner record
// is absent, not when it is live or unreadable.
func TestInventoryClassifiesEveryRecordClass(t *testing.T) {
	f := emptyFixture()
	f.keys.scan = &ec2key.AccountScan{
		LiveKeyPairIDs: []string{"key-0live"}, UnreadableKeyPairIDs: []string{"key-0broken"}, UnresolvedPublicKeys: 2,
	}
	f.groups.scan = &ec2placementgroup.AccountScan{Groups: []ec2placementgroup.ScannedGroup{
		{GroupID: "pg-0live"},
		{GroupID: "pg-0held", MemberCount: 2},
	}}
	f.templates.scan = &ec2launchtemplate.AccountScan{
		LiveTemplateIDs: []string{"lt-0live"}, UnreadableTemplateIDs: []string{"lt-0broken"},
		OrphanVersionBodies: 3, OrphanNameIndexEntries: 2, UnreadableNameIndexEntries: 1, UnrecognisedEntries: 1,
	}
	f.tags.ids = []string{"key-0broken", "key-0gone", "key-0live", "lt-0broken", "lt-0gone", "pg-0gone", "pg-0held"}

	reply := f.run(t)

	assert.ElementsMatch(t, []ec2v1.PlatformItem{
		{ResourceID: "key-0live", Kind: ec2v1.PlatformKindKeyPair, Disposition: ec2v1.PlatformDispositionLive},
		{ResourceID: "pg-0live", Kind: ec2v1.PlatformKindPlacementGroup, Disposition: ec2v1.PlatformDispositionLive},
		{ResourceID: "pg-0held", Kind: ec2v1.PlatformKindPlacementGroup, Disposition: ec2v1.PlatformDispositionDependencyHeld},
		{ResourceID: "lt-0live", Kind: ec2v1.PlatformKindLaunchTemplate, Disposition: ec2v1.PlatformDispositionLive},
		{ResourceID: "key-0gone", Kind: ec2v1.PlatformKindTagIndex, Disposition: ec2v1.PlatformDispositionResidue},
		{ResourceID: "lt-0gone", Kind: ec2v1.PlatformKindTagIndex, Disposition: ec2v1.PlatformDispositionResidue},
		{ResourceID: "pg-0gone", Kind: ec2v1.PlatformKindTagIndex, Disposition: ec2v1.PlatformDispositionResidue},
	}, reply.Items)
	assert.ElementsMatch(t, []ec2v1.PlatformRecordCount{
		{Kind: ec2v1.PlatformKindKeyPair, Disposition: ec2v1.PlatformDispositionUnreadable, Class: ec2v1.PlatformClassUnreadableRecord, Count: 1},
		{Kind: ec2v1.PlatformKindKeyPair, Disposition: ec2v1.PlatformDispositionUnreadable, Class: ec2v1.PlatformClassPublicKeyUnresolved, Count: 2},
		{Kind: ec2v1.PlatformKindLaunchTemplate, Disposition: ec2v1.PlatformDispositionUnreadable, Class: ec2v1.PlatformClassUnreadableRecord, Count: 2},
		{Kind: ec2v1.PlatformKindLaunchTemplate, Disposition: ec2v1.PlatformDispositionUnreadable, Class: ec2v1.PlatformClassUnrecognisedTemplateEntry, Count: 1},
		{Kind: ec2v1.PlatformKindLaunchTemplate, Disposition: ec2v1.PlatformDispositionResidue, Class: ec2v1.PlatformClassVersionBodyWithoutHeader, Count: 3},
		{Kind: ec2v1.PlatformKindLaunchTemplate, Disposition: ec2v1.PlatformDispositionResidue, Class: ec2v1.PlatformClassNameIndexWithoutHeader, Count: 2},
	}, reply.Counts)
	for _, report := range reply.Kinds {
		assert.Equal(t, ec2v1.PlatformStatusRecordsFound, report.Status, report.Kind)
	}
}

// Orphan public keys are residue when no metadata is unreadable.
func TestInventoryCountsOrphanPublicKeysAsResidue(t *testing.T) {
	f := emptyFixture()
	f.keys.scan = &ec2key.AccountScan{OrphanPublicKeys: 2}
	reply := f.run(t)
	assert.Equal(t, []ec2v1.PlatformRecordCount{{
		Kind: ec2v1.PlatformKindKeyPair, Disposition: ec2v1.PlatformDispositionResidue,
		Class: ec2v1.PlatformClassPublicKeyWithoutMetadata, Count: 2,
	}}, reply.Counts)
	assert.Empty(t, reply.Items)
	assert.Equal(t, ec2v1.PlatformStatusRecordsFound, kindsByName(reply)[ec2v1.PlatformKindKeyPair].Status)
}

// An unreadable placement group's ID is unknown, so a pg- tag entry without a
// readable owner cannot be called residue and the tag index is incomplete.
func TestUnreadablePlacementGroupLeavesItsTagEntriesUnresolved(t *testing.T) {
	f := emptyFixture()
	f.groups.scan = &ec2placementgroup.AccountScan{Unreadable: 1}
	f.tags.ids = []string{"lt-0gone", "pg-0maybe"}

	reply := f.run(t)

	assert.Equal(t, []ec2v1.PlatformItem{
		{ResourceID: "lt-0gone", Kind: ec2v1.PlatformKindTagIndex, Disposition: ec2v1.PlatformDispositionResidue},
	}, reply.Items)
	tagIndex := kindsByName(reply)[ec2v1.PlatformKindTagIndex]
	assert.Equal(t, ec2v1.PlatformStatusScanIncomplete, tagIndex.Status)
	assert.Equal(t, []string{ec2v1.PlatformReasonOwnerRecordsUnresolved}, tagIndex.IncompleteReasons)
	assert.Equal(t, ec2v1.PlatformStatusRecordsFound, kindsByName(reply)[ec2v1.PlatformKindPlacementGroup].Status)
}

// A failed listing or a failed record read makes that kind scan-incomplete with
// a generic reason, and a listing failure also leaves its tag entries unresolved.
func TestStoreFailuresAreScanIncomplete(t *testing.T) {
	tests := []struct {
		name    string
		fault   func(f *fixture)
		kind    string
		reasons []string
	}{
		{"key listing", func(f *fixture) { f.keys.err = errList }, ec2v1.PlatformKindKeyPair, []string{ec2v1.PlatformReasonStoreListFailed}},
		{"group listing", func(f *fixture) { f.groups.err = errList }, ec2v1.PlatformKindPlacementGroup, []string{ec2v1.PlatformReasonStoreListFailed}},
		{"template listing", func(f *fixture) { f.templates.err = errList }, ec2v1.PlatformKindLaunchTemplate, []string{ec2v1.PlatformReasonStoreListFailed}},
		{"tag listing", func(f *fixture) { f.tags.err = errList }, ec2v1.PlatformKindTagIndex, []string{ec2v1.PlatformReasonStoreListFailed}},
		{"key read", func(f *fixture) { f.keys.scan.ReadFailures = 1 }, ec2v1.PlatformKindKeyPair, []string{ec2v1.PlatformReasonRecordReadFailed}},
		{"group read", func(f *fixture) { f.groups.scan.ReadFailures = 1 }, ec2v1.PlatformKindPlacementGroup, []string{ec2v1.PlatformReasonRecordReadFailed}},
		{"template read", func(f *fixture) { f.templates.scan.ReadFailures = 1 }, ec2v1.PlatformKindLaunchTemplate, []string{ec2v1.PlatformReasonRecordReadFailed}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := emptyFixture()
			tc.fault(&f)
			reply := f.run(t)
			for _, report := range reply.Kinds {
				if report.Kind == tc.kind {
					assert.Equal(t, ec2v1.PlatformStatusScanIncomplete, report.Status)
					assert.Equal(t, tc.reasons, report.IncompleteReasons)
					continue
				}
				assert.Equal(t, ec2v1.PlatformStatusObservedClean, report.Status, report.Kind)
			}
		})
	}

	for _, prefix := range []string{"key-0x", "pg-0x", "lt-0x"} {
		f := emptyFixture()
		f.keys.err, f.groups.err, f.templates.err = errList, errList, errList
		f.tags.ids = []string{prefix}
		reply := f.run(t)
		assert.Empty(t, reply.Items, prefix)
		assert.Equal(t, []string{ec2v1.PlatformReasonOwnerRecordsUnresolved}, kindsByName(reply)[ec2v1.PlatformKindTagIndex].IncompleteReasons, prefix)
	}
}

func TestInventoryRejectsAMissingOrMalformedAccount(t *testing.T) {
	inv := New(emptyFixture().keys, emptyFixture().groups, emptyFixture().templates, &fakeTags{})
	for account, code := range map[string]string{
		"":              awserrors.ErrorMissingParameter,
		"1111":          awserrors.ErrorInvalidParameterValue,
		"1111.lt-":      awserrors.ErrorInvalidParameterValue,
		"11112222333a":  awserrors.ErrorInvalidParameterValue,
		"1111222233334": awserrors.ErrorInvalidParameterValue,
	} {
		_, err := inv.PlatformInventory(context.Background(), &ec2v1.PlatformInventoryRequest{AccountID: account})
		got, _ := awserrors.ResolveErrorCode(err)
		assert.Equal(t, code, got, account)
	}
	_, err := inv.PlatformInventory(context.Background(), nil)
	got, _ := awserrors.ResolveErrorCode(err)
	assert.Equal(t, awserrors.ErrorMissingParameter, got)
}
