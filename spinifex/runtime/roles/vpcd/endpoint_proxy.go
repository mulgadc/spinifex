package vpcd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/guestmetadata"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/prefixlist"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/domains/network/policy"
	"github.com/mulgadc/spinifex/spinifex/domains/network/reconcile"
	"github.com/mulgadc/spinifex/spinifex/foundation/lifecycle/reconciler"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// featureLevelBucket is the cluster-config bucket holding the feature-level
// record, named here rather than imported from the daemon.
const featureLevelBucket = "spinifex-cluster-config"

// defaultMgmtBridge is the management bridge when the node config names none.
const defaultMgmtBridge = "br-mgmt"

const (
	// endpointProbeRetry is how soon a pass whose capture or gate probe has not
	// yet passed asks to be run again, ahead of the resync.
	endpointProbeRetry = 30 * time.Second
	// endpointDialTimeout bounds the TCP connect to a node's endpoint listener.
	endpointDialTimeout = 5 * time.Second
	// gateVerdictWait is how long the gate probe waits for the gate to refuse
	// its certificate after the client's handshake completes.
	gateVerdictWait = time.Second
)

// endpointViewSink is the proxy the capture loop publishes each pass's view to.
type endpointViewSink interface {
	Publish(v guestmetadata.EndpointView) int
	Invalidate()
	Sessions() int
}

var _ endpointViewSink = (*guestmetadata.EndpointProxy)(nil)

// endpointLoop keeps this node's capture rules and the proxy's view in step
// with the gateway endpoints, and reports what the rollback drain and the
// node's advertised level need to know.
type endpointLoop struct {
	probe   func(ctx context.Context) error
	capture func(ctx context.Context, want []host.EndpointEntry) error
	cluster *config.ClusterConfig
	mgmtIP  func() string
	load    func(ctx context.Context) (map[string]projection.GatewayEndpoint, error)
	proxy   endpointViewSink
	// gate checks that a gate accepts the proxy's mutual TLS: this node's own
	// when ownGate, else a peer's, since this node's proxy dials only peers.
	gate    func(ctx context.Context, addr netip.Addr) error
	ownGate bool
	// dialErr is why the proxy cannot present this node's certificate; while
	// set, the node is never ready, whether or not it runs a gate.
	dialErr error
	// levelRevision reads the feature-level record's current revision.
	levelRevision func(ctx context.Context) (uint64, error)
	// grant is the prefix-list grant; nil on a vpcd that serves none.
	grant *prefixListGrant

	probePassed   atomic.Bool
	gateReady     atomic.Bool
	revision      atomic.Uint64 // the level revision read before the last published view
	viewEndpoints atomic.Int64  // endpoints in the view the proxy admits by
	probeFailed   atomic.Bool   // the last capture probe failed, so the next pass logs only a change
}

// reconcile is one node-local pass. A failed projection read withdraws the
// proxy's view and leaves the rules as they are; either way nothing is
// admitted that this pass cannot vouch for.
func (l *endpointLoop) reconcile(ctx context.Context) (time.Duration, error) {
	l.probeCapture(ctx)
	mgmt := l.mgmtIP()
	l.probeGate(ctx, mgmt)

	// Read before the endpoints, so an acknowledged revision means the view
	// published after it was loaded once that revision was written.
	rev, revErr := l.levelRevision(ctx)
	endpoints, err := l.load(ctx)
	if err != nil {
		l.proxy.Invalidate()
		l.viewEndpoints.Store(0)
		return l.revisit(), fmt.Errorf("load gateway endpoints: %w", err)
	}
	// While level 3 holds, the gossiped derivation, so capture, discovery and
	// the security-group set list one set of addresses on every node.
	listMgmt := mgmt
	if l.grant != nil && l.grant.level.Granted() {
		listMgmt = ""
	}
	entries := prefixlist.Entries(l.cluster, listMgmt)
	view := guestmetadata.EndpointView{Entries: make([]netip.AddrPort, 0, len(entries)), Endpoints: endpoints}
	var want []host.EndpointEntry
	for _, e := range entries {
		view.Entries = append(view.Entries, netip.AddrPortFrom(e.Addr, e.GatePort))
		if len(endpoints) > 0 && l.probePassed.Load() {
			want = append(want, host.EndpointEntry{Addr: e.Addr, Port: e.GatePort})
		}
	}
	// Published first, so a withdrawn endpoint's sessions close even if the
	// rule change below fails.
	l.proxy.Publish(view)
	ids := make(map[string]struct{}, len(endpoints))
	for _, ep := range endpoints {
		ids[ep.ID] = struct{}{}
	}
	l.viewEndpoints.Store(int64(len(ids)))
	if revErr != nil {
		slog.WarnContext(ctx, "vpcd: feature level unreadable; endpoint proxy revision not advanced", "err", revErr)
	} else {
		l.revision.Store(rev)
	}
	if err := l.capture(ctx, want); err != nil {
		return l.revisit(), fmt.Errorf("reconcile endpoint capture: %w", err)
	}
	return l.revisit(), nil
}

