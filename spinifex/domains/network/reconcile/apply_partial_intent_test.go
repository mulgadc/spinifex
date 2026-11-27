//test:in-package — sets the reconciler's unexported reloadIntent and localPorts
// hooks, so a drift pass reads intent from a real JetStream the way vpcd does.

package reconcile

// A record the intent loader cannot decode or parse is skipped, so it is absent
// from intent. Every orphan sweep reads that absence as permission to delete.
// These tests hold the sweeps to a stricter rule: partial intent removes nothing.

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	ec2igw "github.com/mulgadc/spinifex/spinifex/domains/ec2/igw"
	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/domains/network/policy"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
	"github.com/nats-io/nats.go/jetstream"
)

var poison = []byte("{not json")

// kvFixture is the control-plane KV contents, bucket to key to value.
type kvFixture map[string]map[string][]byte

func (f kvFixture) set(bucket, key string, v []byte) {
	if f[bucket] == nil {
		f[bucket] = map[string][]byte{}
	}
	f[bucket][key] = v
}

func (f kvFixture) seed(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	for bucket, entries := range f {
		testutil.SeedKV(t, js, bucket, entries)
	}
}

func eniRecord(id, subnet, vpc, ip, mac, sg, publicIP string) ec2vpc.ENIRecord {
	return ec2vpc.ENIRecord{
		NetworkInterfaceId: id, SubnetId: subnet, VpcId: vpc,
		PrivateIpAddress: ip, MacAddress: mac, SecurityGroupIds: []string{sg},
		PublicIpAddress: publicIP, Status: "in-use", InstanceId: "i-" + id,
	}
}

// twoVPCFixture is two VPCs, each with one subnet, one SG and one ENI, so a
// fault in vpc-a leaves intent non-empty and the empty-intent guards silent.
func twoVPCFixture(t *testing.T) kvFixture {
	t.Helper()
	f := kvFixture{}
	f.set(ec2vpc.KVBucketVPCs, "acct.vpc-a", mustJSON(t, ec2vpc.VPCRecord{VpcId: "vpc-a", CidrBlock: "10.0.0.0/16", VNI: 100}))
	f.set(ec2vpc.KVBucketVPCs, "acct.vpc-b", mustJSON(t, ec2vpc.VPCRecord{VpcId: "vpc-b", CidrBlock: "10.1.0.0/16", VNI: 101}))
	f.set(ec2vpc.KVBucketSubnets, "acct.subnet-a", mustJSON(t, ec2vpc.SubnetRecord{SubnetId: "subnet-a", VpcId: "vpc-a", CidrBlock: "10.0.1.0/24"}))
	f.set(ec2vpc.KVBucketSubnets, "acct.subnet-b", mustJSON(t, ec2vpc.SubnetRecord{SubnetId: "subnet-b", VpcId: "vpc-b", CidrBlock: "10.1.1.0/24"}))
	f.set(ec2vpc.KVBucketSecurityGroups, "acct.sg-a", mustJSON(t, ec2vpc.SecurityGroupRecord{GroupId: "sg-a", VpcId: "vpc-a"}))
	f.set(ec2vpc.KVBucketSecurityGroups, "acct.sg-b", mustJSON(t, ec2vpc.SecurityGroupRecord{GroupId: "sg-b", VpcId: "vpc-b"}))
	f.set(ec2vpc.KVBucketENIs, "acct.eni-a", mustJSON(t, eniRecord("eni-a", "subnet-a", "vpc-a", "10.0.1.10", "02:00:00:00:00:0a", "sg-a", "")))
	f.set(ec2vpc.KVBucketENIs, "acct.eni-b", mustJSON(t, eniRecord("eni-b", "subnet-b", "vpc-b", "10.1.1.10", "02:00:00:00:00:0b", "sg-b", "")))
	return f
}

func kvLoader(js jetstream.JetStream) func(context.Context) (IntentState, error) {
	return func(ctx context.Context) (IntentState, error) {
		return LoadIntentFromKV(ctx, js, "us-east-1a")
	}
}

// noLocalTaps is a drift leader that hosts none of the guests, which is the
// case for every guest on the other hosts of a cluster.
func noLocalTaps(context.Context) (map[string]struct{}, error) { return map[string]struct{}{}, nil }

func putRaw(t *testing.T, js jetstream.JetStream, bucket, key string, v []byte) {
	t.Helper()
	kv, err := js.KeyValue(t.Context(), bucket)
	if err != nil {
		t.Fatalf("open bucket %s: %v", bucket, err)
	}
	if _, err := kv.Put(t.Context(), key, v); err != nil {
		t.Fatalf("put %s/%s: %v", bucket, key, err)
	}
}

