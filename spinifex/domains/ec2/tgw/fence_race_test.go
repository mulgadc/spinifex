package tgw

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	raceRouteTableBucket = "spinifex-vpc-route-tables"
	holdWait             = 10 * time.Second
)

// hold stops the first matching KV call, before or after it runs, until the
// test frees it.
type hold struct {
	op      string
	after   bool
	match   func(key string) bool
	reached chan struct{}
	release chan struct{}
}

func (h *hold) wait(t *testing.T) {
	t.Helper()
	select {
	case <-h.reached:
	case <-time.After(holdWait):
		t.Fatalf("no %s call reached the hold", h.op)
	}
}

func (h *hold) free() { close(h.release) }

// holdKV passes every call through, and stops the calls a test armed.
type holdKV struct {
	jetstream.KeyValue

	mu    sync.Mutex
	holds []*hold
}

func (kv *holdKV) arm(op string, after bool, match func(string) bool) *hold {
	h := &hold{op: op, after: after, match: match, reached: make(chan struct{}), release: make(chan struct{})}
	kv.mu.Lock()
	kv.holds = append(kv.holds, h)
	kv.mu.Unlock()
	return h
}

func (kv *holdKV) pass(ctx context.Context, op, key string, after bool) error {
	kv.mu.Lock()
	var h *hold
	for i, c := range kv.holds {
		if c.op == op && c.after == after && c.match(key) {
			h = c
			kv.holds = append(kv.holds[:i], kv.holds[i+1:]...)
			break
		}
	}
	kv.mu.Unlock()
	if h == nil {
		return nil
	}
	close(h.reached)
	select {
	case <-h.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (kv *holdKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if err := kv.pass(ctx, "Get", key, false); err != nil {
		return nil, err
	}
	return kv.KeyValue.Get(ctx, key)
}

func (kv *holdKV) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	if err := kv.pass(ctx, "Put", key, false); err != nil {
		return 0, err
	}
	return kv.KeyValue.Put(ctx, key, value)
}

func (kv *holdKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if err := kv.pass(ctx, "Create", key, false); err != nil {
		return 0, err
	}
	return kv.KeyValue.Create(ctx, key, value, opts...)
}

func (kv *holdKV) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	if err := kv.pass(ctx, "Update", key, false); err != nil {
		return 0, err
	}
	rev, err := kv.KeyValue.Update(ctx, key, value, last)
	if perr := kv.pass(ctx, "Update", key, true); perr != nil {
		return rev, perr
	}
	return rev, err
}

// heldCheck stops a dependency check after it answers, once armed, so the
// delete is held between its index check and its marked write.
type heldCheck struct {
	mu   sync.Mutex
	next *hold
}

func (c *heldCheck) arm() *hold {
	h := &hold{op: "dependency check", reached: make(chan struct{}), release: make(chan struct{})}
	c.mu.Lock()
	c.next = h
	c.mu.Unlock()
	return h
}

