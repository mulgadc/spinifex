package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/nats-io/nats.go"
)

// proxyStatusWait bounds each node's vpcd answer to the drain.
const proxyStatusWait = 3 * time.Second

// gatewayEndpointDrain is the gateway-endpoint level's rollback drain. Its
// fields are filled once the CLI is connected; until then the check fails.
type gatewayEndpointDrain struct {
	mu      sync.Mutex
	store   *featurelevel.Store
	nodes   []string
	daemons func(ctx context.Context) []featurelevel.NodeStatus
	proxy   func(ctx context.Context, node string) (projection.EndpointProxyStatus, error)
	live    func(ctx context.Context) (int, error)
	out     io.Writer
}

var endpointDrain = &gatewayEndpointDrain{}

// check counts what still depends on the endpoint listener: live endpoints,
// endpoints a proxy still admits by, unfinished connections and demux flows.
// A vpcd that is silent or has not published since the rollback fails it.
func (g *gatewayEndpointDrain) check(ctx context.Context) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.store == nil || g.daemons == nil || g.proxy == nil || g.live == nil {
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

	var unread []string
	demuxNodes := 0
	for _, s := range g.daemons(ctx) {
		if s.Reply == nil {
			unread = append(unread, "daemon on "+s.Node+": "+s.Reason)
			continue
		}
		demuxNodes += s.Reply.Holding[featurelevel.GatewayEndpoints]
	}

	sessions, admitting := 0, 0
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
			default:
				sessions += st.Sessions
				admitting += st.Endpoints
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
	}
	return live + admitting + sessions + demuxNodes, nil
}

// requestProxyStatus asks node's vpcd for its endpoint proxy status.
func requestProxyStatus(nc *nats.Conn) func(ctx context.Context, node string) (projection.EndpointProxyStatus, error) {
	return func(ctx context.Context, node string) (projection.EndpointProxyStatus, error) {
		var st projection.EndpointProxyStatus
		if !featurelevel.ValidNode(node) {
			return st, errors.New("invalid node ID")
		}
		body, err := json.Marshal(projection.EndpointProxyStatusRequest{Node: node})
		if err != nil {
			return st, err
		}
		msg, err := nc.RequestWithContext(ctx, projection.EndpointProxyStatusSubject(node), body)
		if err != nil {
			return st, fmt.Errorf("no reply: %w", err)
		}
		if err := json.Unmarshal(msg.Data, &st); err != nil {
			return st, fmt.Errorf("malformed reply: %w", err)
		}
		switch {
		case st.Error != "":
			return st, fmt.Errorf("refused: %s", st.Error)
		case st.Node != node:
			return st, fmt.Errorf("answered by %q", st.Node)
		}
		return st, nil
	}
}
