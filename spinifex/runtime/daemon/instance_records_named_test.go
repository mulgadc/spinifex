package daemon_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/mulgadc/spinifex/spinifex/runtime/daemon"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	namedAccount = "111122223333"
	namedOther   = "444455556666"
	namedID      = "i-0123456789abcdef0"
)

// namedVM is a running instance whose primary interface is eni-primary in
// vpc-a, as launch records it.
func namedVM(id string) *vm.VM {
	return &vm.VM{
		ID: id, AccountID: namedAccount, Status: vm.StateRunning, ENIId: "eni-primary",
		Instance: &ec2.Instance{
			InstanceId: aws.String(id), VpcId: aws.String("vpc-a"), SubnetId: aws.String("subnet-a"),
			NetworkInterfaces: []*ec2.InstanceNetworkInterface{{
				NetworkInterfaceId: aws.String("eni-primary"), VpcId: aws.String("vpc-a"), SubnetId: aws.String("subnet-a"),
				Attachment: &ec2.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int64(0)},
			}},
		},
	}
}

func namedBucket(t *testing.T) (*nats.Conn, jetstream.JetStream, jetstream.KeyValue) {
	_, nc, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: daemon.InstanceStateBucket, History: 1})
	require.NoError(t, err)
	return nc, js, kv
}

func putNamedRaw(t *testing.T, kv jetstream.KeyValue, id string, raw []byte) {
	_, err := kv.Put(t.Context(), daemon.InstanceRecordPrefix+id, raw)
	require.NoError(t, err)
}

func putNamed(t *testing.T, kv jetstream.KeyValue, id string, v *vm.VM) {
	raw, err := json.Marshal(v.Record())
	require.NoError(t, err)
	putNamedRaw(t, kv, id, raw)
}

func TestNamedInstanceReader_PointReadsLiveRecord(t *testing.T) {
	_, js, kv := namedBucket(t)
	putNamed(t, kv, namedID, namedVM(namedID))
	stopped := namedVM("i-0000000000000000a")
	stopped.Status = vm.StateStopped
	putNamed(t, kv, stopped.ID, stopped)
	for id, drop := range map[string]func(context.Context, string, ...jetstream.KVDeleteOpt) error{
		"i-0000000000000000d": kv.Delete, "i-0000000000000000e": kv.Purge,
	} {
		putNamed(t, kv, id, namedVM(id))
		require.NoError(t, drop(t.Context(), daemon.InstanceRecordPrefix+id))
	}
	r := daemon.NewNamedInstanceReader(js)

	got, err := r.NamedInstance(t.Context(), namedAccount, namedID)
	require.NoError(t, err)
	assert.Equal(t, &projection.NamedInstance{
		InstanceID: namedID, AccountID: namedAccount, State: "running",
		PrimaryInterfaceID: "eni-primary", VpcID: "vpc-a", SubnetID: "subnet-a",
	}, got)
	assert.True(t, got.Running())

	got, err = r.NamedInstance(t.Context(), namedAccount, stopped.ID)
	require.NoError(t, err)
	assert.False(t, got.Running(), "a stopped instance keeps its live record")

	for name, tc := range map[string]struct{ caller, id string }{
		"absent":        {namedAccount, "i-0000000000000000b"},
		"deleted":       {namedAccount, "i-0000000000000000d"},
		"purged":        {namedAccount, "i-0000000000000000e"},
		"other account": {namedOther, namedID},
	} {
		got, err := r.NamedInstance(t.Context(), tc.caller, tc.id)
		require.NoError(t, err, name)
		assert.Nil(t, got, name)
	}
}

// apiSubjects records the JetStream API subjects that run sends.
func apiSubjects(t *testing.T, nc *nats.Conn, run func()) []string {
	sub, err := nc.SubscribeSync("$JS.API.>")
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()
	require.NoError(t, nc.Flush())
	run()
	// The flush round trip delivers every earlier message to sub first.
	require.NoError(t, nc.Flush())
	n, _, err := sub.Pending()
	require.NoError(t, err)
	subjects := make([]string, 0, n)
	for range n {
		msg, err := sub.NextMsg(time.Second)
		require.NoError(t, err)
		subjects = append(subjects, msg.Subject)
	}
	return subjects
}

