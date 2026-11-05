//go:build e2e

package lbrecovery

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// Older than every timeout in the repair path, so a repair that reaches a
	// customer instance would find one the platform has long stopped expecting
	// to change.
	shTargetAge = 21 * time.Minute

	// The requirement: traffic is back within five minutes of the fault.
	shRecoveryBudget = 5 * time.Minute

	// The data NIC of a load balancer guest; the management NIC is the last.
	shDataNIC = "net0"

	stateActive   = "active"
	stateImpaired = "active_impaired"
)

type shKind struct {
	name    string
	lbType  string
	scheme  string
	tgProto string
}

var (
	shInternetFacingNLB = shKind{"InternetFacingNLB", "network", "internet-facing", "TCP"}
	shInternalALB       = shKind{"InternalALB", "application", "internal", "HTTP"}
)

// TestLBSelfHealsADeadDataNIC takes one data NIC of a running load balancer
// guest away and asserts the platform notices, reports the load balancer as
// impaired, reboots that guest once, and leaves everything else alone.
//
// Each kind is its own subtest because the guest, its ENIs and its backends are
// different, but the assertions are the same: the customer's name and address,
// the guest's identity, and every instance that is not the load balancer.
func TestLBSelfHealsADeadDataNIC(t *testing.T) {
	fix := requireFixture(t)
	for _, kind := range []shKind{shInternetFacingNLB, shInternalALB} {
		t.Run(kind.name, func(t *testing.T) { runLBSelfHeal(t, fix, kind) })
	}
}

// shInstanceSnap is what a customer instance must still be after the repair.
type shInstanceSnap struct {
	id      string
	launch  time.Time
	process string
	node    harness.Node
}

func runLBSelfHeal(t *testing.T, fix *Fixture, kind shKind) {
	harness.Phase(t, "A %s repairs a dead data NIC", kind.name)
	f := shSetup(t, fix, kind)
	internetFacing := kind.scheme == "internet-facing"
	cli := fix.AWS

	// The unrelated customer instance shares the subnet and the security group
	// with the backends. It is the witness that the repair touched only the
	// load balancer's own guest.
	bystander := shLaunchBystander(t, fix, f)

	harness.Step(t, "wait until every customer instance is older than %s", shTargetAge)
	customers := append(slices.Clone(f.TargetIDs), bystander)
	shWaitOlderThan(t, cli, customers, shTargetAge)

	before := shSnapshot(t, fix, customers)
	lbBefore := shLBIdentity(t, fix, f)
	lbProcBefore, err := harness.ProcessIdentity(f.Host, f.SysInstanceID)
	require.NoError(t, err, "the load balancer's guest must be running on %s", f.Host.Name)

	if internetFacing {
		harness.Step(t, "prove the name serves traffic before anything is broken")
		lbrRequireServing(t, fix, f.Host, harness.Node{}, f, "before the fault", 90*time.Second)
	}
	harness.WaitForTargetsHealthy(t, cli, f.TGArn, lbrTargets, "backends before the fault", targetsHealthyBudget)

	states := newStateWatch(t, cli, f.LBArn)
	defer states.stop()
	var dns *dnsWatch
	if internetFacing {
		dns = newDNSWatch(t, fix, harness.Node{}, f)
		defer dns.stop()
	}

	faultAt := time.Now()
	harness.Step(t, "disable %s of %s on %s", shDataNIC, f.SysInstanceID, f.Host.Name)
	require.NoError(t, harness.LBSetLink(t, f.Host, f.SysInstanceID, shDataNIC, false))

	harness.Step(t, "the load balancer reports %s while the NIC is dead", stateImpaired)
	require.Eventuallyf(t, func() bool { return states.seen(stateImpaired) }, shRecoveryBudget, 3*time.Second,
		"the load balancer never reported %s after its data NIC was disabled; states seen: %v",
		stateImpaired, states.sequence())

	harness.Step(t, "the load balancer is repaired and active again within %s of the fault", shRecoveryBudget)
	require.Eventuallyf(t, func() bool { return states.current() == stateActive && states.seen(stateImpaired) },
		shRecoveryBudget-time.Since(faultAt), 3*time.Second,
		"the load balancer was not active again %s after the fault; states seen: %v", shRecoveryBudget, states.sequence())

	if internetFacing {
		remaining := max(shRecoveryBudget-time.Since(faultAt), 10*time.Second)
		lbrRequireServing(t, fix, f.Host, harness.Node{}, f, "after the repair", remaining)
	}
	harness.WaitForTargetsHealthy(t, cli, f.TGArn, lbrTargets, "backends after the repair", targetsHealthyBudget)

	states.stop()
	// Never active between the first impaired report and the recovery: a
	// load balancer with a dead NIC must not claim to be healthy.
	assert.Equalf(t, []string{stateActive, stateImpaired, stateActive}, states.sequence(),
		"the load balancer's state must go active, active_impaired, active and nothing else")
	if dns != nil {
		dns.stop()
		dns.requireNameNeverBroke(t, f)
	}

	harness.Step(t, "same guest, same interfaces, same address")
	assert.Equal(t, lbBefore, shLBIdentity(t, fix, f), "the repair must not replace the guest, its ENIs or its address")
	lbProcAfter, err := harness.ProcessIdentity(f.Host, f.SysInstanceID)
	require.NoError(t, err, "the load balancer's guest must be running on %s after the repair", f.Host.Name)
	assert.NotEqual(t, lbProcBefore, lbProcAfter, "the guest process is unchanged, so nothing rebooted it")

	harness.Step(t, "customer instances were not touched (ADR-0003:S7)")
	assert.Equal(t, before, shSnapshot(t, fix, customers),
		"a customer instance changed identity, launch time or process during a load balancer repair")

	harness.Step(t, "exactly one repair line, with the fields an operator needs")
	started := harness.DaemonLogLines(fix.Cluster, faultAt.Add(-time.Minute), "ELBv2 data-path repair started", f.LBID)
	total := 0
	var line string
	for _, lines := range started {
		total += len(lines)
		if len(lines) > 0 {
			line = lines[0]
		}
	}
	require.Equalf(t, 1, total, "want exactly one repair for %s, got %d: %v", f.LBID, total, started)
	for _, field := range []string{"lbArn", "lbId", "instanceId", "eniId", "mac", "failing_ms",
		"consecutive_failures", "rx_packets", "tx_packets", "repair_attempt", "reason"} {
		assert.Containsf(t, line, field, "the repair line lacks %s: %s", field, line)
	}
	assert.Containsf(t, line, f.SysInstanceID, "the repair line names another guest: %s", line)
}

