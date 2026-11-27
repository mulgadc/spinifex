package vpc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// laggingIPAMKV serves pinned keys from an older entry, as a lagging replica
// would, runs a hook ahead of each write, can lose a committed write's reply,
// and counts ordinary reads and the leader reads of its ipamLeaderJS.
type laggingIPAMKV struct {
	jetstream.KeyValue

	mu           sync.Mutex
	stale        map[string]jetstream.KeyValueEntry
	staleReads   int
	gets         int
	leaderReads  int
	updates      int
	creates      int
	beforeUpdate func(attempt int)
	beforeCreate func(attempt int)
	lostAck      bool
}

func (k *laggingIPAMKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	k.mu.Lock()
	k.gets++
	entry, ok := k.stale[key]
	if ok {
		k.staleReads++
	}
	k.mu.Unlock()
	if ok {
		return entry, nil
	}
	return k.KeyValue.Get(ctx, key)
}

func (k *laggingIPAMKV) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	k.mu.Lock()
	k.updates++
	attempt, hook := k.updates, k.beforeUpdate
	k.mu.Unlock()
	if hook != nil {
		hook(attempt)
	}
	rev, err := k.KeyValue.Update(ctx, key, value, revision)
	return k.reply(rev, err)
}

func (k *laggingIPAMKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	k.mu.Lock()
	k.creates++
	attempt, hook := k.creates, k.beforeCreate
	k.mu.Unlock()
	if hook != nil {
		hook(attempt)
	}
	rev, err := k.KeyValue.Create(ctx, key, value, opts...)
	return k.reply(rev, err)
}

// errLostAck is the reply of a write that committed but whose answer was lost.
var errLostAck = errors.New("nats: timeout")

func (k *laggingIPAMKV) reply(rev uint64, err error) (uint64, error) {
	k.mu.Lock()
	lost := k.lostAck
	k.mu.Unlock()
	if err == nil && lost {
		return 0, errLostAck
	}
	return rev, err
}

func (k *laggingIPAMKV) counts() (staleReads, updates int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.staleReads, k.updates
}

// reads is every ordinary and leader read attempt since the store opened.
func (k *laggingIPAMKV) reads() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.gets + k.leaderReads
}

func (k *laggingIPAMKV) countLeaderRead() {
	k.mu.Lock()
	k.leaderReads++
	k.mu.Unlock()
}

func (k *laggingIPAMKV) writes() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.updates + k.creates
}

// ipamLeaderJS hands the IPAM store the lagging handle while leader reads go
// to the real client. leaderDown points leader reads at a prefix nothing serves.
// leaderRead, when set, runs at the start of every leader read.
type ipamLeaderJS struct {
	jetstream.JetStream

	kv         jetstream.KeyValue
	leaderDown bool
	leaderRead func()
}

func (j ipamLeaderJS) KeyValue(ctx context.Context, bucket string) (jetstream.KeyValue, error) {
	if bucket == KVBucketIPAM {
		return j.kv, nil
	}
	return j.JetStream.KeyValue(ctx, bucket)
}

func (j ipamLeaderJS) Options() jetstream.JetStreamOptions {
	if j.leaderRead != nil {
		j.leaderRead()
	}
	opts := j.JetStream.Options()
	if j.leaderDown {
		opts.APIPrefix = "$JS.UNSERVED.API"
	}
	return opts
}

// laggingIPAM opens an IPAM over the real bucket on js whose ordinary reads go
// through the returned lagging handle.
func laggingIPAM(t *testing.T, js jetstream.JetStream, leaderDown bool) (*IPAM, *laggingIPAMKV, jetstream.KeyValue) {
	t.Helper()
	direct, err := NewIPAM(t.Context(), js)
	require.NoError(t, err)
	realKV, err := direct.store.KV(t.Context())
	require.NoError(t, err)
	lagging := &laggingIPAMKV{KeyValue: realKV, stale: map[string]jetstream.KeyValueEntry{}}
	ipam, err := NewIPAM(t.Context(), ipamLeaderJS{JetStream: js, kv: lagging, leaderDown: leaderDown, leaderRead: lagging.countLeaderRead})
	require.NoError(t, err)
	lagging.mu.Lock()
	lagging.gets, lagging.leaderReads = 0, 0
	lagging.mu.Unlock()
	return ipam, lagging, realKV
}

// putIPAMRow commits record as its subnet's row and returns the new entry.
func putIPAMRow(t *testing.T, kv jetstream.KeyValue, record IPAMRecord) jetstream.KeyValueEntry {
	t.Helper()
	return putIPAMRowAt(t, kv, record.SubnetId, record)
}

// putIPAMRowAt commits record under key, which can differ from its subnet.
func putIPAMRowAt(t *testing.T, kv jetstream.KeyValue, key string, record IPAMRecord) jetstream.KeyValueEntry {
	t.Helper()
	data, err := json.Marshal(record)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), key, data)
	require.NoError(t, err)
	entry, err := kv.Get(t.Context(), key)
	require.NoError(t, err)
	return entry
}

