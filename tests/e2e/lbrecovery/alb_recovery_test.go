//go:build e2e

package lbrecovery

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runALBSurvivesItsHostFailing takes away the node running a load balancer and
// asserts the only things a customer of one holds: the name, the address behind
// it, and that requests to it are answered again without anybody being called.
//
// The name is the part with no second chance. An address that moves is an
// outage; a name that stops resolving, or resolves somewhere else, is an outage
// that outlives the fix for as long as anything cached the answer. So it is
// watched continuously rather than sampled at the end.
func runALBSurvivesItsHostFailing(t *testing.T, fix *Fixture) {
	harness.Phase(t, "A Load Balancer Survives Its Host Failing")

	f := setupALBFixture(t, fix)
	victim := f.Host
	observer := lbrSurvivor(t, fix, victim)

	// Every assertion after the node goes away is made through a survivor's
	// gateway, because the load balancer is free to have landed on the node this
	// suite is about to take down — including the one the environment names as
	// the endpoint.
	cli := harness.AWSClientForGateway(t, fix.Env, observer)
	harness.Detail(t, "victim_node", victim.Name, "observer_node", observer.Name)

	harness.Step(t, "prove the name serves traffic before anything is broken")
	lbrRequireResolvesEverywhere(t, fix, harness.Node{}, f, "before the fault")
	lbrRequireServing(t, fix, observer, harness.Node{}, f, "before the fault", 90*time.Second)

	watch := newDNSWatch(t, fix, victim, f)
	defer watch.stop()

	harness.Step(t, "stop the spinifex services on %s — the load balancer guest keeps running, nothing drains it",
		victim.Name)
	harness.StopNode(t, victim)
	t.Cleanup(func() { harness.StartNode(t, victim) })

	fix.Cluster.WaitNATSPeers(t, len(fix.Cluster.Nodes)-2, harness.WithTimeout(90*time.Second),
		harness.WithPoll(2*time.Second), harness.WithSkipNodes(victim.Name))

	// AWS never reports a load balancer as degraded because a host under it
	// failed — the service is the abstraction, and its state describes the
	// customer's configuration, not our hardware. So active is the right answer
	// here, and the honest signal about the outage is target health and the
	// traffic itself, both asserted below.
	harness.Step(t, "the load balancer record survives the outage unchanged")
	lbrRequireLBRecord(t, cli, f)

	harness.Step(t, "wait for a survivor to claim and relaunch the load balancer")
	var hosting []harness.Node
	require.Eventuallyf(t, func() bool {
		hosting = harness.InstanceHostingNodes(t, fix.Cluster, f.SysInstanceID, []string{victim.Name})
		return len(hosting) > 0
	}, albRecoveryBudget, 15*time.Second,
		"the load balancer's guest %s never came back on a survivor after %s stopped answering\n%s",
		f.SysInstanceID, victim.Name, lbrWhy(t, fix, victim, f))

	require.Lenf(t, hosting, 1, "the load balancer is running on more than one survivor: %v",
		lbrNodeNames(hosting))
	harness.Detail(t, "recovered_onto", hosting[0].Name)

	// The address is the part a customer may have written down, put in a
	// firewall rule, or handed to a partner. A recovery that renumbers it has
	// moved the outage rather than ended it, so this is asserted before any
	// traffic — a passing probe against a new address would hide it.
	harness.Step(t, "the address did not move with the node")
	lbrRequireAddressUnchanged(t, cli, f)

	harness.Step(t, "wait for the name to serve traffic again")
	lbrRequireServing(t, fix, observer, victim, f, "after the recovery", albServingBudget)

	// The backends never moved, so anything unhealthy here is the recovered load
	// balancer's view of them rather than the backends themselves.
	harness.WaitForTargetsHealthy(t, cli, f.TGArn, lbrTargets, "backends after the recovery", targetsHealthyBudget)

	harness.Step(t, "bring %s back and assert it gives up what it no longer owns", victim.Name)
	harness.StartNode(t, victim)
	harness.WaitNodeServiceReady(t, victim, harness.WithTimeout(3*time.Minute))
	fix.Cluster.WaitNATSPeers(t, len(fix.Cluster.Nodes)-1, harness.WithTimeout(3*time.Minute),
		harness.WithPoll(2*time.Second))

	// Long enough for the returning node to have got it wrong. Its restore runs
	// early in startup, so a second copy of the guest, or a tap still claiming
	// the logical port, would already be visible.
	time.Sleep(90 * time.Second)

	after := harness.InstanceHostingNodes(t, fix.Cluster, f.SysInstanceID, nil)
	require.Lenf(t, after, 1,
		"the load balancer is running on %v after %s returned; a returning node must drop a local copy the records say moved",
		lbrNodeNames(after), victim.Name)
	assert.NotEqualf(t, victim.Name, after[0].Name,
		"the load balancer went back to %s, so the returning node relaunched it rather than forgetting it", victim.Name)

	lbrRequireNoStaleTap(t, victim, f)

	// Last, and the assertion the returning node exists in this test for. Its
	// tap carried the load balancer's logical port, and the column that binds a
	// port names one chassis — so a returning node still offering that port does
	// not split the traffic, it contends for it, and the address answers from
	// whichever won last. Serving after the return is what says it did not.
	harness.Step(t, "the name still serves traffic with %s back in the cluster", victim.Name)
	lbrRequireServing(t, fix, observer, harness.Node{}, f, "after the node returned", 3*time.Minute)

	watch.stop()
	watch.requireNameNeverBroke(t, f)
}