func (l *endpointLoop) revisit() time.Duration {
	if l.probePassed.Load() && l.gateReady.Load() {
		return 0
	}
	return endpointProbeRetry
}

// probeCapture runs the TPROXY capability probe until it passes once.
func (l *endpointLoop) probeCapture(ctx context.Context) {
	if l.probePassed.Load() {
		return
	}
	if err := l.probe(ctx); err != nil {
		if !l.probeFailed.Swap(true) {
			slog.WarnContext(ctx, "vpcd: kernel cannot capture gateway endpoint traffic; node not endpoint capable", "err", err)
		}
		return
	}
	l.probePassed.Store(true)
	slog.InfoContext(ctx, "vpcd: gateway endpoint capture probe passed")
}

// probeGate checks a gate endpoint listener until it accepts the proxy once:
// this node's own on a node running Predastore, else a peer's.
func (l *endpointLoop) probeGate(ctx context.Context, mgmt string) {
	if l.gateReady.Load() || l.dialErr != nil {
		return
	}
	addr, ok := l.gateAddr(mgmt)
	if !ok {
		slog.WarnContext(ctx, "vpcd: no gate address to check the endpoint listener on", "node", l.cluster.Node, "own_gate", l.ownGate)
		return
	}
	if err := l.gate(ctx, addr); err != nil {
		slog.WarnContext(ctx, "vpcd: gate endpoint listener not ready", "addr", addr.String(), "own_gate", l.ownGate, "err", err)
		return
	}
	l.gateReady.Store(true)
	slog.InfoContext(ctx, "vpcd: gate endpoint listener accepted the proxy", "addr", addr.String(), "own_gate", l.ownGate)
}

// gateAddr is the address probeGate checks: this node's own service address,
// or the first peer running Predastore.
func (l *endpointLoop) gateAddr(mgmt string) (netip.Addr, bool) {
	if l.ownGate {
		return localServiceAddr(l.cluster, mgmt)
	}
	for _, ep := range config.ServiceEndpoints(l.cluster, mgmt) {
		if ep.Node != l.cluster.Node && ep.Addr.Is4() && l.cluster.Nodes[ep.Node].HasService("predastore") {
			return ep.Addr, true
		}
	}
	return netip.Addr{}, false
}

// localServiceAddr is the prefix-list address of this node.
func localServiceAddr(cc *config.ClusterConfig, mgmt string) (netip.Addr, bool) {
	for _, ep := range config.ServiceEndpoints(cc, mgmt) {
		if ep.Node == cc.Node && ep.Addr.Is4() {
			return ep.Addr, true
		}
	}
	return netip.Addr{}, false
}

// statusWait bounds the NB read behind one status reply.
const statusWait = 2 * time.Second

// status is the reply on this node's endpoint proxy status subject.
func (l *endpointLoop) status(ctx context.Context, node string, countACLs bool) (projection.EndpointProxyStatus, error) {
	st := projection.EndpointProxyStatus{
		Node:        node,
		Revision:    l.revision.Load(),
		Sessions:    l.proxy.Sessions(),
		Endpoints:   int(l.viewEndpoints.Load()),
		ProbePassed: l.probePassed.Load(),
		GateReady:   l.gateReady.Load(),
		Build:       featurelevel.Build,
	}
	if l.grant == nil {
		return st, nil
	}
	// The revision first: an apply or ACL counted after it was read cannot
	// predate the grant it reports.
	st.LevelRevision = l.grant.level.Revision()
	st.PrefixListApplies = l.grant.applies()
	if !countACLs {
		return st, nil
	}
	acls, err := l.grant.acls(ctx)
	if err != nil {
		return st, fmt.Errorf("list ACLs: %w", err)
	}
	for _, a := range acls {
		if strings.Contains(a.Match, "$"+policy.PrefixListSetPrefix) {
			st.PrefixListACLs++
		}
	}
	return st, nil
}