// convergeFromKV runs one clean drift pass, so every row a later pass could
// sweep exists before the fault is injected.
func convergeFromKV(t *testing.T, r *reconciler, js jetstream.JetStream) {
	t.Helper()
	intent, err := LoadIntentFromKV(t.Context(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV before the fault: %v", err)
	}
	if err := r.Reconcile(t.Context(), intent); err != nil {
		t.Fatalf("converging Reconcile: %v", err)
	}
}

// addThirdENI writes an ENI in vpc-b that OVN does not hold yet. It proves the
// apply phase still converges a healthy record while another one is bad.
func addThirdENI(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	putRaw(t, js, ec2vpc.KVBucketENIs, "acct.eni-c",
		mustJSON(t, eniRecord("eni-c", "subnet-b", "vpc-b", "10.1.1.11", "02:00:00:00:00:0c", "sg-b", "")))
}

func hasPort(m *mock.Client, eniID string) bool {
	_, err := m.GetLogicalSwitchPort(context.Background(), topology.Port(eniID))
	return err == nil
}

func TestS7_DriftPassKeepsGuestPortsWhenTopologyIntentIsPartial(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		bucket, key   string
		value         func(t *testing.T) []byte
		lostPortgroup string
	}{
		{name: "eni record does not decode", bucket: ec2vpc.KVBucketENIs, key: "acct.eni-a",
			value: func(*testing.T) []byte { return poison }},
		{name: "vpc record does not decode", bucket: ec2vpc.KVBucketVPCs, key: "acct.vpc-a",
			value: func(*testing.T) []byte { return poison }},
		{name: "vpc cidr does not parse", bucket: ec2vpc.KVBucketVPCs, key: "acct.vpc-a",
			value: func(t *testing.T) []byte {
				return mustJSON(t, ec2vpc.VPCRecord{VpcId: "vpc-a", CidrBlock: "10.0.0.0/99", VNI: 100})
			}},
		{name: "eni private address does not parse", bucket: ec2vpc.KVBucketENIs, key: "acct.eni-a",
			value: func(t *testing.T) []byte {
				return mustJSON(t, eniRecord("eni-a", "subnet-a", "vpc-a", "10.0.1.x", "02:00:00:00:00:0a", "sg-a", ""))
			}},
		{name: "eni mac does not parse", bucket: ec2vpc.KVBucketENIs, key: "acct.eni-a",
			value: func(t *testing.T) []byte {
				return mustJSON(t, eniRecord("eni-a", "subnet-a", "vpc-a", "10.0.1.10", "not-a-mac", "sg-a", ""))
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			js := startKV(t)
			twoVPCFixture(t).seed(t, js)
			r, m := newTestReconciler(t)
			r.reloadIntent = kvLoader(js)
			r.localPorts = noLocalTaps
			convergeFromKV(t, r, js)

			putRaw(t, js, tc.bucket, tc.key, tc.value(t))
			addThirdENI(t, js)
			intent, err := LoadIntentFromKV(t.Context(), js, "us-east-1a")
			if err != nil {
				t.Fatalf("LoadIntentFromKV after the fault: %v", err)
			}
			err = r.Reconcile(t.Context(), intent)

			if !hasPort(m, "eni-a") {
				t.Errorf("drift pass deleted port-eni-a, whose record is present but unreadable")
			}
			if !hasPort(m, "eni-c") {
				t.Errorf("drift pass did not create port-eni-c, a healthy record beside the bad one")
			}
			if !errors.Is(err, ErrPassIncomplete) {
				t.Errorf("Reconcile = %v, want ErrPassIncomplete so the refused sweep is visible", err)
			}
		})
	}
}

