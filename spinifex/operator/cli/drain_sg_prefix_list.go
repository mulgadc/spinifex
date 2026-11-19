package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// sgPrefixListDrain is the prefix-list level's rollback drain. Its fields are
// filled once the CLI is connected; until then the check fails.
type sgPrefixListDrain struct {
	mu    sync.Mutex
	store *featurelevel.Store
	nodes []string
	proxy func(ctx context.Context, node string) (projection.EndpointProxyStatus, error)
	rules func(ctx context.Context) (int, error)
	out   io.Writer
}

var prefixListDrain = &sgPrefixListDrain{}

// connect points the drain at a connected formation: the level store, the
// configured nodes, each vpcd and the security-group bucket.
func (g *sgPrefixListDrain) connect(store *featurelevel.Store, nodes []string, nc *nats.Conn, js jetstream.JetStream, out io.Writer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.store, g.nodes, g.out = store, nodes, out
	g.proxy = func(ctx context.Context, node string) (projection.EndpointProxyStatus, error) {
		return projection.RequestPrefixListStatus(ctx, nc, node)
	}
	g.rules = func(ctx context.Context) (int, error) { return ec2vpc.CountPrefixListRules(ctx, js) }
}

// check counts stored rules naming a prefix list, ACLs naming a prefix-list
// set, and applies a vpcd started before observing the rollback. A vpcd that
// is silent, too old to report them, or behind the rollback fails it.
func (g *sgPrefixListDrain) check(ctx context.Context) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.store == nil || g.proxy == nil || g.rules == nil {
		return 0, errors.New("security-group prefix-list drain is not connected")
	}
	_, rev, err := g.store.Read(ctx)
	if err != nil {
		return 0, fmt.Errorf("read feature level: %w", err)
	}
	rules, err := g.rules(ctx)
	if err != nil {
		return 0, fmt.Errorf("count prefix-list rules: %w", err)
	}

	var unread []string
	applies, acls := 0, 0
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
			case st.Build < featurelevel.GatewayEndpoints:
				unread = append(unread, fmt.Sprintf("vpcd on %s: reports build %d, which cannot report prefix-list state", node, st.Build))
			case st.LevelRevision < rev:
				unread = append(unread, fmt.Sprintf("vpcd on %s: has observed revision %d, need %d", node, st.LevelRevision, rev))
			default:
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
		fmt.Fprintf(g.out, "Security-group prefix lists: %d rule(s) naming a prefix list, %d ACL(s) naming a prefix-list set, %d apply(ies) started before the rollback.\n",
			rules, acls, applies)
		if rules > 0 {
			fmt.Fprintln(g.out, "Revoke every security-group rule naming a prefix list before rolling back: rollback never revokes one.")
		}
	}
	return rules + acls + applies, nil
}