// stateWatch samples a load balancer's reported state continuously and keeps the
// run of distinct states, so an interval in which it claimed to be healthy is
// seen even if nothing was looking at that moment.
type stateWatch struct {
	stopOnce sync.Once
	done     chan struct{}
	finished chan struct{}

	mu   sync.Mutex
	runs []string
}

func newStateWatch(t *testing.T, c *harness.AWSClient, arn string) *stateWatch {
	t.Helper()
	w := &stateWatch{done: make(chan struct{}), finished: make(chan struct{})}
	go func() {
		defer close(w.finished)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-w.done:
				return
			case <-ticker.C:
				out, err := c.ELBv2.DescribeLoadBalancers(&elbv2.DescribeLoadBalancersInput{
					LoadBalancerArns: []*string{aws.String(arn)},
				})
				if err != nil || len(out.LoadBalancers) == 0 || out.LoadBalancers[0].State == nil {
					continue
				}
				w.record(aws.StringValue(out.LoadBalancers[0].State.Code))
			}
		}
	}()
	return w
}

func (w *stateWatch) record(state string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.runs) == 0 || w.runs[len(w.runs)-1] != state {
		w.runs = append(w.runs, state)
	}
}

func (w *stateWatch) stop() {
	w.stopOnce.Do(func() {
		close(w.done)
		<-w.finished
	})
}

func (w *stateWatch) sequence() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.runs)
}

func (w *stateWatch) current() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.runs) == 0 {
		return ""
	}
	return w.runs[len(w.runs)-1]
}

func (w *stateWatch) seen(state string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Contains(w.runs, state)
}

// shLBIdentity is what must survive the repair unchanged: the guest, its
// interfaces and the address.
func shLBIdentity(t *testing.T, fix *Fixture, f *albFixture) string {
	t.Helper()
	out, err := fix.AWS.EC2.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("description"),
			Values: []*string{aws.String(shENIDescription(f))},
		}},
	})
	require.NoError(t, err, "describe the load balancer's interfaces")
	ids := make([]string, 0, len(out.NetworkInterfaces))
	for _, eni := range out.NetworkInterfaces {
		attached, public := "", ""
		if eni.Attachment != nil {
			attached = aws.StringValue(eni.Attachment.InstanceId)
		}
		if eni.Association != nil {
			public = aws.StringValue(eni.Association.PublicIp)
		}
		ids = append(ids, fmt.Sprintf("%s|%s|%s", aws.StringValue(eni.NetworkInterfaceId), attached, public))
	}
	slices.Sort(ids)
	return strings.Join(ids, ",")
}

