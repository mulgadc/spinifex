package handlers_imds

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLookup is a programmable instanceLookup so resolver tests never need the
// account-scoped instance fan-out.
type fakeLookup struct {
	facts       *instanceFacts
	err         error
	calls       int
	lastAccount string
}

func (f *fakeLookup) describe(_ context.Context, accountID, _ string) (*instanceFacts, error) {
	f.calls++
	f.lastAccount = accountID
	return f.facts, f.err
}

// newTestResolver wires a metadataResolver onto a live test JetStream with the
// two buckets it reads (ENI source-of-truth, SG names) created empty, plus a
// fake instance lookup the caller can program.
func newTestResolver(t *testing.T) (*metadataResolver, *fakeLookup) {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	eniKV, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: kvBucketENIs, History: 1})
	require.NoError(t, err)
	sgKV, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: kvBucketSecurityGroups, History: 1})
	require.NoError(t, err)

	lookup := &fakeLookup{}
	return &metadataResolver{eniKV: eniKV, sgKV: sgKV, lookup: lookup}, lookup
}

func putJSON(t *testing.T, kv jetstream.KeyValue, key string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), key, data)
	require.NoError(t, err)
}

// seedENIByID writes an ENI record keyed "{accountID}.{eniID}" — the per-tap
// path's source of truth.
func seedENIByID(t *testing.T, r *metadataResolver, accountID, eniID string, rec eniRecord) {
	t.Helper()
	putJSON(t, r.eniKV, accountID+"."+eniID, rec)
}

// The per-tap path resolves an ENI from its ID alone and recovers the owning
// account from the bucket key, with no reverse-index lookup.
func TestResolveENIByID_Hit(t *testing.T) {
	r, _ := newTestResolver(t)
	seedENIByID(t, r, "111122223333", "eni-aaa", eniRecord{
		NetworkInterfaceId: "eni-aaa",
		SubnetId:           "subnet-1",
		VpcId:              testVPC,
		AvailabilityZone:   "ap-southeast-2a",
		PrivateIpAddress:   testIP,
		MacAddress:         "02:11:22:33:44:55",
		InstanceId:         "i-0123456789",
		PublicIpAddress:    "203.0.113.7",
		SecurityGroupIds:   []string{"sg-1", "sg-2"},
	})

	eni, err := r.resolveENIByID(context.Background(), "eni-aaa")
	require.NoError(t, err)
	require.NotNil(t, eni)
	assert.Equal(t, "eni-aaa", eni.eniID)
	assert.Equal(t, "111122223333", eni.accountID)
	assert.Equal(t, "i-0123456789", eni.instanceID)
	assert.Equal(t, testVPC, eni.vpcID)
	assert.Equal(t, "subnet-1", eni.subnetID)
	assert.Equal(t, testIP, eni.privateIP)
	assert.Equal(t, "203.0.113.7", eni.publicIP)
	assert.Equal(t, "02:11:22:33:44:55", eni.mac)
	assert.Equal(t, "ap-southeast-2a", eni.availabilityZone)
	assert.Equal(t, []string{"sg-1", "sg-2"}, eni.securityGroupIDs)
}

// The tap identity is unambiguous even when two tenants share a CIDR and IP: the
// ENI ID selects exactly one record, and its key supplies the right account.
func TestResolveENIByID_RecoversAccountAcrossTenants(t *testing.T) {
	r, _ := newTestResolver(t)
	seedENIByID(t, r, "111122223333", "eni-aaa", eniRecord{
		NetworkInterfaceId: "eni-aaa", VpcId: testVPC, PrivateIpAddress: testIP, InstanceId: "i-aaaa1111",
	})
	seedENIByID(t, r, "444455556666", "eni-bbb", eniRecord{
		NetworkInterfaceId: "eni-bbb", VpcId: testVPC, PrivateIpAddress: testIP, InstanceId: "i-bbbb2222",
	})

	a, err := r.resolveENIByID(context.Background(), "eni-aaa")
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, "111122223333", a.accountID)
	assert.Equal(t, "i-aaaa1111", a.instanceID)

	b, err := r.resolveENIByID(context.Background(), "eni-bbb")
	require.NoError(t, err)
	require.NotNil(t, b)
	assert.Equal(t, "444455556666", b.accountID)
	assert.Equal(t, "i-bbbb2222", b.instanceID)
}

// An unknown ENI ID (or empty bucket) is a miss, not an error — the caller maps
// it to a 404, matching the boot-time "not yet visible" posture.
func TestResolveENIByID_MissIsNilNil(t *testing.T) {
	r, _ := newTestResolver(t)
	eni, err := r.resolveENIByID(context.Background(), "eni-nope")
	require.NoError(t, err)
	assert.Nil(t, eni)

	seedENIByID(t, r, "111122223333", "eni-aaa", eniRecord{NetworkInterfaceId: "eni-aaa"})
	eni, err = r.resolveENIByID(context.Background(), "eni-other")
	require.NoError(t, err)
	assert.Nil(t, eni)
}

