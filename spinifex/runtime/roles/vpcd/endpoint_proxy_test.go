package vpcd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/guestmetadata"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/prefixlist"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/domains/network/policy"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/mulgadc/spinifex/spinifex/runtime/daemon"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingSink is the proxy as the loop sees it, keeping what it was handed.
type recordingSink struct {
	mu          sync.Mutex
	views       []guestmetadata.EndpointView
	invalidated int
	sessions    int
}

var _ endpointViewSink = (*recordingSink)(nil)

func (s *recordingSink) Publish(v guestmetadata.EndpointView) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.views = append(s.views, v)
	return 0
}

func (s *recordingSink) last() guestmetadata.EndpointView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.views[len(s.views)-1]
}

func (s *recordingSink) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated++
}

func (s *recordingSink) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions
}

// loopHarness is an endpoint loop over a two-node cluster whose host side and
// projection are injected.
type loopHarness struct {
	loop      *endpointLoop
	sink      *recordingSink
	captures  [][]host.EndpointEntry
	probeErr  error
	loadErr   error
	endpoints map[string]projection.GatewayEndpoint
	gateErr   error
	gateAsked []netip.Addr
	rev       uint64
	revErr    error
}

// newLoopHarness builds the loop; ownGate makes n1 check its own gate rather
// than its peer's.
func newLoopHarness(ownGate bool) *loopHarness {
	h := &loopHarness{sink: &recordingSink{}}
	h.loop = &endpointLoop{
		probe: func(context.Context) error { return h.probeErr },
		capture: func(_ context.Context, want []host.EndpointEntry) error {
			h.captures = append(h.captures, want)
			return nil
		},
		cluster: &config.ClusterConfig{Node: "n1", Nodes: map[string]config.Config{
			"n1": {AdvertiseIP: "10.0.0.2", Predastore: config.PredastoreConfig{Host: "0.0.0.0:8443"}},
			"n2": {AdvertiseIP: "10.0.0.3", Predastore: config.PredastoreConfig{Host: "0.0.0.0:9443"}},
		}},
		mgmtIP: func() string { return "" },
		load: func(context.Context) (map[string]projection.GatewayEndpoint, error) {
			return h.endpoints, h.loadErr
		},
		proxy: h.sink,
		levelRevision: func(context.Context) (uint64, error) {
			return h.rev, h.revErr
		},
	}
	h.loop.ownGate = ownGate
	h.loop.gate = func(_ context.Context, addr netip.Addr) error {
		h.gateAsked = append(h.gateAsked, addr)
		return h.gateErr
	}
	return h
}

var (
	loopEntries = []netip.AddrPort{netip.MustParseAddrPort("10.0.0.2:8443"), netip.MustParseAddrPort("10.0.0.3:9443")}
	loopCapture = []host.EndpointEntry{
		{Addr: netip.MustParseAddr("10.0.0.2"), Port: 8443},
		{Addr: netip.MustParseAddr("10.0.0.3"), Port: 9443},
	}
	loopEndpoints = map[string]projection.GatewayEndpoint{"subnet-1": {ID: "vpce-0123456789abcdef0", VpcID: "vpc-0123456789abcdef0"}}
)

// Traffic is captured only while an endpoint exists and the kernel can capture
// it, but the proxy always learns the full prefix list.
func TestEndpointLoop_CapturesOnlyWhileAnEndpointExistsOnACapableNode(t *testing.T) {
	cases := []struct {
		name      string
		endpoints map[string]projection.GatewayEndpoint
		probeErr  error
		want      []host.EndpointEntry
		revisit   time.Duration
	}{
		{name: "no endpoint removes the capture", want: nil},
		{name: "an endpoint installs every entry", endpoints: loopEndpoints, want: loopCapture},
		{name: "a failed probe captures nothing", endpoints: loopEndpoints, probeErr: errors.New("no TPROXY"), revisit: endpointProbeRetry},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newLoopHarness(false)
			h.endpoints, h.probeErr = tc.endpoints, tc.probeErr
			revisit, err := h.loop.reconcile(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.revisit, revisit)
			require.Len(t, h.captures, 1)
			assert.Equal(t, tc.want, h.captures[0])
			require.Len(t, h.sink.views, 1)
			assert.Equal(t, loopEntries, h.sink.views[0].Entries)
			assert.Equal(t, tc.probeErr == nil, mustStatus(t, h.loop).ProbePassed)
		})
	}
}