func (c *heldCheck) wrap(check ec2vpc.DependencyCheck) ec2vpc.DependencyCheck {
	return func(ctx context.Context, accountID, id string) (bool, error) {
		blocks, err := check(ctx, accountID, id)
		c.mu.Lock()
		h := c.next
		c.next = nil
		c.mu.Unlock()
		if h != nil {
			close(h.reached)
			select {
			case <-h.release:
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		return blocks, err
	}
}

// raceFixture runs admission and the parent deletes as separate services over
// the same buckets, so each side's KV calls can be held on their own.
type raceFixture struct {
	t        *testing.T
	nc       *nats.Conn
	js       jetstream.JetStream
	vpcs     *ec2vpc.VPCServiceImpl
	admit    *ServiceImpl
	del      *ServiceImpl
	atts     *holdKV // admission's attachment bucket
	vpcKV    *holdKV // admission's view of the VPC bucket
	delTGWs  *holdKV // the Transit Gateway delete's bucket
	vpcCheck *heldCheck
	subCheck *heldCheck

	tgwID, vpcID, subnetID string
}

func newRaceFixture(t *testing.T) *raceFixture {
	t.Helper()
	ctx := t.Context()
	_, nc, js := testutil.StartTestJetStream(t)
	testutil.StubVpcdSGResponder(t, nc)
	vpcs, err := ec2vpc.NewVPCServiceImplWithNATS(ctx, nil, nc)
	require.NoError(t, err)
	base, err := OpenStore(ctx, js)
	require.NoError(t, err)
	vpcKV, err := js.KeyValue(ctx, ec2vpc.KVBucketVPCs)
	require.NoError(t, err)
	subnetKV, err := js.KeyValue(ctx, ec2vpc.KVBucketSubnets)
	require.NoError(t, err)
	c := &clock{now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}

	f := &raceFixture{
		t: t, nc: nc, js: js, vpcs: vpcs,
		atts:     &holdKV{KeyValue: base.atts},
		vpcKV:    &holdKV{KeyValue: vpcKV},
		delTGWs:  &holdKV{KeyValue: base.tgws},
		vpcCheck: &heldCheck{},
		subCheck: &heldCheck{},
	}
	service := func(store *Store) *ServiceImpl {
		store.now = c.Now
		svc := NewServiceImpl(store, "ap-southeast-2")
		g := &gate{ok: true}
		svc.SetActivation(g.admit, g.offered)
		svc.SetAccountStatus(&fakeAccounts{active: true})
		return svc
	}
	f.admit = service(NewStore(base.tgws, f.atts, base.term, ec2vpc.NewParents(f.vpcKV, subnetKV)))
	f.del = service(NewStore(f.delTGWs, base.atts, base.term, ec2vpc.NewParents(vpcKV, subnetKV)))
	vpcs.SetDependencyCheck("transit-gateway-attachment", f.vpcCheck.wrap(f.del.VpcBlocksDeletion))
	vpcs.SetSubnetDependencyCheck("transit-gateway-attachment", f.subCheck.wrap(f.del.SubnetBlocksDeletion))

	out, err := f.admit.CreateTransitGateway(ctx, &ec2.CreateTransitGatewayInput{}, testAccount)
	require.NoError(t, err)
	f.tgwID = aws.StringValue(out.TransitGateway.TransitGatewayId)
	require.NoError(t, f.admit.MarkTransitGatewayAvailable(ctx, testAccount, f.tgwID, 1))
	vpcOut, err := vpcs.CreateVpc(ctx, &ec2.CreateVpcInput{CidrBlock: aws.String("10.70.0.0/16")}, testAccount)
	require.NoError(t, err)
	f.vpcID = aws.StringValue(vpcOut.Vpc.VpcId)
	subOut, err := vpcs.CreateSubnet(ctx, &ec2.CreateSubnetInput{VpcId: vpcOut.Vpc.VpcId, CidrBlock: aws.String("10.70.1.0/24")}, testAccount)
	require.NoError(t, err)
	f.subnetID = aws.StringValue(subOut.Subnet.SubnetId)
	return f
}

// attachAsync starts the public attachment create and returns its result.
func (f *raceFixture) attachAsync() <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := f.admit.CreateTransitGatewayVpcAttachment(f.t.Context(), &ec2.CreateTransitGatewayVpcAttachmentInput{
			TransitGatewayId: aws.String(f.tgwID), VpcId: aws.String(f.vpcID), SubnetIds: aws.StringSlice([]string{f.subnetID}),
		}, testAccount)
		done <- err
	}()
	return done
}

func (f *raceFixture) result(ch <-chan error) error {
	f.t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(holdWait):
		f.t.Fatal("call did not return")
		return nil
	}
}

