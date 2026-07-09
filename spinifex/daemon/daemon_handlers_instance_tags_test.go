package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_instance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/instance"
	handlers_ec2_tags "github.com/mulgadc/spinifex/spinifex/handlers/ec2/tags"
	"github.com/mulgadc/spinifex/spinifex/objectstore"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memStoppedStore is a map-backed StoppedInstanceStore standing in for the
// shared JetStream KV in tag fallback tests.
type memStoppedStore struct {
	instances map[string]*vm.VM
}

var _ handlers_ec2_instance.StoppedInstanceStore = (*memStoppedStore)(nil)

func (m *memStoppedStore) LoadStoppedInstance(id string) (*vm.VM, error) {
	return m.instances[id], nil
}

func (m *memStoppedStore) ListStoppedInstances() ([]*vm.VM, error) {
	var out []*vm.VM
	for _, v := range m.instances {
		out = append(out, v)
	}
	return out, nil
}

func (m *memStoppedStore) ListTerminatedInstances() ([]*vm.VM, error) { return nil, nil }

func (m *memStoppedStore) WriteStoppedInstance(id string, instance *vm.VM) error {
	if m.instances == nil {
		m.instances = make(map[string]*vm.VM)
	}
	m.instances[id] = instance
	return nil
}

func (m *memStoppedStore) DeleteStoppedInstance(id string) error {
	delete(m.instances, id)
	return nil
}

// UpdateStoppedInstance mimics the real CAS semantics: a missing record
// returns nats.ErrKeyNotFound instead of resurrecting it.
func (m *memStoppedStore) UpdateStoppedInstance(id string, mutate func(*vm.VM)) (*vm.VM, error) {
	v, ok := m.instances[id]
	if !ok {
		return nil, nats.ErrKeyNotFound
	}
	mutate(v)
	return v, nil
}

func (m *memStoppedStore) ClaimStoppedInstance(id string) (*vm.VM, error) {
	v, ok := m.instances[id]
	if !ok {
		return nil, vm.ErrStoppedInstanceClaimed
	}
	delete(m.instances, id)
	return v, nil
}

func (m *memStoppedStore) WriteTerminatedInstance(string, *vm.VM) error { return nil }

// tagTestDaemon returns a test daemon with an in-memory central tag store, an
// empty stopped store, and a running VM carrying the given initial tags.
func tagTestDaemon(t *testing.T, instanceID string, initial map[string]string) *Daemon {
	d, _ := tagTestDaemonWithStopped(t, instanceID, initial)
	return d
}