// serveStatus answers a status request naming node, and refuses any other.
func (l *endpointLoop) serveStatus(node string) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var req projection.EndpointProxyStatusRequest
		var reply any
		switch err := json.Unmarshal(msg.Data, &req); {
		case err != nil:
			reply = statusRefusal{Error: "malformed endpoint proxy status request"}
		case req.Node != node:
			reply = statusRefusal{Error: fmt.Sprintf("endpoint proxy status for node %q answered by node %q", req.Node, node)}
		default:
			ctx, cancel := context.WithTimeout(context.Background(), statusWait)
			st, err := l.status(ctx, node, req.CountPrefixListACLs)
			cancel()
			if err != nil {
				reply = statusRefusal{Error: "endpoint proxy status unavailable: " + err.Error()}
			} else {
				reply = st
			}
		}
		data, err := json.Marshal(reply)
		if err != nil {
			slog.Error("vpcd: encode endpoint proxy status", "err", err)
			return
		}
		if err := msg.Respond(data); err != nil {
			slog.Warn("vpcd: endpoint proxy status reply failed", "err", err)
		}
	}
}

// statusRefusal is the whole reply to a request vpcd will not answer.
type statusRefusal struct {
	Error string `json:"error"`
}

// featureLevelRevision reads the feature-level record's revision; a record
// never written is revision 0.
func featureLevelRevision(js jetstream.JetStream) func(ctx context.Context) (uint64, error) {
	level := kvstore.NewBucket(js, kvstore.Config{Name: featureLevelBucket, AttachOnly: true})
	return func(ctx context.Context) (uint64, error) {
		kv, err := level.KV(ctx)
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return 0, nil
		}
		if err != nil {
			return 0, fmt.Errorf("open %s: %w", featureLevelBucket, err)
		}
		e, err := kv.Get(ctx, featurelevel.Key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return 0, nil
		}
		if err != nil {
			return 0, fmt.Errorf("read %s/%s: %w", featureLevelBucket, featurelevel.Key, err)
		}
		return e.Revision(), nil
	}
}

// s3PrefixLists resolves the Region's S3 prefix list to the entries every node
// derives from the gossiped node set alone, so any pass winner converges the
// set to the same members; it knows no other list.
func s3PrefixLists(region string, cluster *config.ClusterConfig) (policy.PrefixLists, string) {
	id := prefixlist.ID(regionOrDefault(region))
	return func(want string) ([]string, bool) {
		if want != id {
			return nil, false
		}
		entries := prefixlist.Entries(cluster, "")
		cidrs := make([]string, 0, len(entries))
		for _, e := range entries {
			cidrs = append(cidrs, e.CIDR())
		}
		return cidrs, true
	}, id
}

func regionOrDefault(region string) string {
	if region == "" {
		return config.DefaultAWSRegion
	}
	return region
}

// prefixListGrant is what the proxy and its status need of the prefix-list
// grant: whether it holds, the security-group applies that started while it
// did, and the NB ACLs.
type prefixListGrant struct {
	level   *levelWatch
	applies func() int
	acls    func(ctx context.Context) ([]nbdb.ACL, error)
	// set reads a prefix list's NB address set from vpcd's NB cache.
	set func(ctx context.Context, id string) ([]string, error)
	id  string
}

// lists is the proxy's view of the prefix lists: the members of the NB set the
// ACLs match, so the two enforce one membership. While the grant is withdrawn,
// or the set is absent or unreadable, a list grants nothing.
func (g *prefixListGrant) lists() policy.PrefixLists {
	return func(want string) ([]string, bool) {
		if want != g.id || !g.level.Granted() {
			return nil, false
		}
		members, err := g.set(context.Background(), want)
		if err != nil {
			return nil, false
		}
		return members, true
	}
}

