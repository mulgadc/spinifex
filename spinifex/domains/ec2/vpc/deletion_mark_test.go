package vpc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failDeleteKV fails Delete while fail is set, and passes every other call.
type failDeleteKV struct {
	jetstream.KeyValue

	fail atomic.Bool
}

func (kv *failDeleteKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if kv.fail.Load() {
		return errors.New("simulated KV delete failure")
	}
	return kv.KeyValue.Delete(ctx, key, opts...)
}

// beforeKV runs a hook once before the first Update or Delete, so a test can
// change the record between the delete's read and its write.
type beforeKV struct {
	jetstream.KeyValue

	once         sync.Once
	beforeUpdate func(ctx context.Context, key string)
	beforeDelete func(ctx context.Context, key string)
}

func (kv *beforeKV) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	if kv.beforeUpdate != nil {
		kv.once.Do(func() { kv.beforeUpdate(ctx, key) })
	}
	return kv.KeyValue.Update(ctx, key, value, last)
}

func (kv *beforeKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if kv.beforeDelete != nil {
		kv.once.Do(func() { kv.beforeDelete(ctx, key) })
	}
	return kv.KeyValue.Delete(ctx, key, opts...)
}

// bump rewrites the stored bytes unchanged, which moves the revision.
func bump(t *testing.T, kv jetstream.KeyValue, key string) {
	t.Helper()
	entry, err := kv.Get(context.Background(), key)
	require.NoError(t, err)
	_, err = kv.Put(context.Background(), key, entry.Value())
	require.NoError(t, err)
}

func marked(t *testing.T, kv jetstream.KeyValue, id string) bool {
	t.Helper()
	entry, err := kv.Get(t.Context(), kvstore.AccountKey(testAccountID, id))
	require.NoError(t, err)
	var rec struct {
		Deleting bool `json:"deleting"`
	}
	require.NoError(t, json.Unmarshal(entry.Value(), &rec))
	return rec.Deleting
}

func requireGone(t *testing.T, kv jetstream.KeyValue, id string) {
	t.Helper()
	_, err := kv.Get(t.Context(), kvstore.AccountKey(testAccountID, id))
	require.ErrorIs(t, err, jetstream.ErrKeyNotFound)
}

func deleteVpc(svc *VPCServiceImpl, id string) error {
	_, err := svc.DeleteVpc(context.Background(), &ec2.DeleteVpcInput{VpcId: aws.String(id)}, testAccountID)
	return err
}

func deleteSubnet(svc *VPCServiceImpl, id string) error {
	_, err := svc.DeleteSubnet(context.Background(), &ec2.DeleteSubnetInput{SubnetId: aws.String(id)}, testAccountID)
	return err
}

func TestCreateVpc_RefusesLinkLocalCIDR(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	for _, cidr := range []string{"169.254.0.0/16", "169.254.10.0/24"} {
		_, err := svc.CreateVpc(context.Background(), &ec2.CreateVpcInput{CidrBlock: aws.String(cidr)}, testAccountID)
		testutil.RequireAWSError(t, err, awserrors.ErrorInvalidVpcRange, "The CIDR '"+cidr+"' is invalid.")
	}
	createTestVPC(t, svc, "100.64.0.0/16")
}

// A failure after the default security group cascade leaves the mark, and
// the VPC stays visible; a retry finishes the delete.
func TestDeleteVpc_FailureAfterSGCascadeKeepsMark(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.70.0.0/16")
	sgID, err := svc.findDefaultSGForVPC(t.Context(), testAccountID, vpcID)
	require.NoError(t, err)
	require.NotEmpty(t, sgID)
	rtb := &failDeleteKV{KeyValue: svc.rtbKV}
	rtb.fail.Store(true)
	svc.rtbKV = rtb

	testutil.RequireAWSCode(t, deleteVpc(svc, vpcID), awserrors.ErrorServerInternal)
	assert.True(t, marked(t, svc.vpcKV, vpcID), "the mark stays once the cascade may have run")
	requireGone(t, svc.sgKV, sgID)
	desc, err := svc.DescribeVpcs(context.Background(), &ec2.DescribeVpcsInput{VpcIds: []*string{aws.String(vpcID)}}, testAccountID)
	require.NoError(t, err)
	require.Len(t, desc.Vpcs, 1, "a marked VPC stays visible")

	rtb.fail.Store(false)
	require.NoError(t, deleteVpc(svc, vpcID))
	requireGone(t, svc.vpcKV, vpcID)
}

