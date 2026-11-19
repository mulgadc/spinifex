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
			assert.Equal(t, tc.probeErr == nil, h.loop.status("n1").ProbePassed)
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
	assert.False(t, h.loop.status("n1").GateReady)
	assert.Equal(t, endpointProbeRetry, revisit)

	h.gateErr = nil
	revisit, err = h.loop.reconcile(t.Context())
	require.NoError(t, err)
	assert.True(t, h.loop.status("n1").GateReady)
	assert.Zero(t, revisit)

	h.gateErr = errors.New("gate restarted")
	_, err = h.loop.reconcile(t.Context())
	require.NoError(t, err)
	assert.True(t, h.loop.status("n1").GateReady)
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
	assert.False(t, h.loop.status("n1").GateReady)

	h.gateErr = nil
	_, err = h.loop.reconcile(t.Context())
	require.NoError(t, err)
	assert.True(t, h.loop.status("n1").GateReady)
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
	assert.False(t, h.loop.status("n1").GateReady)
	assert.Empty(t, h.gateAsked)
}

// The status counts endpoints, not the subnets they govern.
func TestEndpointLoop_CountsEachEndpointOnce(t *testing.T) {
	h := newLoopHarness(false)
	ep := loopEndpoints["subnet-1"]
	h.endpoints = map[string]projection.GatewayEndpoint{"subnet-1": ep, "subnet-2": ep}
	_, err := h.loop.reconcile(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, h.loop.status("n1").Endpoints)
}

// A node whose proxy cannot present its certificate resets every connection,
// so it never reads ready, with or without a gate of its own.
func TestEndpointLoop_UnusableCredentialsAreNeverReady(t *testing.T) {
	for _, withGate := range []bool{false, true} {
		h := newLoopHarness(withGate)
		h.loop.dialErr = errors.New("no client key")
		revisit, err := h.loop.reconcile(t.Context())
		require.NoError(t, err)
		assert.False(t, h.loop.status("n1").GateReady, "with gate %v", withGate)
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
	st := h.loop.status("n1")
	assert.Equal(t, uint64(7), st.Revision)
	assert.Equal(t, 1, st.Endpoints)

	h.loadErr = errors.New("kv down")
	_, err = h.loop.reconcile(t.Context())
	require.Error(t, err)
	st = h.loop.status("n1")
	assert.Equal(t, uint64(7), st.Revision)
	assert.Zero(t, st.Endpoints, "a withdrawn view admits by no endpoint")

	h.loadErr, h.endpoints = nil, nil
	h.loop.load = load
	_, err = h.loop.reconcile(t.Context())
	require.NoError(t, err)
	st = h.loop.status("n1")
	assert.Equal(t, uint64(8), st.Revision)
	assert.Zero(t, st.Endpoints)

	h.rev, h.revErr = 9, errors.New("kv down")
	_, err = h.loop.reconcile(t.Context())
	require.NoError(t, err)
	assert.Equal(t, uint64(8), h.loop.status("n1").Revision)
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
	assert.Equal(t, projection.EndpointProxyStatus{Node: "n1", Revision: 42, Sessions: 3, Endpoints: 1, ProbePassed: true, GateReady: true}, got)

	for _, body := range [][]byte{[]byte(`{"node":"n2"}`), []byte(`not json`)} {
		var refusal map[string]any
		require.NoError(t, json.Unmarshal(ask(body), &refusal))
		require.Len(t, refusal, 1, "%s", body)
		assert.NotEmpty(t, refusal["error"])
	}
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

// A rule naming the S3 prefix list allows exactly the list's entries, as this
// node derives them, on the rule's ports.
func TestSecurityGroupEgress_PrefixListRule(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: ec2vpc.KVBucketSecurityGroups})
	require.NoError(t, err)
	const account = "123456789012"
	cluster := &config.ClusterConfig{Node: "n1", Nodes: map[string]config.Config{
		"n1": {AdvertiseIP: "10.0.0.2"},
		"n2": {AdvertiseIP: "10.0.0.3"},
	}}
	putSecurityGroup(t, kv, account, ec2vpc.SecurityGroupRecord{GroupId: "sg-s3", EgressRules: []ec2vpc.SGRule{
		{IpProtocol: "tcp", FromPort: 8443, ToPort: 8443, PrefixListId: prefixlist.ID("ap-southeast-2")},
	}})
	egress := securityGroupEgress(js, s3PrefixLists("ap-southeast-2", cluster, func() string { return "" }))

	for dst, want := range map[string]bool{
		"10.0.0.2:8443": true,
		"10.0.0.3:8443": true,
		"10.0.0.4:8443": false,
		"10.0.0.2:443":  false,
	} {
		ok, err := egress(t.Context(), account, []string{"sg-s3"}, netip.MustParseAddrPort(dst))
		require.NoError(t, err)
		assert.Equal(t, want, ok, dst)
	}

	other := securityGroupEgress(js, s3PrefixLists("eu-west-1", cluster, func() string { return "" }))
	ok, err := other(t.Context(), account, []string{"sg-s3"}, netip.MustParseAddrPort("10.0.0.2:8443"))
	require.NoError(t, err)
	assert.False(t, ok, "another Region's list is unknown here and grants nothing")
}

// The source knows only the Region's S3 list; an unset Region is the default
// one, as the API's is.
func TestS3PrefixLists(t *testing.T) {
	cluster := &config.ClusterConfig{Node: "n1", Nodes: map[string]config.Config{"n1": {AdvertiseIP: "10.0.0.2"}}}
	lists := s3PrefixLists("", cluster, func() string { return "" })
	cidrs, ok := lists(prefixlist.ID(config.DefaultAWSRegion))
	require.True(t, ok)
	assert.Equal(t, []string{"10.0.0.2/32"}, cidrs)
	_, ok = lists("pl-0123456789abcdef0")
	assert.False(t, ok)

	cidrs, ok = s3PrefixLists("us-east-1", nil, func() string { return "" })(prefixlist.ID("us-east-1"))
	require.True(t, ok, "with no cluster config the list is known and empty")
	assert.Empty(t, cidrs)
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
