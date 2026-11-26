package instance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	namedAccount = "111122223333"
	namedOther   = "444455556666"
	namedID      = "i-0123456789abcdef0"
)

func namedNIC(eni, vpc, subnet string, index int64) *ec2.InstanceNetworkInterface {
	return &ec2.InstanceNetworkInterface{
		NetworkInterfaceId: aws.String(eni), VpcId: aws.String(vpc), SubnetId: aws.String(subnet),
		Attachment: &ec2.InstanceNetworkInterfaceAttachment{DeviceIndex: aws.Int64(index)},
	}
}

// namedVM is a running instance whose primary interface is eni-primary in
// vpc-a, as launch records it.
func namedVM() *vm.VM {
	return &vm.VM{
		ID: namedID, AccountID: namedAccount, Status: vm.StateRunning, ENIId: "eni-primary",
		Instance: &ec2.Instance{
			InstanceId: aws.String(namedID), VpcId: aws.String("vpc-a"), SubnetId: aws.String("subnet-a"),
			NetworkInterfaces: []*ec2.InstanceNetworkInterface{namedNIC("eni-primary", "vpc-a", "subnet-a", 0)},
		},
	}
}

func TestProjectNamedInstance(t *testing.T) {
	running := &NamedInstance{
		InstanceID: namedID, AccountID: namedAccount, State: vm.StateRunning,
		PrimaryInterfaceID: "eni-primary", VpcID: "vpc-a", SubnetID: "subnet-a",
	}
	now := time.Now()
	for name, tc := range map[string]struct {
		caller string
		edit   func(*vm.VM)
		want   func(*NamedInstance) *NamedInstance
	}{
		"running": {namedAccount, func(*vm.VM) {}, func(n *NamedInstance) *NamedInstance { return n }},
		"stopped keeps its placement": {namedAccount, func(v *vm.VM) { v.Status = vm.StateStopped },
			func(n *NamedInstance) *NamedInstance { n.State = vm.StateStopped; return n }},
		"terminate decided": {namedAccount, func(v *vm.VM) { v.DeletionTimestamp = &now },
			func(n *NamedInstance) *NamedInstance { n.Deleting = true; return n }},
		"other account":            {namedOther, func(*vm.VM) {}, nil},
		"managed, customer caller": {namedAccount, func(v *vm.VM) { v.ManagedBy = "elbv2" }, nil},
		"unowned, customer caller": {namedAccount, func(v *vm.VM) { v.AccountID = "" }, nil},
		"managed, global caller": {awsidentifiers.GlobalAccountID, func(v *vm.VM) { v.ManagedBy, v.AccountID = "elbv2", awsidentifiers.GlobalAccountID },
			func(n *NamedInstance) *NamedInstance { n.AccountID = awsidentifiers.GlobalAccountID; return n }},
		"primary in another VPC, matching secondary": {namedAccount, func(v *vm.VM) {
			v.Instance.NetworkInterfaces = []*ec2.InstanceNetworkInterface{
				namedNIC("eni-second", "vpc-a", "subnet-a", 1), namedNIC("eni-primary", "vpc-b", "subnet-b", 0),
			}
		}, func(n *NamedInstance) *NamedInstance { n.VpcID, n.SubnetID = "vpc-b", "subnet-b"; return n }},
		"device 0 is not the recorded primary": {namedAccount, func(v *vm.VM) {
			v.Instance.NetworkInterfaces = []*ec2.InstanceNetworkInterface{namedNIC("eni-other", "vpc-a", "subnet-a", 0)}
		}, func(n *NamedInstance) *NamedInstance {
			n.PrimaryInterfaceID, n.VpcID, n.SubnetID = "", "", ""
			return n
		}},
		"no recorded primary": {namedAccount, func(v *vm.VM) { v.ENIId = "" },
			func(n *NamedInstance) *NamedInstance {
				n.PrimaryInterfaceID, n.VpcID, n.SubnetID = "", "", ""
				return n
			}},
		"no interfaces": {namedAccount, func(v *vm.VM) { v.Instance.NetworkInterfaces = nil },
			func(n *NamedInstance) *NamedInstance {
				n.PrimaryInterfaceID, n.VpcID, n.SubnetID = "", "", ""
				return n
			}},
		"no attachment index": {namedAccount, func(v *vm.VM) { v.Instance.NetworkInterfaces[0].Attachment.DeviceIndex = nil },
			func(n *NamedInstance) *NamedInstance {
				n.PrimaryInterfaceID, n.VpcID, n.SubnetID = "", "", ""
				return n
			}},
		"no instance projection": {namedAccount, func(v *vm.VM) { v.Instance = nil },
			func(n *NamedInstance) *NamedInstance {
				n.PrimaryInterfaceID, n.VpcID, n.SubnetID = "", "", ""
				return n
			}},
	} {
		t.Run(name, func(t *testing.T) {
			v := namedVM()
			tc.edit(v)
			got := ProjectNamedInstance(tc.caller, namedID, v)
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			want := *running
			assert.Equal(t, tc.want(&want), got)
		})
	}
}