// A failure at the record delete, after the main route table reap, leaves
// the mark. A subnet created then makes the retry answer DependencyViolation
// and keep the mark.
func TestDeleteVpc_FailureAtRecordDeleteKeepsMark(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.71.0.0/16")
	rtbID, err := svc.findMainRouteTableID(t.Context(), testAccountID, vpcID)
	require.NoError(t, err)
	require.NotEmpty(t, rtbID)
	vpcs := &failDeleteKV{KeyValue: svc.vpcKV}
	vpcs.fail.Store(true)
	svc.vpcKV = vpcs

	testutil.RequireAWSCode(t, deleteVpc(svc, vpcID), awserrors.ErrorServerInternal)
	assert.True(t, marked(t, svc.vpcKV, vpcID))
	requireGone(t, svc.rtbKV, rtbID)
	vpcs.fail.Store(false)

	subnetID := createTestSubnet(t, svc, vpcID, "10.71.1.0/24")
	testutil.RequireAWSError(t, deleteVpc(svc, vpcID), awserrors.ErrorDependencyViolation,
		"The vpc '"+vpcID+"' has dependencies and cannot be deleted.")
	assert.True(t, marked(t, svc.vpcKV, vpcID), "a retry that finds a dependency keeps the mark")

	require.NoError(t, deleteSubnet(svc, subnetID))
	require.NoError(t, deleteVpc(svc, vpcID))
	requireGone(t, svc.vpcKV, vpcID)
}

// The record delete runs at the mark's revision: a write after the mark makes
// it fail, and the record and its mark stay for the later delete.
func TestDeleteVpc_RecordDeleteIsAtMarkRevision(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.72.0.0/16")
	raw := svc.vpcKV
	svc.vpcKV = &beforeKV{KeyValue: raw, beforeDelete: func(_ context.Context, key string) { bump(t, raw, key) }}

	testutil.RequireAWSCode(t, deleteVpc(svc, vpcID), awserrors.ErrorServerInternal)
	assert.True(t, marked(t, raw, vpcID))

	require.NoError(t, deleteVpc(svc, vpcID), "the later delete finishes")
	requireGone(t, raw, vpcID)
}

// A rewrite between the checks and the mark makes the mark lose; the delete
// reads again and completes.
func TestDeleteVpc_MarkLosesRaceAndRetries(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.73.0.0/16")
	raw := svc.vpcKV
	var raced atomic.Bool
	svc.vpcKV = &beforeKV{KeyValue: raw, beforeUpdate: func(_ context.Context, key string) {
		raced.Store(true)
		bump(t, raw, key)
	}}

	require.NoError(t, deleteVpc(svc, vpcID))
	assert.True(t, raced.Load())
	requireGone(t, raw, vpcID)
}

// When the racing write adds a dependency, the re-read answers
// DependencyViolation and no mark is written.
func TestDeleteVpc_MarkLosesRaceToNewDependency(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.74.0.0/16")
	raw := svc.vpcKV
	svc.vpcKV = &beforeKV{KeyValue: raw, beforeUpdate: func(_ context.Context, key string) {
		createTestSubnet(t, svc, vpcID, "10.74.1.0/24")
		bump(t, raw, key)
	}}

	testutil.RequireAWSError(t, deleteVpc(svc, vpcID), awserrors.ErrorDependencyViolation,
		"The vpc '"+vpcID+"' has dependencies and cannot be deleted.")
	assert.False(t, marked(t, raw, vpcID))
}

// A failure at the record delete after the association clear keeps the mark;
// a retry finishes.
func TestDeleteSubnet_FailureAfterAssociationClearKeepsMark(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.75.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.75.1.0/24")
	rtbKey := kvstore.AccountKey(testAccountID, "rtb-0assoc")
	_, err := svc.rtbKV.Put(t.Context(), rtbKey, []byte(`{"route_table_id":"rtb-0assoc","vpc_id":"`+vpcID+
		`","associations":[{"association_id":"rtbassoc-1","subnet_id":"`+subnetID+`","main":false}]}`))
	require.NoError(t, err)
	subnets := &failDeleteKV{KeyValue: svc.subnetKV}
	subnets.fail.Store(true)
	svc.subnetKV = subnets

	testutil.RequireAWSCode(t, deleteSubnet(svc, subnetID), awserrors.ErrorServerInternal)
	assert.True(t, marked(t, svc.subnetKV, subnetID))
	entry, err := svc.rtbKV.Get(t.Context(), rtbKey)
	require.NoError(t, err)
	var rtb struct {
		Associations []json.RawMessage `json:"associations"`
	}
	require.NoError(t, json.Unmarshal(entry.Value(), &rtb))
	assert.Empty(t, rtb.Associations, "the association clear ran before the failure")

	subnets.fail.Store(false)
	require.NoError(t, deleteSubnet(svc, subnetID))
	requireGone(t, svc.subnetKV, subnetID)
}