func (f *raceFixture) deleteTGW() error {
	_, err := f.del.DeleteTransitGateway(f.t.Context(), &ec2.DeleteTransitGatewayInput{TransitGatewayId: aws.String(f.tgwID)}, testAccount)
	return err
}

func (f *raceFixture) deleteVPC() error {
	_, err := f.vpcs.DeleteVpc(f.t.Context(), &ec2.DeleteVpcInput{VpcId: aws.String(f.vpcID)}, testAccount)
	return err
}

func (f *raceFixture) deleteSubnet() error {
	_, err := f.vpcs.DeleteSubnet(f.t.Context(), &ec2.DeleteSubnetInput{SubnetId: aws.String(f.subnetID)}, testAccount)
	return err
}

// async runs call in the background.
func async(call func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- call() }()
	return done
}

func isPairClaim(key string) bool { return strings.HasPrefix(key, "pair.") }

func isAttachmentRecord(key string) bool {
	return strings.HasPrefix(key, testAccount+".tgw-attach-")
}

// liveAttachments counts attachment records in the live bucket.
func (f *raceFixture) liveAttachments() int {
	n := 0
	for _, k := range keys(f.t, f.atts.KeyValue) {
		if isAttachmentRecord(k) {
			n++
		}
	}
	return n
}

func (f *raceFixture) tgwState() string {
	f.t.Helper()
	rec, _, err := f.admit.transitGateway(f.t.Context(), testAccount, f.tgwID)
	require.NoError(f.t, err)
	require.NotNil(f.t, rec)
	return rec.State
}

// recordsNaming lists the records in bucket whose field equals value.
func (f *raceFixture) recordsNaming(bucket, field, value string) int {
	f.t.Helper()
	kv, err := f.js.KeyValue(f.t.Context(), bucket)
	require.NoError(f.t, err)
	n := 0
	for _, k := range keys(f.t, kv) {
		entry, err := kv.Get(f.t.Context(), k)
		require.NoError(f.t, err)
		var rec map[string]any
		require.NoError(f.t, json.Unmarshal(entry.Value(), &rec))
		if rec[field] == value {
			n++
		}
	}
	return n
}

func (f *raceFixture) parentMarked(bucket, id string) bool {
	f.t.Helper()
	kv, err := f.js.KeyValue(f.t.Context(), bucket)
	require.NoError(f.t, err)
	entry, err := kv.Get(f.t.Context(), kvstore.AccountKey(testAccount, id))
	require.NoError(f.t, err)
	var rec struct {
		Deleting bool `json:"deleting"`
	}
	require.NoError(f.t, json.Unmarshal(entry.Value(), &rec))
	return rec.Deleting
}

// putAssociatedRouteTable writes a route table with an association of the
// subnet, which DeleteSubnet clears after its mark.
func (f *raceFixture) putAssociatedRouteTable(id string) {
	f.t.Helper()
	data, err := json.Marshal(map[string]any{
		"route_table_id": id,
		"vpc_id":         f.vpcID,
		"associations":   []map[string]any{{"association_id": "rtbassoc-0123456789abcdef0", "subnet_id": f.subnetID, "main": false}},
	})
	require.NoError(f.t, err)
	kv, err := f.js.KeyValue(f.t.Context(), raceRouteTableBucket)
	require.NoError(f.t, err)
	_, err = kv.Put(f.t.Context(), kvstore.AccountKey(testAccount, id), data)
	require.NoError(f.t, err)
}

func (f *raceFixture) associationKept(id string) bool {
	f.t.Helper()
	kv, err := f.js.KeyValue(f.t.Context(), raceRouteTableBucket)
	require.NoError(f.t, err)
	entry, err := kv.Get(f.t.Context(), kvstore.AccountKey(testAccount, id))
	require.NoError(f.t, err)
	var rec struct {
		Associations []struct {
			SubnetID string `json:"subnet_id"`
		} `json:"associations"`
	}
	require.NoError(f.t, json.Unmarshal(entry.Value(), &rec))
	return len(rec.Associations) == 1 && rec.Associations[0].SubnetID == f.subnetID
}