func ipamTestJS(t *testing.T) jetstream.JetStream {
	t.Helper()
	_, nc := acquireJetStream(t)
	return testutil.NewJetStream(t, nc)
}

func TestIPAM_LegacyReserveRefusesVIPPurpose(t *testing.T) {
	t.Parallel()
	ipam := setupTestIPAM(t)
	kv, err := ipam.store.KV(t.Context())
	require.NoError(t, err)

	_, err = ipam.AllocateIP(t.Context(), "subnet-vip-alloc", "10.0.42.0/24", PurposeLBVIP, "lb-0c")
	require.ErrorIs(t, err, ErrPurposeReserved)
	_, err = ipam.ClaimIP(t.Context(), "subnet-vip-claim", "10.0.42.0/24", PurposeLBVIP, "lb-0c", "10.0.42.9")
	require.ErrorIs(t, err, ErrPurposeReserved)

	for _, subnet := range []string{"subnet-vip-alloc", "subnet-vip-claim"} {
		_, err := kv.Get(t.Context(), subnet)
		assert.ErrorIs(t, err, jetstream.ErrKeyNotFound, "a refused %s writes no row", subnet)
	}
}

func TestIPAM_ReleaseIPLeaderDecidesStaleVIP(t *testing.T) {
	t.Parallel()
	const subnet = "subnet-vip-stale"
	vip := IPAMRecord{SubnetId: subnet, CidrBlock: "10.0.43.0/24", Allocated: []IPEntry{
		{IP: "10.0.43.4", Purpose: PurposeLBVIP, OwnerID: "lb-0d"},
	}}
	reused := IPAMRecord{SubnetId: subnet, CidrBlock: "10.0.43.0/24", Allocated: []IPEntry{
		{IP: "10.0.43.4", Purpose: PurposeENIPrimary, OwnerID: "eni-0d"},
	}}

	t.Run("failed leader read writes nothing", func(t *testing.T) {
		t.Parallel()
		ipam, lagging, realKV := laggingIPAM(t, ipamTestJS(t), true)
		lagging.stale[subnet] = putIPAMRow(t, realKV, vip)
		before := putIPAMRow(t, realKV, reused).Value()

		err := ipam.ReleaseIP(t.Context(), subnet, "10.0.43.4")
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrPurposeReserved, "an unread leader is not a refusal")
		assert.Equal(t, string(before), string(committedIPAMRow(t, realKV, subnet)))
		staleReads, updates := lagging.counts()
		assert.Equal(t, 1, staleReads, "the replica read took the confirmation path")
		assert.Zero(t, updates, "no write without the leader's answer")
	})

	t.Run("current VIP on the leader is refused", func(t *testing.T) {
		t.Parallel()
		ipam, lagging, realKV := laggingIPAM(t, ipamTestJS(t), false)
		entry := putIPAMRow(t, realKV, vip)
		lagging.stale[subnet] = entry

		err := ipam.ReleaseIP(t.Context(), subnet, "10.0.43.4")
		require.ErrorIs(t, err, ErrPurposeReserved)
		assert.Equal(t, string(entry.Value()), string(committedIPAMRow(t, realKV, subnet)))
		_, updates := lagging.counts()
		assert.Zero(t, updates)
	})

	t.Run("leader lacks the address", func(t *testing.T) {
		t.Parallel()
		ipam, lagging, realKV := laggingIPAM(t, ipamTestJS(t), false)
		lagging.stale[subnet] = putIPAMRow(t, realKV, vip)
		putIPAMRow(t, realKV, IPAMRecord{SubnetId: subnet, CidrBlock: "10.0.43.0/24"})

		err := ipam.ReleaseIP(t.Context(), subnet, "10.0.43.4")
		assert.ErrorContains(t, err, "not allocated")
	})

	t.Run("leader lacks the row", func(t *testing.T) {
		t.Parallel()
		ipam, lagging, realKV := laggingIPAM(t, ipamTestJS(t), false)
		lagging.stale[subnet] = putIPAMRow(t, realKV, vip)
		require.NoError(t, realKV.Delete(t.Context(), subnet))

		err := ipam.ReleaseIP(t.Context(), subnet, "10.0.43.4")
		assert.ErrorContains(t, err, "no IPAM record for subnet "+subnet)
	})

	t.Run("wildcard key never reaches the leader", func(t *testing.T) {
		t.Parallel()
		const wildcard = "subnet-vip.*"
		ipam, lagging, realKV := laggingIPAM(t, ipamTestJS(t), true)
		lagging.stale[wildcard] = putIPAMRow(t, realKV, vip)

		err := ipam.ReleaseIP(t.Context(), wildcard, "10.0.43.4")
		assert.ErrorContains(t, err, "is not a literal key")
	})
}