func shENIDescription(f *albFixture) string {
	parts := strings.Split(f.LBArn, "/")
	// arn:...:loadbalancer/<app|net>/<name>/<id>
	return fmt.Sprintf("ELB %s/%s/%s", parts[len(parts)-3], parts[len(parts)-2], parts[len(parts)-1])
}

// shSnapshot records, per customer instance, what a reboot or a relaunch would
// change.
func shSnapshot(t *testing.T, fix *Fixture, ids []string) []shInstanceSnap {
	t.Helper()
	out, err := fix.AWS.EC2.DescribeInstances(&ec2.DescribeInstancesInput{InstanceIds: aws.StringSlice(ids)})
	require.NoError(t, err, "describe customer instances")
	launch := map[string]time.Time{}
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			require.Equalf(t, "running", aws.StringValue(inst.State.Name), "%s is not running", aws.StringValue(inst.InstanceId))
			launch[aws.StringValue(inst.InstanceId)] = aws.TimeValue(inst.LaunchTime)
		}
	}
	snaps := make([]shInstanceSnap, 0, len(ids))
	for _, id := range ids {
		host := harness.InstanceHostingNode(t, fix.Cluster, id)
		require.NotNilf(t, host, "no node is running %s", id)
		proc, perr := harness.ProcessIdentity(*host, id)
		require.NoError(t, perr)
		snaps = append(snaps, shInstanceSnap{id: id, launch: launch[id], process: proc, node: *host})
	}
	return snaps
}

func shWaitOlderThan(t *testing.T, c *harness.AWSClient, ids []string, age time.Duration) {
	t.Helper()
	out, err := c.EC2.DescribeInstances(&ec2.DescribeInstancesInput{InstanceIds: aws.StringSlice(ids)})
	require.NoError(t, err, "describe customer instances")
	var newest time.Time
	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			if lt := aws.TimeValue(inst.LaunchTime); lt.After(newest) {
				newest = lt
			}
		}
	}
	if wait := time.Until(newest.Add(age)); wait > 0 {
		harness.Detail(t, "waiting", wait.Round(time.Second).String())
		time.Sleep(wait)
	}
}

func shLaunchBystander(t *testing.T, fix *Fixture, f *albFixture) string {
	t.Helper()
	id := lbrLaunchTarget(t, fix, f)
	harness.WaitForInstanceRunning(t, fix.AWS, id, 3*time.Minute)
	harness.Detail(t, "bystander", id)
	return id
}

