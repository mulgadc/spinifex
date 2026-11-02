package reconcile

import (
	"context"
	"net/netip"
	"slices"
	"sync"
	"testing"

	ec2eip "github.com/mulgadc/spinifex/spinifex/domains/ec2/eip"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/domains/network/policy"
	"github.com/mulgadc/spinifex/spinifex/domains/network/topology"
	"github.com/nats-io/nats.go/jetstream"
)

// hostBindings stands in for the routed-mode host route and proxy-ARP table.
type hostBindings struct {
	mu    sync.Mutex
	bound map[string]struct{}
}

func (h *hostBindings) hooks() policy.HostEIPBinder {
	return policy.HostEIPBinder{
		Bind: func(eip policy.EIPSpec, _ string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.bound[eip.ExternalIP] = struct{}{}
			return nil
		},
		Unbind: func(externalIP string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			delete(h.bound, externalIP)
			return nil
		},
		List: func() ([]string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			out := make([]string, 0, len(h.bound))
			for ip := range h.bound {
				out = append(out, ip)
			}
			slices.Sort(out)
			return out, nil
		},
	}
}

// seedOverlappingTenants writes two accounts' default-style VPCs on the same
// CIDR, each with one ENI at 172.31.0.4. eni-a carries publicA on its stamp.
func seedOverlappingTenants(t *testing.T, js jetstream.JetStream, publicA string) {
	t.Helper()
	for _, tenant := range []struct{ acct, suffix, mac, public string }{
		{"111111111111", "a", "02:00:00:00:0a:04", publicA},
		{"222222222222", "b", "02:00:00:00:0b:04", ""},
	} {
		vpcID, subnetID, sgID, eniID := "vpc-"+tenant.suffix, "subnet-"+tenant.suffix, "sg-"+tenant.suffix, "eni-"+tenant.suffix
		seedKV(t, js, ec2vpc.KVBucketVPCs, tenant.acct+"."+vpcID, ec2vpc.VPCRecord{
			VpcId: vpcID, CidrBlock: "172.31.0.0/16", AZ: "us-east-1a", VNI: 100,
		})
		seedKV(t, js, ec2vpc.KVBucketSubnets, tenant.acct+"."+subnetID, ec2vpc.SubnetRecord{
			SubnetId: subnetID, VpcId: vpcID, CidrBlock: "172.31.0.0/20",
		})
		seedKV(t, js, ec2vpc.KVBucketSecurityGroups, tenant.acct+"."+sgID, ec2vpc.SecurityGroupRecord{
			GroupId: sgID, VpcId: vpcID,
		})
		seedKV(t, js, ec2vpc.KVBucketENIs, tenant.acct+"."+eniID, ec2vpc.ENIRecord{
			NetworkInterfaceId: eniID, SubnetId: subnetID, VpcId: vpcID,
			PrivateIpAddress: "172.31.0.4", MacAddress: tenant.mac,
			SecurityGroupIds: []string{sgID}, PublicIpAddress: tenant.public,
		})
	}
}

func seedAssociatedEIP(t *testing.T, js jetstream.JetStream, acct, suffix, publicIP, mac string) {
	t.Helper()
	seedKV(t, js, ec2eip.KVBucketEIPs, acct+".eipalloc-"+suffix, ec2eip.EIPRecord{
		AllocationId: "eipalloc-" + suffix, PublicIp: publicIP, State: "associated",
		AssociationId: "eipassoc-" + suffix, ENIId: "eni-" + suffix,
		PrivateIp: "172.31.0.4", MacAddress: mac, VpcId: "vpc-" + suffix,
	})
}

// Two tenants' guests share a private IP on overlapping VPC CIDRs. Tenant B
// associating its own EIP must leave tenant A's public address, NAT row and
// host binding alone, whether A's address is a user EIP or auto-assigned.
func TestReconcile_OverlappingVPCsKeepEachTenantsPublicIP(t *testing.T) {
	cases := []struct {
		name    string
		publicA string
		eipA    bool
	}{
		{name: "eip beside eip", publicA: "203.0.113.10", eipA: true},
		{name: "eip beside auto-assigned", publicA: "198.51.100.20"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			js := startKV(t)
			seedOverlappingTenants(t, js, tc.publicA)
			if tc.eipA {
				seedAssociatedEIP(t, js, "111111111111", "a", tc.publicA, "02:00:00:00:0a:04")
			}

			r, m := newTestReconciler(t)
			hosts := &hostBindings{bound: map[string]struct{}{}}
			nat, err := policy.NewNATManager(m, policy.NATModeRouted, policy.WithHostEIPBinder(hosts.hooks()))
			if err != nil {
				t.Fatalf("NewNATManager: %v", err)
			}
			r.nat = nat
			r.reloadIntent = func(ctx context.Context) (IntentState, error) {
				return LoadIntentFromKV(ctx, js, "us-east-1a")
			}

			pass := func() {
				t.Helper()
				intent, err := r.reloadIntent(ctx)
				if err != nil {
					t.Fatalf("LoadIntentFromKV: %v", err)
				}
				if err := r.Reconcile(ctx, intent); err != nil {
					t.Fatalf("Reconcile: %v", err)
				}
				if err := r.ReconcileHostEIPs(ctx); err != nil {
					t.Fatalf("ReconcileHostEIPs: %v", err)
				}
			}
			assertPublic := func(stage, externalIP, port string) {
				t.Helper()
				row := findNATByExternal(m, "dnat_and_snat", externalIP)
				if row == nil {
					t.Fatalf("%s: dnat_and_snat for %s missing", stage, externalIP)
				}
				if got := row.ExternalIDs["spinifex:logical_port"]; got != topology.Port(port) {
					t.Errorf("%s: %s row owned by %q, want %q", stage, externalIP, got, topology.Port(port))
				}
				if _, ok := hosts.bound[externalIP]; !ok {
					t.Errorf("%s: host binding for %s missing", stage, externalIP)
				}
			}

			pass()
			assertPublic("before tenant B associates", tc.publicA, "eni-a")

			seedAssociatedEIP(t, js, "222222222222", "b", "203.0.113.20", "02:00:00:00:0b:04")
			for _, stage := range []string{"after tenant B associates", "on the next pass"} {
				pass()
				assertPublic(stage, tc.publicA, "eni-a")
				assertPublic(stage, "203.0.113.20", "eni-b")
			}
		})
	}
}

// The drop-gate exemption must name the EIP's own subnet, not a subnet in
// another VPC whose CIDR happens to contain the same private IP.
func TestSubnetIDForIP_ScopedToVPC(t *testing.T) {
	subnets := map[string]topology.SubnetSpec{
		"subnet-a": {SubnetID: "subnet-a", VPCID: "vpc-a", CIDR: netip.MustParsePrefix("172.31.0.0/20")},
		"subnet-b": {SubnetID: "subnet-b", VPCID: "vpc-b", CIDR: netip.MustParsePrefix("172.31.0.0/20")},
	}
	for range 20 {
		if got := subnetIDForIP(subnets, "vpc-a", "172.31.0.4"); got != "subnet-a" {
			t.Fatalf("vpc-a lookup = %q, want subnet-a", got)
		}
		if got := subnetIDForIP(subnets, "vpc-b", "172.31.0.4"); got != "subnet-b" {
			t.Fatalf("vpc-b lookup = %q, want subnet-b", got)
		}
	}
	if got := subnetIDForIP(subnets, "vpc-c", "172.31.0.4"); got != "" {
		t.Errorf("lookup in a VPC with no subnets = %q, want empty", got)
	}
}
