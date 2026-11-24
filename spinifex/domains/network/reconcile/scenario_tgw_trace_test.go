package reconcile

import (
	"context"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	ec2projection "github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	ec2routetable "github.com/mulgadc/spinifex/spinifex/domains/ec2/routetable"
	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	networkids "github.com/mulgadc/spinifex/spinifex/domains/network/identifiers"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
)

// traceLab is an NB and SB with ovn-northd between them, named by
// SPINIFEX_OVN_TRACE_NB and SPINIFEX_OVN_TRACE_SB. The test needs an empty
// NB, so it never runs against a deployment.
type traceLab struct {
	t      *testing.T
	nb, sb string
}

func (l traceLab) nbctl(args ...string) string {
	l.t.Helper()
	out, err := exec.Command("ovn-nbctl", append([]string{"--db=" + l.nb}, args...)...).CombinedOutput()
	if err != nil {
		l.t.Fatalf("ovn-nbctl %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// addVIF plugs a guest at ip into subnetID's switch.
func (l traceLab) addVIF(subnetID, ip string) {
	name := "vif-" + subnetID
	l.nbctl("lsp-add", topology.SubnetSwitch(subnetID), name, "--", "lsp-set-addresses", name, networkids.HashMAC(name)+" "+ip)
}

var traceOutput = regexp.MustCompile(`output\("([^"]+)"\)`)

// trace sends ICMP from subnetID's guest at src to dst and returns the port
// the packet leaves on, or "dropped".
func (l traceLab) trace(subnetID, src, dst string) string {
	l.t.Helper()
	l.nbctl("--wait=sb", "sync")
	vif := "vif-" + subnetID
	flow := `inport == "` + vif + `" && eth.src == ` + networkids.HashMAC(vif) + ` && eth.dst == ` + networkids.HashMAC(subnetID) +
		` && ip4.src == ` + src + ` && ip4.dst == ` + dst + ` && ip.ttl == 64 && icmp4`
	out, err := exec.Command("ovn-trace", "--db="+l.sb, "--minimal", topology.SubnetSwitch(subnetID), flow).CombinedOutput()
	if err != nil {
		l.t.Fatalf("ovn-trace %s: %v\n%s", flow, err, out)
	}
	m := traceOutput.FindAllStringSubmatch(string(out), -1)
	if len(m) == 0 {
		return "dropped"
	}
	return m[len(m)-1][1]
}

type traceVPC struct {
	id, cidr string
	subnets  map[string]string // subnet ID → zone; each subnet is the VPC's 10.x.n.0/24
}

// traceIntent builds intent from records through the same classification and
// route fan-out the KV loader uses.
func traceIntent(vpcs []traceVPC, atts []ec2projection.TransitGatewayAttachment, tables []ec2routetable.RouteTableRecord) IntentState {
	intent := tgwIntent(true)
	intent.VPCs = map[string]topology.VPCSpec{}
	intent.Subnets = map[string]topology.SubnetSpec{}
	intent.IGWs = map[string]external.IGWSpec{}
	azs := map[string]string{}
	vpcIDs := map[string]struct{}{}
	for _, v := range vpcs {
		intent.VPCs[v.id] = topology.VPCSpec{VPCID: v.id, CIDR: netip.MustParsePrefix(v.cidr)}
		vpcIDs[v.id] = struct{}{}
		for id, az := range v.subnets {
			intent.Subnets[id] = topology.SubnetSpec{SubnetID: id, VPCID: v.id, CIDR: subnetCIDR(id)}
			azs[id] = az
		}
	}
	ti := newTransitGatewayIntent(azs)
	classifyTransitGateways(&ti, []ec2projection.TransitGateway{{ID: testTGW, AccountID: testAcct, Available: true}}, atts, intent.VPCs, vpcIDs)
	ti.TGWRoutes = loadTGWRoutes(intent.VPCs, intent.Subnets, tables)
	intent.TransitGatewayIntent = ti
	return intent
}

var traceSubnets = map[string]string{
	"subnet-a1": "10.0.1.0/24", "subnet-a2": "10.0.2.0/24", "subnet-a3": "10.0.3.0/24",
	"subnet-b1": "10.1.1.0/24", "subnet-c1": "10.2.1.0/24", "subnet-d1": "10.1.1.0/24", "subnet-e1": "10.1.5.0/24",
}

func subnetCIDR(id string) netip.Prefix { return netip.MustParsePrefix(traceSubnets[id]) }

func traceAttachment(id, vpcID, subnetID string, slot, minute int, cleanup bool) ec2projection.TransitGatewayAttachment {
	a := linkedAttachment(id, vpcID, slot, minute)
	a.SubnetIDs = []string{subnetID}
	if cleanup {
		a.Available, a.CleanupPending = false, true
	}
	return a
}

func tgwRoute(dest string) ec2routetable.RouteRecord {
	return ec2routetable.RouteRecord{DestinationCidrBlock: dest, TransitGatewayId: testTGW}
}

// The datapath that northd compiles from the reconciler's rows forwards and
// drops as AWS does for the Transit Gateway cases in the plan's test list.
func TestScenario_TransitGatewayTrace(t *testing.T) {
	lab := traceLab{t: t, nb: os.Getenv("SPINIFEX_OVN_TRACE_NB"), sb: os.Getenv("SPINIFEX_OVN_TRACE_SB")}
	if lab.nb == "" || lab.sb == "" {
		t.Skip("SPINIFEX_OVN_TRACE_NB and SPINIFEX_OVN_TRACE_SB name a lab NB and SB with ovn-northd")
	}
	if got := lab.nbctl("lr-list") + lab.nbctl("ls-list"); got != "" {
		t.Fatalf("lab NB is not empty:\n%s", got)
	}
	rec, _ := newReconcilerAt(t, lab.nb)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	vpcs := []traceVPC{
		{"vpc-a", "10.0.0.0/16", map[string]string{"subnet-a1": "us-east-1a", "subnet-a2": "us-east-1b", "subnet-a3": "us-east-1a"}},
		{"vpc-b", "10.1.0.0/16", map[string]string{"subnet-b1": "us-east-1a"}},
		{"vpc-c", "10.2.0.0/16", map[string]string{"subnet-c1": "us-east-1a"}},
		{"vpc-d", "10.1.0.0/16", map[string]string{"subnet-d1": "us-east-1a"}},
		{"vpc-e", "10.1.5.0/24", map[string]string{"subnet-e1": "us-east-1a"}},
	}
	tables := []ec2routetable.RouteTableRecord{
		{RouteTableId: "rtb-a", VpcId: "vpc-a", IsMain: true, Routes: []ec2routetable.RouteRecord{tgwRoute("10.0.0.0/8")}},
		{RouteTableId: "rtb-a3", VpcId: "vpc-a", Associations: []ec2routetable.AssociationRecord{{SubnetId: "subnet-a3"}}},
		{RouteTableId: "rtb-b", VpcId: "vpc-b", IsMain: true, Routes: []ec2routetable.RouteRecord{
			tgwRoute("10.0.0.0/16"), {DestinationCidrBlock: "0.0.0.0/0", GatewayId: "igw-b"},
		}},
	}
	// Remove only what this test creates, so a lab shared with other work keeps it.
	t.Cleanup(func() {
		routers := []string{topology.TransitGatewayRouter(testTGW)}
		switches := []string{"ext-trace"}
		for _, v := range vpcs {
			routers = append(routers, topology.VPCRouter(v.id))
			for id := range v.subnets {
				switches = append(switches, topology.SubnetSwitch(id))
				for uuid := range strings.FieldsSeq(lab.nbctl("--bare", "--columns=_uuid", "find", "DHCP_Options", "external_ids:spinifex\\:subnet_id="+id)) {
					lab.nbctl("destroy", "DHCP_Options", uuid)
				}
			}
		}
		for _, lr := range routers {
			lab.nbctl("--if-exists", "lr-del", lr)
		}
		for _, ls := range switches {
			lab.nbctl("--if-exists", "ls-del", ls)
		}
		lab.nbctl("--if-exists", "destroy", "Port_Group", "pg_trace")
	})
	atts := func(bDeleted bool) []ec2projection.TransitGatewayAttachment {
		return []ec2projection.TransitGatewayAttachment{
			traceAttachment("tgw-attach-a", "vpc-a", "subnet-a1", 1, 1, false),
			traceAttachment("tgw-attach-b", "vpc-b", "subnet-b1", 2, 2, bDeleted),
			traceAttachment("tgw-attach-c", "vpc-c", "subnet-c1", 3, 3, false),
			traceAttachment("tgw-attach-d", "vpc-d", "subnet-d1", 4, 4, false),
			traceAttachment("tgw-attach-e", "vpc-e", "subnet-e1", 5, 5, false),
		}
	}
	if err := rec.ReconcileApplyOnly(ctx, traceIntent(vpcs, atts(false), tables)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for id, cidr := range traceSubnets {
		lab.addVIF(id, strings.TrimSuffix(cidr, ".0/24")+".10")
	}
	// VPC B's internet gateway: a default route to a next hop on an outside switch.
	lab.nbctl("lrp-add", "vpc-vpc-b", "gw-trace", "00:00:00:00:ee:01", "192.0.2.1/24")
	lab.nbctl("ls-add", "ext-trace")
	lab.nbctl("lsp-add", "ext-trace", "rp-ext-trace", "--", "lsp-set-type", "rp-ext-trace", "router",
		"--", "lsp-set-addresses", "rp-ext-trace", "router", "--", "lsp-set-options", "rp-ext-trace", "router-port=gw-trace")
	lab.nbctl("lsp-add", "ext-trace", "vif-ext", "--", "lsp-set-addresses", "vif-ext", "00:00:00:00:ee:fe 192.0.2.254")
	lab.nbctl("lr-route-add", "vpc-vpc-b", "0.0.0.0/0", "192.0.2.254", "gw-trace")

	for _, tc := range []struct {
		name, from, src, dst, want string
	}{
		{"route to the peer delivers", "subnet-a1", "10.0.1.10", "10.1.1.10", "vif-subnet-b1"},
		{"return path needs its own route", "subnet-b1", "10.1.1.10", "10.0.1.10", "vif-subnet-a1"},
		{"attached VPC with no route sends nothing", "subnet-c1", "10.2.1.10", "10.0.1.10", "dropped"},
		{"summary route reaches every attached VPC", "subnet-a1", "10.0.1.10", "10.2.1.10", "vif-subnet-c1"},
		{"partial overlap forwards by longest prefix", "subnet-a1", "10.0.1.10", "10.1.5.10", "vif-subnet-e1"},
		{"zone without attachment subnet is dropped", "subnet-a2", "10.0.2.10", "10.1.1.10", "dropped"},
		{"table without the route is dropped", "subnet-a3", "10.0.3.10", "10.1.1.10", "dropped"},
		{"default route still reaches the gateway", "subnet-b1", "10.1.1.10", "198.51.100.7", "vif-ext"},
	} {
		if got := lab.trace(tc.from, tc.src, tc.dst); got != tc.want {
			t.Errorf("%s: %s → %s left on %s, want %s", tc.name, tc.src, tc.dst, got, tc.want)
		}
	}

	pg := "pg_trace"
	lab.nbctl("pg-add", pg, "vif-subnet-c1")
	lab.nbctl("acl-add", pg, "to-lport", "1001", `outport == @`+pg+` && ip4`, "drop")
	if got := lab.trace("subnet-a1", "10.0.1.10", "10.2.1.10"); got != "dropped" {
		t.Errorf("a security group drop on the destination left on %s", got)
	}
	lab.nbctl("pg-del", pg)

	if err := rec.ReconcileApplyOnly(ctx, traceIntent(vpcs, atts(true), tables)); err != nil {
		t.Fatalf("Reconcile after delete: %v", err)
	}
	for _, tc := range []struct {
		name, from, src, dst, want string
	}{
		{"deleted attachment is a blackhole, not the default route", "subnet-b1", "10.1.1.10", "10.0.1.10", "dropped"},
		{"next earliest identical range takes over", "subnet-a1", "10.0.1.10", "10.1.1.10", "vif-subnet-d1"},
	} {
		if got := lab.trace(tc.from, tc.src, tc.dst); got != tc.want {
			t.Errorf("%s: %s → %s left on %s, want %s", tc.name, tc.src, tc.dst, got, tc.want)
		}
	}
}