// lbrSurvivor returns a node that is not the victim, for the gateway every
// assertion during the outage is made through.
func lbrSurvivor(t *testing.T, fix *Fixture, victim harness.Node) harness.Node {
	t.Helper()
	for _, node := range fix.Cluster.Nodes {
		if node.Name != victim.Name {
			return node
		}
	}
	t.Fatalf("no node other than %s to observe from", victim.Name)
	return harness.Node{}
}

// lbrPeers is every node but victim.
func lbrPeers(fix *Fixture, victim harness.Node) []harness.Node {
	out := make([]harness.Node, 0, len(fix.Cluster.Nodes)-1)
	for _, node := range fix.Cluster.Nodes {
		if node.Name != victim.Name {
			out = append(out, node)
		}
	}
	return out
}

func lbrNodeNames(nodes []harness.Node) []string {
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	return names
}

// lbrRequireResolvesEverywhere asserts every surviving node's northstar answers
// the load balancer's name with its address.
//
// Every node, not one: the zone is cluster-wide and a client may ask any of
// them, so a record served by one node and missing on another is a customer who
// resolves it or not depending on which server they reached.
func lbrRequireResolvesEverywhere(t *testing.T, fix *Fixture, skip harness.Node, f *albFixture, when string) {
	t.Helper()
	for _, node := range lbrPeers(fix, skip) {
		n := node
		var last []string
		var lastErr error
		require.Eventuallyf(t, func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			last, lastErr = harness.ResolveViaNode(ctx, n, f.DNSName)
			return lastErr == nil && slices.Contains(last, f.PublicIP)
		}, 90*time.Second, 3*time.Second,
			"%s's northstar does not answer %s with %s %s (last=%v err=%v)",
			n.Name, f.DNSName, f.PublicIP, when, last, lastErr)
	}
	harness.Detail(t, "dns_name", f.DNSName, "resolves_to", f.PublicIP)
}

// lbrRequireServing drives HTTP through the load balancer's name until both
// backends answer, resolving the name through a node's own northstar.
//
// Through the name rather than the address, because that is what a customer
// uses, and through a node's resolver rather than the runner's, because the
// runner's is not part of this product.
func lbrRequireServing(t *testing.T, fix *Fixture, via, down harness.Node, f *albFixture, when string, budget time.Duration) {
	t.Helper()
	url := fmt.Sprintf("http://%s:%d/", f.DNSName, lbrHTTPPort)
	client := harness.NodeResolverHTTPClient(via, 5*time.Second)

	var last harness.TrafficResult
	require.Eventuallyf(t, func() bool {
		last = harness.HTTPRoundRobinWithClient(client, url, lbrProbes)
		return last.Unique() >= lbrTargets && last.Successful >= lbrProbes/2
	}, budget, 10*time.Second,
		"%s did not serve traffic from %d backends %s: %d/%d probes succeeded across %d responders %v\n%s",
		f.DNSName, lbrTargets, when, last.Successful, last.Total, last.Unique(), last.Distribution,
		lbrWhy(t, fix, down, f))

	harness.AssertRoundRobin(t, last, lbrTargets, lbrProbes/2, "ALB via its name "+when)
}