// A pass that cannot read the projection withdraws the proxy's view and leaves
// the capture rules as they were.
func TestEndpointLoop_LoadFailureWithdrawsTheViewAndLeavesTheRules(t *testing.T) {
	h := newLoopHarness(false)
	h.loadErr = errors.New("kv down")
	_, err := h.loop.reconcile(t.Context())
	require.Error(t, err)
	assert.Equal(t, 1, h.sink.invalidated)
	assert.Empty(t, h.sink.views)
	assert.Empty(t, h.captures)
}

// The gate is checked at this node's own service address on every pass until
// it accepts once, then never again.
func TestEndpointLoop_GateReadinessIsCheckedUntilItHoldsThenSticks(t *testing.T) {
	h := newLoopHarness(true)
	h.gateErr = errors.New("handshake refused")
	revisit, err := h.loop.reconcile(t.Context())
	require.NoError(t, err)
	assert.False(t, mustStatus(t, h.loop).GateReady)
	assert.Equal(t, endpointProbeRetry, revisit)

	h.gateErr = nil
	revisit, err = h.loop.reconcile(t.Context())
	require.NoError(t, err)
	assert.True(t, mustStatus(t, h.loop).GateReady)
	assert.Zero(t, revisit)

	h.gateErr = errors.New("gate restarted")
	_, err = h.loop.reconcile(t.Context())
	require.NoError(t, err)
	assert.True(t, mustStatus(t, h.loop).GateReady)
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.2")}, h.gateAsked)
}

// A node running no Predastore is ready only once a peer's gate accepts its
// proxy, since every connection it proxies goes to a peer.
func TestEndpointLoop_ANodeWithoutAGateChecksAPeers(t *testing.T) {
	h := newLoopHarness(false)
	n1 := h.loop.cluster.Nodes["n1"]
	n1.Services = []string{"vpcd"}
	h.loop.cluster.Nodes["n1"] = n1
	h.gateErr = errors.New("certificate refused")
	_, err := h.loop.reconcile(t.Context())
	require.NoError(t, err)
	assert.False(t, mustStatus(t, h.loop).GateReady)

	h.gateErr = nil
	_, err = h.loop.reconcile(t.Context())
	require.NoError(t, err)
	assert.True(t, mustStatus(t, h.loop).GateReady)
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.0.0.3"), netip.MustParseAddr("10.0.0.3")}, h.gateAsked)
}

// A node with no peer running Predastore has no gate to check and stays not ready.
func TestEndpointLoop_NoGateAnywhereIsNotReady(t *testing.T) {
	h := newLoopHarness(false)
	for name, n := range h.loop.cluster.Nodes {
		n.Services = []string{"vpcd"}
		h.loop.cluster.Nodes[name] = n
	}
	_, err := h.loop.reconcile(t.Context())
	require.NoError(t, err)
	assert.False(t, mustStatus(t, h.loop).GateReady)
	assert.Empty(t, h.gateAsked)
}

// The status counts endpoints, not the subnets they govern.
func TestEndpointLoop_CountsEachEndpointOnce(t *testing.T) {
	h := newLoopHarness(false)
	ep := loopEndpoints["subnet-1"]
	h.endpoints = map[string]projection.GatewayEndpoint{"subnet-1": ep, "subnet-2": ep}
	_, err := h.loop.reconcile(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, mustStatus(t, h.loop).Endpoints)
}