// nbPrefixListSet reads a prefix list's address set from the NB cache.
func nbPrefixListSet(client ovn.Client) func(ctx context.Context, id string) ([]string, error) {
	return func(ctx context.Context, id string) ([]string, error) {
		name, err := policy.PrefixListAddressSet(id)
		if err != nil {
			return nil, err
		}
		as, err := client.GetAddressSet(ctx, name)
		if err != nil {
			return nil, err
		}
		return as.Addresses, nil
	}
}

// mgmtBridgeIP reads the management bridge's address on every call, since the
// bridge can be readdressed while vpcd runs.
func mgmtBridgeIP(bridge string) func() string {
	if bridge == "" {
		bridge = defaultMgmtBridge
	}
	return func() string {
		ip, err := host.GetBridgeIPv4(bridge)
		if err != nil {
			slog.Debug("vpcd: management bridge address unavailable", "bridge", bridge, "err", err)
		}
		return ip
	}
}

// securityGroupEgress evaluates an ENI's security groups from their records,
// through the same rule conversion the ACL compiler is fed. A group whose
// record is gone grants nothing; any other read failure refuses.
func securityGroupEgress(js jetstream.JetStream, lists policy.PrefixLists) guestmetadata.EndpointEgressFunc {
	groups := kvstore.NewBucket(js, kvstore.Config{Name: ec2vpc.KVBucketSecurityGroups, AttachOnly: true})
	return func(ctx context.Context, accountID string, groupIDs []string, dst netip.AddrPort) (bool, error) {
		if len(groupIDs) == 0 {
			return false, nil
		}
		kv, err := groups.KV(ctx)
		if err != nil {
			return false, fmt.Errorf("open %s: %w", ec2vpc.KVBucketSecurityGroups, err)
		}
		var rules []policy.Rule
		for _, id := range groupIDs {
			entry, err := kv.Get(ctx, kvstore.AccountKey(accountID, id))
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			if err != nil {
				return false, fmt.Errorf("read security group %s: %w", id, err)
			}
			var rec ec2vpc.SecurityGroupRecord
			if err := json.Unmarshal(entry.Value(), &rec); err != nil {
				return false, fmt.Errorf("decode security group %s: %w", id, err)
			}
			rules = append(rules, reconcile.SGRulesToPolicyRules(rec.EgressRules)...)
		}
		return policy.EgressAllows(rules, lists, dst.Addr(), "tcp", dst.Port()), nil
	}
}

// newEndpointDialer opens mutual-TLS sessions to a node's endpoint listener on
// port, presenting this node's certificate and trusting only the formation CA.
func newEndpointDialer(certFile, keyFile, caFile string, port uint16) (guestmetadata.EndpointDialFunc, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load node certificate: %w", err)
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read formation CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("formation CA %s holds no certificate", caFile)
	}
	return func(ctx context.Context, addr netip.Addr) (net.Conn, error) {
		d := tls.Dialer{
			NetDialer: &net.Dialer{Timeout: endpointDialTimeout},
			Config: &tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      pool,
				ServerName:   addr.String(),
				MinVersion:   tls.VersionTLS13,
			},
		}
		return d.DialContext(ctx, "tcp4", netip.AddrPortFrom(addr, port).String())
	}, nil
}

// unavailableDialer refuses every session, for a node whose certificate could
// not be loaded: its proxy resets every connection and its gate never reads ready.
func unavailableDialer(cause error) guestmetadata.EndpointDialFunc {
	return func(context.Context, netip.Addr) (net.Conn, error) {
		return nil, fmt.Errorf("endpoint channel unavailable: %w", cause)
	}
}

// gateAccepts reports whether the gate at addr completes a session with the
// proxy's certificate. TLS 1.3 finishes the client's handshake before the
// server has checked that certificate, so a refusal arrives on the first read.
func gateAccepts(dial guestmetadata.EndpointDialFunc) func(ctx context.Context, addr netip.Addr) error {
	return func(ctx context.Context, addr netip.Addr) error {
		c, err := dial(ctx, addr)
		if err != nil {
			return err
		}
		defer c.Close()
		if err := c.SetReadDeadline(time.Now().Add(gateVerdictWait)); err != nil {
			return err
		}
		_, err = c.Read(make([]byte, 1))
		if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
			return nil // waiting for the PROXY header, so the certificate was accepted
		}
		if err == nil {
			return errors.New("gate sent data before the proxy's header")
		}
		return fmt.Errorf("gate refused the session: %w", err)
	}
}

