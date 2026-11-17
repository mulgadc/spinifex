package vpcpeering

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

const (
	testAccount = "123456789012"
	testRegion  = "ap-southeast-2"
	vpcA        = "vpc-0a000000000000001"
	vpcB        = "vpc-0b000000000000002"
	vpcC        = "vpc-0c000000000000003"
)

// fakeClock is a settable clock shared by the store under test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fixture struct {
	svc   *ServiceImpl
	js    jetstream.JetStream
	clock *fakeClock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	store, err := OpenStore(t.Context(), js)
	require.NoError(t, err)
	clock := &fakeClock{t: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}
	store.now = clock.now
	f := &fixture{svc: NewServiceImpl(store, testRegion), js: js, clock: clock}
	f.putVPC(t, testAccount, vpcA, "10.1.0.0/16")
	f.putVPC(t, testAccount, vpcB, "10.2.0.0/16")
	f.putVPC(t, testAccount, vpcC, "10.3.0.0/16")
	return f
}

func (f *fixture) putVPC(t *testing.T, account, id, cidr string) {
	t.Helper()
	data, err := json.Marshal(ec2vpc.VPCRecord{VpcId: id, CidrBlock: cidr, State: "available", EnableDnsSupport: true})
	require.NoError(t, err)
	_, err = f.svc.vpcKV.Put(t.Context(), kvstore.AccountKey(account, id), data)
	require.NoError(t, err)
}

func (f *fixture) updateVPC(t *testing.T, account, id string, mut func(*ec2vpc.VPCRecord)) {
	t.Helper()
	_, err := kvstore.Update(t.Context(), f.svc.vpcKV, kvstore.AccountKey(account, id), kvstore.CASConfig{}, func(r *ec2vpc.VPCRecord) (bool, error) {
		mut(r)
		return true, nil
	})
	require.NoError(t, err)
}

func (f *fixture) deleteVPC(t *testing.T, account, id string) {
	t.Helper()
	require.NoError(t, f.svc.vpcKV.Delete(t.Context(), kvstore.AccountKey(account, id)))
}

func (f *fixture) create(t *testing.T, vpc, peer string) *ec2.VpcPeeringConnection {
	t.Helper()
	out, err := f.svc.CreateVpcPeeringConnection(t.Context(), &ec2.CreateVpcPeeringConnectionInput{VpcId: aws.String(vpc), PeerVpcId: aws.String(peer)}, testAccount)
	require.NoError(t, err)
	return out.VpcPeeringConnection
}

func (f *fixture) accept(t *testing.T, id string) (*ec2.VpcPeeringConnection, error) {
	out, err := f.svc.AcceptVpcPeeringConnection(t.Context(), &ec2.AcceptVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(id)}, testAccount)
	if err != nil {
		return nil, err
	}
	return out.VpcPeeringConnection, nil
}

func (f *fixture) del(t *testing.T, id string) error {
	_, err := f.svc.DeleteVpcPeeringConnection(t.Context(), &ec2.DeleteVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(id)}, testAccount)
	return err
}

func (f *fixture) reject(t *testing.T, id string) error {
	_, err := f.svc.RejectVpcPeeringConnection(t.Context(), &ec2.RejectVpcPeeringConnectionInput{VpcPeeringConnectionId: aws.String(id)}, testAccount)
	return err
}

func (f *fixture) describe(t *testing.T, ids ...string) ([]*ec2.VpcPeeringConnection, error) {
	t.Helper()
	out, err := f.svc.DescribeVpcPeeringConnections(t.Context(), &ec2.DescribeVpcPeeringConnectionsInput{VpcPeeringConnectionIds: aws.StringSlice(ids)}, testAccount)
	if err != nil {
		return nil, err
	}
	return out.VpcPeeringConnections, nil
}

// status describes one connection and returns its status code, or "" when it
// is not visible.
func (f *fixture) status(t *testing.T, id string) string {
	t.Helper()
	conns, err := f.describe(t, id)
	if awserrors.IsErrorCode(err, awserrors.ErrorInvalidVpcPeeringConnectionIDNotFound) {
		return ""
	}
	require.NoError(t, err)
	require.Len(t, conns, 1)
	return aws.StringValue(conns[0].Status.Code)
}

// activate accepts id and confirms realisation as the reconciler would.
func (f *fixture) activate(t *testing.T, id string) {
	t.Helper()
	_, err := f.accept(t, id)
	require.NoError(t, err)
	rec := f.liveRecord(t, id)
	require.NotNil(t, rec)
	require.NoError(t, f.svc.MarkActive(t.Context(), id, rec.Generation))
}

func (f *fixture) liveRecord(t *testing.T, id string) *PeeringRecord {
	t.Helper()
	rec, _, err := getJSON[PeeringRecord](t.Context(), f.svc.live, recordKey(id))
	require.NoError(t, err)
	return rec
}

func (f *fixture) termRecord(t *testing.T, id string) *PeeringRecord {
	t.Helper()
	rec, _, err := getJSON[PeeringRecord](t.Context(), f.svc.term, recordKey(id))
	require.NoError(t, err)
	return rec
}

// liveKeys returns every key in the live bucket except the version key.
func (f *fixture) liveKeys(t *testing.T) map[string]bool {
	t.Helper()
	keys, err := kvstore.Keys(t.Context(), f.svc.live)
	require.NoError(t, err)
	out := map[string]bool{}
	for _, k := range keys {
		if k != kvstore.VersionKey {
			out[k] = true
		}
	}
	return out
}

func (f *fixture) has(t *testing.T, key string) bool {
	t.Helper()
	return f.liveKeys(t)[key]
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	got, ok := awserrors.ResolveErrorCode(err)
	require.True(t, ok, "error %v carries no AWS code", err)
	require.Equal(t, code, got, "error: %v", err)
}

func requireMessage(t *testing.T, err error, code, msg string) {
	t.Helper()
	gotCode, gotMsg, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "error %v carries no AWS detail", err)
	require.Equal(t, code, gotCode)
	require.Equal(t, msg, gotMsg)
}

func vpcID(n int) string { return fmt.Sprintf("vpc-%017x", n) }
