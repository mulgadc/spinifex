//test:in-package — sets the reconciler's unexported reloadIntent and localPorts
// hooks, so a drift pass reads intent from a real JetStream the way vpcd does.

package reconcile

// A record the intent loader cannot decode or parse is skipped, so it is absent
// from intent. Every orphan sweep reads that absence as permission to delete.
// These tests hold the sweeps to a stricter rule: partial intent removes nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	ec2eip "github.com/mulgadc/spinifex/spinifex/domains/ec2/eip"
	ec2igw "github.com/mulgadc/spinifex/spinifex/domains/ec2/igw"
	ec2natgw "github.com/mulgadc/spinifex/spinifex/domains/ec2/natgw"
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
			if hasPort(m, "eni-c") {
				t.Errorf("partial drift pass created port-eni-c; it must change nothing until the record is repaired")
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
				// A partial pass intent is stopped by the pass gate, so that case
				// goes through Reconcile; a partial re-read reaches the sweep itself.
				refused := false
				if freshPartial {
					res := &passResult{}
					sw.run(t, r, snapshot, res)
					refused = len(res.failures) > 0
				} else {
					refused = errors.Is(r.Reconcile(t.Context(), snapshot), ErrPassIncomplete)
				}

				if !sw.alive(m) {
					t.Errorf("sweep removed the %s against a partial read", sw.name)
				}
				if !refused {
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
	if hasPort(m, "eni-c") {
		t.Errorf("partial drift pass created port-eni-c; it must change nothing until the record is repaired")
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
	binds   map[string]int
	unbinds []string
	neigh   int
}

func (b *routedBinder) neighCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.neigh
}

func (b *routedBinder) countNeigh() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.neigh++
}

func newRoutedBinder() *routedBinder {
	return &routedBinder{bound: map[string]struct{}{}, binds: map[string]int{}}
}

func (b *routedBinder) bindCount(ip string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.binds[ip]
}