func TestDeleteSubnet_OwnedDependencyRefusesBeforeMark(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	vpcID := createTestVPC(t, svc, "10.76.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.76.1.0/24")
	var asked string
	svc.SetSubnetDependencyCheck("transit-gateway-attachment", func(_ context.Context, account, id string) (bool, error) {
		asked = account + " " + id
		return true, nil
	})

	testutil.RequireAWSError(t, deleteSubnet(svc, subnetID), awserrors.ErrorDependencyViolation,
		"The subnet '"+subnetID+"' has dependencies and cannot be deleted.")
	assert.Equal(t, testAccountID+" "+subnetID, asked)
	assert.False(t, marked(t, svc.subnetKV, subnetID))

	svc.SetSubnetDependencyCheck("transit-gateway-attachment", func(context.Context, string, string) (bool, error) {
		return false, errors.New("bucket unavailable")
	})
	testutil.RequireAWSCode(t, deleteSubnet(svc, subnetID), awserrors.ErrorServerInternal)
	assert.False(t, marked(t, svc.subnetKV, subnetID), "a failed check fails closed")
}

func TestMarkDeleting_KeepsUnknownFields(t *testing.T) {
	t.Parallel()
	_, _, js := testutil.StartTestJetStream(t)
	kv := testutil.SeedKV(t, js, "mark-test", map[string][]byte{
		"a.vpc-1": []byte(`{"vpc_id":"vpc-1","future_field":{"n":1}}`),
	})
	entry, err := kv.Get(t.Context(), "a.vpc-1")
	require.NoError(t, err)

	rev, err := markDeleting(t.Context(), kv, "a.vpc-1", entry)
	require.NoError(t, err)
	after, err := kv.Get(t.Context(), "a.vpc-1")
	require.NoError(t, err)
	assert.Equal(t, after.Revision(), rev)
	assert.JSONEq(t, `{"vpc_id":"vpc-1","future_field":{"n":1},"deleting":true}`, string(after.Value()))

	_, err = markDeleting(t.Context(), kv, "a.vpc-1", entry)
	require.ErrorIs(t, err, ErrParentChanged, "a mark at a stale revision loses")
}

func TestCountDeletionMarks(t *testing.T) {
	t.Parallel()
	_, _, empty := testutil.StartTestJetStream(t)
	n, err := CountDeletionMarks(t.Context(), empty)
	require.NoError(t, err)
	assert.Zero(t, n, "missing buckets hold no mark")

	svc, nc := setupTestVPCServiceWithNC(t)
	js := testutil.NewJetStream(t, nc)
	vpcID := createTestVPC(t, svc, "10.77.0.0/16")
	createTestVPC(t, svc, "10.78.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.77.1.0/24")
	n, err = CountDeletionMarks(t.Context(), js)
	require.NoError(t, err)
	assert.Zero(t, n)

	for kv, id := range map[jetstream.KeyValue]string{svc.vpcKV: vpcID, svc.subnetKV: subnetID} {
		key := kvstore.AccountKey(testAccountID, id)
		entry, err := kv.Get(t.Context(), key)
		require.NoError(t, err)
		_, err = markDeleting(t.Context(), kv, key, entry)
		require.NoError(t, err)
	}
	n, err = CountDeletionMarks(t.Context(), js)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

// olderVPCRecord is VPCRecord as a daemon before the deletion mark encodes it.
type olderVPCRecord struct {
	VpcId                            string            `json:"vpc_id"`
	CidrBlock                        string            `json:"cidr_block"`
	State                            string            `json:"state"`
	IsDefault                        bool              `json:"is_default"`
	VNI                              int64             `json:"vni"`
	AZ                               string            `json:"az,omitempty"`
	EnableDnsHostnames               bool              `json:"enable_dns_hostnames"`
	EnableDnsSupport                 bool              `json:"enable_dns_support"`
	EnableNetworkAddressUsageMetrics bool              `json:"enable_network_address_usage_metrics"`
	Tags                             map[string]string `json:"tags"`
	CreatedAt                        time.Time         `json:"created_at"`
}

// An older writer's rewrite drops the mark, so the drain must count the mark
// before any older daemon returns.
func TestDeletionMark_OlderWriterDropsIt(t *testing.T) {
	t.Parallel()
	svc, nc := setupTestVPCServiceWithNC(t)
	js := testutil.NewJetStream(t, nc)
	vpcID := createTestVPC(t, svc, "10.79.0.0/16")
	key := kvstore.AccountKey(testAccountID, vpcID)
	entry, err := svc.vpcKV.Get(t.Context(), key)
	require.NoError(t, err)
	_, err = markDeleting(t.Context(), svc.vpcKV, key, entry)
	require.NoError(t, err)
	n, err := CountDeletionMarks(t.Context(), js)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	entry, err = svc.vpcKV.Get(t.Context(), key)
	require.NoError(t, err)
	var older olderVPCRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &older))
	data, err := json.Marshal(older)
	require.NoError(t, err)
	_, err = svc.vpcKV.Update(t.Context(), key, data, entry.Revision())
	require.NoError(t, err)

	assert.False(t, marked(t, svc.vpcKV, vpcID))
	n, err = CountDeletionMarks(t.Context(), js)
	require.NoError(t, err)
	assert.Zero(t, n)
}

