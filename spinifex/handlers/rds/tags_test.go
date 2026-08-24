package handlers_rds

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func awsTags(kv ...string) []*rds.Tag {
	tags := make([]*rds.Tag, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		tags = append(tags, &rds.Tag{Key: aws.String(kv[i]), Value: aws.String(kv[i+1])})
	}
	return tags
}

// The tags on a DB instance as a map, read back through the public action so
// the assertion covers the projection and not just the record.
func listTags(t *testing.T, h *createHarness, id string) map[string]string {
	t.Helper()
	out, err := h.svc.ListTagsForResource(t.Context(), &rds.ListTagsForResourceInput{
		ResourceName: aws.String(FormatARN(ResourceKindDBInstance, testRegion, testAccountID, id)),
	}, testAccountID)
	require.NoError(t, err)
	got := make(map[string]string, len(out.TagList))
	for _, tag := range out.TagList {
		got[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}
	return got
}

func TestCreateDBInstance_TagsRoundTripThroughBothReadPaths(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	input := validCreateInput()
	input.Tags = awsTags("env", "prod", "team", "platform")

	out, err := h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	require.NoError(t, err)

	// Terraform reads tags from both the describe and the list, so the two
	// disagreeing would show up as permanent drift.
	assert.Equal(t, input.Tags, out.DBInstance.TagList)
	described, err := h.svc.DescribeDBInstances(t.Context(), &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(testDBInstanceID),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, described.DBInstances, 1)
	assert.Equal(t, input.Tags, described.DBInstances[0].TagList)

	assert.Equal(t, map[string]string{"env": "prod", "team": "platform"}, listTags(t, h, testDBInstanceID))
}

// The same rules apply at create as at AddTagsToResource, and they run before
// the identifier is reserved so a rejected create leaves nothing behind.
func TestCreateDBInstance_InvalidTagsLeaveNoRecord(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	input := validCreateInput()
	input.Tags = awsTags("aws:cloudformation:stack-name", "mine")

	_, err := h.svc.CreateDBInstance(t.Context(), input, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	assert.False(t, h.recordExists(t, testDBInstanceID),
		"a create rejected on its tags must not reserve the identifier")
}

func TestListTagsForResource_UntaggedInstanceIsAnEmptyList(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	seedCreated(t, h, testDBInstanceID)

	assert.Empty(t, listTags(t, h, testDBInstanceID))
}

// AddTagsToResource is a merge, and a repeated key overwrites rather than
// duplicates, so a re-run of the same apply converges.
func TestAddTagsToResource_MergesAndOverwrites(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	seedCreated(t, h, testDBInstanceID)
	arn := aws.String(FormatARN(ResourceKindDBInstance, testRegion, testAccountID, testDBInstanceID))

	_, err := h.svc.AddTagsToResource(t.Context(), &rds.AddTagsToResourceInput{
		ResourceName: arn, Tags: awsTags("env", "staging", "team", "platform"),
	}, testAccountID)
	require.NoError(t, err)

	_, err = h.svc.AddTagsToResource(t.Context(), &rds.AddTagsToResourceInput{
		ResourceName: arn, Tags: awsTags("env", "prod"),
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"env": "prod", "team": "platform"}, listTags(t, h, testDBInstanceID))
}

// A key repeated inside one request keeps its last value rather than failing,
// which is what AWS does.
func TestAddTagsToResource_DuplicateKeyInOneRequestKeepsTheLastValue(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	seedCreated(t, h, testDBInstanceID)

	_, err := h.svc.AddTagsToResource(t.Context(), &rds.AddTagsToResourceInput{
		ResourceName: aws.String(FormatARN(ResourceKindDBInstance, testRegion, testAccountID, testDBInstanceID)),
		Tags:         awsTags("env", "staging", "env", "prod"),
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"env": "prod"}, listTags(t, h, testDBInstanceID))
}

func TestRemoveTagsFromResource_RemovesNamedKeysAndIgnoresAbsentOnes(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	seedCreated(t, h, testDBInstanceID)
	arn := aws.String(FormatARN(ResourceKindDBInstance, testRegion, testAccountID, testDBInstanceID))

	_, err := h.svc.AddTagsToResource(t.Context(), &rds.AddTagsToResourceInput{
		ResourceName: arn, Tags: awsTags("env", "prod", "team", "platform"),
	}, testAccountID)
	require.NoError(t, err)

	// "owner" was never set: a destroy that already ran must not fail on its
	// second attempt.
	_, err = h.svc.RemoveTagsFromResource(t.Context(), &rds.RemoveTagsFromResourceInput{
		ResourceName: arn, TagKeys: aws.StringSlice([]string{"env", "owner"}),
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"team": "platform"}, listTags(t, h, testDBInstanceID))
}

func TestTagActions_RejectInvalidTags(t *testing.T) {
	overLimit := make([]string, 0, (maxTagsPerResource+1)*2)
	for i := 0; i <= maxTagsPerResource; i++ {
		overLimit = append(overLimit, fmt.Sprintf("key-%d", i), "v")
	}

	cases := []struct {
		name string
		tags []*rds.Tag
	}{
		{"over the tag limit", awsTags(overLimit...)},
		{"oversized key", awsTags(strings.Repeat("k", maxTagKeyLen+1), "v")},
		{"oversized value", awsTags("env", strings.Repeat("v", maxTagValueLen+1))},
		{"reserved prefix", awsTags("aws:createdBy", "me")},
		{"reserved prefix in any case", awsTags("AWS:createdBy", "me")},
		{"empty key", awsTags("", "v")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newCreateHarness(t, "")
			seedCreated(t, h, testDBInstanceID)

			_, err := h.svc.AddTagsToResource(t.Context(), &rds.AddTagsToResourceInput{
				ResourceName: aws.String(FormatARN(ResourceKindDBInstance, testRegion, testAccountID, testDBInstanceID)),
				Tags:         tc.tags,
			}, testAccountID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
			assert.Empty(t, listTags(t, h, testDBInstanceID), "a rejected request must write nothing")
		})
	}
}

// The limit is a per-resource one, so two individually legal requests that
// together exceed it are rejected on the second.
func TestAddTagsToResource_LimitAppliesToTheMergedResult(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	seedCreated(t, h, testDBInstanceID)
	arn := aws.String(FormatARN(ResourceKindDBInstance, testRegion, testAccountID, testDBInstanceID))

	half := make([]string, 0, maxTagsPerResource)
	for i := range maxTagsPerResource / 2 {
		half = append(half, fmt.Sprintf("first-%d", i), "v")
	}
	_, err := h.svc.AddTagsToResource(t.Context(), &rds.AddTagsToResourceInput{
		ResourceName: arn, Tags: awsTags(half...),
	}, testAccountID)
	require.NoError(t, err)

	second := make([]string, 0, maxTagsPerResource)
	for i := 0; i <= maxTagsPerResource/2; i++ {
		second = append(second, fmt.Sprintf("second-%d", i), "v")
	}
	_, err = h.svc.AddTagsToResource(t.Context(), &rds.AddTagsToResourceInput{
		ResourceName: arn, Tags: awsTags(second...),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	assert.Len(t, listTags(t, h, testDBInstanceID), maxTagsPerResource/2,
		"the rejected merge must leave the existing tags untouched")
}

// A well-formed ARN naming nothing is the resource's own fault, not an ARN
// error: the two failures carry different messages on purpose.
func TestTagActions_MissingResourceIsTheResourcesOwnFault(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	arn := aws.String(FormatARN(ResourceKindDBInstance, testRegion, testAccountID, "no-such-db"))

	_, err := h.svc.ListTagsForResource(t.Context(), &rds.ListTagsForResourceInput{ResourceName: arn}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBInstanceNotFound)

	_, err = h.svc.AddTagsToResource(t.Context(), &rds.AddTagsToResourceInput{
		ResourceName: arn, Tags: awsTags("env", "prod"),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBInstanceNotFound)

	_, err = h.svc.RemoveTagsFromResource(t.Context(), &rds.RemoveTagsFromResourceInput{
		ResourceName: arn, TagKeys: aws.StringSlice([]string{"env"}),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBInstanceNotFound)
}

// Answering with an empty list would be indistinguishable from an untagged
// snapshot and would let an apply appear to tag something that does not exist.
func TestTagActions_MissingSnapshotIsRejected(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")

	_, err := h.svc.ListTagsForResource(t.Context(), &rds.ListTagsForResourceInput{
		ResourceName: aws.String(FormatARN(ResourceKindDBSnapshot, testRegion, testAccountID, "orders-db-snap")),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBSnapshotNotFound)
}

func TestListTagsForResource_AcceptsAnAutomatedSnapshotARN(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	id := AutomatedSnapshotIdentifier(testDBInstanceID, time.Date(2026, 7, 24, 3, 4, 0, 0, time.UTC))
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	require.NoError(t, putJSON(t.Context(), kv, DBSnapshotKey(id), &DBSnapshotRecord{
		DBSnapshotIdentifier: id,
		AccountID:            testAccountID,
		Tags:                 map[string]string{"retention": "automated"},
	}))

	out, err := h.svc.ListTagsForResource(t.Context(), &rds.ListTagsForResourceInput{
		ResourceName: aws.String(FormatARN(ResourceKindDBSnapshot, testRegion, testAccountID, id)),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, awsTags("retention", "automated"), out.TagList)
}

func TestTagActions_ForeignAccountARNIsRejected(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	seedCreated(t, h, testDBInstanceID)

	_, err := h.svc.ListTagsForResource(t.Context(), &rds.ListTagsForResourceInput{
		ResourceName: aws.String(FormatARN(ResourceKindDBInstance, testRegion, "210987654321", testDBInstanceID)),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
}

// Every tag write is a read-modify-write of the same record, so without CAS the
// last writer would silently discard the others' keys.
func TestTagWrites_ConcurrentAddsAndRemovesDoNotLoseEachOther(t *testing.T) {
	t.Parallel()
	h := newCreateHarness(t, "")
	seedCreated(t, h, testDBInstanceID)
	arn := aws.String(FormatARN(ResourceKindDBInstance, testRegion, testAccountID, testDBInstanceID))

	// Seeded first so the concurrent removes have something to take away.
	const writers = 4
	seed := make([]string, 0, writers*2)
	for i := range writers {
		seed = append(seed, fmt.Sprintf("doomed-%d", i), "v")
	}
	_, err := h.svc.AddTagsToResource(t.Context(), &rds.AddTagsToResourceInput{
		ResourceName: arn, Tags: awsTags(seed...),
	}, testAccountID)
	require.NoError(t, err)

	var wg sync.WaitGroup
	errs := make(chan error, writers*2)
	for i := range writers {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := h.svc.AddTagsToResource(t.Context(), &rds.AddTagsToResourceInput{
				ResourceName: arn, Tags: awsTags(fmt.Sprintf("kept-%d", i), "v"),
			}, testAccountID)
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := h.svc.RemoveTagsFromResource(t.Context(), &rds.RemoveTagsFromResourceInput{
				ResourceName: arn, TagKeys: aws.StringSlice([]string{fmt.Sprintf("doomed-%d", i)}),
			}, testAccountID)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	got := listTags(t, h, testDBInstanceID)
	want := map[string]string{}
	for i := range writers {
		want[fmt.Sprintf("kept-%d", i)] = "v"
	}
	assert.Equal(t, want, got, "every add must survive and every remove must take effect")
}