// Each read guards each sweep on its own. The pass intent is the only source
// when no re-read is wired, and only the re-read can know about a resource
// created after the pass snapshot, so each subtest leaves one read partial.
func TestS7_SweepsRefuseWhenEitherReadIsPartial(t *testing.T) {
	t.Parallel()
	type sweep struct {
		name    string
		fixture func(t *testing.T) kvFixture
		fault   [2]string // bucket, key
		// forget removes the faulted resource from a complete snapshot.
		forget func(intent *IntentState)
		run    func(t *testing.T, r *reconciler, intent IntentState, res *passResult)
		alive  func(m *mock.Client) bool
	}
	sweeps := []sweep{
		{
			name: "guest port", fixture: twoVPCFixture, fault: [2]string{ec2vpc.KVBucketENIs, "acct.eni-a"},
			forget: func(in *IntentState) { delete(in.Ports, "eni-a") },
			run: func(t *testing.T, r *reconciler, in IntentState, res *passResult) {
				r.pruneOrphanPorts(t.Context(), in, res)
			},
			alive: func(m *mock.Client) bool { return hasPort(m, "eni-a") },
		},
		{
			name: "security group port group", fixture: twoVPCFixture, fault: [2]string{ec2vpc.KVBucketSecurityGroups, "acct.sg-a"},
			forget: func(in *IntentState) { delete(in.SGs, "sg-a") },
			run: func(t *testing.T, r *reconciler, in IntentState, res *passResult) {
				actual, err := scanActual(t.Context(), r.ovn)
				if err != nil {
					t.Fatalf("scanActual: %v", err)
				}
				r.applySGs(t.Context(), in, actual, true, res)
			},
			alive: func(m *mock.Client) bool {
				_, ok := m.PortGroups[topology.SecurityGroupPortGroup("sg-a")]
				return ok
			},
		},
		{
			name: "elastic ip row", fixture: publicIPFixture, fault: [2]string{ec2vpc.KVBucketENIs, "acct.eni-a"},
			forget: func(in *IntentState) { delete(in.Ports, "eni-a") },
			run: func(t *testing.T, r *reconciler, in IntentState, res *passResult) {
				r.pruneOrphanEIPs(t.Context(), in, res)
			},
			alive: func(m *mock.Client) bool { return findNATByExternal(m, "dnat_and_snat", publicA) != nil },
		},
	}
	for _, sw := range sweeps {
		for _, freshPartial := range []bool{false, true} {
			name := sw.name + ", pass intent partial, no re-read"
			if freshPartial {
				name = sw.name + ", pass intent complete, re-read partial"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				js := startKV(t)
				sw.fixture(t).seed(t, js)
				r, m := newRoutedReconciler(t, js, newRoutedBinder())
				convergeFromKV(t, r, js)
				if !sw.alive(m) {
					t.Fatalf("%s missing after converging", sw.name)
				}

				snapshot, err := LoadIntentFromKV(t.Context(), js, "us-east-1a")
				if err != nil {
					t.Fatalf("LoadIntentFromKV: %v", err)
				}
				putRaw(t, js, sw.fault[0], sw.fault[1], poison)
				if freshPartial {
					// A complete snapshot taken before the resource was created.
					sw.forget(&snapshot)
				} else {
					r.reloadIntent = nil
					if snapshot, err = LoadIntentFromKV(t.Context(), js, "us-east-1a"); err != nil {
						t.Fatalf("LoadIntentFromKV after the fault: %v", err)
					}
				}
				res := &passResult{}
				sw.run(t, r, snapshot, res)

				if !sw.alive(m) {
					t.Errorf("sweep removed the %s against a partial read", sw.name)
				}
				if len(res.failures) == 0 {
					t.Errorf("refused sweep not recorded on the pass")
				}
			})
		}
	}
}