// The read is one stream message get, which only the stream leader answers.
// It opens no bucket, so it never waits for a reopen or its migrations.
func TestNamedInstanceReader_OneLeaderReadOpensNoBucket(t *testing.T) {
	nc, js, kv := namedBucket(t)
	putNamed(t, kv, namedID, namedVM(namedID))
	r := daemon.NewNamedInstanceReader(js)

	got := apiSubjects(t, nc, func() {
		_, err := r.NamedInstance(t.Context(), namedAccount, namedID)
		require.NoError(t, err)
	})
	assert.Equal(t, []string{"$JS.API.STREAM.MSG.GET.KV_" + daemon.InstanceStateBucket}, got)

	replica := apiSubjects(t, nc, func() {
		_, err := kv.Get(t.Context(), daemon.InstanceRecordPrefix+namedID)
		require.NoError(t, err)
	})
	direct := "$JS.API.DIRECT.GET.KV_" + daemon.InstanceStateBucket + ".$KV." + daemon.InstanceStateBucket + "." + daemon.InstanceRecordPrefix + namedID
	assert.Equal(t, []string{direct}, replica,
		"an ordinary read is a direct get, which a lagging replica also answers")
}

func TestNamedInstanceReader_FailuresAreNotAbsence(t *testing.T) {
	_, js, kv := namedBucket(t)
	putNamed(t, kv, namedID, namedVM(namedID))
	r := daemon.NewNamedInstanceReader(js)

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := r.NamedInstance(ctx, namedAccount, namedID)
		require.Error(t, err)
	})
	t.Run("record under another name", func(t *testing.T) {
		putNamed(t, kv, "i-0000000000000000d", namedVM("i-0000000000000000c"))
		_, err := r.NamedInstance(t.Context(), namedAccount, "i-0000000000000000d")
		require.ErrorContains(t, err, `names "i-0000000000000000c"`)
	})
	t.Run("record with no stored identity", func(t *testing.T) {
		record := namedVM("i-0000000000000000f").Record()
		record.Metadata.Name = ""
		raw, err := json.Marshal(record)
		require.NoError(t, err)
		putNamedRaw(t, kv, "i-0000000000000000f", raw)
		got, err := r.NamedInstance(t.Context(), namedAccount, "i-0000000000000000f")
		require.ErrorContains(t, err, `names ""`)
		assert.Nil(t, got)
	})
	t.Run("undecodable record", func(t *testing.T) {
		putNamedRaw(t, kv, "i-0000000000000000e", []byte("{"))
		_, err := r.NamedInstance(t.Context(), namedAccount, "i-0000000000000000e")
		require.Error(t, err)
	})
	t.Run("missing bucket", func(t *testing.T) {
		_, _, bare := testutil.StartTestJetStream(t)
		_, err := daemon.NewNamedInstanceReader(bare).NamedInstance(t.Context(), namedAccount, namedID)
		require.Error(t, err)
		_, err = bare.KeyValue(t.Context(), daemon.InstanceStateBucket)
		require.ErrorIs(t, err, jetstream.ErrBucketNotFound, "a reader never creates the record space")
	})
}

func TestNamedInstanceReader_MalformedIDReadsNothing(t *testing.T) {
	_, _, bare := testutil.StartTestJetStream(t)
	r := daemon.NewNamedInstanceReader(bare)
	for _, id := range []string{"", "i-*", "i->", "i-01234567.x", "vol-01234567", "i-0123 4567"} {
		got, err := r.NamedInstance(t.Context(), namedAccount, id)
		require.NoError(t, err, "a read of the missing bucket would fail: %q", id)
		assert.Nil(t, got)
	}
}