// endpointSources are the buckets whose changes alter which subnets route to
// an endpoint. Attach-only: vpcd reads them and never creates one.
func endpointSources(js jetstream.JetStream) []reconciler.Source {
	out := make([]reconciler.Source, 0, len(projection.EndpointBuckets)+1)
	for _, b := range projection.EndpointBuckets {
		out = append(out, reconciler.Fixed(kvstore.NewBucket(js, kvstore.Config{Name: b, AttachOnly: true}), ">"))
	}
	// A rollback writes the level record, and its drain waits on this pass.
	return append(out, reconciler.Fixed(kvstore.NewBucket(js, kvstore.Config{Name: featureLevelBucket, AttachOnly: true}), featurelevel.Key))
}

// setupEndpointProxy builds the proxy and hands it to IMDS, which must not yet
// be running. A node certificate that cannot be loaded leaves a proxy that
// resets every connection and a gate that never reads ready.
func setupEndpointProxy(cfg *Config, js jetstream.JetStream, imds *guestmetadata.IMDSServiceImpl, grant *prefixListGrant) (*endpointLoop, error) {
	if cfg.Cluster == nil {
		slog.Warn("vpcd: no cluster config; the gateway endpoint proxy is not served")
		return nil, nil
	}
	dial, dialErr := newEndpointDialer(cfg.NodeTLSCert, cfg.NodeTLSKey, cfg.CACert, projection.EndpointListenerPort)
	if dialErr != nil {
		slog.Error("vpcd: endpoint proxy cannot present this node's certificate; every endpoint connection will be reset", "err", dialErr)
		dial = unavailableDialer(dialErr)
	}
	mgmtIP := mgmtBridgeIP(cfg.MgmtBridge)
	proxy, err := guestmetadata.NewEndpointProxy(guestmetadata.EndpointProxyConfig{
		Port:   host.EndpointProxyPort,
		Egress: securityGroupEgress(js, grant.lists()),
		Dial:   dial,
		// Twice the resync, so only a loop that has stopped passing stops admitting.
		MaxAge: 2 * reconciler.DefaultResync,
	})
	if err != nil {
		return nil, fmt.Errorf("construct endpoint proxy: %w", err)
	}
	imds.EnableEndpointProxy(proxy)

	runner := host.NewExecRunner()
	l := &endpointLoop{
		probe: func(ctx context.Context) error { return host.ProbeEndpointCapture(ctx, runner) },
		capture: func(ctx context.Context, want []host.EndpointEntry) error {
			return host.EnsureEndpointCapture(ctx, runner, want)
		},
		cluster: cfg.Cluster,
		mgmtIP:  mgmtIP,
		load: func(ctx context.Context) (map[string]projection.GatewayEndpoint, error) {
			return projection.LoadGatewayEndpoints(ctx, js)
		},
		proxy:         proxy,
		dialErr:       dialErr,
		levelRevision: featureLevelRevision(js),
		grant:         grant,
		gate:          gateAccepts(dial),
		ownGate:       cfg.Cluster.Nodes[cfg.Cluster.Node].HasService("predastore"),
	}
	return l, nil
}

// run drives the loop and serves its status until ctx ends. The returned
// subscription is nil when the node name cannot be a subject token.
func (l *endpointLoop) run(ctx context.Context, nc *nats.Conn, js jetstream.JetStream) (*nats.Subscription, error) {
	// The node set is the config loaded at start, as the daemon's prefix list
	// is, so no gossip signal exists to trigger on; the resync is the backstop.
	go reconciler.Run(ctx, reconciler.Config{
		Name:      "vpc-endpoint-capture",
		Sources:   endpointSources(js),
		Reconcile: l.reconcile,
		Resync:    reconciler.DefaultResync,
	})
	node := l.cluster.Node
	if !featurelevel.ValidNode(node) {
		slog.Error("vpcd: node name is not a subject token; endpoint proxy status not served", "node", node)
		return nil, nil
	}
	sub, err := nc.Subscribe(projection.EndpointProxyStatusSubject(node), l.serveStatus(node))
	if err != nil {
		return nil, fmt.Errorf("subscribe endpoint proxy status: %w", err)
	}
	return sub, nil
}