func TestS7_DriftPassKeepsAPortGroupWhenItsSGRecordDoesNotDecode(t *testing.T) {
	t.Parallel()
	js := startKV(t)
	twoVPCFixture(t).seed(t, js)
	r, m := newTestReconciler(t)
	r.reloadIntent = kvLoader(js)
	r.localPorts = noLocalTaps
	convergeFromKV(t, r, js)

	putRaw(t, js, ec2vpc.KVBucketSecurityGroups, "acct.sg-a", poison)
	addThirdENI(t, js)
	intent, err := LoadIntentFromKV(t.Context(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}
	err = r.Reconcile(t.Context(), intent)

	if _, ok := m.PortGroups[topology.SecurityGroupPortGroup("sg-a")]; !ok {
		t.Errorf("drift pass deleted the sg-a port group, whose record is present but unreadable")
	}
	if !hasPort(m, "eni-c") {
		t.Errorf("drift pass did not create port-eni-c, a healthy record beside the bad one")
	}
	if !errors.Is(err, ErrPassIncomplete) {
		t.Errorf("Reconcile = %v, want ErrPassIncomplete", err)
	}
}

// routedBinder is one node's host EIP bindings, read and changed through the
// NAT manager's routed-mode hooks.
type routedBinder struct {
	mu      sync.Mutex
	bound   map[string]struct{}
	unbinds []string
}

func newRoutedBinder() *routedBinder { return &routedBinder{bound: map[string]struct{}{}} }

func (b *routedBinder) hooks() policy.HostEIPBinder {
	return policy.HostEIPBinder{
		Bind: func(eip policy.EIPSpec, _ string) error {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.bound[eip.ExternalIP] = struct{}{}
			return nil
		},
		Unbind: func(ip string) error {
			b.mu.Lock()
			defer b.mu.Unlock()
			delete(b.bound, ip)
			b.unbinds = append(b.unbinds, ip)
			return nil
		},
		List: func() ([]string, error) {
			b.mu.Lock()
			defer b.mu.Unlock()
			return slices.Sorted(maps.Keys(b.bound)), nil
		},
	}
}

func (b *routedBinder) has(ip string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.bound[ip]
	return ok
}

// newRoutedReconciler is newTestReconciler with routed NAT, so the EIP sweep
// also reaches this node's host bindings.
func newRoutedReconciler(t *testing.T, js jetstream.JetStream, b *routedBinder) (*reconciler, *mock.Client) {
	t.Helper()
	m := mock.New()
	nat, err := policy.NewNATManager(m, policy.NATModeRouted, policy.WithHostEIPBinder(b.hooks()))
	if err != nil {
		t.Fatalf("NewNATManager: %v", err)
	}
	routes := policy.NewRouteManager(m)
	igw, err := external.NewIGWManager(external.IGWManagerConfig{
		OVN: m, Routes: routes, NAT: nat, Allocator: external.LinkLocalAllocator{}, NATMode: policy.NATModeRouted,
	})
	if err != nil {
		t.Fatalf("NewIGWManager: %v", err)
	}
	rec, err := New(Config{
		OVN: m, SG: policy.NewSecurityGroupManager(m, policy.EgressPolicy{}), NAT: nat, Routes: routes, IGW: igw,
		Topology: topology.NewLiveManager(m), LocalAZ: "us-east-1a", NodeHostname: "test-host",
		FreshIntent: kvLoader(js), LocalPorts: noLocalTaps,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r, ok := rec.(*reconciler)
	if !ok {
		t.Fatalf("New returned %T, want *reconciler", rec)
	}
	return r, m
}

const (
	publicA = "203.0.113.10"
	publicB = "203.0.113.11"
)

// publicIPFixture gives both ENIs an auto-assigned public address. eni-b is the
// survivor that keeps the EIP sweep's live set non-empty.
func publicIPFixture(t *testing.T) kvFixture {
	t.Helper()
	f := twoVPCFixture(t)
	f.set(ec2vpc.KVBucketENIs, "acct.eni-a", mustJSON(t, eniRecord("eni-a", "subnet-a", "vpc-a", "10.0.1.10", "02:00:00:00:00:0a", "sg-a", publicA)))
	f.set(ec2vpc.KVBucketENIs, "acct.eni-b", mustJSON(t, eniRecord("eni-b", "subnet-b", "vpc-b", "10.1.1.10", "02:00:00:00:00:0b", "sg-b", publicB)))
	return f
}

var publicIPFaults = []struct {
	name  string
	value func(t *testing.T) []byte
}{
	{name: "eni record does not decode", value: func(*testing.T) []byte { return poison }},
	{name: "eni public address does not parse", value: func(t *testing.T) []byte {
		return mustJSON(t, eniRecord("eni-a", "subnet-a", "vpc-a", "10.0.1.10", "02:00:00:00:00:0a", "sg-a", "203.0.113.999"))
	}},
}

// requireStampedRow pins the precondition the EIP sweep acts on: a
// dnat_and_snat row stamped with its owning port. An unstamped row is never swept.
func requireStampedRow(t *testing.T, m *mock.Client, externalIP, eniID string) {
	t.Helper()
	row := findNATByExternal(m, "dnat_and_snat", externalIP)
	if row == nil {
		t.Fatalf("no dnat_and_snat row for %s after converging", externalIP)
	}
	if got := row.ExternalIDs["spinifex:logical_port"]; got != topology.Port(eniID) {
		t.Fatalf("row for %s stamped %q, want %q", externalIP, got, topology.Port(eniID))
	}
}

func TestS7_DriftPassKeepsAPublicIPWhenItsENIIsPartial(t *testing.T) {
	t.Parallel()
	for _, tc := range publicIPFaults {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			js := startKV(t)
			publicIPFixture(t).seed(t, js)
			b := newRoutedBinder()
			r, m := newRoutedReconciler(t, js, b)
			convergeFromKV(t, r, js)
			requireStampedRow(t, m, publicA, "eni-a")
			requireStampedRow(t, m, publicB, "eni-b")
			if !b.has(publicA) || !b.has(publicB) {
				t.Fatalf("host bindings after converging = %v, want both addresses", b.bound)
			}

			putRaw(t, js, ec2vpc.KVBucketENIs, "acct.eni-a", tc.value(t))
			intent, err := LoadIntentFromKV(t.Context(), js, "us-east-1a")
			if err != nil {
				t.Fatalf("LoadIntentFromKV: %v", err)
			}
			err = r.Reconcile(t.Context(), intent)

			if findNATByExternal(m, "dnat_and_snat", publicA) == nil {
				t.Errorf("drift pass deleted the dnat_and_snat row for %s", publicA)
			}
			if !b.has(publicA) {
				t.Errorf("drift pass removed the host binding for %s (unbinds %v)", publicA, b.unbinds)
			}
			if findNATByExternal(m, "dnat_and_snat", publicB) == nil {
				t.Errorf("drift pass deleted the survivor's row for %s", publicB)
			}
			if !errors.Is(err, ErrPassIncomplete) {
				t.Errorf("Reconcile = %v, want ErrPassIncomplete", err)
			}
		})
	}
}

// The host EIP pass runs on every node with no lease, so it is a second path
// from a partial read to a removed binding.
func TestS7_HostEIPPassKeepsABindingWhenItsENIIsPartial(t *testing.T) {
	t.Parallel()
	for _, tc := range publicIPFaults {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			js := startKV(t)
			publicIPFixture(t).seed(t, js)
			b := newRoutedBinder()
			r, _ := newRoutedReconciler(t, js, b)
			if err := r.ReconcileHostEIPs(t.Context()); err != nil {
				t.Fatalf("converging ReconcileHostEIPs: %v", err)
			}
			if !b.has(publicA) || !b.has(publicB) {
				t.Fatalf("host bindings after converging = %v, want both addresses", b.bound)
			}

			putRaw(t, js, ec2vpc.KVBucketENIs, "acct.eni-a", tc.value(t))
			err := r.ReconcileHostEIPs(t.Context())

			if !b.has(publicA) {
				t.Errorf("host EIP pass removed the binding for %s (unbinds %v)", publicA, b.unbinds)
			}
			if !b.has(publicB) {
				t.Errorf("host EIP pass removed the survivor's binding for %s", publicB)
			}
			if err == nil {
				t.Errorf("ReconcileHostEIPs = nil, want an error naming the partial intent")
			}
		})
	}
}