// tagTestDaemonWithStopped is tagTestDaemon exposing the stopped store, so
// tests can seed stopped instances for the no-running-owner fallback.
func tagTestDaemonWithStopped(t *testing.T, instanceID string, initial map[string]string) (*Daemon, *memStoppedStore) {
	t.Helper()
	d := createTestDaemon(t, sharedNATSURL)
	d.tagsService = handlers_ec2_tags.NewTagsServiceImplWithStore(d.config, objectstore.NewMemoryObjectStore())

	stopped := &memStoppedStore{instances: map[string]*vm.VM{}}
	d.instanceService = handlers_ec2_instance.NewInstanceServiceImpl(
		d.config, d.resourceMgr.instanceTypes, d.natsConn,
		objectstore.NewMemoryObjectStore(), d.vmMgr, d.resourceMgr, stopped)

	var tags []*ec2.Tag
	for k, v := range initial {
		tags = append(tags, &ec2.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	d.vmMgr.Insert(&vm.VM{
		ID:        instanceID,
		Status:    vm.StateRunning,
		AccountID: testAccountID,
		Instance:  &ec2.Instance{InstanceId: aws.String(instanceID), Tags: tags},
	})
	return d, stopped
}

// centralTags reads a resource's tags back out of the central store via
// DescribeTags, as the given account.
func centralTags(t *testing.T, d *Daemon, accountID, resourceID string) map[string]string {
	t.Helper()
	out, err := d.tagsService.DescribeTags(t.Context(), &ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{{Name: aws.String("resource-id"), Values: []*string{aws.String(resourceID)}}},
	}, accountID)
	require.NoError(t, err)
	tags := make(map[string]string, len(out.Tags))
	for _, td := range out.Tags {
		tags[aws.StringValue(td.Key)] = aws.StringValue(td.Value)
	}
	return tags
}

func recordTags(t *testing.T, d *Daemon, instanceID string) map[string]string {
	t.Helper()
	got, ok := d.vmMgr.Get(instanceID)
	require.True(t, ok)
	return tagsAsMap(got.Instance.Tags)
}

func tagsAsMap(tags []*ec2.Tag) map[string]string {
	out := make(map[string]string, len(tags))
	for _, tag := range tags {
		out[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}
	return out
}

func tagCommand(instanceID string, attrs types.EC2CommandAttributes, data *types.InstanceTagsData) []byte {
	body, _ := json.Marshal(types.EC2InstanceCommand{ID: instanceID, Attributes: attrs, InstanceTagsData: data})
	return body
}

// SetInstanceTags merges the upsert set into the record and projects the full
// tag set into the central store in the same call.
func TestHandleSetInstanceTags_MergesAndWritesCentral(t *testing.T) {
	const id = "i-tag-set"
	d := tagTestDaemon(t, id, map[string]string{"Name": "web", "env": "dev"})

	body := tagCommand(id, types.EC2CommandAttributes{SetInstanceTags: true},
		&types.InstanceTagsData{Tags: map[string]string{"env": "prod", "team": "infra"}})
	reply := requestHandler(t, d.natsConn, "ec2.cmd."+id, d.handleEC2Events, testAccountID, body)
	assert.JSONEq(t, `{}`, string(reply.Data))

	want := map[string]string{"Name": "web", "env": "prod", "team": "infra"}
	assert.Equal(t, want, recordTags(t, d, id))
	assert.Equal(t, want, centralTags(t, d, testAccountID, id))
}

// RemoveInstanceTags removes named keys unconditionally, value-matched keys
// only on match, and both stores reflect the result.
func TestHandleSetInstanceTags_RemoveWritesBothStores(t *testing.T) {
	const id = "i-tag-remove"
	d := tagTestDaemon(t, id, map[string]string{"Name": "web", "env": "dev", "team": "infra"})
	require.NoError(t, d.tagsService.PutResourceTags(t.Context(), testAccountID, id,
		map[string]string{"Name": "web", "env": "dev", "team": "infra"}))

	body := tagCommand(id, types.EC2CommandAttributes{RemoveInstanceTags: true},
		&types.InstanceTagsData{TagKeys: []string{"team"}, Tags: map[string]string{"env": "prod", "Name": "web"}})
	reply := requestHandler(t, d.natsConn, "ec2.cmd."+id, d.handleEC2Events, testAccountID, body)
	assert.JSONEq(t, `{}`, string(reply.Data))

	want := map[string]string{"env": "dev"}
	assert.Equal(t, want, recordTags(t, d, id))
	assert.Equal(t, want, centralTags(t, d, testAccountID, id))
}

// RemoveInstanceTags with empty Tags and TagKeys clears every tag.
func TestHandleSetInstanceTags_RemoveClearAll(t *testing.T) {
	const id = "i-tag-clear"
	d := tagTestDaemon(t, id, map[string]string{"Name": "web", "env": "dev"})
	require.NoError(t, d.tagsService.PutResourceTags(t.Context(), testAccountID, id,
		map[string]string{"Name": "web", "env": "dev"}))

	body := tagCommand(id, types.EC2CommandAttributes{RemoveInstanceTags: true}, &types.InstanceTagsData{})
	reply := requestHandler(t, d.natsConn, "ec2.cmd."+id, d.handleEC2Events, testAccountID, body)
	assert.JSONEq(t, `{}`, string(reply.Data))

	assert.Empty(t, recordTags(t, d, id))
	assert.Empty(t, centralTags(t, d, testAccountID, id))
}

// A cross-account caller is rejected with InvalidID.NotFound before dispatch
// and neither the record nor either central namespace is written.
func TestHandleSetInstanceTags_CrossAccountRejected(t *testing.T) {
	const id = "i-tag-cross"
	const attacker = "999999999999"
	d := tagTestDaemon(t, id, map[string]string{"Name": "web"})

	body := tagCommand(id, types.EC2CommandAttributes{SetInstanceTags: true},
		&types.InstanceTagsData{Tags: map[string]string{"stolen": "yes"}})
	reply := requestHandler(t, d.natsConn, "ec2.cmd."+id, d.handleEC2Events, attacker, body)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, decodeError(t, reply.Data)["Code"])

	assert.Equal(t, map[string]string{"Name": "web"}, recordTags(t, d, id))
	assert.Empty(t, centralTags(t, d, testAccountID, id))
	assert.Empty(t, centralTags(t, d, attacker, id))
}

// RunInstances with instance-scoped TagSpecifications projects the launch tags
// into the central tag store, so describe-tags sees them from birth. The write
// happens after the reservation reply, hence the Eventually.
func TestHandleEC2RunInstances_LaunchTagsWriteCentralStore(t *testing.T) {
	daemon, memStore := createFullTestDaemonWithStore(t, sharedNATSURL)
	seedTestAMI(t, memStore, daemon.config.Predastore.Bucket, "ami-launchtags")

	sub, err := daemon.natsConn.QueueSubscribe("ec2.RunInstances.launchtags", "spinifex-workers", daemon.handleEC2RunInstances)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-launchtags"),
		InstanceType: aws.String(getTestInstanceType(t)),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("instance"),
			Tags: []*ec2.Tag{
				{Key: aws.String("Name"), Value: aws.String("web")},
				{Key: aws.String("env"), Value: aws.String("dev")},
			},
		}},
	}
	reply, err := natsRequest(daemon.natsConn, "ec2.RunInstances.launchtags", mustMarshal(t, input), 5*time.Second)
	require.NoError(t, err)

	var reservation ec2.Reservation
	require.NoError(t, json.Unmarshal(reply.Data, &reservation))
	require.Len(t, reservation.Instances, 1, "launch must succeed and return the instance: %s", reply.Data)
	id := aws.StringValue(reservation.Instances[0].InstanceId)

	want := map[string]string{"Name": "web", "env": "dev"}
	assert.Eventually(t, func() bool {
		return assert.ObjectsAreEqual(want, centralTags(t, daemon, testAccountID, id))
	}, 5*time.Second, 50*time.Millisecond, "central tag store must receive launch tags")
	assert.Equal(t, want, recordTags(t, daemon, id))
}

// Missing InstanceTagsData, and a set with no tags, are rejected with
// MissingParameter and leave both stores untouched.
func TestHandleSetInstanceTags_RejectsMissingData(t *testing.T) {
	const id = "i-tag-nodata"
	d := tagTestDaemon(t, id, map[string]string{"Name": "web"})

	for _, data := range []*types.InstanceTagsData{nil, {}} {
		body := tagCommand(id, types.EC2CommandAttributes{SetInstanceTags: true}, data)
		reply := requestHandler(t, d.natsConn, "ec2.cmd."+id, d.handleEC2Events, testAccountID, body)
		assert.Equal(t, awserrors.ErrorMissingParameter, decodeError(t, reply.Data)["Code"])
	}

	assert.Equal(t, map[string]string{"Name": "web"}, recordTags(t, d, id))
	assert.Empty(t, centralTags(t, d, testAccountID, id))
}