// A node whose proxy cannot present its certificate resets every connection,
// so it never reads ready, with or without a gate of its own.
func TestEndpointLoop_UnusableCredentialsAreNeverReady(t *testing.T) {
	for _, withGate := range []bool{false, true} {
		h := newLoopHarness(withGate)
		h.loop.dialErr = errors.New("no client key")
		revisit, err := h.loop.reconcile(t.Context())
		require.NoError(t, err)
		assert.False(t, mustStatus(t, h.loop).GateReady, "with gate %v", withGate)
		assert.Equal(t, endpointProbeRetry, revisit)
		assert.Empty(t, h.gateAsked)
	}
}

// The revision acknowledged is the one read before the endpoints the published
// view was built from: a rollback written during the load waits for the next
// pass, and a pass that publishes nothing acknowledges nothing.
func TestEndpointLoop_AcknowledgesOnlyARevisionItsViewPostdates(t *testing.T) {
	h := newLoopHarness(false)
	h.rev, h.endpoints = 7, loopEndpoints
	load := h.loop.load
	h.loop.load = func(ctx context.Context) (map[string]projection.GatewayEndpoint, error) {
		h.rev = 8
		return load(ctx)
	}
	_, err := h.loop.reconcile(t.Context())
	require.NoError(t, err)
	st := mustStatus(t, h.loop)
	assert.Equal(t, uint64(7), st.Revision)
	assert.Equal(t, 1, st.Endpoints)

	h.loadErr = errors.New("kv down")
	_, err = h.loop.reconcile(t.Context())
	require.Error(t, err)
	st = mustStatus(t, h.loop)
	assert.Equal(t, uint64(7), st.Revision)
	assert.Zero(t, st.Endpoints, "a withdrawn view admits by no endpoint")

	h.loadErr, h.endpoints = nil, nil
	h.loop.load = load
	_, err = h.loop.reconcile(t.Context())
	require.NoError(t, err)
	st = mustStatus(t, h.loop)
	assert.Equal(t, uint64(8), st.Revision)
	assert.Zero(t, st.Endpoints)

	h.rev, h.revErr = 9, errors.New("kv down")
	_, err = h.loop.reconcile(t.Context())
	require.NoError(t, err)
	assert.Equal(t, uint64(8), mustStatus(t, h.loop).Revision)
}

