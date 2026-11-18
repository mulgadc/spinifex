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
	"sync/atomic"
	"time"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/guestmetadata"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/prefixlist"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/network/host"
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
	// gate checks that this node's own gate accepts the proxy's mutual TLS;
	// nil when the node runs no Predastore.
	gate func(ctx context.Context, addr netip.Addr) error
	// dialErr is why the proxy cannot present this node's certificate; while
	// set, the node is never ready, whether or not it runs a gate.
	dialErr error
	// levelRevision reads the feature-level record's current revision.
	levelRevision func(ctx context.Context) (uint64, error)

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
	entries := prefixlist.Entries(l.cluster, mgmt)
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
	l.viewEndpoints.Store(int64(len(endpoints)))
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

// probeGate checks this node's own gate endpoint listener until it accepts the
// proxy once. A node running no Predastore has no gate to wait for.
func (l *endpointLoop) probeGate(ctx context.Context, mgmt string) {
	if l.gateReady.Load() || l.dialErr != nil {
		return
	}
	if l.gate == nil {
		l.gateReady.Store(true)
		return
	}
	addr, ok := localServiceAddr(l.cluster, mgmt)
	if !ok {
		slog.WarnContext(ctx, "vpcd: this node has no service address to check its gate endpoint listener on", "node", l.cluster.Node)
		return
	}
	if err := l.gate(ctx, addr); err != nil {
		slog.WarnContext(ctx, "vpcd: own gate endpoint listener not ready", "addr", addr.String(), "err", err)
		return
	}
	l.gateReady.Store(true)
	slog.InfoContext(ctx, "vpcd: own gate endpoint listener accepted the proxy", "addr", addr.String())
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

// status is the reply on this node's endpoint proxy status subject.
func (l *endpointLoop) status(node string) projection.EndpointProxyStatus {
	return projection.EndpointProxyStatus{
		Node:        node,
		Revision:    l.revision.Load(),
		Sessions:    l.proxy.Sessions(),
		Endpoints:   int(l.viewEndpoints.Load()),
		ProbePassed: l.probePassed.Load(),
		GateReady:   l.gateReady.Load(),
	}
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
			reply = l.status(node)
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
	return func(ctx context.Context) (uint64, error) {
		kv, err := js.KeyValue(ctx, featureLevelBucket)
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

// securityGroupEgress evaluates an ENI's security groups from their records,
// through the same rule conversion the ACL compiler is fed. A group whose
// record is gone grants nothing; any other read failure refuses.
func securityGroupEgress(js jetstream.JetStream) guestmetadata.EndpointEgressFunc {
	return func(ctx context.Context, accountID string, groupIDs []string, dst netip.AddrPort) (bool, error) {
		if len(groupIDs) == 0 {
			return false, nil
		}
		kv, err := js.KeyValue(ctx, ec2vpc.KVBucketSecurityGroups)
		if err != nil {
			return false, fmt.Errorf("open %s: %w", ec2vpc.KVBucketSecurityGroups, err)
		}
		var rules []policy.Rule
		for _, id := range groupIDs {
			entry, err := kv.Get(ctx, accountID+"."+id)
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
		return policy.EgressAllows(rules, dst.Addr(), "tcp", dst.Port()), nil
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
func setupEndpointProxy(cfg *Config, js jetstream.JetStream, imds *guestmetadata.IMDSServiceImpl) (*endpointLoop, error) {
	if cfg.Cluster == nil {
		slog.Warn("vpcd: no cluster config; the gateway endpoint proxy is not served")
		return nil, nil
	}
	dial, dialErr := newEndpointDialer(cfg.NodeTLSCert, cfg.NodeTLSKey, cfg.CACert, projection.EndpointListenerPort)
	if dialErr != nil {
		slog.Error("vpcd: endpoint proxy cannot present this node's certificate; every endpoint connection will be reset", "err", dialErr)
		dial = unavailableDialer(dialErr)
	}
	proxy, err := guestmetadata.NewEndpointProxy(guestmetadata.EndpointProxyConfig{
		Port:   host.EndpointProxyPort,
		Egress: securityGroupEgress(js),
		Dial:   dial,
		// Twice the resync, so only a loop that has stopped passing stops admitting.
		MaxAge: 2 * reconciler.DefaultResync,
	})
	if err != nil {
		return nil, fmt.Errorf("construct endpoint proxy: %w", err)
	}
	imds.EnableEndpointProxy(proxy)

	bridge := cfg.MgmtBridge
	if bridge == "" {
		bridge = defaultMgmtBridge
	}
	runner := host.NewExecRunner()
	l := &endpointLoop{
		probe: func(ctx context.Context) error { return host.ProbeEndpointCapture(ctx, runner) },
		capture: func(ctx context.Context, want []host.EndpointEntry) error {
			return host.EnsureEndpointCapture(ctx, runner, want)
		},
		cluster: cfg.Cluster,
		mgmtIP: func() string {
			ip, err := host.GetBridgeIPv4(bridge)
			if err != nil {
				slog.Debug("vpcd: management bridge address unavailable", "bridge", bridge, "err", err)
			}
			return ip
		},
		load: func(ctx context.Context) (map[string]projection.GatewayEndpoint, error) {
			return projection.LoadGatewayEndpoints(ctx, js)
		},
		proxy:         proxy,
		dialErr:       dialErr,
		levelRevision: featureLevelRevision(js),
	}
	if cfg.Cluster.Nodes[cfg.Cluster.Node].HasService("predastore") {
		l.gate = gateAccepts(dial)
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
