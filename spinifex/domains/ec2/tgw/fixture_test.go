package tgw

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
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

const testAccount = "123456789012"

// clock is an injected time source that tests move forward.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fakeAccounts struct {
	mu     sync.Mutex
	active bool
	err    error
	calls  int
}

var _ AccountStatus = (*fakeAccounts)(nil)

func (f *fakeAccounts) AccountActive(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.active, f.err
}

// gate is a feature-level admission whose answer a test sets, and which
// counts admissions still held.
type gate struct {
	mu      sync.Mutex
	ok      bool
	err     error
	holding int
}

func (g *gate) admit(context.Context) (func(), bool, error) {
	g.mu.Lock()
	g.holding++
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.holding--
			g.mu.Unlock()
		})
	}, g.ok, g.err
}

func (g *gate) offered(context.Context) (bool, error) { return g.ok, g.err }

func (g *gate) held() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.holding
}

type fixture struct {
	t        *testing.T
	js       jetstream.JetStream
	svc      *ServiceImpl
	clock    *clock
	accounts *fakeAccounts
	gate     *gate
	vpcKV    jetstream.KeyValue
	subnetKV jetstream.KeyValue
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	store, err := OpenStore(t.Context(), js)
	require.NoError(t, err)
	c := &clock{now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	store.now = c.Now
	svc := NewServiceImpl(store, "ap-southeast-2")
	g := &gate{ok: true}
	svc.SetActivation(g.admit, g.offered)
	accts := &fakeAccounts{active: true}
	svc.SetAccountStatus(accts)
	vpcKV, err := js.KeyValue(t.Context(), ec2vpc.KVBucketVPCs)
	require.NoError(t, err)
	subnetKV, err := js.KeyValue(t.Context(), ec2vpc.KVBucketSubnets)
	require.NoError(t, err)
	return &fixture{t: t, js: js, svc: svc, clock: c, accounts: accts, gate: g, vpcKV: vpcKV, subnetKV: subnetKV}
}

func (f *fixture) putVPC(id, cidr string) {
	f.t.Helper()
	data, err := json.Marshal(ec2vpc.VPCRecord{VpcId: id, CidrBlock: cidr, State: "available"})
	require.NoError(f.t, err)
	_, err = f.vpcKV.Put(f.t.Context(), kvstore.AccountKey(testAccount, id), data)
	require.NoError(f.t, err)
}

func (f *fixture) putSubnet(id, vpcID, az string) {
	f.t.Helper()
	data, err := json.Marshal(ec2vpc.SubnetRecord{SubnetId: id, VpcId: vpcID, AvailabilityZone: az, State: "available"})
	require.NoError(f.t, err)
	_, err = f.subnetKV.Put(f.t.Context(), kvstore.AccountKey(testAccount, id), data)
	require.NoError(f.t, err)
}

// markVPC writes the deletion mark as DeleteVpc does.
func (f *fixture) markVPC(id string) {
	f.t.Helper()
	data, err := json.Marshal(ec2vpc.VPCRecord{VpcId: id, CidrBlock: "10.0.0.0/16", State: "available", Deleting: true})
	require.NoError(f.t, err)
	_, err = f.vpcKV.Put(f.t.Context(), kvstore.AccountKey(testAccount, id), data)
	require.NoError(f.t, err)
}

// availableTGW creates a Transit Gateway and confirms it as the reconciler does.
func (f *fixture) availableTGW() string {
	f.t.Helper()
	out, err := f.svc.CreateTransitGateway(f.t.Context(), &ec2.CreateTransitGatewayInput{}, testAccount)
	require.NoError(f.t, err)
	id := aws.StringValue(out.TransitGateway.TransitGatewayId)
	require.NoError(f.t, f.svc.MarkTransitGatewayAvailable(f.t.Context(), testAccount, id, 1))
	return id
}

// network writes a VPC with one subnet and returns their IDs.
func (f *fixture) network(vpcID string) (string, string) {
	f.t.Helper()
	subnetID := "subnet-" + vpcID[len("vpc-"):]
	f.putVPC(vpcID, "10.0.0.0/16")
	f.putSubnet(subnetID, vpcID, "ap-southeast-2a")
	return vpcID, subnetID
}

func (f *fixture) attach(tgwID, vpcID string, subnets ...string) (*ec2.TransitGatewayVpcAttachment, error) {
	out, err := f.svc.CreateTransitGatewayVpcAttachment(f.t.Context(), &ec2.CreateTransitGatewayVpcAttachmentInput{
		TransitGatewayId: aws.String(tgwID), VpcId: aws.String(vpcID), SubnetIds: aws.StringSlice(subnets),
	}, testAccount)
	if err != nil {
		return nil, err
	}
	return out.TransitGatewayVpcAttachment, nil
}

// keys lists every key in kv except the schema version.
func keys(t *testing.T, kv jetstream.KeyValue) []string {
	t.Helper()
	all, err := kvstore.Keys(t.Context(), kv)
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, k := range all {
		if k != kvstore.VersionKey {
			out = append(out, k)
		}
	}
	return out
}

// requireCode asserts err resolves to the AWS code and returns its message.
func requireCode(t *testing.T, err error, code string) string {
	t.Helper()
	require.Error(t, err)
	got, msg, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok, "unregistered error: %v", err)
	require.Equal(t, code, got, "error: %v", err)
	return msg
}