// lbrRequireLBRecord asserts the load balancer still describes itself the same
// way while its host is gone: same name, same address, still active.
func lbrRequireLBRecord(t *testing.T, cli *harness.AWSClient, f *albFixture) {
	t.Helper()
	out, err := cli.ELBv2.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{
		LoadBalancerArns: []*string{aws.String(f.LBArn)},
	})
	require.NoError(t, err, "describe the load balancer through a survivor")
	require.NotEmpty(t, out.LoadBalancers, "the load balancer vanished from the API when its host did")

	lb := out.LoadBalancers[0]
	assert.Equalf(t, f.DNSName, aws.StringValue(lb.DNSName),
		"the load balancer's name changed while its host was gone, so every client holding the old one is stranded")
	assert.Equalf(t, "active", aws.StringValue(lb.State.Code),
		"the load balancer reports state %q while a host failure is being recovered; a customer's configuration did not change",
		aws.StringValue(lb.State.Code))
}

// lbrRequireAddressUnchanged asserts the recovered load balancer answers on the
// address it was created with, and is the same instance rather than a new one.
func lbrRequireAddressUnchanged(t *testing.T, cli *harness.AWSClient, f *albFixture) {
	t.Helper()
	out, err := cli.EC2.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: []*string{aws.String(f.ENIID)},
	})
	require.NoError(t, err, "describe the load balancer's interface")
	require.NotEmptyf(t, out.NetworkInterfaces, "the load balancer's interface %s is gone", f.ENIID)

	eni := out.NetworkInterfaces[0]
	require.NotNilf(t, eni.Association, "%s no longer has a public address at all", f.ENIID)
	assert.Equalf(t, f.PublicIP, aws.StringValue(eni.Association.PublicIp),
		"the load balancer came back on %s having been reachable on %s; an address a customer may have written down "+
			"must survive a host failure", aws.StringValue(eni.Association.PublicIp), f.PublicIP)
	if eni.Attachment != nil {
		assert.Equalf(t, f.SysInstanceID, aws.StringValue(eni.Attachment.InstanceId),
			"the interface is attached to %s rather than the load balancer's own guest, so this is a different load balancer",
			aws.StringValue(eni.Attachment.InstanceId))
	}
}

// lbrRequireNoStaleTap asserts the returning node no longer offers the load
// balancer's logical port.
//
// This is the mechanism behind the assertion above it. OVN binds a logical port
// to one chassis, and a tap carrying that port's iface-id is how a node claims
// it, so a returning node that kept its tap is a second claimant — and it is
// also what keeps the external address's host ingress from being pruned, since
// that prune waits on evidence the guest sits elsewhere.
func lbrRequireNoStaleTap(t *testing.T, victim harness.Node, f *albFixture) {
	t.Helper()
	tap := vm.TapDeviceName(f.ENIID)
	harness.Step(t, "%s must no longer carry %s, the tap that claims the load balancer's port", victim.Name, tap)

	out, err := lbrRunErr(victim, "ip -o link show "+harness.ShellQuote(tap)+" 2>/dev/null || true")
	require.NoErrorf(t, err, "ask %s about %s: %s", victim.Name, tap, out)
	assert.Emptyf(t, strings.TrimSpace(out),
		"%s still carries %s after the load balancer moved, so two nodes are claiming one logical port and the "+
			"address answers from whichever won last:\n%s", victim.Name, tap, out)
}

