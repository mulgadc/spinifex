package zoneid

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// awsPage is the Region to AZ ID prefix list printed on
// docs.aws.amazon.com/global-infrastructure/latest/regions/aws-availability-zones.html.
var awsPage = map[string]string{
	"us-east-1": "use1", "us-east-2": "use2", "us-west-1": "usw1", "us-west-2": "usw2",
	"ca-central-1": "cac1", "ca-west-1": "caw1", "mx-central-1": "mxc1", "af-south-1": "afs1",
	"ap-east-1": "ape1", "ap-east-2": "ape2",
	"ap-northeast-1": "apne1", "ap-northeast-2": "apne2", "ap-northeast-3": "apne3",
	"ap-south-1": "aps1", "ap-south-2": "aps2",
	"ap-southeast-1": "apse1", "ap-southeast-2": "apse2", "ap-southeast-3": "apse3",
	"ap-southeast-4": "apse4", "ap-southeast-5": "apse5", "ap-southeast-6": "apse6",
	"ap-southeast-7": "apse7",
	"eu-central-1":   "euc1", "eu-central-2": "euc2", "eu-north-1": "eun1",
	"eu-south-1": "eus1", "eu-south-2": "eus2",
	"eu-west-1": "euw1", "eu-west-2": "euw2", "eu-west-3": "euw3",
	"il-central-1": "ilc1", "me-central-1": "mec1", "me-south-1": "mes1", "sa-east-1": "sae1",
}

func newJS(t *testing.T) jetstream.JetStream {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	return js
}

func newResolver(t *testing.T, js jetstream.JetStream, region, override string) *Resolver {
	t.Helper()
	r, err := NewResolver(js, region, override)
	require.NoError(t, err)
	return r
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestShortCodeFor_MatchesTheAWSPage(t *testing.T) {
	assert.Len(t, awsShortCodes, len(awsPage), "table has an entry the AWS page does not list")
	for region, want := range awsPage {
		got, err := ShortCodeFor(region, "")
		require.NoError(t, err, region)
		assert.Equal(t, want, got, region)
	}
}

func TestShortCodeFor_DerivedRule(t *testing.T) {
	for region, want := range map[string]string{
		"mulga-lab":       "mul1",
		"mulga-lab-2":     "mul2",
		"xx-southwest-3":  "xxsw3",
		"ap-southeast-99": "apse99",
		"edge":            "ed1",
		"us-gov-west-1":   "usgw1",
	} {
		got, err := ShortCodeFor(region, "")
		require.NoError(t, err, region)
		assert.Equal(t, want, got, region)
		assert.True(t, ValidShortCode(got), region)
	}
}

func TestShortCodeFor_OverrideReplacesTableAndRule(t *testing.T) {
	got, err := ShortCodeFor("ap-southeast-2", "lab1")
	require.NoError(t, err)
	assert.Equal(t, "lab1", got)
	got, err = ShortCodeFor("mulga-lab", "lab1")
	require.NoError(t, err)
	assert.Equal(t, "lab1", got)
}

func TestShortCodeFor_InvalidOverrideRefused(t *testing.T) {
	for _, bad := range []string{"a", "UPPER", "has-dash", "toolongcode", "sp ace"} {
		_, err := ShortCodeFor("us-east-1", bad)
		assert.Error(t, err, bad)
		_, err = NewResolver(nil, "us-east-1", bad)
		assert.Error(t, err, bad)
	}
}

func TestPin_LetterOrdinals(t *testing.T) {
	r := newResolver(t, newJS(t), "ap-southeast-2", "")
	ids, err := r.Pin(context.Background(), "ap-southeast-2c", "ap-southeast-2a", "ap-southeast-2b")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"ap-southeast-2a": "apse2-az1", "ap-southeast-2b": "apse2-az2", "ap-southeast-2c": "apse2-az3",
	}, ids)
}

func TestPin_TwoCustomNamesGetDistinctIDs(t *testing.T) {
	r := newResolver(t, newJS(t), "mulga-lab", "")
	ids, err := r.Pin(context.Background(), "rack-2", "rack-1")
	require.NoError(t, err)
	assert.Equal(t, "mul1-az1", ids["rack-1"])
	assert.Equal(t, "mul1-az2", ids["rack-2"])
}

func TestPin_LetterZoneWhoseOrdinalIsTakenGetsNextFree(t *testing.T) {
	r := newResolver(t, newJS(t), "mulga-lab", "")
	ctx := context.Background()
	_, err := r.Pin(ctx, "rack-1")
	require.NoError(t, err)
	ids, err := r.Pin(ctx, "mulga-laba")
	require.NoError(t, err)
	assert.Equal(t, "mul1-az2", ids["mulga-laba"])
}