func (b *routedBinder) hooks() policy.HostEIPBinder {
	return policy.HostEIPBinder{
		Bind: func(eip policy.EIPSpec, _ string) error {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.bound[eip.ExternalIP] = struct{}{}
			b.binds[eip.ExternalIP]++
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
	nat, err := policy.NewNATManager(m, policy.NATModeRouted, policy.WithHostEIPBinder(b.hooks()),
		policy.WithNeighFlusher(func(string) error { b.countNeigh(); return nil }),
		policy.WithNeighPrimer(func(policy.EIPSpec) error { b.countNeigh(); return nil }))
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
			addThirdENI(t, js)
			intent, err := LoadIntentFromKV(t.Context(), js, "us-east-1a")
			if err != nil {
				t.Fatalf("LoadIntentFromKV: %v", err)
			}
			err = r.Reconcile(t.Context(), intent)

			if hasPort(m, "eni-c") {
				t.Errorf("partial drift pass created port-eni-c; it must change nothing until the record is repaired")
			}
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

			// A healthy VPC whose route went missing stays without it: a partial
			// pass changes nothing, not even to restore.
			if tc.vpcB != "10.0.0.0/16" {
				if err := routes.Remove(t.Context(), tc.vpcB); err != nil {
					t.Fatalf("remove route: %v", err)
				}
				before, _ = routes.List(t.Context())
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

// An EIP record with an address that does not parse is skipped like any other
// unusable record, so its absence cannot remove the ENI's live public address.
func TestS7_DriftPassKeepsAPublicIPWhenAnEIPAddressDoesNotParse(t *testing.T) {
	t.Parallel()
	js := startKV(t)
	publicIPFixture(t).seed(t, js)
	b := newRoutedBinder()
	r, m := newRoutedReconciler(t, js, b)
	convergeFromKV(t, r, js)
	requireStampedRow(t, m, publicA, "eni-a")

	testutil.SeedKV(t, js, ec2eip.KVBucketEIPs, map[string][]byte{
		"acct.eipalloc-a": mustJSON(t, ec2eip.EIPRecord{
			AllocationId: "eipalloc-a", PublicIp: "203.0.113.999", State: "associated",
			ENIId: "eni-a", PrivateIp: "10.0.1.10", MacAddress: "02:00:00:00:00:0a", VpcId: "vpc-a",
		}),
	})
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
	if !errors.Is(err, ErrPassIncomplete) {
		t.Errorf("Reconcile = %v, want ErrPassIncomplete", err)
	}
}

// When an EIP record cannot be read, the ENI's own public address can be a
// stale copy. The apply phase must not swap the live EIP for it.
func TestS7_DriftPassKeepsALiveEIPWhenItsRecordDoesNotDecode(t *testing.T) {
	t.Parallel()
	const userEIP = "203.0.113.20"
	js := startKV(t)
	publicIPFixture(t).seed(t, js)
	testutil.SeedKV(t, js, ec2eip.KVBucketEIPs, map[string][]byte{
		"acct.eipalloc-a": mustJSON(t, ec2eip.EIPRecord{
			AllocationId: "eipalloc-a", PublicIp: userEIP, State: "associated",
			ENIId: "eni-a", PrivateIp: "10.0.1.10", MacAddress: "02:00:00:00:00:0a", VpcId: "vpc-a",
		}),
	})
	b := newRoutedBinder()
	r, m := newRoutedReconciler(t, js, b)
	convergeFromKV(t, r, js)
	requireStampedRow(t, m, userEIP, "eni-a")
	if !b.has(userEIP) {
		t.Fatalf("host bindings after converging = %v, want %s", b.bound, userEIP)
	}

	putRaw(t, js, ec2eip.KVBucketEIPs, "acct.eipalloc-a", poison)
	intent, err := LoadIntentFromKV(t.Context(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}
	err = r.Reconcile(t.Context(), intent)

	if findNATByExternal(m, "dnat_and_snat", userEIP) == nil {
		t.Errorf("drift pass replaced the live EIP %s with the ENI's stale address", userEIP)
	}
	if !b.has(userEIP) {
		t.Errorf("drift pass removed the host binding for %s (unbinds %v)", userEIP, b.unbinds)
	}
	if !errors.Is(err, ErrPassIncomplete) {
		t.Errorf("Reconcile = %v, want ErrPassIncomplete", err)
	}
}

// A bucket that cannot be opened is not an empty bucket: reading it as empty
// lets the host loops remove every route and binding it would have named.
func TestLoadIntentFromKV_FailsWhenABucketCannotBeOpened(t *testing.T) {
	t.Parallel()
	_, nc, js := testutil.StartTestJetStream(t)
	twoVPCFixture(t).seed(t, js)
	nc.Close()

	intent, err := LoadIntentFromKV(context.Background(), js, "us-east-1a")
	if err == nil {
		t.Fatalf("LoadIntentFromKV on a closed connection = nil error with %d VPCs, want an error", len(intent.VPCs))
	}
}

// Bind replaces an address's route and the rules beside it, and partial
// intent can hold a stale address, so a partial pass binds nothing at all.
func TestS7_HostEIPPassBindsNothingWhenPartial(t *testing.T) {
	t.Parallel()
	js := startKV(t)
	publicIPFixture(t).seed(t, js)
	b := newRoutedBinder()
	r, _ := newRoutedReconciler(t, js, b)
	if err := r.ReconcileHostEIPs(t.Context()); err != nil {
		t.Fatalf("converging ReconcileHostEIPs: %v", err)
	}
	b.mu.Lock()
	delete(b.bound, publicB)
	b.mu.Unlock()
	bindsA, bindsB := b.bindCount(publicA), b.bindCount(publicB)

	putRaw(t, js, ec2vpc.KVBucketSecurityGroups, "acct.sg-a", poison)
	err := r.ReconcileHostEIPs(t.Context())

	if got := b.bindCount(publicA); got != bindsA {
		t.Errorf("partial pass re-bound %s (%d binds, want %d)", publicA, got, bindsA)
	}
	if got := b.bindCount(publicB); got != bindsB {
		t.Errorf("partial pass bound %s (%d binds, want %d)", publicB, got, bindsB)
	}
	if !b.has(publicA) {
		t.Errorf("partial pass removed the binding for %s (unbinds %v)", publicA, b.unbinds)
	}
	if err == nil {
		t.Errorf("ReconcileHostEIPs = nil, want an error naming the partial intent")
	}
}

// The drift pass also binds host state for every EIP it applies, and a bind
// replaces the route. Under partial intent it changes no NAT row, binding or
// neighbour entry.
func TestS7_DriftPassDoesNotRebindABoundEIPWhenPartial(t *testing.T) {
	t.Parallel()
	js := startKV(t)
	publicIPFixture(t).seed(t, js)
	b := newRoutedBinder()
	r, m := newRoutedReconciler(t, js, b)
	convergeFromKV(t, r, js)
	bindsA, bindsB := b.bindCount(publicA), b.bindCount(publicB)
	rowsBefore := natRows(m)
	neighBefore := b.neighCalls()

	putRaw(t, js, ec2vpc.KVBucketSecurityGroups, "acct.sg-a", poison)
	intent, err := LoadIntentFromKV(t.Context(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}
	_ = r.Reconcile(t.Context(), intent)

	if after := natRows(m); !maps.Equal(after, rowsBefore) {
		t.Errorf("partial drift pass changed NAT rows: before %v, after %v", rowsBefore, after)
	}
	if got := b.neighCalls(); got != neighBefore {
		t.Errorf("partial drift pass touched neighbour state %d times", got-neighBefore)
	}

	if got := b.bindCount(publicA); got != bindsA {
		t.Errorf("partial drift pass re-bound %s, which replaces its route (%d binds, want %d)", publicA, got, bindsA)
	}
	if got := b.bindCount(publicB); got != bindsB {
		t.Errorf("partial drift pass re-bound %s (%d binds, want %d)", publicB, got, bindsB)
	}
}

// An associated EIP must name its addresses and VPC. A record that lacks one is
// unusable, and its absence must not remove the live row it stands for.
func TestS7_DriftPassKeepsAnEIPWhoseAssociatedRecordLacksAnAddress(t *testing.T) {
	t.Parallel()
	const userEIP = "203.0.113.20"
	record := func(publicIP, privateIP, vpc string) []byte {
		return mustJSON(t, ec2eip.EIPRecord{
			AllocationId: "eipalloc-a", PublicIp: publicIP, State: "associated",
			ENIId: "eni-a", PrivateIp: privateIP, MacAddress: "02:00:00:00:00:0a", VpcId: vpc,
		})
	}
	cases := map[string][]byte{
		"no public IP":  record("", "10.0.1.10", "vpc-a"),
		"no private IP": record(userEIP, "", "vpc-a"),
		"no VPC":        record(userEIP, "10.0.1.10", ""),
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			js := startKV(t)
			f := twoVPCFixture(t)
			f.set(ec2vpc.KVBucketENIs, "acct.eni-b", mustJSON(t, eniRecord("eni-b", "subnet-b", "vpc-b", "10.1.1.10", "02:00:00:00:00:0b", "sg-b", publicB)))
			f.set(ec2eip.KVBucketEIPs, "acct.eipalloc-a", record(userEIP, "10.0.1.10", "vpc-a"))
			f.seed(t, js)
			b := newRoutedBinder()
			r, m := newRoutedReconciler(t, js, b)
			convergeFromKV(t, r, js)
			requireStampedRow(t, m, userEIP, "eni-a")

			putRaw(t, js, ec2eip.KVBucketEIPs, "acct.eipalloc-a", bad)
			intent, err := LoadIntentFromKV(t.Context(), js, "us-east-1a")
			if err != nil {
				t.Fatalf("LoadIntentFromKV: %v", err)
			}
			err = r.Reconcile(t.Context(), intent)

			if findNATByExternal(m, "dnat_and_snat", userEIP) == nil {
				t.Errorf("drift pass deleted the live row for %s", userEIP)
			}
			if !b.has(userEIP) {
				t.Errorf("drift pass removed the host binding for %s (unbinds %v)", userEIP, b.unbinds)
			}
			if !errors.Is(err, ErrPassIncomplete) {
				t.Errorf("Reconcile = %v, want ErrPassIncomplete", err)
			}
		})
	}
}

// natgwFixture is vpc-a with a private subnet routed through nat-1.
func natgwFixture(t *testing.T, publicIP string) kvFixture {
	t.Helper()
	f := twoVPCFixture(t)
	f.set(ec2natgw.KVBucketNatGateways, "acct.nat-1", natgwRecord(t, publicIP))
	f.set(ec2routetable.KVBucketRouteTables, "acct.rtb-priv", mustJSON(t, ec2routetable.RouteTableRecord{
		RouteTableId: "rtb-priv", VpcId: "vpc-a",
		Routes:       []ec2routetable.RouteRecord{{DestinationCidrBlock: "0.0.0.0/0", NatGatewayId: "nat-1", State: "active"}},
		Associations: []ec2routetable.AssociationRecord{{AssociationId: "rtbassoc-a", SubnetId: "subnet-a"}},
	}))
	return f
}

func natgwRecord(t *testing.T, publicIP string) []byte {
	return mustJSON(t, ec2natgw.NatGatewayRecord{
		NatGatewayId: "nat-1", VpcId: "vpc-a", SubnetId: "subnet-a", PublicIp: publicIP, State: "available",
	})
}

// Under partial intent a NAT gateway's subnet SNAT row is created if missing,
// never replaced: the address intent now holds may be the unusable one.
func TestS7_NATGatewayPassKeepsTheLiveSNATRowWhenPartial(t *testing.T) {
	t.Parallel()
	const live = "198.51.100.5"
	cases := []struct {
		name  string
		fault func(t *testing.T, js jetstream.JetStream)
	}{
		{"nat gateway address does not parse", func(t *testing.T, js jetstream.JetStream) {
			putRaw(t, js, ec2natgw.KVBucketNatGateways, "acct.nat-1", natgwRecord(t, "198.51.100.999"))
		}},
		{"another record is bad and the address changed", func(t *testing.T, js jetstream.JetStream) {
			putRaw(t, js, ec2vpc.KVBucketSecurityGroups, "acct.sg-b", poison)
			putRaw(t, js, ec2natgw.KVBucketNatGateways, "acct.nat-1", natgwRecord(t, "198.51.100.6"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			js := startKV(t)
			natgwFixture(t, live).seed(t, js)
			r, m := newTestReconciler(t)
			ctx := t.Context()
			intent, err := LoadIntentFromKV(ctx, js, "us-east-1a")
			if err != nil {
				t.Fatalf("LoadIntentFromKV: %v", err)
			}
			actual, err := scanActual(ctx, r.ovn)
			if err != nil {
				t.Fatalf("scanActual: %v", err)
			}
			r.applyVPCs(ctx, intent, actual, &passResult{})
			converge := &passResult{}
			r.applyNATGWs(ctx, intent, actual, converge)
			if len(converge.failures) != 0 {
				t.Fatalf("converging applyNATGWs: %+v", converge.failures)
			}
			if findNATByExternal(m, "snat", live) == nil {
				t.Fatalf("no snat row for %s after converging", live)
			}

			tc.fault(t, js)
			intent, err = LoadIntentFromKV(ctx, js, "us-east-1a")
			if err != nil {
				t.Fatalf("LoadIntentFromKV: %v", err)
			}
			err = r.Reconcile(ctx, intent)

			if findNATByExternal(m, "snat", live) == nil {
				t.Errorf("NAT gateway pass removed the live snat row for %s", live)
			}
			if intent.TopologyIncomplete == nil {
				t.Errorf("intent read as complete after the fault")
			}
			if !errors.Is(err, ErrPassIncomplete) {
				t.Errorf("Reconcile = %v, want ErrPassIncomplete", err)
			}
		})
	}
}

// The VPC service creates the topology buckets at start, so one that is gone
// later is lost state, not an empty cluster, and must not authorize cleanup.
func TestS7_HostIngressKeepsRoutesWhenARequiredBucketIsGone(t *testing.T) {
	t.Parallel()
	js := startKV(t)
	f := kvFixture{}
	f.set(ec2vpc.KVBucketVPCs, "acct.vpc-a", mustJSON(t, ec2vpc.VPCRecord{VpcId: "vpc-a", CidrBlock: "10.0.0.0/16"}))
	f.set(ec2igw.KVBucketIGW, "acct.igw-a", mustJSON(t, ec2igw.IGWRecord{InternetGatewayId: "igw-a", VpcId: "vpc-a", State: "available"}))
	f.seed(t, js)
	m := mock.New()
	seedGatewayLRP(t, m, "vpc-a", "100.127.0.10")
	routes := &hostRouteTable{routes: map[string]string{}}
	h := &HostIngress{OVN: m, Routes: routes, LoadIntent: kvLoader(js)}
	if _, err := h.Pass(t.Context()); err != nil {
		t.Fatalf("converging pass: %v", err)
	}
	before, _ := routes.List(t.Context())

	if err := js.DeleteKeyValue(t.Context(), ec2vpc.KVBucketVPCs); err != nil {
		t.Fatalf("delete VPC bucket: %v", err)
	}
	_, err := h.Pass(t.Context())

	after, _ := routes.List(t.Context())
	if !maps.Equal(after, before) {
		t.Errorf("host ingress routes changed after the VPC bucket was lost: before %v, after %v", before, after)
	}
	if err == nil {
		t.Errorf("Pass = nil, want an error naming the lost bucket")
	}
}

// An available NAT gateway must name its VPC and address. A record that lacks
// one is unusable, so intent is incomplete and its live state stays.
func TestLoadIntentFromKV_AvailableNATGatewayWithABlankFieldIsPartial(t *testing.T) {
	t.Parallel()
	cases := map[string][]byte{
		"no public IP": mustJSON(t, ec2natgw.NatGatewayRecord{NatGatewayId: "nat-1", VpcId: "vpc-a", SubnetId: "subnet-a", State: "available"}),
		"no VPC":       mustJSON(t, ec2natgw.NatGatewayRecord{NatGatewayId: "nat-1", SubnetId: "subnet-a", PublicIp: "198.51.100.5", State: "available"}),
	}
	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			js := startKV(t)
			f := natgwFixture(t, "198.51.100.5")
			f.set(ec2natgw.KVBucketNatGateways, "acct.nat-1", rec)
			f.seed(t, js)

			_, err := partialLoad(t, js)
			if err == nil {
				t.Errorf("an available NAT gateway with a blank field read as complete intent")
			}
		})
	}
}

// partialLoad returns the partial-intent marker as an error, so a test can assert it
// without naming the field.
func partialLoad(t *testing.T, js jetstream.JetStream) (IntentState, error) {
	t.Helper()
	intent, err := LoadIntentFromKV(t.Context(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}
	return intent, intent.TopologyIncomplete
}

// natRows summarises every NAT row as type, addresses and owner, so a test can
// tell whether a pass changed any of them.
func natRows(m *mock.Client) map[string]string {
	out := map[string]string{}
	for _, n := range m.NATs {
		out[n.Type+"|"+n.ExternalIP+"|"+n.LogicalIP] = n.ExternalIDs["spinifex:logical_port"] + "|" + n.ExternalIDs["spinifex:vpc_id"]
	}
	return out
}

// A NAT gateway record with a blank address leaves intent partial, so neither
// the per-node pass nor the EIP sweep removes the gateway's live host binding.
func TestS7_NATGatewayWithABlankAddressKeepsItsHostBinding(t *testing.T) {
	t.Parallel()
	const live = "198.51.100.5"
	js := startKV(t)
	natgwFixture(t, live).seed(t, js)
	b := newRoutedBinder()
	r, m := newRoutedReconciler(t, js, b)
	seedGatewayLRP(t, m, "vpc-a", "100.127.0.10")
	if err := r.ReconcileHostEIPs(t.Context()); err != nil {
		t.Fatalf("converging ReconcileHostEIPs: %v", err)
	}
	if !b.has(live) {
		t.Fatalf("host bindings after converging = %v, want %s", b.bound, live)
	}

	putRaw(t, js, ec2natgw.KVBucketNatGateways, "acct.nat-1", mustJSON(t, ec2natgw.NatGatewayRecord{
		NatGatewayId: "nat-1", VpcId: "vpc-a", SubnetId: "subnet-a", State: "available",
	}))
	_ = r.ReconcileHostEIPs(t.Context())
	intent, err := LoadIntentFromKV(t.Context(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}
	r.pruneOrphanEIPs(t.Context(), intent, &passResult{})

	if !b.has(live) {
		t.Errorf("the NAT gateway's host binding for %s was removed (unbinds %v)", live, b.unbinds)
	}
}

// Every topology bucket is created by its EC2 service at start, so losing any
// of them later leaves intent partial.
func TestLoadIntentFromKV_AnyLostTopologyBucketIsPartial(t *testing.T) {
	t.Parallel()
	for _, bucket := range []string{
		ec2vpc.KVBucketVPCs, ec2vpc.KVBucketSubnets, ec2vpc.KVBucketSecurityGroups, ec2vpc.KVBucketENIs,
		ec2igw.KVBucketIGW, ec2eip.KVBucketEIPs, ec2routetable.KVBucketRouteTables, ec2natgw.KVBucketNatGateways,
	} {
		t.Run(bucket, func(t *testing.T) {
			t.Parallel()
			js := startKV(t)
			f := natgwFixture(t, "198.51.100.5")
			f.set(ec2igw.KVBucketIGW, "acct.igw-a", mustJSON(t, ec2igw.IGWRecord{InternetGatewayId: "igw-a", VpcId: "vpc-a", State: "available"}))
			f.set(ec2eip.KVBucketEIPs, "acct.eipalloc-x", mustJSON(t, ec2eip.EIPRecord{AllocationId: "eipalloc-x", PublicIp: "203.0.113.30", State: "allocated"}))
			f.seed(t, js)
			if err := js.DeleteKeyValue(t.Context(), bucket); err != nil {
				t.Fatalf("delete %s: %v", bucket, err)
			}
			if _, err := partialLoad(t, js); err == nil {
				t.Errorf("intent read as complete after %s was lost", bucket)
			}
		})
	}
}

// A keyed ENI record that decodes to zero fields is present but unusable, and
// must not let the sweep take the port it stands for.
func TestS7_DriftPassKeepsAGuestPortWhenItsENIRecordIsEmpty(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"{}", "null"} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			js := startKV(t)
			twoVPCFixture(t).seed(t, js)
			r, m := newTestReconciler(t)
			r.reloadIntent = kvLoader(js)
			r.localPorts = noLocalTaps
			convergeFromKV(t, r, js)

			putRaw(t, js, ec2vpc.KVBucketENIs, "acct.eni-a", []byte(raw))
			intent, err := LoadIntentFromKV(t.Context(), js, "us-east-1a")
			if err != nil {
				t.Fatalf("LoadIntentFromKV: %v", err)
			}
			err = r.Reconcile(t.Context(), intent)

			if !hasPort(m, "eni-a") {
				t.Errorf("drift pass deleted port-eni-a, whose record is present but empty")
			}
			if !errors.Is(err, ErrPassIncomplete) {
				t.Errorf("Reconcile = %v, want ErrPassIncomplete", err)
			}
		})
	}
}

// ovnSnapshot serialises every row the OVN mock holds, so a test can tell
// whether a pass changed anything at all.
func ovnSnapshot(t *testing.T, m *mock.Client) string {
	t.Helper()
	// Marshal through a map: encoding/json sorts map keys, so equal state gives
	// equal bytes, and the mock's field names are the snapshot keys.
	raw, err := json.Marshal(map[string]any{"ovn": m})
	if err != nil {
		t.Fatalf("marshal OVN mock: %v", err)
	}
	return string(raw)
}

// While intent is incomplete, no stage of the drift pass and neither per-node
// loop may change OVN or the host, not even to apply a healthy record. The
// first pass after the record is repaired applies what was held.
func TestS7_PartialIntentChangesNothingUntilRepaired(t *testing.T) {
	t.Parallel()
	js := startKV(t)
	f := publicIPFixture(t)
	f.set(ec2igw.KVBucketIGW, "acct.igw-a", mustJSON(t, ec2igw.IGWRecord{InternetGatewayId: "igw-a", VpcId: "vpc-a", State: "available"}))
	f.seed(t, js)
	b := newRoutedBinder()
	r, m := newRoutedReconciler(t, js, b)
	seedGatewayLRP(t, m, "vpc-b", "100.127.0.11")
	convergeFromKV(t, r, js)
	if err := r.ReconcileHostEIPs(t.Context()); err != nil {
		t.Fatalf("converging ReconcileHostEIPs: %v", err)
	}
	routes := &hostRouteTable{routes: map[string]string{}}
	h := &HostIngress{OVN: m, Routes: routes, LoadIntent: kvLoader(js)}
	_, _ = h.Pass(t.Context())

	ovnBefore := ovnSnapshot(t, m)
	routesBefore, _ := routes.List(t.Context())
	b.mu.Lock()
	bindsBefore, unbindsBefore := maps.Clone(b.binds), len(b.unbinds)
	b.mu.Unlock()
	neighBefore := b.neighCalls()

	// The SG record goes bad, and a healthy ENI is added while it is bad.
	good := f[ec2vpc.KVBucketSecurityGroups]["acct.sg-a"]
	putRaw(t, js, ec2vpc.KVBucketSecurityGroups, "acct.sg-a", poison)
	addThirdENI(t, js)
	intent, err := LoadIntentFromKV(t.Context(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}
	passErr := r.Reconcile(t.Context(), intent)
	hostErr := r.ReconcileHostEIPs(t.Context())
	_, ingressErr := h.Pass(t.Context())

	if after := ovnSnapshot(t, m); after != ovnBefore {
		t.Errorf("the partial drift pass changed OVN state")
	}
	if after, _ := routes.List(t.Context()); !maps.Equal(after, routesBefore) {
		t.Errorf("host ingress routes changed on partial intent: before %v, after %v", routesBefore, after)
	}
	b.mu.Lock()
	bindsAfter, unbindsAfter := maps.Clone(b.binds), len(b.unbinds)
	b.mu.Unlock()
	if !maps.Equal(bindsAfter, bindsBefore) || unbindsAfter != unbindsBefore {
		t.Errorf("host EIP bindings changed on partial intent: binds %v -> %v, unbinds %d -> %d",
			bindsBefore, bindsAfter, unbindsBefore, unbindsAfter)
	}
	if got := b.neighCalls(); got != neighBefore {
		t.Errorf("neighbour state touched %d times on partial intent", got-neighBefore)
	}
	if !errors.Is(passErr, ErrPassIncomplete) || hostErr == nil || ingressErr == nil {
		t.Errorf("partial passes not reported: drift %v, host EIP %v, ingress %v", passErr, hostErr, ingressErr)
	}

	putRaw(t, js, ec2vpc.KVBucketSecurityGroups, "acct.sg-a", good)
	intent, err = LoadIntentFromKV(t.Context(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV after repair: %v", err)
	}
	if err := r.Reconcile(t.Context(), intent); err != nil {
		t.Errorf("Reconcile after repair = %v, want a clean pass", err)
	}
	if !hasPort(m, "eni-c") {
		t.Errorf("the first pass after repair did not apply port-eni-c, held while intent was partial")
	}
}

// A record of any topology kind that decodes to no identity is present but
// unusable, so intent is partial rather than missing that resource.
func TestLoadIntentFromKV_AnEmptyRecordOfAnyKindIsPartial(t *testing.T) {
	t.Parallel()
	for _, bucket := range []string{
		ec2vpc.KVBucketVPCs, ec2vpc.KVBucketSubnets, ec2vpc.KVBucketSecurityGroups, ec2vpc.KVBucketENIs,
		ec2igw.KVBucketIGW, ec2eip.KVBucketEIPs, ec2routetable.KVBucketRouteTables, ec2natgw.KVBucketNatGateways,
	} {
		for _, raw := range []string{"{}", "null"} {
			t.Run(bucket+" "+raw, func(t *testing.T) {
				t.Parallel()
				js := startKV(t)
				twoVPCFixture(t).seed(t, js)
				putRaw(t, js, bucket, "acct.empty", []byte(raw))
				if _, err := partialLoad(t, js); err == nil {
					t.Errorf("an empty %s record read as complete intent", bucket)
				}
			})
		}
	}
}

// An empty SG record must not let the sweep delete its live port group, which
// would drop the group's ACLs from every port in it.
func TestS7_DriftPassKeepsAPortGroupWhenItsSGRecordIsEmpty(t *testing.T) {
	t.Parallel()
	js := startKV(t)
	twoVPCFixture(t).seed(t, js)
	r, m := newTestReconciler(t)
	r.reloadIntent = kvLoader(js)
	r.localPorts = noLocalTaps
	convergeFromKV(t, r, js)

	putRaw(t, js, ec2vpc.KVBucketSecurityGroups, "acct.sg-a", []byte("{}"))
	intent, err := LoadIntentFromKV(t.Context(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}
	_ = r.Reconcile(t.Context(), intent)

	if _, ok := m.PortGroups[topology.SecurityGroupPortGroup("sg-a")]; !ok {
		t.Errorf("drift pass deleted the sg-a port group, whose record is present but empty")
	}
}

// An empty IGW record must not let the host ingress pass remove its route.
func TestS7_HostIngressKeepsRoutesWhenAnIGWRecordIsEmpty(t *testing.T) {
	t.Parallel()
	js := startKV(t)
	f := kvFixture{}
	f.set(ec2vpc.KVBucketVPCs, "acct.vpc-a", mustJSON(t, ec2vpc.VPCRecord{VpcId: "vpc-a", CidrBlock: "10.0.0.0/16"}))
	f.set(ec2igw.KVBucketIGW, "acct.igw-a", mustJSON(t, ec2igw.IGWRecord{InternetGatewayId: "igw-a", VpcId: "vpc-a", State: "available"}))
	f.seed(t, js)
	m := mock.New()
	seedGatewayLRP(t, m, "vpc-a", "100.127.0.10")
	routes := &hostRouteTable{routes: map[string]string{}}
	h := &HostIngress{OVN: m, Routes: routes, LoadIntent: kvLoader(js)}
	if _, err := h.Pass(t.Context()); err != nil {
		t.Fatalf("converging pass: %v", err)
	}
	before, _ := routes.List(t.Context())

	putRaw(t, js, ec2igw.KVBucketIGW, "acct.igw-a", []byte("null"))
	_, _ = h.Pass(t.Context())

	if after, _ := routes.List(t.Context()); !maps.Equal(after, before) {
		t.Errorf("host ingress routes changed after the IGW record went empty: before %v, after %v", before, after)
	}
}

// A route whose destination does not parse is tenant input, not corruption:
// one tenant's bad route must not stop reconcile for every other tenant.
func TestLoadIntentFromKV_AnUnparsableRouteDoesNotStopReconcile(t *testing.T) {
	t.Parallel()
	js := startKV(t)
	f := twoVPCFixture(t)
	f.set(ec2routetable.KVBucketRouteTables, "acct.rtb-b", mustJSON(t, ec2routetable.RouteTableRecord{
		RouteTableId: "rtb-b", VpcId: "vpc-b", IsMain: true,
		Routes: []ec2routetable.RouteRecord{{DestinationCidrBlock: "not-a-cidr", GatewayId: "igw-b", State: "active"}},
	}))
	f.seed(t, js)
	if _, err := partialLoad(t, js); err != nil {
		t.Errorf("one tenant's unparsable route made intent partial for every tenant: %v", err)
	}
}

// failingScanOVN fails every listing, so a pass that reads OVN at all fails
// with a scan error instead of the incomplete-intent one.
type failingScanOVN struct {
	*mock.Client
}

func (failingScanOVN) ListLogicalRouters(context.Context) ([]nbdb.LogicalRouter, error) {
	return nil, errors.New("OVN must not be read while intent is incomplete")
}

// Both entry points stop at the gate, before any OVN read.
func TestS7_BothReconcileEntryPointsStopBeforeReadingOVN(t *testing.T) {
	t.Parallel()
	js := startKV(t)
	twoVPCFixture(t).seed(t, js)
	putRaw(t, js, ec2vpc.KVBucketENIs, "acct.eni-a", poison)
	intent, err := LoadIntentFromKV(t.Context(), js, "us-east-1a")
	if err != nil {
		t.Fatalf("LoadIntentFromKV: %v", err)
	}
	r, m := newTestReconciler(t)
	r.ovn = failingScanOVN{Client: m}

	for name, run := range map[string]func(context.Context, IntentState) error{
		"Reconcile": r.Reconcile, "ReconcileApplyOnly": r.ReconcileApplyOnly,
	} {
		err := run(t.Context(), intent)
		if !errors.Is(err, ErrPassIncomplete) || strings.Contains(err.Error(), "scan actual") {
			t.Errorf("%s = %v, want ErrPassIncomplete before any OVN read", name, err)
		}
	}
}