// hostRouteTable is one node's VPC ingress routes, CIDR to gateway IP.
type hostRouteTable struct {
	mu     sync.Mutex
	routes map[string]string
}

func (h *hostRouteTable) List(context.Context) (map[string]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return maps.Clone(h.routes), nil
}

func (h *hostRouteTable) Ensure(_ context.Context, cidr, via string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.routes[cidr] = via
	return nil
}

func (h *hostRouteTable) Remove(_ context.Context, cidr string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.routes, cidr)
	return nil
}

func seedGatewayLRP(t *testing.T, m *mock.Client, vpcID, gwIP string) {
	t.Helper()
	if err := m.CreateLogicalRouter(t.Context(), &nbdb.LogicalRouter{Name: topology.VPCRouter(vpcID)}); err != nil {
		t.Fatalf("seed router %s: %v", vpcID, err)
	}
	if err := m.CreateLogicalRouterPort(t.Context(), topology.VPCRouter(vpcID), &nbdb.LogicalRouterPort{
		Name:        topology.GatewayRouterPort(vpcID),
		Networks:    []string{gwIP + "/24"},
		ExternalIDs: map[string]string{policy.GatewayIPExtIDKey: gwIP},
	}); err != nil {
		t.Fatalf("seed gateway %s: %v", vpcID, err)
	}
}

