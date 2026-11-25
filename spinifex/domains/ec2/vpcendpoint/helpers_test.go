package vpcendpoint

import (
	"context"
	"encoding/json"
	"errors"
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
	testAccount  = "123456789012"
	otherAccount = "210987654321"
	testRegion   = "ap-southeast-2"
	s3Service    = "com.amazonaws.ap-southeast-2.s3"
	vpcA         = "vpc-0a000000000000001"
	vpcB         = "vpc-0b000000000000002"
	rtbA1        = "rtb-0a000000000000001"
	rtbA2        = "rtb-0a000000000000002"
	rtbA3        = "rtb-0a000000000000003"
	rtbB1        = "rtb-0b000000000000001"
)

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

// fakeTables is the route-table lookup: account and table to VPC.
type fakeTables struct {
	mu     sync.Mutex
	tables map[string]string
}

var _ TableLookup = (*fakeTables)(nil)

func (f *fakeTables) RouteTableVpc(_ context.Context, accountID, rtbID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tables[accountID+"/"+rtbID], nil
}

func (f *fakeTables) put(accountID, rtbID, vpcID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tables[accountID+"/"+rtbID] = vpcID
}

func (f *fakeTables) drop(accountID, rtbID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tables, accountID+"/"+rtbID)
}

type fixture struct {
	svc    *ServiceImpl
	clock  *fakeClock
	tables *fakeTables
	gate   *fakeGate
	js     jetstream.JetStream
}

// fakeGate is a feature-level admission: whether it admits, its read error,
// and what each release saw.
type fakeGate struct {
	closed  bool
	err     error
	onDone  func()
	held    int
	maxHeld int
}

func (g *fakeGate) offered(context.Context) (bool, error) {
	return !g.closed && g.err == nil, g.err
}

func (g *fakeGate) admit(context.Context) (func(), bool, error) {
	g.held++
	g.maxHeld = max(g.maxHeld, g.held)
	return func() {
		if g.onDone != nil {
			g.onDone()
		}
		g.held--
	}, !g.closed && g.err == nil, g.err
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	store, err := OpenStore(t.Context(), js)
	require.NoError(t, err)
	clock := &fakeClock{t: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)}
	store.now = clock.now
	tables := &fakeTables{tables: map[string]string{}}
	store.SetTableLookup(tables)
	svc := NewServiceImpl(store, Settings{Region: testRegion, ServicesDomain: "services.internal", AZ: testRegion + "a"})
	gate := &fakeGate{}
	svc.SetActivation(gate.admit, gate.offered)
	f := &fixture{svc: svc, clock: clock, tables: tables, gate: gate, js: js}
	f.putVPC(t, testAccount, vpcA)
	f.putVPC(t, testAccount, vpcB)
	for _, rtb := range []string{rtbA1, rtbA2, rtbA3} {
		tables.put(testAccount, rtb, vpcA)
	}
	tables.put(testAccount, rtbB1, vpcB)
	return f
}

func (f *fixture) putVPC(t *testing.T, account, id string) {
	t.Helper()
	data, err := json.Marshal(ec2vpc.VPCRecord{VpcId: id, CidrBlock: "10.0.0.0/16", State: "available"})
	require.NoError(t, err)
	_, err = f.svc.vpcKV.Put(t.Context(), kvstore.AccountKey(account, id), data)
	require.NoError(t, err)
}

func (f *fixture) createIn(t *testing.T, vpcID string, tables ...string) (*ec2.VpcEndpoint, error) {
	t.Helper()
	out, err := f.svc.CreateVpcEndpoint(t.Context(), &ec2.CreateVpcEndpointInput{
		VpcId: aws.String(vpcID), ServiceName: aws.String(s3Service), RouteTableIds: aws.StringSlice(tables),
	}, testAccount)
	if err != nil {
		return nil, err
	}
	return out.VpcEndpoint, nil
}

func (f *fixture) create(t *testing.T, tables ...string) string {
	t.Helper()
	e, err := f.createIn(t, vpcA, tables...)
	require.NoError(t, err)
	return aws.StringValue(e.VpcEndpointId)
}

func (f *fixture) describe(t *testing.T, id string) *ec2.VpcEndpoint {
	t.Helper()
	out, err := f.svc.DescribeVpcEndpoints(t.Context(), &ec2.DescribeVpcEndpointsInput{VpcEndpointIds: aws.StringSlice([]string{id})}, testAccount)
	require.NoError(t, err)
	require.Len(t, out.VpcEndpoints, 1)
	return out.VpcEndpoints[0]
}

func (f *fixture) del(t *testing.T, ids ...string) *ec2.DeleteVpcEndpointsOutput {
	t.Helper()
	out, err := f.svc.DeleteVpcEndpoints(t.Context(), &ec2.DeleteVpcEndpointsInput{VpcEndpointIds: aws.StringSlice(ids)}, testAccount)
	require.NoError(t, err)
	return out
}

func (f *fixture) modify(t *testing.T, in *ec2.ModifyVpcEndpointInput) error {
	t.Helper()
	_, err := f.svc.ModifyVpcEndpoint(t.Context(), in, testAccount)
	return err
}

// routeOn returns the endpoint table's S3 route goes to, or "".
func (f *fixture) routeOn(t *testing.T, table string) string {
	t.Helper()
	got, err := f.svc.EffectiveEndpoints(t.Context(), []string{table})
	require.NoError(t, err)
	return got[table]
}

func (f *fixture) sweep(t *testing.T) SweepResult {
	t.Helper()
	res, err := f.svc.Sweep(t.Context())
	require.NoError(t, err)
	return res
}

// keys lists every key in the bucket but the version key.
func (f *fixture) keys(t *testing.T) []string {
	t.Helper()
	keys, err := kvstore.Keys(t.Context(), f.svc.kv)
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, k := range keys {
		if k != kvstore.VersionKey {
			out = append(out, k)
		}
	}
	return out
}

func requireCode(t *testing.T, err error, code, msg string) {
	t.Helper()
	require.Error(t, err)
	gotCode, gotMsg, ok := awserrors.ResolveErrorDetail(err)
	require.Truef(t, ok, "not an AWS error: %v", err)
	require.Equal(t, code, gotCode)
	if msg != "" {
		require.Equal(t, msg, gotMsg)
	}
}