func TestDeleteNetworkInterface_ReleasesFormerVIPOnStaleRead(t *testing.T) {
	t.Parallel()
	svc, nc := setupTestVPCServiceWithNC(t)
	js := testutil.NewJetStream(t, nc)
	vpcID := createTestVPC(t, svc, "10.0.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.0.1.0/24")

	ipam, lagging, realKV := laggingIPAM(t, js, false)
	lagging.stale[subnetID] = putIPAMRow(t, realKV, IPAMRecord{SubnetId: subnetID, CidrBlock: "10.0.1.0/24", Allocated: []IPEntry{
		{IP: "10.0.1.4", Purpose: PurposeLBVIP, OwnerID: "lb-0e"},
	}})
	putIPAMRow(t, realKV, IPAMRecord{SubnetId: subnetID, CidrBlock: "10.0.1.0/24"})
	eniID := createTestENI(t, svc, subnetID)
	require.Equal(t, []IPEntry{{IP: "10.0.1.4", Purpose: PurposeENIPrimary, OwnerID: eniID}}, currentIPAMEntries(t, realKV, subnetID),
		"the ENI now owns the former load balancer address")

	competitor, err := NewIPAM(t.Context(), js)
	require.NoError(t, err)
	var competing string
	lagging.beforeUpdate = func(attempt int) {
		if attempt != 1 {
			return
		}
		ip, allocErr := competitor.AllocateIP(context.Background(), subnetID, "10.0.1.0/24", PurposeENIPrimary, "eni-competing")
		require.NoError(t, allocErr)
		competing = ip
	}
	svc.ipam = ipam

	_, err = svc.DeleteNetworkInterface(t.Context(), &ec2.DeleteNetworkInterfaceInput{NetworkInterfaceId: aws.String(eniID)}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, []IPEntry{{IP: competing, Purpose: PurposeENIPrimary, OwnerID: "eni-competing"}}, currentIPAMEntries(t, realKV, subnetID),
		"the ENI's address is free and the entry written between the leader read and the CAS survives")
	staleReads, updates := lagging.counts()
	assert.Equal(t, 1, staleReads, "the ordinary read showed the old load balancer address")
	assert.Equal(t, 2, updates, "the first CAS lost to the competitor and the retry used the leader's new revision")
}

func currentIPAMEntries(t *testing.T, kv jetstream.KeyValue, subnetID string) []IPEntry {
	t.Helper()
	var record IPAMRecord
	require.NoError(t, json.Unmarshal(committedIPAMRow(t, kv, subnetID), &record))
	return record.Allocated
}

var _ jetstream.KeyValue = (*laggingIPAMKV)(nil)

// An address-only release that read an ENI loses its CAS to the same address
// becoming a load balancer address, and the reread refuses to free it. Both
// the ordinary path and the leader-confirmed path recheck the purpose.
func TestIPAM_ReleaseIPRefusesPurposeChangeBeforeCAS(t *testing.T) {
	t.Parallel()
	const subnet = "subnet-vip-swap"
	eni := IPAMRecord{SubnetId: subnet, CidrBlock: "10.0.44.0/24", Allocated: []IPEntry{
		{IP: "10.0.44.4", Purpose: PurposeENIPrimary, OwnerID: "eni-0f"},
	}}
	vip := IPAMRecord{SubnetId: subnet, CidrBlock: "10.0.44.0/24", Allocated: []IPEntry{
		{IP: "10.0.44.4", Purpose: PurposeLBVIP, OwnerID: "lb-0f"},
	}}

	for name, staleVIP := range map[string]bool{"ordinary read": false, "leader-confirmed read": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			js := ipamTestJS(t)
			ipam, lagging, realKV := laggingIPAM(t, js, false)
			if staleVIP {
				lagging.stale[subnet] = putIPAMRow(t, realKV, vip)
			}
			putIPAMRow(t, realKV, eni)
			competitor, err := NewIPAM(t.Context(), js)
			require.NoError(t, err)
			var replaced jetstream.KeyValueEntry
			lagging.beforeUpdate = func(attempt int) {
				if attempt != 1 {
					return
				}
				assert.NoError(t, competitor.ReleaseIP(context.Background(), subnet, "10.0.44.4"))
				ip, err := competitor.ReserveVIP(context.Background(), subnet, "10.0.44.0/24", "lb-0f")
				assert.NoError(t, err)
				assert.Equal(t, "10.0.44.4", ip, "the replacement reserves the same address")
				replaced, err = realKV.Get(context.Background(), subnet)
				assert.NoError(t, err)
			}

			err = ipam.ReleaseIP(t.Context(), subnet, "10.0.44.4")
			require.ErrorIs(t, err, ErrPurposeReserved)
			require.NotNil(t, replaced, "the release reached its CAS")
			after, err := realKV.Get(t.Context(), subnet)
			require.NoError(t, err)
			assert.Equal(t, replaced.Revision(), after.Revision(), "the replacement's revision survives")
			assert.Equal(t, string(replaced.Value()), string(after.Value()))
			assert.Equal(t, vip.Allocated, currentIPAMEntries(t, realKV, subnet))
			staleReads, updates := lagging.counts()
			assert.Equal(t, 1, updates, "only the stale CAS was attempted, and it lost")
			wantStale, wantLeader := 0, 1
			if staleVIP {
				wantStale, wantLeader = 1, 2
			}
			assert.Equal(t, wantStale, staleReads, "the first read took the intended path")
			assert.Equal(t, wantLeader, lagging.leaderReads, "the leader decided after the lost CAS")
		})
	}
}
