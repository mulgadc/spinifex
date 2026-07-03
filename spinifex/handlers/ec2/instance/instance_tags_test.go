package handlers_ec2_instance

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	spxtypes "github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tagList(pairs ...string) []*ec2.Tag {
	var tags []*ec2.Tag
	for i := 0; i+1 < len(pairs); i += 2 {
		tags = append(tags, &ec2.Tag{Key: aws.String(pairs[i]), Value: aws.String(pairs[i+1])})
	}
	return tags
}

func tagsAsMap(tags []*ec2.Tag) map[string]string {
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		out[aws.StringValue(t.Key)] = aws.StringValue(t.Value)
	}
	return out
}

func TestApplyInstanceTagMutation_MergeUpsert(t *testing.T) {
	existing := tagList("Name", "web", "env", "dev")
	out := ApplyInstanceTagMutation(existing, &spxtypes.InstanceTagsData{
		Tags: map[string]string{"env": "prod", "team": "infra"},
	}, false)
	assert.Equal(t, map[string]string{"Name": "web", "env": "prod", "team": "infra"}, tagsAsMap(out))
}

func TestApplyInstanceTagMutation_RemoveUnconditional(t *testing.T) {
	existing := tagList("Name", "web", "env", "dev")
	out := ApplyInstanceTagMutation(existing, &spxtypes.InstanceTagsData{
		TagKeys: []string{"env", "missing"},
	}, true)
	assert.Equal(t, map[string]string{"Name": "web"}, tagsAsMap(out))
}

func TestApplyInstanceTagMutation_RemoveValueMatch(t *testing.T) {
	existing := tagList("Name", "web", "env", "dev")
	out := ApplyInstanceTagMutation(existing, &spxtypes.InstanceTagsData{
		Tags: map[string]string{"env": "prod", "Name": "web"},
	}, true)
	assert.Equal(t, map[string]string{"env": "dev"}, tagsAsMap(out))
}

func TestApplyInstanceTagMutation_RemoveClearAll(t *testing.T) {
	existing := tagList("Name", "web", "env", "dev")
	out := ApplyInstanceTagMutation(existing, &spxtypes.InstanceTagsData{}, true)
	assert.Empty(t, out)
}

func TestApplyInstanceTagMutation_SortedAndNilSafe(t *testing.T) {
	existing := []*ec2.Tag{nil, {Key: aws.String("b"), Value: aws.String("2")}, {Key: nil}}
	out := ApplyInstanceTagMutation(existing, &spxtypes.InstanceTagsData{
		Tags: map[string]string{"a": "1"},
	}, false)
	require.Len(t, out, 2)
	assert.Equal(t, "a", aws.StringValue(out[0].Key))
	assert.Equal(t, "b", aws.StringValue(out[1].Key))
}

type fakeTagWriter struct {
	accountID   string
	resourceID  string
	tags        map[string]string
	err         error
	calls       int
	deleteCalls int
}

func (f *fakeTagWriter) PutResourceTags(accountID, resourceID string, tags map[string]string) error {
	f.calls++
	f.accountID = accountID
	f.resourceID = resourceID
	f.tags = tags
	return f.err
}

func (f *fakeTagWriter) DeleteAllTags(accountID, resourceID string) error {
	f.deleteCalls++
	f.accountID = accountID
	f.resourceID = resourceID
	return f.err
}

func TestWriteInstanceTags_WritesRecordAndCentral(t *testing.T) {
	instance := &vm.VM{ID: "i-123", Instance: &ec2.Instance{Tags: tagList("Name", "web")}}
	writer := &fakeTagWriter{}

	err := WriteInstanceTags(instance, &spxtypes.InstanceTagsData{
		Tags: map[string]string{"env": "prod"},
	}, false, writer, "111122223333")
	require.NoError(t, err)

	want := map[string]string{"Name": "web", "env": "prod"}
	assert.Equal(t, want, tagsAsMap(instance.Instance.Tags))
	assert.Equal(t, 1, writer.calls)
	assert.Equal(t, "111122223333", writer.accountID)
	assert.Equal(t, "i-123", writer.resourceID)
	assert.Equal(t, want, writer.tags)
}