func TestPin_ConcurrentFirstStartWritesOnceByCAS(t *testing.T) {
	js := newJS(t)
	ctx := context.Background()
	names := []string{"us-east-1a", "us-east-1b", "us-east-1c"}
	var wg sync.WaitGroup
	results := make([]map[string]string, 8)
	resolvers := make([]*Resolver, len(results))
	for i := range resolvers {
		resolvers[i] = newResolver(t, js, "us-east-1", "")
	}
	for i := range results {
		wg.Go(func() {
			ids, err := resolvers[i].Pin(ctx, names...)
			assert.NoError(t, err)
			results[i] = ids
		})
	}
	wg.Wait()
	for _, ids := range results {
		assert.Equal(t, results[0], ids)
	}
	kv, err := js.KeyValue(ctx, BucketName)
	require.NoError(t, err)
	e, err := kv.Get(ctx, "zoneids.us-east-1")
	require.NoError(t, err)
	assert.Equal(t, uint64(1), e.Revision(), "record must be created once and never rewritten")
}

func TestPin_ChangedOverrideOrRuleKeepsExistingIDsAndLogsError(t *testing.T) {
	js := newJS(t)
	ctx := context.Background()
	first := newResolver(t, js, "mulga-lab", "")
	before, err := first.Pin(ctx, "rack-1", "rack-2")
	require.NoError(t, err)

	logs := captureLogs(t)
	changed := newResolver(t, js, "mulga-lab", "zzz9")
	after, err := changed.Pin(ctx, "rack-1", "rack-2")
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Contains(t, logs.String(), "level=ERROR")
	assert.Contains(t, logs.String(), "configured=zzz9")
	assert.Contains(t, logs.String(), "pinned=mul1")

	ruleChanged := newResolver(t, js, "mulga-lab", "")
	ruleChanged.shortCode = "newrule1"
	again, err := ruleChanged.Pin(ctx, "rack-1", "rack-2")
	require.NoError(t, err)
	assert.Equal(t, before, again)
}

func TestPin_NewZoneAfterChangeDiffersFromEveryPinnedOne(t *testing.T) {
	js := newJS(t)
	ctx := context.Background()
	_, err := newResolver(t, js, "mulga-lab", "").Pin(ctx, "rack-1", "rack-2")
	require.NoError(t, err)
	ids, err := newResolver(t, js, "mulga-lab", "zzz9").Pin(ctx, "rack-3")
	require.NoError(t, err)
	assert.Equal(t, "mul1-az3", ids["rack-3"])
}

func TestBackfill_CustomSubnetZonePinnedAndStableAcrossRestartWithChangedOverride(t *testing.T) {
	js := newJS(t)
	ctx := context.Background()
	start1 := newResolver(t, js, "mulga-lab", "")
	_, err := start1.Pin(ctx, "mulga-laba")
	require.NoError(t, err)
	_, err = start1.Pin(ctx, "rack-b", "rack-a")
	require.NoError(t, err)
	pinned, err := start1.ZoneID(ctx, "rack-b")
	require.NoError(t, err)

	start2 := newResolver(t, js, "mulga-lab", "other2")
	got, err := start2.ZoneID(ctx, "rack-b")
	require.NoError(t, err)
	assert.Equal(t, pinned, got)
}

func TestZoneID_DescribeBeforeAnyStartPinsAndIsStable(t *testing.T) {
	js := newJS(t)
	ctx := context.Background()
	id, err := newResolver(t, js, "mulga-lab", "").ZoneID(ctx, "rack-9")
	require.NoError(t, err)

	later := newResolver(t, js, "mulga-lab", "chg2")
	_, err = later.Pin(ctx, "rack-1", "rack-9", "mulga-laba")
	require.NoError(t, err)
	got, err := later.ZoneID(ctx, "rack-9")
	require.NoError(t, err)
	assert.Equal(t, id, got)
	name, ok, err := later.ZoneName(ctx, id)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "rack-9", name)
}

func TestBackfill_OwnZoneFirstThenOthersSorted(t *testing.T) {
	js := newJS(t)
	ctx := context.Background()
	r := newResolver(t, js, "mulga-lab", "")
	require.NoError(t, r.Backfill(ctx, "mulga-labb", []string{"rack-z", "rack-a"}))
	got := map[string]string{}
	for _, n := range []string{"mulga-labb", "rack-a", "rack-z"} {
		id, err := r.ZoneID(ctx, n)
		require.NoError(t, err)
		got[n] = id
	}
	assert.Equal(t, map[string]string{"mulga-labb": "mul1-az2", "rack-a": "mul1-az1", "rack-z": "mul1-az3"}, got)

	require.NoError(t, r.Backfill(ctx, "mulga-labb", []string{"rack-z", "rack-a"}))
	again, err := r.ZoneID(ctx, "rack-a")
	require.NoError(t, err)
	assert.Equal(t, "mul1-az1", again, "a second backfill must not move any ID")
}

func TestPin_RegionRecordCappedAtMaxZones(t *testing.T) {
	r := newResolver(t, newJS(t), "mulga-lab", "")
	ctx := context.Background()
	names := make([]string, MaxZones)
	for i := range names {
		names[i] = fmt.Sprintf("rack-%02d", i)
	}
	before, err := r.Pin(ctx, names...)
	require.NoError(t, err)
	assert.Len(t, before, MaxZones)

	_, err = r.Pin(ctx, "one-too-many")
	require.ErrorIs(t, err, ErrTooManyZones)
	_, err = r.ZoneID(ctx, "one-too-many")
	require.ErrorIs(t, err, ErrTooManyZones)

	after, err := r.Pin(ctx, names...)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a refused pin must not change or remove any pinned ID")
}