// removeSubnetBeforeIndexes holds admission after it judged its parents,
// deletes the subnet, and returns the running admission. The VPC then has no
// subnet, so only the attachment can block DeleteVpc.
func (f *raceFixture) removeSubnetBeforeIndexes() (<-chan error, *hold) {
	f.t.Helper()
	claim := f.atts.arm("Create", false, isPairClaim)
	admission := f.attachAsync()
	claim.wait(f.t)
	require.NoError(f.t, f.deleteSubnet())
	return admission, claim
}

func TestFenceRace_TransitGateway(t *testing.T) {
	t.Parallel()

	t.Run("index keys before the delete's check", func(t *testing.T) {
		t.Parallel()
		f := newRaceFixture(t)
		record := f.atts.arm("Create", false, isAttachmentRecord)
		admission := f.attachAsync()
		record.wait(t)

		msg := requireCode(t, f.deleteTGW(), awserrors.ErrorIncorrectState)
		assert.Contains(t, msg, "non-deleted VPC Attachments")
		record.free()
		require.NoError(t, f.result(admission))
		assert.Equal(t, StateAvailable, f.tgwState())
		assert.Equal(t, 1, f.liveAttachments())
	})

	t.Run("admission rewrites between the check and the marked write", func(t *testing.T) {
		t.Parallel()
		f := newRaceFixture(t)
		mark := f.delTGWs.arm("Update", false, func(k string) bool { return k == recordKey(testAccount, f.tgwID) })
		deletion := async(f.deleteTGW)
		mark.wait(t)
		require.NoError(t, f.result(f.attachAsync()))

		mark.free()
		requireCode(t, f.result(deletion), awserrors.ErrorIncorrectState)
		assert.Equal(t, StateAvailable, f.tgwState(), "the retried check refuses before any write")
		assert.Equal(t, 1, f.liveAttachments())
	})

	t.Run("marked write before admission's rewrite", func(t *testing.T) {
		t.Parallel()
		f := newRaceFixture(t)
		claim := f.atts.arm("Create", false, isPairClaim)
		admission := f.attachAsync()
		claim.wait(t)
		require.NoError(t, f.deleteTGW())

		claim.free()
		requireCode(t, f.result(admission), awserrors.ErrorIncorrectState)
		assert.Equal(t, StateDeleting, f.tgwState())
		assert.Empty(t, keys(t, f.atts.KeyValue), "no record, index key or pair claim remains")
	})
}