func TestValidInstanceID(t *testing.T) {
	for _, id := range []string{"i-01234567", namedID} {
		assert.True(t, ValidInstanceID(id), id)
	}
	for _, id := range []string{"", "i-", "i-0123456", "i-0123456789abcdef", "i-0123456789ABCDEF0", "i-0123456*", "i-01234567.x", "x-01234567", "i-01234567\n"} {
		assert.False(t, ValidInstanceID(id), "%q", id)
	}
}

func namedBucket(t *testing.T) (jetstream.JetStream, jetstream.KeyValue) {
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: instanceStateBucket, History: 1})
	require.NoError(t, err)
	return js, kv
}

func putNamed(t *testing.T, kv jetstream.KeyValue, key string, v *vm.VM) {
	raw, err := json.Marshal(v.Record())
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), key, raw)
	require.NoError(t, err)
}

func TestNamedInstanceReader_PointReadsLiveRecord(t *testing.T) {
	js, kv := namedBucket(t)
	putNamed(t, kv, instanceRecordPrefix+namedID, namedVM())
	stopped := namedVM()
	stopped.ID, stopped.Status = "i-0000000000000000a", vm.StateStopped
	putNamed(t, kv, instanceRecordPrefix+stopped.ID, stopped)
	r := NewNamedInstanceReader(js)

	got, err := r.NamedInstance(t.Context(), namedAccount, namedID)
	require.NoError(t, err)
	assert.Equal(t, &NamedInstance{
		InstanceID: namedID, AccountID: namedAccount, State: vm.StateRunning,
		PrimaryInterfaceID: "eni-primary", VpcID: "vpc-a", SubnetID: "subnet-a",
	}, got)

	got, err = r.NamedInstance(t.Context(), namedAccount, stopped.ID)
	require.NoError(t, err)
	assert.Equal(t, vm.StateStopped, got.State, "a stopped instance keeps its live record")

	for name, tc := range map[string]struct{ caller, id string }{
		"absent":        {namedAccount, "i-0000000000000000b"},
		"other account": {namedOther, namedID},
	} {
		got, err := r.NamedInstance(t.Context(), tc.caller, tc.id)
		require.NoError(t, err, name)
		assert.Nil(t, got, name)
	}
}

func TestNamedInstanceReader_FailuresAreNotAbsence(t *testing.T) {
	js, kv := namedBucket(t)
	putNamed(t, kv, instanceRecordPrefix+namedID, namedVM())

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := NewNamedInstanceReader(js).NamedInstance(ctx, namedAccount, namedID)
		require.Error(t, err)
	})
	t.Run("record under another name", func(t *testing.T) {
		other := namedVM()
		other.ID = "i-0000000000000000c"
		putNamed(t, kv, instanceRecordPrefix+"i-0000000000000000d", other)
		_, err := NewNamedInstanceReader(js).NamedInstance(t.Context(), namedAccount, "i-0000000000000000d")
		require.ErrorContains(t, err, "names i-0000000000000000c")
	})
	t.Run("undecodable record", func(t *testing.T) {
		_, err := kv.Put(t.Context(), instanceRecordPrefix+"i-0000000000000000e", []byte("{"))
		require.NoError(t, err)
		_, err = NewNamedInstanceReader(js).NamedInstance(t.Context(), namedAccount, "i-0000000000000000e")
		require.Error(t, err)
	})
	t.Run("missing bucket", func(t *testing.T) {
		_, _, bare := testutil.StartTestJetStream(t)
		_, err := NewNamedInstanceReader(bare).NamedInstance(t.Context(), namedAccount, namedID)
		require.Error(t, err)
		_, err = bare.KeyValue(t.Context(), instanceStateBucket)
		require.ErrorIs(t, err, jetstream.ErrBucketNotFound, "a reader never creates the record space")
	})
}

func TestNamedInstanceReader_MalformedIDReadsNothing(t *testing.T) {
	_, _, bare := testutil.StartTestJetStream(t)
	r := NewNamedInstanceReader(bare)
	for _, id := range []string{"", "i-*", "i->", "i-01234567.x", "vol-01234567", "i-0123 4567"} {
		got, err := r.NamedInstance(t.Context(), namedAccount, id)
		require.NoError(t, err, "a read of the missing bucket would fail: %q", id)
		assert.Nil(t, got)
	}
}