// markGate is a feature-level admission whose answer a test sets.
type markGate struct {
	ok      bool
	err     error
	holding atomic.Int32
}

func (g *markGate) admit(context.Context) (func(), bool, error) {
	g.holding.Add(1)
	var once sync.Once
	return func() { once.Do(func() { g.holding.Add(-1) }) }, g.ok, g.err
}

// Below the level a failed delete leaves no mark, so a rollback that counted
// zero marks stays true; the delete held the admission until it ended.
func TestDeleteVpc_BelowLevelWritesNoMark(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	g := &markGate{}
	svc.SetDeletionMarkAdmission(g.admit)
	vpcID := createTestVPC(t, svc, "10.76.0.0/16")
	vpcs := &failDeleteKV{KeyValue: svc.vpcKV}
	vpcs.fail.Store(true)
	svc.vpcKV = vpcs

	testutil.RequireAWSCode(t, deleteVpc(svc, vpcID), awserrors.ErrorServerInternal)
	assert.False(t, marked(t, svc.vpcKV, vpcID))
	assert.Zero(t, g.holding.Load())

	g.ok = true
	testutil.RequireAWSCode(t, deleteVpc(svc, vpcID), awserrors.ErrorServerInternal)
	assert.True(t, marked(t, svc.vpcKV, vpcID), "at the level the mark is written")
	assert.Zero(t, g.holding.Load())
}

func TestDeleteSubnet_BelowLevelWritesNoMark(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	g := &markGate{}
	svc.SetDeletionMarkAdmission(g.admit)
	vpcID := createTestVPC(t, svc, "10.77.0.0/16")
	subnetID := createTestSubnet(t, svc, vpcID, "10.77.1.0/24")
	subnets := &failDeleteKV{KeyValue: svc.subnetKV}
	subnets.fail.Store(true)
	svc.subnetKV = subnets

	testutil.RequireAWSCode(t, deleteSubnet(svc, subnetID), awserrors.ErrorServerInternal)
	assert.False(t, marked(t, svc.subnetKV, subnetID))
	assert.Zero(t, g.holding.Load())

	subnets.fail.Store(false)
	require.NoError(t, deleteSubnet(svc, subnetID))
	requireGone(t, svc.subnetKV, subnetID)
}

// An unreadable level fails the delete before it changes anything.
func TestDeleteVpc_UnreadableLevelRefuses(t *testing.T) {
	t.Parallel()
	svc := setupTestVPCService(t)
	g := &markGate{err: errors.New("level store unreachable")}
	svc.SetDeletionMarkAdmission(g.admit)
	vpcID := createTestVPC(t, svc, "10.78.0.0/16")

	testutil.RequireAWSCode(t, deleteVpc(svc, vpcID), awserrors.ErrorServerInternal)
	assert.False(t, marked(t, svc.vpcKV, vpcID))
	assert.Zero(t, g.holding.Load())
	_, err := svc.vpcKV.Get(t.Context(), kvstore.AccountKey(testAccountID, vpcID))
	require.NoError(t, err, "the VPC stays")
}