func TestFenceRace_Vpc(t *testing.T) {
	t.Parallel()

	t.Run("index keys before the delete's check", func(t *testing.T) {
		t.Parallel()
		f := newRaceFixture(t)
		record := f.atts.arm("Create", false, isAttachmentRecord)
		admission, claim := f.removeSubnetBeforeIndexes()
		claim.free()
		record.wait(t)

		requireCode(t, f.deleteVPC(), awserrors.ErrorDependencyViolation)
		assert.False(t, f.parentMarked(ec2vpc.KVBucketVPCs, f.vpcID), "a refused delete writes no mark")
		record.free()
		requireCode(t, f.result(admission), awserrors.ErrorInvalidSubnetIDNotFound)
		assert.Empty(t, keys(t, f.atts.KeyValue))
	})

	t.Run("admission rewrites between the check and the marked write", func(t *testing.T) {
		t.Parallel()
		f := newRaceFixture(t)
		touched := f.vpcKV.arm("Update", true, func(k string) bool { return k == kvstore.AccountKey(testAccount, f.vpcID) })
		admission, claim := f.removeSubnetBeforeIndexes()
		check := f.vpcCheck.arm()
		deletion := async(f.deleteVPC)
		check.wait(t)
		claim.free()
		touched.wait(t)

		check.free()
		requireCode(t, f.result(deletion), awserrors.ErrorDependencyViolation)
		sg, err := f.vpcs.FindDefaultSGForVPC(testAccount, f.vpcID)
		require.NoError(t, err)
		assert.NotEmpty(t, sg, "the default security group is not cascaded")
		assert.Equal(t, 1, f.recordsNaming(raceRouteTableBucket, "vpc_id", f.vpcID), "the main route table is not reaped")
		assert.False(t, f.parentMarked(ec2vpc.KVBucketVPCs, f.vpcID))

		touched.free()
		requireCode(t, f.result(admission), awserrors.ErrorInvalidSubnetIDNotFound)
		assert.Empty(t, keys(t, f.atts.KeyValue))
	})

	t.Run("marked write before admission's rewrite", func(t *testing.T) {
		t.Parallel()
		f := newRaceFixture(t)
		admission, claim := f.removeSubnetBeforeIndexes()
		// The cascade fails after the mark, so the marked record stays.
		testutil.OverrideVpcdStubResponse(f.nc, "vpc.delete-sg", []byte(`{"success":false,"error":"held"}`))
		require.Error(t, f.deleteVPC())
		require.True(t, f.parentMarked(ec2vpc.KVBucketVPCs, f.vpcID))

		claim.free()
		requireCode(t, f.result(admission), awserrors.ErrorInvalidVpcIDNotFound)
		assert.Empty(t, keys(t, f.atts.KeyValue), "no record, index key or pair claim remains")
	})
}

func TestFenceRace_Subnet(t *testing.T) {
	t.Parallel()

	t.Run("index keys before the delete's check", func(t *testing.T) {
		t.Parallel()
		f := newRaceFixture(t)
		record := f.atts.arm("Create", false, isAttachmentRecord)
		admission := f.attachAsync()
		record.wait(t)

		requireCode(t, f.deleteSubnet(), awserrors.ErrorDependencyViolation)
		assert.False(t, f.parentMarked(ec2vpc.KVBucketSubnets, f.subnetID))
		record.free()
		require.NoError(t, f.result(admission))
		assert.Equal(t, 1, f.liveAttachments())
	})

	t.Run("admission rewrites between the check and the marked write", func(t *testing.T) {
		t.Parallel()
		f := newRaceFixture(t)
		f.putAssociatedRouteTable("rtb-0123456789abcdef0")
		check := f.subCheck.arm()
		deletion := async(f.deleteSubnet)
		check.wait(t)
		require.NoError(t, f.result(f.attachAsync()))

		check.free()
		requireCode(t, f.result(deletion), awserrors.ErrorDependencyViolation)
		assert.True(t, f.associationKept("rtb-0123456789abcdef0"), "the association clear does not run")
		assert.False(t, f.parentMarked(ec2vpc.KVBucketSubnets, f.subnetID))
		assert.Equal(t, 1, f.liveAttachments())
	})

	t.Run("marked write before admission's rewrite", func(t *testing.T) {
		t.Parallel()
		f := newRaceFixture(t)
		claim := f.atts.arm("Create", false, isPairClaim)
		admission := f.attachAsync()
		claim.wait(t)
		// An undecodable route table fails the association clear after the
		// mark, so the marked record stays.
		kv, err := f.js.KeyValue(t.Context(), raceRouteTableBucket)
		require.NoError(t, err)
		_, err = kv.Put(t.Context(), kvstore.AccountKey(testAccount, "rtb-0123456789abcdef1"), []byte("{"))
		require.NoError(t, err)
		require.Error(t, f.deleteSubnet())
		require.True(t, f.parentMarked(ec2vpc.KVBucketSubnets, f.subnetID))

		claim.free()
		requireCode(t, f.result(admission), awserrors.ErrorInvalidSubnetIDNotFound)
		assert.Empty(t, keys(t, f.atts.KeyValue), "no record, index key or pair claim remains")
	})
}