// shSetup builds the VPC, a load balancer of the given kind with a listener,
// and two registered backends.
func shSetup(t *testing.T, fix *Fixture, kind shKind) *albFixture {
	t.Helper()
	f := &albFixture{}
	instanceType, arch := harness.DiscoverNanoInstanceType(t, fix.Harness)
	f.InstanceType = instanceType
	f.AMIID = harness.DiscoverUbuntuAMI(t, fix.Harness, arch)
	f.KeyName, _ = harness.EnsureKeyPair(t, fix.Harness)

	lbrCreateVPC(t, fix.AWS, f)
	lbrCreateIGW(t, fix.AWS, f)
	lbrCreateSubnet(t, fix.AWS, f)
	lbrAddPublicRoute(t, fix.AWS, f)
	lbrOpenDefaultSG(t, fix.AWS, f)

	c := fix.AWS
	tgIn := &elbv2.CreateTargetGroupInput{
		Name:     aws.String("lbr-sh-" + strings.ToLower(kind.lbType[:3])),
		Protocol: aws.String(kind.tgProto),
		Port:     aws.Int64(lbrHTTPPort),
		VpcId:    aws.String(f.VPCID),

		HealthCheckIntervalSeconds: aws.Int64(5),
		HealthyThresholdCount:      aws.Int64(2),
		UnhealthyThresholdCount:    aws.Int64(2),
	}
	if kind.tgProto == "HTTP" {
		tgIn.HealthCheckPath = aws.String("/index.html")
	}
	tg, err := c.ELBv2.CreateTargetGroup(tgIn)
	require.NoError(t, err, "create target group")
	f.TGArn = aws.StringValue(tg.TargetGroups[0].TargetGroupArn)
	t.Cleanup(func() {
		if _, derr := c.ELBv2.DeleteTargetGroup(&elbv2.DeleteTargetGroupInput{TargetGroupArn: aws.String(f.TGArn)}); derr != nil {
			t.Logf("delete target group: %v", derr)
		}
	})

	lbIn := &elbv2.CreateLoadBalancerInput{
		Name:    aws.String("lbr-sh-" + strings.ToLower(kind.lbType[:3])),
		Subnets: []*string{aws.String(f.SubnetID)},
		Scheme:  aws.String(kind.scheme),
		Type:    aws.String(kind.lbType),
	}
	if kind.lbType == "application" {
		lbIn.SecurityGroups = []*string{aws.String(f.LBSGID)}
	}
	lbOut, err := c.ELBv2.CreateLoadBalancer(lbIn)
	require.NoError(t, err, "create load balancer")
	require.NotEmpty(t, lbOut.LoadBalancers, "CreateLoadBalancer returned nothing")
	f.LBArn = aws.StringValue(lbOut.LoadBalancers[0].LoadBalancerArn)
	f.DNSName = aws.StringValue(lbOut.LoadBalancers[0].DNSName)
	parts := strings.Split(f.LBArn, "/")
	f.LBID = parts[len(parts)-1]
	t.Cleanup(func() { lbrDeleteLB(t, c, f) })

	ls, err := c.ELBv2.CreateListener(&elbv2.CreateListenerInput{
		LoadBalancerArn: aws.String(f.LBArn),
		Protocol:        aws.String(kind.tgProto),
		Port:            aws.Int64(lbrHTTPPort),
		DefaultActions: []*elbv2.Action{{
			Type:           aws.String("forward"),
			TargetGroupArn: aws.String(f.TGArn),
		}},
	})
	require.NoError(t, err, "create listener")
	f.ListenerArn = aws.StringValue(ls.Listeners[0].ListenerArn)
	t.Cleanup(func() { lbrDeleteListener(t, c, f) })

	harness.WaitForLBActive(t, c, f.LBArn, kind.name, albActiveBudget)
	shDiscoverPlacement(t, fix, f, kind)

	for len(f.TargetIDs) < lbrTargets {
		id := lbrLaunchTarget(t, fix, f)
		harness.WaitForInstanceRunning(t, c, id, 3*time.Minute)
		f.TargetIDs = append(f.TargetIDs, id)
	}
	lbrRegisterTargets(t, c, f)
	harness.WaitForTargetsHealthy(t, c, f.TGArn, lbrTargets, kind.name+" targets", targetsHealthyBudget)
	return f
}

// shDiscoverPlacement finds the load balancer's guest through the ENI it is
// described by, because a system instance is hidden from DescribeInstances.
func shDiscoverPlacement(t *testing.T, fix *Fixture, f *albFixture, kind shKind) {
	t.Helper()
	desc := shENIDescription(f)

	var eni *ec2.NetworkInterface
	harness.EventuallyErr(t, func() error {
		out, err := fix.AWS.EC2.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
			Filters: []*ec2.Filter{{Name: aws.String("description"), Values: []*string{aws.String(desc)}}},
		})
		if err != nil {
			return err
		}
		if len(out.NetworkInterfaces) == 0 {
			return fmt.Errorf("no ENI described as %q", desc)
		}
		eni = out.NetworkInterfaces[0]
		if eni.Attachment == nil || aws.StringValue(eni.Attachment.InstanceId) == "" {
			return fmt.Errorf("ENI %s is not attached to an instance yet", aws.StringValue(eni.NetworkInterfaceId))
		}
		if kind.scheme == "internet-facing" && (eni.Association == nil || aws.StringValue(eni.Association.PublicIp) == "") {
			return fmt.Errorf("ENI %s has no public address yet", aws.StringValue(eni.NetworkInterfaceId))
		}
		return nil
	}, 60*time.Second, 3*time.Second)

	f.ENIID = aws.StringValue(eni.NetworkInterfaceId)
	f.SysInstanceID = aws.StringValue(eni.Attachment.InstanceId)
	if eni.Association != nil {
		f.PublicIP = aws.StringValue(eni.Association.PublicIp)
	}
	host := harness.InstanceHostingNode(t, fix.Cluster, f.SysInstanceID)
	require.NotNilf(t, host, "the load balancer is active but no node is running %s", f.SysInstanceID)
	f.Host = *host
	harness.Detail(t, "lb_instance", f.SysInstanceID, "lb_eni", f.ENIID, "lb_node", f.Host.Name, "lb_public_ip", f.PublicIP)
}