// An empty ENI ID short-circuits to a miss without scanning the bucket.
func TestResolveENIByID_EmptyIDIsNilNil(t *testing.T) {
	r, _ := newTestResolver(t)
	seedENIByID(t, r, "111122223333", "eni-aaa", eniRecord{NetworkInterfaceId: "eni-aaa"})
	eni, err := r.resolveENIByID(context.Background(), "")
	require.NoError(t, err)
	assert.Nil(t, eni)
}

func TestResolveENIByID_BadJSONErrors(t *testing.T) {
	r, _ := newTestResolver(t)
	_, err := r.eniKV.Put(t.Context(), "111122223333.eni-aaa", []byte("not json"))
	require.NoError(t, err)

	_, err = r.resolveENIByID(context.Background(), "eni-aaa")
	require.Error(t, err)
}

// End-to-end identity chain: tap-supplied ENI ID → ENI facts → instance facts,
// surfacing the account and the IAM instance-profile ARN with no srcIP lookup.
func TestResolveENIByID_ChainsToInstanceProfile(t *testing.T) {
	r, lookup := newTestResolver(t)
	lookup.facts = &instanceFacts{
		iamInstanceProfileArn: "arn:aws:iam::111122223333:instance-profile/app-profile",
	}
	seedENIByID(t, r, "111122223333", "eni-aaa", eniRecord{
		NetworkInterfaceId: "eni-aaa", InstanceId: "i-0123456789",
	})

	eni, err := r.resolveENIByID(context.Background(), "eni-aaa")
	require.NoError(t, err)
	require.NotNil(t, eni)
	assert.Equal(t, "111122223333", eni.accountID)

	inst, err := r.resolveInstance(context.Background(), eni)
	require.NoError(t, err)
	require.NotNil(t, inst)
	assert.Equal(t, "arn:aws:iam::111122223333:instance-profile/app-profile", inst.iamInstanceProfileArn)
	assert.Equal(t, 1, lookup.calls)
}

func TestResolveInstance_NoAttachedInstance(t *testing.T) {
	r, lookup := newTestResolver(t)
	inst, err := r.resolveInstance(context.Background(), &eniFacts{instanceID: ""})
	require.NoError(t, err)
	assert.Nil(t, inst)
	assert.Equal(t, 0, lookup.calls, "lookup must be skipped when no instance is attached")
}

func TestResolveInstance_DelegatesToLookup(t *testing.T) {
	r, lookup := newTestResolver(t)
	lookup.facts = &instanceFacts{instanceType: "t3.micro", imageID: "ami-1"}

	inst, err := r.resolveInstance(context.Background(), &eniFacts{accountID: "111122223333", instanceID: "i-1"})
	require.NoError(t, err)
	require.NotNil(t, inst)
	assert.Equal(t, "t3.micro", inst.instanceType)
	assert.Equal(t, 1, lookup.calls)
}

func TestResolveInstance_LookupErrorPropagates(t *testing.T) {
	r, lookup := newTestResolver(t)
	lookup.err = errors.New("fan-out failed")
	_, err := r.resolveInstance(context.Background(), &eniFacts{accountID: "111122223333", instanceID: "i-1"})
	require.Error(t, err)
}

// A nil SG bucket (not yet up at start) must degrade the endpoint to raw IDs
// rather than failing the request.
func TestResolveSGNames_NilBucketDegradesToIDs(t *testing.T) {
	r, _ := newTestResolver(t)
	r.sgKV = nil
	got := r.resolveSGNames(context.Background(), "111122223333", []string{"sg-1", "sg-2"})
	assert.Equal(t, []string{"sg-1", "sg-2"}, got)
}

// Names resolve where a record exists; a missing record or one with an empty
// name falls back to the ID, and order is preserved throughout.
func TestResolveSGNames_ResolvesAndFallsBack(t *testing.T) {
	r, _ := newTestResolver(t)
	putJSON(t, r.sgKV, "111122223333.sg-1", sgNameRecord{GroupName: "web-sg"})
	putJSON(t, r.sgKV, "111122223333.sg-3", sgNameRecord{GroupName: ""}) // empty → ID

	got := r.resolveSGNames(context.Background(), "111122223333", []string{"sg-1", "sg-2", "sg-3"})
	assert.Equal(t, []string{"web-sg", "sg-2", "sg-3"}, got)
}

func TestResolveSGNames_BadJSONFallsBackToID(t *testing.T) {
	r, _ := newTestResolver(t)
	_, err := r.sgKV.Put(t.Context(), "111122223333.sg-1", []byte("not json"))
	require.NoError(t, err)

	got := r.resolveSGNames(context.Background(), "111122223333", []string{"sg-1"})
	assert.Equal(t, []string{"sg-1"}, got)
}