// The status subject answers for its own node only; anything else gets an
// error and nothing that could be read as a status.
func TestEndpointLoop_StatusAnswersOnlyItsOwnNode(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	h := newLoopHarness(false)
	h.sink.sessions = 3
	h.rev = 42
	h.endpoints = loopEndpoints
	_, err := h.loop.reconcile(t.Context())
	require.NoError(t, err)
	sub, err := nc.Subscribe(projection.EndpointProxyStatusSubject("n1"), h.loop.serveStatus("n1"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	ask := func(body []byte) []byte {
		t.Helper()
		msg, err := nc.Request(projection.EndpointProxyStatusSubject("n1"), body, 2*time.Second)
		require.NoError(t, err)
		return msg.Data
	}

	var got projection.EndpointProxyStatus
	require.NoError(t, json.Unmarshal(ask([]byte(`{"node":"n1"}`)), &got))
	assert.Equal(t, projection.EndpointProxyStatus{Node: "n1", Revision: 42, Sessions: 3, Endpoints: 1, ProbePassed: true, GateReady: true, Build: featurelevel.Build}, got)

	for _, body := range [][]byte{[]byte(`{"node":"n2"}`), []byte(`not json`)} {
		var refusal map[string]any
		require.NoError(t, json.Unmarshal(ask(body), &refusal))
		require.Len(t, refusal, 1, "%s", body)
		assert.NotEmpty(t, refusal["error"])
	}
}

// The heartbeat's status never reads the ACL table, so an unreadable NB view
// cannot hold a node below level 2; only the drain's request reads and counts.
func TestEndpointLoop_OnlyTheDrainsRequestReadsACLs(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	h := newLoopHarness(false)
	h.loop.grant = &prefixListGrant{
		level:   grantedLevel(true, 4),
		applies: func() int { return 0 },
		acls:    func(context.Context) ([]nbdb.ACL, error) { return nil, errors.New("nb gone") },
	}
	sub, err := nc.Subscribe(projection.EndpointProxyStatusSubject("n1"), h.loop.serveStatus("n1"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	st, err := projection.RequestEndpointProxyStatus(t.Context(), nc, "n1")
	require.NoError(t, err)
	assert.Equal(t, uint64(4), st.LevelRevision)
	_, err = projection.RequestPrefixListStatus(t.Context(), nc, "n1")
	require.ErrorContains(t, err, "nb gone")
}

// The revision read is the feature-level record's latest version, and 0 before
// one is written.
func TestFeatureLevelRevision_ReadsTheLatestVersion(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ctx := t.Context()
	read := featureLevelRevision(js)
	rev, err := read(ctx)
	require.NoError(t, err)
	assert.Zero(t, rev)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: daemon.ClusterConfigBucket})
	require.NoError(t, err)
	rev, err = read(ctx)
	require.NoError(t, err)
	assert.Zero(t, rev)

	_, err = kv.Put(ctx, featurelevel.Key, []byte("1"))
	require.NoError(t, err)
	second, err := kv.Put(ctx, featurelevel.Key, []byte("2"))
	require.NoError(t, err)
	_, err = kv.Put(ctx, "other", []byte("x"))
	require.NoError(t, err)
	rev, err = read(ctx)
	require.NoError(t, err)
	assert.Equal(t, second, rev)
}

func putSecurityGroup(t *testing.T, kv jetstream.KeyValue, account string, rec ec2vpc.SecurityGroupRecord) {
	t.Helper()
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), account+"."+rec.GroupId, data)
	require.NoError(t, err)
}

// Egress is judged from the ENI's groups as stored, any group allowing it.
func TestSecurityGroupEgress_UsesTheStoredGroups(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: ec2vpc.KVBucketSecurityGroups})
	require.NoError(t, err)
	const account = "123456789012"
	putSecurityGroup(t, kv, account, ec2vpc.SecurityGroupRecord{GroupId: "sg-open", EgressRules: []ec2vpc.SGRule{
		{IpProtocol: "tcp", FromPort: 8443, ToPort: 8443, CidrIp: "10.0.0.0/24"},
	}})
	putSecurityGroup(t, kv, account, ec2vpc.SecurityGroupRecord{GroupId: "sg-web", EgressRules: []ec2vpc.SGRule{
		{IpProtocol: "tcp", FromPort: 443, ToPort: 443, CidrIp: "0.0.0.0/0"},
	}})
	dst := netip.MustParseAddrPort("10.0.0.2:8443")
	egress := securityGroupEgress(js, nil)

	cases := []struct {
		name    string
		account string
		groups  []string
		want    bool
	}{
		{name: "a group allows the port", account: account, groups: []string{"sg-web", "sg-open"}, want: true},
		{name: "no group allows the port", account: account, groups: []string{"sg-web"}},
		{name: "a deleted group grants nothing", account: account, groups: []string{"sg-gone"}},
		{name: "another account's group grants nothing", account: "210987654321", groups: []string{"sg-open"}},
		{name: "no groups", account: account},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := egress(t.Context(), tc.account, tc.groups, dst)
			require.NoError(t, err)
			assert.Equal(t, tc.want, ok)
		})
	}
}

func mustStatus(t *testing.T, l *endpointLoop) projection.EndpointProxyStatus {
	t.Helper()
	st, err := l.status(t.Context(), "n1", true)
	require.NoError(t, err)
	return st
}

// grantedLevel is a level watch that has observed level 3 active, or not.
func grantedLevel(granted bool, rev uint64) *levelWatch {
	w := &levelWatch{}
	w.granted.Store(granted)
	w.revision.Store(rev)
	return w
}

