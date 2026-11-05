package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/systeminstance"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A customer can write any non-aws: tag, so each path that takes customer tags
// is checked to leave the ownership field empty and the classifier on Customer.
func tagPathManagerTag() []*ec2.Tag {
	return []*ec2.Tag{{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByEKS)}}
}

func attachClassifierStore(t *testing.T, d *Daemon) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	jsm, err := NewJetStreamManager(nc)
	require.NoError(t, err)
	require.NoError(t, jsm.InitKVBucket())
	require.NoError(t, jsm.InitTerminatedInstanceBucket())
	d.jsManager = jsm
}

func TestClassify_RunInstancesTagSpecificationsCannotMakeSystem(t *testing.T) {
	t.Parallel()
	ns, _, _ := testutil.StartTestJetStream(t)
	d, memStore := createFullTestDaemonWithStore(t, ns.ClientURL())
	seedTestAMI(t, memStore, d.config.Predastore.Bucket, "ami-tagpath")

	jsm, err := NewJetStreamManager(d.natsConn)
	require.NoError(t, err)
	require.NoError(t, jsm.InitKVBucket())
	require.NoError(t, jsm.InitTerminatedInstanceBucket())
	d.jsManager = jsm
	d.stateStore = newStateStoreAdapter(jsm, d.persistState)
	d.vmMgr.SetDeps(vm.Deps{NodeID: d.node, StateStore: d.stateStore})

	sub, err := d.natsConn.QueueSubscribe("ec2.RunInstances.tagpath", "spinifex-workers", asMsgHandler(d.handleEC2RunInstances))
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	input := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-tagpath"),
		InstanceType: aws.String(getTestInstanceType(t)),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("instance"),
			Tags:         tagPathManagerTag(),
		}},
	}
	reply, err := natsRequest(d.natsConn, "ec2.RunInstances.tagpath", mustMarshal(t, input), 5*time.Second)
	require.NoError(t, err)
	var reservation ec2.Reservation
	require.NoError(t, json.Unmarshal(reply.Data, &reservation))
	require.Len(t, reservation.Instances, 1, "launch must return the instance: %s", reply.Data)
	id := aws.StringValue(reservation.Instances[0].InstanceId)

	// The launch fails afterwards in this harness, so the record ends up in the
	// terminated bucket; either location is read by the classifier.
	require.Eventually(t, func() bool {
		c, err := jsm.Classify(id)
		return err == nil && c.Ownership != systeminstance.OwnershipAbsent
	}, 5*time.Second, 20*time.Millisecond, "the launched instance must have a record")

	c, err := jsm.Classify(id)
	require.NoError(t, err)
	assert.Equal(t, systeminstance.OwnershipCustomer, c.Ownership)
	if rec, _ := jsm.LoadInstanceRecord(id); rec != nil {
		assert.Empty(t, rec.Spec.ManagedBy)
	}
	if rec, _ := jsm.LoadTerminatedInstanceRecord(id); rec != nil {
		assert.Empty(t, rec.Spec.ManagedBy)
	}
}

func TestClassify_CreateTagsCannotMakeRunningInstanceSystem(t *testing.T) {
	t.Parallel()
	const id = "i-tagpath-running"
	d := tagTestDaemon(t, id, nil)
	attachClassifierStore(t, d)
	subscribeOwner(t, d, id)

	_, err := d.createTags(context.Background(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String(id)},
		Tags:      tagPathManagerTag(),
	}, testAccountID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{tags.ManagedByKey: tags.ManagedByEKS}, recordTags(t, d, id), "the tag itself is stored")

	v, ok := d.vmMgr.Get(id)
	require.True(t, ok)
	assert.Empty(t, v.ManagedBy)
	record := v.Record()
	assert.Empty(t, record.Spec.ManagedBy)
	require.NoError(t, d.jsManager.WriteInstanceRecord(id, record))

	c, err := d.jsManager.Classify(id)
	require.NoError(t, err)
	assert.Equal(t, systeminstance.OwnershipCustomer, c.Ownership)
}

func TestClassify_CreateTagsCannotMakeStoppedInstanceSystem(t *testing.T) {
	t.Parallel()
	const id = "i-tagpath-stopped"
	d, stopped := tagTestDaemonWithStopped(t, "i-tagpath-unrelated", nil)
	attachClassifierStore(t, d)
	stopped.Stopped[id] = &vm.VM{
		ID:        id,
		Status:    vm.StateStopped,
		AccountID: testAccountID,
		Instance:  &ec2.Instance{InstanceId: aws.String(id)},
	}

	_, err := d.createTags(context.Background(), &ec2.CreateTagsInput{
		Resources: []*string{aws.String(id)},
		Tags:      tagPathManagerTag(),
	}, testAccountID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{tags.ManagedByKey: tags.ManagedByEKS}, tagsAsMap(stopped.Stopped[id].Instance.Tags), "the tag itself is stored")

	got := stopped.Stopped[id]
	assert.Empty(t, got.ManagedBy)
	require.NoError(t, d.jsManager.WriteStoppedInstance(id, got))

	c, err := d.jsManager.Classify(id)
	require.NoError(t, err)
	assert.Equal(t, systeminstance.OwnershipCustomer, c.Ownership)
}
