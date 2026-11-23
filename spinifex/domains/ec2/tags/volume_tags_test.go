package tags

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeVolumeTags serves volume records and records whether each call was a
// list (nil IDs) or a point read.
type fakeVolumeTags struct {
	tags  map[string]map[string]string
	calls [][]string
}

func (f *fakeVolumeTags) VolumeTags(_ context.Context, _ string, volumeIDs []string) (map[string]map[string]string, error) {
	f.calls = append(f.calls, volumeIDs)
	out := map[string]map[string]string{}
	for id, tags := range f.tags {
		if volumeIDs == nil || slices.Contains(volumeIDs, id) {
			out[id] = tags
		}
	}
	return out, nil
}

// volumeTagsService has one instance tagged in the tag store, a stale vol- row
// in the tag store, and two volume records that carry the authoritative tags.
func volumeTagsService(t *testing.T) (*TagsServiceImpl, *fakeVolumeTags) {
	t.Helper()
	svc, _ := setupTestTagsService(t)
	_, err := svc.CreateTags(t.Context(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String("i-test1"), aws.String("vol-aaa")},
		Tags:      []*ec2.Tag{{Key: aws.String("Stale"), Value: aws.String("index")}},
	}, testAccountID)
	require.NoError(t, err)
	vols := &fakeVolumeTags{tags: map[string]map[string]string{
		"vol-aaa": {"Name": "data"},
		"vol-bbb": {"Name": "root", "Team": "storage"},
	}}
	svc.SetVolumeTagReader(vols)
	return svc, vols
}

type row struct{ id, typ, key, value string }

func describeRows(t *testing.T, svc *TagsServiceImpl, filters ...*ec2.Filter) []row {
	t.Helper()
	out, err := svc.DescribeTags(t.Context(), &ec2.DescribeTagsInput{Filters: filters}, testAccountID)
	require.NoError(t, err)
	var rows []row
	for _, tag := range out.Tags {
		rows = append(rows, row{aws.StringValue(tag.ResourceId), aws.StringValue(tag.ResourceType), aws.StringValue(tag.Key), aws.StringValue(tag.Value)})
	}
	return rows
}

func filter(name string, values ...string) *ec2.Filter {
	return &ec2.Filter{Name: aws.String(name), Values: aws.StringSlice(values)}
}

func TestDescribeTags_VolumeRowsComeFromVolumeRecords(t *testing.T) {
	svc, vols := volumeTagsService(t)

	rows := describeRows(t, svc)
	assert.ElementsMatch(t, []row{
		{"i-test1", "instance", "Stale", "index"},
		{"vol-aaa", "volume", "Name", "data"},
		{"vol-bbb", "volume", "Name", "root"},
		{"vol-bbb", "volume", "Team", "storage"},
	}, rows, "the stale vol- row in the tag store is not served")
	assert.Equal(t, [][]string{nil}, vols.calls, "an unfiltered call lists the volumes")
}

func TestDescribeTags_ExactVolumeIDIsAPointRead(t *testing.T) {
	svc, vols := volumeTagsService(t)

	rows := describeRows(t, svc, filter("resource-id", "vol-bbb", "i-test1", "vol-missing"))
	assert.ElementsMatch(t, []row{
		{"i-test1", "instance", "Stale", "index"},
		{"vol-bbb", "volume", "Name", "root"},
		{"vol-bbb", "volume", "Team", "storage"},
	}, rows)
	assert.Equal(t, [][]string{{"vol-bbb", "vol-missing"}}, vols.calls)
}

func TestDescribeTags_WildcardVolumeIDListsTheVolumes(t *testing.T) {
	svc, vols := volumeTagsService(t)

	rows := describeRows(t, svc, filter("resource-id", "vol-a*"))
	assert.Equal(t, []row{{"vol-aaa", "volume", "Name", "data"}}, rows)
	assert.Equal(t, [][]string{nil}, vols.calls)
}

func TestDescribeTags_KeyFilterAppliesToVolumeRows(t *testing.T) {
	svc, _ := volumeTagsService(t)

	rows := describeRows(t, svc, filter("key", "Team"))
	assert.Equal(t, []row{{"vol-bbb", "volume", "Team", "storage"}}, rows)
}

func TestDescribeTags_InstanceTypeFilterSkipsVolumes(t *testing.T) {
	svc, vols := volumeTagsService(t)

	rows := describeRows(t, svc, filter("resource-type", "instance"))
	assert.Equal(t, []row{{"i-test1", "instance", "Stale", "index"}}, rows)
	assert.Empty(t, vols.calls, "no volume read when the type filter excludes volumes")
}

// failingVolumeTags fails every read, so a test can show a request never
// reaches the volume records.
type failingVolumeTags struct{}

func (failingVolumeTags) VolumeTags(context.Context, string, []string) (map[string]map[string]string, error) {
	return nil, errors.New("volume store unavailable")
}

func TestDescribeTags_PatternThatCannotMatchAVolumeSkipsVolumeReads(t *testing.T) {
	svc, _ := volumeTagsService(t)
	svc.SetVolumeTagReader(failingVolumeTags{})

	rows := describeRows(t, svc, filter("resource-id", "i-*"))
	assert.Equal(t, []row{{"i-test1", "instance", "Stale", "index"}}, rows)
}

func TestDescribeTags_PatternThatCanMatchAVolumeListsVolumes(t *testing.T) {
	for _, pattern := range []string{"*", "v*", "vol-?aa", "*aaa"} {
		svc, vols := volumeTagsService(t)
		describeRows(t, svc, filter("resource-id", pattern))
		assert.Equal(t, [][]string{nil}, vols.calls, "pattern %q", pattern)
	}
}

// unlistableKV fails any listing of the tag bucket.
type unlistableKV struct{ jetstream.KeyValue }

func (unlistableKV) Keys(context.Context, ...jetstream.WatchOpt) ([]string, error) {
	return nil, errors.New("tag bucket unavailable")
}

func TestDescribeTags_ExactVolumeIDsDoNotListTheTagStore(t *testing.T) {
	svc, _ := volumeTagsService(t)
	svc.kv = unlistableKV{svc.kv}

	rows := describeRows(t, svc, filter("resource-id", "vol-bbb"))
	assert.ElementsMatch(t, []row{
		{"vol-bbb", "volume", "Name", "root"},
		{"vol-bbb", "volume", "Team", "storage"},
	}, rows)
}