// A rule naming the S3 prefix list allows exactly the members of the list's NB
// set, which the ACLs match, on the rule's ports; an absent set, another
// Region's list, or a withdrawn grant allows nothing.
func TestSecurityGroupEgress_PrefixListRuleReadsTheNBSet(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: ec2vpc.KVBucketSecurityGroups})
	require.NoError(t, err)
	const account = "123456789012"
	id := prefixlist.ID("ap-southeast-2")
	putSecurityGroup(t, kv, account, ec2vpc.SecurityGroupRecord{GroupId: "sg-s3", EgressRules: []ec2vpc.SGRule{
		{IpProtocol: "tcp", FromPort: 8443, ToPort: 8443, PrefixListId: id},
	}})
	m := mock.New()
	set, err := policy.PrefixListAddressSet(id)
	require.NoError(t, err)
	grant := &prefixListGrant{level: grantedLevel(true, 1), set: nbPrefixListSet(m), id: id}
	egress := securityGroupEgress(js, grant.lists())
	allows := func(dst string) bool {
		ok, err := egress(t.Context(), account, []string{"sg-s3"}, netip.MustParseAddrPort(dst))
		require.NoError(t, err)
		return ok
	}

	assert.False(t, allows("10.0.0.2:8443"), "an absent set grants nothing")
	_, err = m.EnsureAddressSet(t.Context(), set, []string{"10.0.0.2/32", "10.0.0.3/32"})
	require.NoError(t, err)
	for dst, want := range map[string]bool{
		"10.0.0.2:8443": true,
		"10.0.0.3:8443": true,
		"10.0.0.4:8443": false,
		"10.0.0.2:443":  false,
	} {
		assert.Equal(t, want, allows(dst), dst)
	}

	grant.level.granted.Store(false)
	assert.False(t, allows("10.0.0.2:8443"), "a withdrawn grant allows nothing, as the ACL compiler then renders nothing")

	other := &prefixListGrant{level: grantedLevel(true, 1), set: nbPrefixListSet(m), id: prefixlist.ID("eu-west-1")}
	ok, err := securityGroupEgress(js, other.lists())(t.Context(), account, []string{"sg-s3"}, netip.MustParseAddrPort("10.0.0.2:8443"))
	require.NoError(t, err)
	assert.False(t, ok, "another Region's list is unknown here and grants nothing")
}

// The set's membership is the gossiped derivation, so two pass winners whose
// local bridge addresses differ converge it to the same members; an unset
// Region is the default one, as the API's is.
func TestS3PrefixLists_EveryPassWinnerDerivesTheSameMembers(t *testing.T) {
	nodes := map[string]config.Config{
		"n1": {AdvertiseIP: "10.0.0.2"},
		"n2": {AdvertiseIP: "10.0.0.3"},
		"n3": {Predastore: config.PredastoreConfig{Host: "0.0.0.0:8443"}},
	}
	onN1, id := s3PrefixLists("", &config.ClusterConfig{Node: "n1", Nodes: nodes})
	onN3, _ := s3PrefixLists("", &config.ClusterConfig{Node: "n3", Nodes: nodes})
	assert.Equal(t, prefixlist.ID(config.DefaultAWSRegion), id)
	a, ok := onN1(id)
	require.True(t, ok)
	b, ok := onN3(id)
	require.True(t, ok)
	assert.Equal(t, a, b)
	assert.Contains(t, a, "10.0.0.2/32")
	_, ok = onN1("pl-0123456789abcdef0")
	assert.False(t, ok)

	empty, id := s3PrefixLists("us-east-1", nil)
	cidrs, ok := empty(id)
	require.True(t, ok, "with no cluster config the list is known and empty")
	assert.Empty(t, cidrs)
}