func TestS7_HostIngressKeepsRoutesWhenTopologyIntentIsPartial(t *testing.T) {
	t.Parallel()
	attached := func(t *testing.T, igw, vpc string) []byte {
		return mustJSON(t, ec2igw.IGWRecord{InternetGatewayId: igw, VpcId: vpc, State: "available"})
	}
	cases := []struct {
		name        string
		vpcB        string // CIDR of vpc-b; equal to vpc-a's for a shared CIDR
		bucket, key string
	}{
		{name: "vpc record does not decode", vpcB: "10.1.0.0/16", bucket: ec2vpc.KVBucketVPCs, key: "acct.vpc-a"},
		{name: "igw record does not decode", vpcB: "10.1.0.0/16", bucket: ec2igw.KVBucketIGW, key: "acct.igw-a"},
		{name: "shared cidr winner does not decode", vpcB: "10.0.0.0/16", bucket: ec2vpc.KVBucketVPCs, key: "acct.vpc-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			js := startKV(t)
			f := kvFixture{}
			f.set(ec2vpc.KVBucketVPCs, "acct.vpc-a", mustJSON(t, ec2vpc.VPCRecord{VpcId: "vpc-a", CidrBlock: "10.0.0.0/16"}))
			f.set(ec2vpc.KVBucketVPCs, "acct.vpc-b", mustJSON(t, ec2vpc.VPCRecord{VpcId: "vpc-b", CidrBlock: tc.vpcB}))
			f.set(ec2igw.KVBucketIGW, "acct.igw-a", attached(t, "igw-a", "vpc-a"))
			f.set(ec2igw.KVBucketIGW, "acct.igw-b", attached(t, "igw-b", "vpc-b"))
			f.seed(t, js)
			m := mock.New()
			seedGatewayLRP(t, m, "vpc-a", "100.127.0.10")
			seedGatewayLRP(t, m, "vpc-b", "100.127.0.11")
			routes := &hostRouteTable{routes: map[string]string{}}
			h := &HostIngress{OVN: m, Routes: routes, LoadIntent: kvLoader(js)}
			if _, err := h.Pass(t.Context()); err != nil {
				t.Fatalf("converging pass: %v", err)
			}
			before, _ := routes.List(t.Context())
			if before["10.0.0.0/16"] != "100.127.0.10" {
				t.Fatalf("routes after converging = %v, want 10.0.0.0/16 via vpc-a's gateway", before)
			}

			putRaw(t, js, tc.bucket, tc.key, poison)
			_, err := h.Pass(t.Context())

			after, _ := routes.List(t.Context())
			if !maps.Equal(after, before) {
				t.Errorf("host ingress routes changed on a partial read: before %v, after %v", before, after)
			}
			if err == nil {
				t.Errorf("Pass = nil, want an error naming the partial intent")
			}
		})
	}
}

func TestS7_DriftPassAddsNoDropGateWhenARouteTableDoesNotDecode(t *testing.T) {
	t.Parallel()
	js := startKV(t)
	f := twoVPCFixture(t)
	f.set(ec2igw.KVBucketIGW, "acct.igw-a", mustJSON(t, ec2igw.IGWRecord{InternetGatewayId: "igw-a", VpcId: "vpc-a", State: "available"}))
	f.set(ec2routetable.KVBucketRouteTables, "acct.rtb-a", mustJSON(t, ec2routetable.RouteTableRecord{
		RouteTableId: "rtb-a", VpcId: "vpc-a", IsMain: true,
		Routes: []ec2routetable.RouteRecord{{DestinationCidrBlock: "0.0.0.0/0", GatewayId: "igw-a", State: "active"}},
	}))
	f.seed(t, js)
	r, m := newTestReconciler(t)
	r.reloadIntent = kvLoader(js)
	r.localPorts = noLocalTaps
	convergeFromKV(t, r, js)
	dropGates := func() []string {
		policies, err := m.ListLogicalRouterPolicies(t.Context(), topology.VPCRouter("vpc-a"))
		if err != nil {
			t.Fatalf("ListLogicalRouterPolicies: %v", err)
		}
		var out []string
		for _, p := range policies {
			if p.Priority == policy.SubnetEgressPriorityDrop {
				out = append(out, p.Match)
			}
		}
		return out
	}
	if gates := dropGates(); len(gates) != 0 {
		t.Fatalf("drop gates after converging a routed subnet = %v, want none", gates)
	}

	putRaw(t, js, ec2routetable.KVBucketRouteTables, "acct.rtb-a", poison)
	intent, err := LoadIntentFromKV(t.Context(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}
	err = r.Reconcile(t.Context(), intent)

	if gates := dropGates(); len(gates) != 0 {
		t.Errorf("drift pass installed a drop gate over a live default route: %v", gates)
	}
	if !errors.Is(err, ErrPassIncomplete) {
		t.Errorf("Reconcile = %v, want ErrPassIncomplete", err)
	}
}