// dnsWatch samples what every surviving node's northstar answers for the load
// balancer's name, continuously, for as long as the scenario lasts.
//
// The point-in-time checks answer "does the name resolve now". This answers "did
// it ever stop, or ever point somewhere else", which is the question a customer
// with a cached answer is really asking, and the one no single sample reaches.
type dnsWatch struct {
	stopOnce sync.Once
	done     chan struct{}
	finished chan struct{}

	mu      sync.Mutex
	samples int
	missing int
	wrong   map[string]int
	errs    map[string]int
}

func newDNSWatch(t *testing.T, fix *Fixture, victim harness.Node, f *albFixture) *dnsWatch {
	t.Helper()
	w := &dnsWatch{
		done:     make(chan struct{}),
		finished: make(chan struct{}),
		wrong:    map[string]int{},
		errs:     map[string]int{},
	}
	peers := lbrPeers(fix, victim)

	go func() {
		defer close(w.finished)
		ticker := time.NewTicker(dnsWatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-w.done:
				return
			case <-ticker.C:
				for _, node := range peers {
					w.sample(node, f)
				}
			}
		}
	}()
	return w
}

// sample records one node's answer. A node that cannot be reached is counted
// separately from one that answers the wrong thing: the first is this harness
// losing contact, the second is the product being wrong.
func (w *dnsWatch) sample(node harness.Node, f *albFixture) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	addrs, err := harness.ResolveViaNode(ctx, node, f.DNSName)

	w.mu.Lock()
	defer w.mu.Unlock()
	w.samples++
	switch {
	case err != nil:
		w.errs[node.Name]++
	case len(addrs) == 0:
		w.missing++
	case !slices.Contains(addrs, f.PublicIP):
		w.wrong[node.Name+" -> "+strings.Join(addrs, ",")]++
	}
}

func (w *dnsWatch) stop() {
	w.stopOnce.Do(func() {
		close(w.done)
		<-w.finished
	})
}

// requireNameNeverBroke is the assertion the watch exists for. An answer naming
// another address is a hard failure: it sends traffic somewhere that is not this
// load balancer. An empty answer, or a query the node refused, is the same
// outage from the client's side, so both are failures too — and they are
// reported apart because they point at different code.
func (w *dnsWatch) requireNameNeverBroke(t *testing.T, f *albFixture) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()

	require.Positivef(t, w.samples, "the DNS watch took no samples, so it proved nothing about the name")
	harness.Detail(t, "dns_samples", fmt.Sprintf("%d", w.samples))

	require.Emptyf(t, w.wrong,
		"%s resolved to an address that is not the load balancer's %s during the outage: %v; "+
			"a name pointing somewhere else is worse than one that does not resolve, because the traffic goes there",
		f.DNSName, f.PublicIP, w.wrong)
	require.Zerof(t, w.missing,
		"%s resolved to nothing in %d of %d samples; the record must survive the loss of the node the load balancer was on",
		f.DNSName, w.missing, w.samples)
	require.Emptyf(t, w.errs,
		"a surviving node's northstar refused to answer for %s during the outage: %v; "+
			"the zone is cluster-wide, so a client reaching that node gets no address at all",
		f.DNSName, w.errs)
}

// lbrWhy gathers what decides a load balancer recovery, for a failure message
// that does not send the reader to three journals to find which step did not
// happen.
func lbrWhy(t *testing.T, fix *Fixture, skip harness.Node, f *albFixture) string {
	t.Helper()
	var b strings.Builder
	for _, node := range lbrPeers(fix, skip) {
		out, err := lbrRunErr(node,
			"sudo journalctl -u spinifex-daemon --no-pager -n 800 | "+
				"grep -iE 'recovery|claim|stopped heartbeating|"+f.SysInstanceID+"|elb|haproxy' | tail -25")
		if err != nil {
			out = "(" + err.Error() + ")"
		}
		fmt.Fprintf(&b, "         %s:\n%s\n", node.Name, strings.TrimSpace(out))
	}
	return b.String()
}

func lbrRunErr(node harness.Node, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := harness.NewPeerSSH().Run(ctx, node.Addr, cmd)
	return string(out), err
}