// The status names vpcd's build and, read after the grant's revision, the
// applies that started while it held and the ACLs naming a prefix-list set.
func TestEndpointLoop_StatusReportsThePrefixListGrant(t *testing.T) {
	h := newLoopHarness(false)
	m := mock.New()
	applies := 0
	h.loop.grant = &prefixListGrant{
		level:   grantedLevel(false, 9),
		applies: func() int { return applies },
		acls:    m.ListACLs,
	}
	st := mustStatus(t, h.loop)
	assert.Equal(t, featurelevel.Build, st.Build)
	assert.Equal(t, uint64(9), st.LevelRevision)
	assert.Zero(t, st.PrefixListApplies)
	assert.Zero(t, st.PrefixListACLs)

	_, _, err := m.EnsurePortGroup(t.Context(), "pg_sg_a", nil)
	require.NoError(t, err)
	require.NoError(t, m.ReplaceACLs(t.Context(), "pg_sg_a", []ovn.ACLSpec{
		{Direction: "from-lport", Priority: 1000, Match: "inport == @pg_sg_a && ip4 && ip4.dst == $spx_pl_6ca54005", Action: "allow-related"},
		{Direction: "from-lport", Priority: 1000, Match: "inport == @pg_sg_a && ip4 && ip4.dst == 10.0.0.0/16", Action: "allow-related"},
	}))
	applies = 2
	st = mustStatus(t, h.loop)
	assert.Equal(t, 2, st.PrefixListApplies)
	assert.Equal(t, 1, st.PrefixListACLs)

	h.loop.grant.acls = func(context.Context) ([]nbdb.ACL, error) { return nil, errors.New("nb gone") }
	_, err = h.loop.status(t.Context(), "n1", true)
	require.Error(t, err, "an unreadable NB view is no answer, never a zero count")
	st, err = h.loop.status(t.Context(), "n1", false)
	require.NoError(t, err, "a status that does not ask for ACLs never reads them")
	assert.Equal(t, 2, st.PrefixListApplies)
	assert.Zero(t, st.PrefixListACLs)
}

// A drain poll that finds a prefix-list ACL left after the grant was withdrawn
// wakes the reconcile pass to remove it; while the grant holds it wakes none.
func TestEndpointLoop_DrainPollWakesThePassForALeftoverACL(t *testing.T) {
	m := mock.New()
	_, _, err := m.EnsurePortGroup(t.Context(), "pg_sg_a", nil)
	require.NoError(t, err)
	require.NoError(t, m.ReplaceACLs(t.Context(), "pg_sg_a", []ovn.ACLSpec{
		{Direction: "from-lport", Priority: 1000, Match: "inport == @pg_sg_a && ip4 && ip4.dst == $spx_pl_6ca54005", Action: "allow-related"},
	}))
	for _, granted := range []bool{true, false} {
		h := newLoopHarness(false)
		level := grantedLevel(granted, 3)
		level.changed = make(chan struct{}, 1)
		h.loop.grant = &prefixListGrant{level: level, applies: func() int { return 0 }, acls: m.ListACLs}
		assert.Equal(t, 1, mustStatus(t, h.loop).PrefixListACLs)
		assert.Equal(t, !granted, len(level.changed) == 1, "granted=%v", granted)
	}
}

// While level 3 holds, capture and the proxy's view use the gossiped
// derivation, ignoring this node's bridge address.
func TestEndpointLoop_GossipedDerivationWhileGranted(t *testing.T) {
	h := newLoopHarness(false)
	h.loop.cluster.Nodes["n1"] = config.Config{Predastore: config.PredastoreConfig{Host: "0.0.0.0:8443"}}
	h.loop.mgmtIP = func() string { return "10.9.9.9" }
	h.endpoints = loopEndpoints
	h.loop.grant = &prefixListGrant{level: grantedLevel(false, 1)}

	_, err := h.loop.reconcile(t.Context())
	require.NoError(t, err)
	assert.Contains(t, h.sink.last().Entries, netip.MustParseAddrPort("10.9.9.9:8443"))

	h.loop.grant.level.granted.Store(true)
	_, err = h.loop.reconcile(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, h.sink.last().Entries, netip.MustParseAddrPort("10.9.9.9:8443"))
}