func TestWriteInstanceTags_CentralWriteErrorPropagates(t *testing.T) {
	instance := &vm.VM{ID: "i-123", Instance: &ec2.Instance{}}
	writer := &fakeTagWriter{err: errors.New("s3 down")}

	err := WriteInstanceTags(instance, &spxtypes.InstanceTagsData{
		Tags: map[string]string{"env": "prod"},
	}, false, writer, "111122223333")
	assert.Error(t, err)
}

func TestWriteInstanceTags_NilRecordRejected(t *testing.T) {
	writer := &fakeTagWriter{}
	err := WriteInstanceTags(nil, nil, false, writer, "111122223333")
	assert.Error(t, err)
	err = WriteInstanceTags(&vm.VM{ID: "i-123"}, nil, false, writer, "111122223333")
	assert.Error(t, err)
	assert.Zero(t, writer.calls)
}

func stoppedTagInstance(id, accountID string, tags []*ec2.Tag) *vm.VM {
	return &vm.VM{
		ID:        id,
		AccountID: accountID,
		Status:    vm.StateStopped,
		Instance:  &ec2.Instance{InstanceId: aws.String(id), Tags: tags},
	}
}

func TestTagStoppedInstance_WritesRecordAndCentral(t *testing.T) {
	stored := stoppedTagInstance("i-123", "111122223333", tagList("Name", "web"))
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{"i-123": stored}}
	writer := &fakeTagWriter{}
	svc := &InstanceServiceImpl{stoppedStore: store}

	err := svc.TagStoppedInstance("i-123", &spxtypes.InstanceTagsData{
		Tags: map[string]string{"env": "prod"},
	}, false, writer, "111122223333")
	require.NoError(t, err)

	want := map[string]string{"Name": "web", "env": "prod"}
	written := store.wroteStopped["i-123"]
	require.NotNil(t, written)
	assert.Equal(t, want, tagsAsMap(written.Instance.Tags))
	assert.Equal(t, want, writer.tags)
	assert.Equal(t, "i-123", writer.resourceID)
	assert.Equal(t, "111122223333", writer.accountID)
}

func TestTagStoppedInstance_RemoveKeys(t *testing.T) {
	stored := stoppedTagInstance("i-123", "111122223333", tagList("Name", "web", "env", "dev"))
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{"i-123": stored}}
	writer := &fakeTagWriter{}
	svc := &InstanceServiceImpl{stoppedStore: store}

	err := svc.TagStoppedInstance("i-123", &spxtypes.InstanceTagsData{
		TagKeys: []string{"env"},
	}, true, writer, "111122223333")
	require.NoError(t, err)

	want := map[string]string{"Name": "web"}
	assert.Equal(t, want, tagsAsMap(store.wroteStopped["i-123"].Instance.Tags))
	assert.Equal(t, want, writer.tags)
}

func TestTagStoppedInstance_NotFound(t *testing.T) {
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{}}
	writer := &fakeTagWriter{}
	svc := &InstanceServiceImpl{stoppedStore: store}

	err := svc.TagStoppedInstance("i-missing", &spxtypes.InstanceTagsData{
		Tags: map[string]string{"env": "prod"},
	}, false, writer, "111122223333")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
	assert.Zero(t, writer.calls)
	assert.Empty(t, store.wroteStopped)
}

func TestTagStoppedInstance_CrossAccountRejected(t *testing.T) {
	stored := stoppedTagInstance("i-123", "999988887777", tagList("Name", "web"))
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{"i-123": stored}}
	writer := &fakeTagWriter{}
	svc := &InstanceServiceImpl{stoppedStore: store}

	err := svc.TagStoppedInstance("i-123", &spxtypes.InstanceTagsData{
		Tags: map[string]string{"env": "prod"},
	}, false, writer, "111122223333")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
	assert.Zero(t, writer.calls)
	assert.Empty(t, store.wroteStopped)
}

func TestTagStoppedInstance_CentralWriteErrorSkipsRecordWrite(t *testing.T) {
	stored := stoppedTagInstance("i-123", "111122223333", tagList("Name", "web"))
	store := &fakeStoppedStore{loadByID: map[string]*vm.VM{"i-123": stored}}
	writer := &fakeTagWriter{err: errors.New("s3 down")}
	svc := &InstanceServiceImpl{stoppedStore: store}

	err := svc.TagStoppedInstance("i-123", &spxtypes.InstanceTagsData{
		Tags: map[string]string{"env": "prod"},
	}, false, writer, "111122223333")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Empty(t, store.wroteStopped)
}
