package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/vpcendpoint"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// proxyStatusWait bounds each node's vpcd answer to the drain.
const proxyStatusWait = 3 * time.Second

// gatewayEndpointDrain is level 2's rollback drain: gateway endpoints and the
// security-group rules naming the S3 prefix list. Its fields are filled once
// the CLI is connected; until then the check fails.
type gatewayEndpointDrain struct {
	mu      sync.Mutex
	store   *featurelevel.Store
	nodes   []string
	daemons func(ctx context.Context) []featurelevel.NodeStatus
	proxy   func(ctx context.Context, node string) (projection.EndpointProxyStatus, error)
	live    func(ctx context.Context) (int, error)
	rules   func(ctx context.Context) (int, error)
	out     io.Writer
}

var endpointDrain = &gatewayEndpointDrain{}

// connect points the drain at a connected formation: the level store, the
// configured nodes, their daemons' status, each vpcd, and the endpoint and
// security-group buckets.
func (g *gatewayEndpointDrain) connect(store *featurelevel.Store, nodes []string,
	daemons func(context.Context) []featurelevel.NodeStatus, nc *nats.Conn, js jetstream.JetStream, out io.Writer,
) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.store, g.nodes, g.daemons, g.out = store, nodes, daemons, out
	g.proxy = requestProxyStatus(nc)
	g.live = func(ctx context.Context) (int, error) { return vpcendpoint.CountLive(ctx, js, time.Now()) }
	g.rules = func(ctx context.Context) (int, error) { return ec2vpc.CountPrefixListRules(ctx, js) }
}

// check counts live endpoints, endpoints a proxy still admits by, open
// connections, demux flows, prefix-list rules, their ACLs and grant-holding
// applies. A vpcd that is silent, too old or behind the rollback fails it.
func (g *gatewayEndpointDrain) check(ctx context.Context) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.store == nil || g.daemons == nil || g.proxy == nil || g.live == nil || g.rules == nil {
		return 0, errors.New("gateway endpoint drain is not connected")
	}
	_, rev, err := g.store.Read(ctx)
	if err != nil {
		return 0, fmt.Errorf("read feature level: %w", err)
	}
	live, err := g.live(ctx)
	if err != nil {
		return 0, fmt.Errorf("count live endpoints: %w", err)
	}
	rules, err := g.rules(ctx)
	if err != nil {
		return 0, fmt.Errorf("count prefix-list rules: %w", err)
	}

	var unread []string
	demuxNodes := 0
	for _, s := range g.daemons(ctx) {
		if s.Reply == nil {
			unread = append(unread, "daemon on "+s.Node+": "+s.Reason)
			continue
		}
		demuxNodes += s.Reply.Holding[featurelevel.GatewayEndpoints]
	}

	sessions, admitting, applies, acls := 0, 0, 0, 0
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, node := range g.nodes {
		wg.Go(func() {
			pctx, cancel := context.WithTimeout(ctx, proxyStatusWait)
			defer cancel()
			st, err := g.proxy(pctx, node)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				unread = append(unread, "vpcd on "+node+": "+err.Error())
			case st.Revision < rev:
				unread = append(unread, fmt.Sprintf("vpcd on %s: has read revision %d, need %d", node, st.Revision, rev))
			case st.Build < featurelevel.GatewayEndpoints:
				unread = append(unread, fmt.Sprintf("vpcd on %s: reports build %d, which cannot report prefix-list state", node, st.Build))
			case st.LevelRevision < rev:
				unread = append(unread, fmt.Sprintf("vpcd on %s: has observed revision %d, need %d", node, st.LevelRevision, rev))
			default:
				sessions += st.Sessions
				admitting += st.Endpoints
				applies += st.PrefixListApplies
				// Every vpcd reads the one NB database, so each reports the same ACLs.
				acls = max(acls, st.PrefixListACLs)
			}
		})
	}
	wg.Wait()
	if len(unread) > 0 {
		return 0, fmt.Errorf("not every node has answered after the rollback: %s", strings.Join(unread, "; "))
	}
	if g.out != nil {
		fmt.Fprintf(g.out, "Gateway endpoints: %d live endpoint(s), %d still admitted by a proxy, %d open proxied connection(s), %d node(s) with endpoint flows.\n",
			live, admitting, sessions, demuxNodes)
		if live > 0 {
			fmt.Fprintln(g.out, "Delete every VPC endpoint before rolling back: rollback never deletes one.")
		}
		fmt.Fprintf(g.out, "Security-group prefix lists: %d rule(s) naming a prefix list, %d ACL(s) naming a prefix-list set, %d apply(ies) started before the rollback.\n",
			rules, acls, applies)
		if rules > 0 {
			fmt.Fprintln(g.out, "Revoke every security-group rule naming a prefix list before rolling back: rollback never revokes one.")
		}
	}
	return live + admitting + sessions + demuxNodes + rules + acls + applies, nil
}

// requestProxyStatus asks each node's vpcd for its endpoint proxy status,
// with its prefix-list ACLs counted.
func requestProxyStatus(nc *nats.Conn) func(ctx context.Context, node string) (projection.EndpointProxyStatus, error) {
	return func(ctx context.Context, node string) (projection.EndpointProxyStatus, error) {
		return projection.RequestPrefixListStatus(ctx, nc, node)
	}
}