// The feature-level read uses the bucket the daemon writes.
func TestFeatureLevelBucket_IsTheDaemonsClusterConfigBucket(t *testing.T) {
	assert.Equal(t, daemon.ClusterConfigBucket, featureLevelBucket)
}

type testPKI struct {
	dir    string
	caFile string
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	pool   *x509.CertPool
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "formation"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	p := &testPKI{dir: t.TempDir(), ca: ca, caKey: key, pool: x509.NewCertPool()}
	p.pool.AddCert(ca)
	p.caFile = filepath.Join(p.dir, "ca.pem")
	require.NoError(t, os.WriteFile(p.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	return p
}

// issue writes a leaf for 127.0.0.1 usable on both ends and returns its files.
func (p *testPKI) issue(t *testing.T, name string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	certFile, keyFile = filepath.Join(p.dir, name+".pem"), filepath.Join(p.dir, name+".key")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certFile, keyFile
}

// startGate is an endpoint listener that requires a client certificate from
// the formation CA and then waits for the PROXY header without answering.
func startGate(t *testing.T, p *testPKI) uint16 {
	t.Helper()
	certFile, keyFile := p.issue(t, "gate")
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	require.NoError(t, err)
	ln, err := tls.Listen("tcp4", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert}, ClientCAs: p.pool,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = c.Read(make([]byte, 1))
			}()
		}
	}()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

// The gate probe succeeds only when the gate accepts this node's certificate,
// which under TLS 1.3 is decided after the client's handshake returns.
func TestGateAccepts_OnlyAFormationCertificateIsAccepted(t *testing.T) {
	formation := newTestPKI(t)
	port := startGate(t, formation)
	loopback := netip.MustParseAddr("127.0.0.1")

	cert, key := formation.issue(t, "node")
	dial, err := newEndpointDialer(cert, key, formation.caFile, port)
	require.NoError(t, err)
	require.NoError(t, gateAccepts(dial)(t.Context(), loopback))

	rogue := newTestPKI(t)
	rogueCert, rogueKey := rogue.issue(t, "rogue")
	dial, err = newEndpointDialer(rogueCert, rogueKey, formation.caFile, port)
	require.NoError(t, err)
	require.Error(t, gateAccepts(dial)(t.Context(), loopback))

	dial, err = newEndpointDialer(cert, key, rogue.caFile, port)
	require.NoError(t, err)
	require.Error(t, gateAccepts(dial)(t.Context(), loopback), "a gate outside the formation is not trusted")
}

func TestNewEndpointDialer_RefusesMissingMaterial(t *testing.T) {
	p := newTestPKI(t)
	cert, key := p.issue(t, "node")
	_, err := newEndpointDialer(cert, key, filepath.Join(p.dir, "absent.pem"), 1)
	require.Error(t, err)
	_, err = newEndpointDialer(filepath.Join(p.dir, "absent.pem"), key, p.caFile, 1)
	require.Error(t, err)
	empty := filepath.Join(p.dir, "empty.pem")
	require.NoError(t, os.WriteFile(empty, []byte("no pem here"), 0o600))
	_, err = newEndpointDialer(cert, key, empty, 1)
	require.Error(t, err)
}

// A loop whose node name cannot be a subject token runs but serves no status.
func TestEndpointLoop_RunSubscribesOnlyForAValidNode(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	h := newLoopHarness(false)
	h.loop.cluster.Node = "bad.node"
	sub, err := h.loop.run(ctx, nc, js)
	require.NoError(t, err)
	assert.Nil(t, sub)

	h = newLoopHarness(false)
	sub, err = h.loop.run(ctx, nc, js)
	require.NoError(t, err)
	require.NotNil(t, sub)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	_, err = nc.Request(projection.EndpointProxyStatusSubject("n1"), []byte(`{"node":"n1"}`), 2*time.Second)
	require.NoError(t, err)
}
