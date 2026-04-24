package vpcd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/services/vpcd/nbdb"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// startTestNATS starts an embedded NATS server for testing.
func startTestNATS(t *testing.T) (*server.Server, *nats.Conn) {
	t.Helper()
	return testutil.StartTestNATS(t)
}

func TestTopologyHandler_VPCCreate(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	evt := VPCEvent{VpcId: "vpc-abc123", CidrBlock: "10.0.0.0/16", VNI: 100}
	data, _ := json.Marshal(evt)

	resp, err := nc.Request(TopicVPCCreate, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.create: %v", err)
	}

	var result struct {
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !result.Success {
		t.Fatalf("vpc.create failed: %s", result.Error)
	}

	// Verify router was created in OVN
	ctx := context.Background()
	lr, err := mock.GetLogicalRouter(ctx, "vpc-vpc-abc123")
	if err != nil {
		t.Fatalf("expected logical router: %v", err)
	}
	if lr.ExternalIDs["spinifex:vpc_id"] != "vpc-abc123" {
		t.Errorf("expected vpc_id external_id, got %v", lr.ExternalIDs)
	}
	if lr.ExternalIDs["spinifex:vni"] != "100" {
		t.Errorf("expected vni external_id=100, got %v", lr.ExternalIDs["spinifex:vni"])
	}
}

func TestTopologyHandler_VPCDelete(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Pre-create a router
	_ = mock.CreateLogicalRouter(ctx, nbdbLogicalRouter("vpc-vpc-xyz", "vpc-xyz"))

	// Delete via NATS
	evt := VPCEvent{VpcId: "vpc-xyz"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicVPCDelete, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.delete: %v", err)
	}

	var result struct {
		Success bool `json:"Success"`
	}
	_ = json.Unmarshal(resp.Data, &result)
	if !result.Success {
		t.Fatal("vpc.delete failed")
	}

	// Verify router is gone
	_, err = mock.GetLogicalRouter(ctx, "vpc-vpc-xyz")
	if err == nil {
		t.Error("expected router to be deleted")
	}
}

func TestTopologyHandler_SubnetCreate(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Pre-create the VPC router
	_ = mock.CreateLogicalRouter(ctx, nbdbLogicalRouter("vpc-vpc-sub1", "vpc-sub1"))

	// Create subnet
	evt := SubnetEvent{SubnetId: "subnet-aaa", VpcId: "vpc-sub1", CidrBlock: "10.0.1.0/24"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicSubnetCreate, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.create-subnet: %v", err)
	}

	var result struct {
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}
	_ = json.Unmarshal(resp.Data, &result)
	if !result.Success {
		t.Fatalf("vpc.create-subnet failed: %s", result.Error)
	}

	// Verify logical switch created
	ls, err := mock.GetLogicalSwitch(ctx, "subnet-subnet-aaa")
	if err != nil {
		t.Fatalf("expected logical switch: %v", err)
	}
	if ls.ExternalIDs["spinifex:subnet_id"] != "subnet-aaa" {
		t.Errorf("expected subnet_id external_id, got %v", ls.ExternalIDs)
	}

	// Verify router port created
	lr, err := mock.GetLogicalRouter(ctx, "vpc-vpc-sub1")
	if err != nil {
		t.Fatalf("expected router: %v", err)
	}
	if len(lr.Ports) != 1 {
		t.Errorf("expected 1 router port, got %d", len(lr.Ports))
	}

	// Verify switch has 1 port (the router port)
	if len(ls.Ports) != 1 {
		t.Errorf("expected 1 switch port, got %d", len(ls.Ports))
	}

	// Verify DHCP options created
	dhcpOpts, err := mock.FindDHCPOptionsByCIDR(ctx, "10.0.1.0/24")
	if err != nil {
		t.Fatalf("expected DHCP options: %v", err)
	}
	if dhcpOpts.Options["router"] != "10.0.1.1" {
		t.Errorf("expected router=10.0.1.1, got %s", dhcpOpts.Options["router"])
	}
	if dhcpOpts.Options["lease_time"] != "3600" {
		t.Errorf("expected lease_time=3600, got %s", dhcpOpts.Options["lease_time"])
	}
	if dhcpOpts.Options["mtu"] != "1442" {
		t.Errorf("expected mtu=1442, got %s", dhcpOpts.Options["mtu"])
	}
}

func TestTopologyHandler_SubnetDelete(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Setup: create VPC router + subnet topology manually
	_ = mock.CreateLogicalRouter(ctx, nbdbLogicalRouter("vpc-vpc-del", "vpc-del"))
	_ = mock.CreateLogicalSwitch(ctx, nbdbLogicalSwitch("subnet-subnet-del", "subnet-del", "vpc-del"))
	_ = mock.CreateLogicalRouterPort(ctx, "vpc-vpc-del", nbdbLogicalRouterPort("rtr-subnet-del", "subnet-del", "vpc-del"))
	_ = mock.CreateLogicalSwitchPort(ctx, "subnet-subnet-del", nbdbLogicalSwitchPortRouter("rtr-port-subnet-del", "rtr-subnet-del", "subnet-del", "vpc-del"))
	_, _ = mock.CreateDHCPOptions(ctx, nbdbDHCPOptions("10.0.2.0/24", "subnet-del", "vpc-del"))

	// Delete subnet via NATS
	evt := SubnetEvent{SubnetId: "subnet-del", VpcId: "vpc-del", CidrBlock: "10.0.2.0/24"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicSubnetDelete, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.delete-subnet: %v", err)
	}

	var result struct {
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}
	_ = json.Unmarshal(resp.Data, &result)
	if !result.Success {
		t.Fatalf("vpc.delete-subnet failed: %s", result.Error)
	}

	// Verify switch is deleted
	_, err = mock.GetLogicalSwitch(ctx, "subnet-subnet-del")
	if err == nil {
		t.Error("expected switch to be deleted")
	}

	// Verify DHCP options are deleted
	_, err = mock.FindDHCPOptionsByCIDR(ctx, "10.0.2.0/24")
	if err == nil {
		t.Error("expected DHCP options to be deleted")
	}

	// Verify router still exists (only subnet topology deleted, not VPC)
	_, err = mock.GetLogicalRouter(ctx, "vpc-vpc-del")
	if err != nil {
		t.Error("expected VPC router to still exist")
	}
}

func TestTopologyHandler_FullLifecycle(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// 1. Create VPC
	vpcEvt := VPCEvent{VpcId: "vpc-full", CidrBlock: "10.0.0.0/16", VNI: 200}
	data, _ := json.Marshal(vpcEvt)
	resp, _ := nc.Request(TopicVPCCreate, data, 5_000_000_000)
	assertSuccess(t, resp, "create VPC")

	// 2. Create Subnet 1
	subEvt1 := SubnetEvent{SubnetId: "subnet-a", VpcId: "vpc-full", CidrBlock: "10.0.1.0/24"}
	data, _ = json.Marshal(subEvt1)
	resp, _ = nc.Request(TopicSubnetCreate, data, 5_000_000_000)
	assertSuccess(t, resp, "create subnet-a")

	// 3. Create Subnet 2
	subEvt2 := SubnetEvent{SubnetId: "subnet-b", VpcId: "vpc-full", CidrBlock: "10.0.2.0/24"}
	data, _ = json.Marshal(subEvt2)
	resp, _ = nc.Request(TopicSubnetCreate, data, 5_000_000_000)
	assertSuccess(t, resp, "create subnet-b")

	// Verify: 1 router with 2 ports, 2 switches, 2 DHCP option sets
	routers, _ := mock.ListLogicalRouters(ctx)
	if len(routers) != 1 {
		t.Errorf("expected 1 router, got %d", len(routers))
	}
	switches, _ := mock.ListLogicalSwitches(ctx)
	if len(switches) != 2 {
		t.Errorf("expected 2 switches, got %d", len(switches))
	}
	dhcpList, _ := mock.ListDHCPOptions(ctx)
	if len(dhcpList) != 2 {
		t.Errorf("expected 2 DHCP option sets, got %d", len(dhcpList))
	}

	// 4. Delete Subnet 1
	data, _ = json.Marshal(subEvt1)
	resp, _ = nc.Request(TopicSubnetDelete, data, 5_000_000_000)
	assertSuccess(t, resp, "delete subnet-a")

	switches, _ = mock.ListLogicalSwitches(ctx)
	if len(switches) != 1 {
		t.Errorf("expected 1 switch after delete, got %d", len(switches))
	}

	// 5. Delete Subnet 2
	data, _ = json.Marshal(subEvt2)
	resp, _ = nc.Request(TopicSubnetDelete, data, 5_000_000_000)
	assertSuccess(t, resp, "delete subnet-b")

	// 6. Delete VPC
	delEvt := VPCEvent{VpcId: "vpc-full"}
	data, _ = json.Marshal(delEvt)
	resp, _ = nc.Request(TopicVPCDelete, data, 5_000_000_000)
	assertSuccess(t, resp, "delete VPC")

	// Verify everything is gone
	routers, _ = mock.ListLogicalRouters(ctx)
	if len(routers) != 0 {
		t.Errorf("expected 0 routers, got %d", len(routers))
	}
	switches, _ = mock.ListLogicalSwitches(ctx)
	if len(switches) != 0 {
		t.Errorf("expected 0 switches, got %d", len(switches))
	}
	dhcpList, _ = mock.ListDHCPOptions(ctx)
	if len(dhcpList) != 0 {
		t.Errorf("expected 0 DHCP options, got %d", len(dhcpList))
	}
}

func TestTopologyHandler_VPCDeleteCascade(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Create VPC + subnet directly in mock (simulating pre-existing state)
	_ = mock.CreateLogicalRouter(ctx, nbdbLogicalRouter("vpc-vpc-cas", "vpc-cas"))
	_ = mock.CreateLogicalSwitch(ctx, nbdbLogicalSwitch("subnet-sub-cas", "sub-cas", "vpc-cas"))
	_, _ = mock.CreateDHCPOptions(ctx, nbdbDHCPOptions("10.0.3.0/24", "sub-cas", "vpc-cas"))

	// Delete VPC should cascade-delete switches and DHCP
	evt := VPCEvent{VpcId: "vpc-cas"}
	data, _ := json.Marshal(evt)
	resp, _ := nc.Request(TopicVPCDelete, data, 5_000_000_000)
	assertSuccess(t, resp, "cascade delete VPC")

	switches, _ := mock.ListLogicalSwitches(ctx)
	if len(switches) != 0 {
		t.Errorf("expected 0 switches after cascade delete, got %d", len(switches))
	}
	dhcpList, _ := mock.ListDHCPOptions(ctx)
	if len(dhcpList) != 0 {
		t.Errorf("expected 0 DHCP options after cascade delete, got %d", len(dhcpList))
	}
}

func TestSubnetGateway(t *testing.T) {
	tests := []struct {
		cidr     string
		wantIP   string
		wantMask int
		wantErr  bool
	}{
		{"10.0.1.0/24", "10.0.1.1", 24, false},
		{"192.168.0.0/16", "192.168.0.1", 16, false},
		{"172.16.0.0/20", "172.16.0.1", 20, false},
		{"invalid", "", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.cidr, func(t *testing.T) {
			ip, mask, err := subnetGateway(tt.cidr)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ip != tt.wantIP {
				t.Errorf("expected IP %s, got %s", tt.wantIP, ip)
			}
			if mask != tt.wantMask {
				t.Errorf("expected mask %d, got %d", tt.wantMask, mask)
			}
		})
	}
}

func TestGenerateMAC(t *testing.T) {
	mac1 := generateMAC("subnet-aaa")
	mac2 := generateMAC("subnet-bbb")

	// Must start with locally-administered unicast prefix
	if mac1[:8] != "02:00:00" {
		t.Errorf("expected prefix 02:00:00, got %s", mac1[:8])
	}

	// Different inputs produce different MACs
	if mac1 == mac2 {
		t.Error("expected different MACs for different inputs")
	}

	// Same input produces same MAC (deterministic)
	mac1b := generateMAC("subnet-aaa")
	if mac1 != mac1b {
		t.Error("expected deterministic MAC")
	}
}

func TestTopologyHandler_CreatePort(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Setup: create VPC router, subnet switch, and DHCP options
	_ = mock.CreateLogicalRouter(ctx, nbdbLogicalRouter("vpc-vpc-port1", "vpc-port1"))
	_ = mock.CreateLogicalSwitch(ctx, nbdbLogicalSwitch("subnet-subnet-port1", "subnet-port1", "vpc-port1"))
	dhcpUUID, _ := mock.CreateDHCPOptions(ctx, nbdbDHCPOptions("10.0.1.0/24", "subnet-port1", "vpc-port1"))

	// Create port via NATS
	evt := PortEvent{
		NetworkInterfaceId: "eni-aaa111",
		SubnetId:           "subnet-port1",
		VpcId:              "vpc-port1",
		PrivateIpAddress:   "10.0.1.4",
		MacAddress:         "02:00:00:11:22:33",
	}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicCreatePort, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.create-port: %v", err)
	}
	assertSuccess(t, resp, "create port")

	// Verify logical switch port created
	lsp, err := mock.GetLogicalSwitchPort(ctx, "port-eni-aaa111")
	if err != nil {
		t.Fatalf("expected logical switch port: %v", err)
	}

	// Verify addresses
	expectedAddr := "02:00:00:11:22:33 10.0.1.4"
	if len(lsp.Addresses) != 1 || lsp.Addresses[0] != expectedAddr {
		t.Errorf("expected addresses [%s], got %v", expectedAddr, lsp.Addresses)
	}

	// Verify port security
	if len(lsp.PortSecurity) != 1 || lsp.PortSecurity[0] != expectedAddr {
		t.Errorf("expected port_security [%s], got %v", expectedAddr, lsp.PortSecurity)
	}

	// Verify DHCPv4 options
	if lsp.DHCPv4Options == nil {
		t.Fatal("expected DHCPv4Options to be set")
	}
	if *lsp.DHCPv4Options != dhcpUUID {
		t.Errorf("expected DHCPv4Options UUID %s, got %s", dhcpUUID, *lsp.DHCPv4Options)
	}

	// Verify external IDs
	if lsp.ExternalIDs["spinifex:eni_id"] != "eni-aaa111" {
		t.Errorf("expected eni_id=eni-aaa111, got %s", lsp.ExternalIDs["spinifex:eni_id"])
	}
	if lsp.ExternalIDs["spinifex:subnet_id"] != "subnet-port1" {
		t.Errorf("expected subnet_id=subnet-port1, got %s", lsp.ExternalIDs["spinifex:subnet_id"])
	}
	if lsp.ExternalIDs["spinifex:vpc_id"] != "vpc-port1" {
		t.Errorf("expected vpc_id=vpc-port1, got %s", lsp.ExternalIDs["spinifex:vpc_id"])
	}

	// Verify port was added to the switch
	ls, err := mock.GetLogicalSwitch(ctx, "subnet-subnet-port1")
	if err != nil {
		t.Fatalf("get switch: %v", err)
	}
	if len(ls.Ports) != 1 {
		t.Errorf("expected 1 port on switch, got %d", len(ls.Ports))
	}
}

func TestTopologyHandler_DeletePort(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Setup: create switch and port
	_ = mock.CreateLogicalSwitch(ctx, nbdbLogicalSwitch("subnet-subnet-del2", "subnet-del2", "vpc-del2"))
	_ = mock.CreateLogicalSwitchPort(ctx, "subnet-subnet-del2", &nbdb.LogicalSwitchPort{
		Name:         "port-eni-bbb222",
		Addresses:    []string{"02:00:00:44:55:66 10.0.2.4"},
		PortSecurity: []string{"02:00:00:44:55:66 10.0.2.4"},
		ExternalIDs: map[string]string{
			"spinifex:eni_id":    "eni-bbb222",
			"spinifex:subnet_id": "subnet-del2",
			"spinifex:vpc_id":    "vpc-del2",
		},
	})

	// Verify port exists before delete
	_, err = mock.GetLogicalSwitchPort(ctx, "port-eni-bbb222")
	if err != nil {
		t.Fatalf("expected port to exist before delete: %v", err)
	}

	// Delete port via NATS
	evt := PortEvent{
		NetworkInterfaceId: "eni-bbb222",
		SubnetId:           "subnet-del2",
		VpcId:              "vpc-del2",
		PrivateIpAddress:   "10.0.2.4",
		MacAddress:         "02:00:00:44:55:66",
	}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicDeletePort, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.delete-port: %v", err)
	}
	assertSuccess(t, resp, "delete port")

	// Verify port is gone
	_, err = mock.GetLogicalSwitchPort(ctx, "port-eni-bbb222")
	if err == nil {
		t.Error("expected port to be deleted")
	}

	// Verify switch still exists but has no ports
	ls, err := mock.GetLogicalSwitch(ctx, "subnet-subnet-del2")
	if err != nil {
		t.Fatalf("expected switch to still exist: %v", err)
	}
	if len(ls.Ports) != 0 {
		t.Errorf("expected 0 ports on switch after delete, got %d", len(ls.Ports))
	}
}

func TestTopologyHandler_CreatePortNoDHCP(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Setup: create switch but NO DHCP options
	_ = mock.CreateLogicalSwitch(ctx, nbdbLogicalSwitch("subnet-subnet-nodhcp", "subnet-nodhcp", "vpc-nodhcp"))

	// Create port — should succeed but without DHCPv4Options
	evt := PortEvent{
		NetworkInterfaceId: "eni-nodhcp",
		SubnetId:           "subnet-nodhcp",
		VpcId:              "vpc-nodhcp",
		PrivateIpAddress:   "10.0.3.4",
		MacAddress:         "02:00:00:77:88:99",
	}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicCreatePort, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.create-port: %v", err)
	}
	assertSuccess(t, resp, "create port without DHCP")

	// Port should exist but without DHCPv4Options
	lsp, err := mock.GetLogicalSwitchPort(ctx, "port-eni-nodhcp")
	if err != nil {
		t.Fatalf("expected port: %v", err)
	}
	if lsp.DHCPv4Options != nil {
		t.Errorf("expected nil DHCPv4Options when no DHCP configured, got %s", *lsp.DHCPv4Options)
	}
}

func TestTopologyHandler_PortLifecycle(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// 1. Create VPC
	vpcEvt := VPCEvent{VpcId: "vpc-plc", CidrBlock: "10.0.0.0/16", VNI: 300}
	data, _ := json.Marshal(vpcEvt)
	resp, _ := nc.Request(TopicVPCCreate, data, 5_000_000_000)
	assertSuccess(t, resp, "create VPC")

	// 2. Create Subnet
	subEvt := SubnetEvent{SubnetId: "subnet-plc", VpcId: "vpc-plc", CidrBlock: "10.0.1.0/24"}
	data, _ = json.Marshal(subEvt)
	resp, _ = nc.Request(TopicSubnetCreate, data, 5_000_000_000)
	assertSuccess(t, resp, "create subnet")

	// 3. Create two ports
	portEvt1 := PortEvent{
		NetworkInterfaceId: "eni-plc-1",
		SubnetId:           "subnet-plc",
		VpcId:              "vpc-plc",
		PrivateIpAddress:   "10.0.1.4",
		MacAddress:         "02:00:00:aa:bb:01",
	}
	data, _ = json.Marshal(portEvt1)
	resp, _ = nc.Request(TopicCreatePort, data, 5_000_000_000)
	assertSuccess(t, resp, "create port 1")

	portEvt2 := PortEvent{
		NetworkInterfaceId: "eni-plc-2",
		SubnetId:           "subnet-plc",
		VpcId:              "vpc-plc",
		PrivateIpAddress:   "10.0.1.5",
		MacAddress:         "02:00:00:aa:bb:02",
	}
	data, _ = json.Marshal(portEvt2)
	resp, _ = nc.Request(TopicCreatePort, data, 5_000_000_000)
	assertSuccess(t, resp, "create port 2")

	// Verify switch has 3 ports (router port + 2 ENI ports)
	ls, err := mock.GetLogicalSwitch(ctx, "subnet-subnet-plc")
	if err != nil {
		t.Fatalf("get switch: %v", err)
	}
	if len(ls.Ports) != 3 {
		t.Errorf("expected 3 ports (1 router + 2 ENI), got %d", len(ls.Ports))
	}

	// 4. Delete port 1
	data, _ = json.Marshal(portEvt1)
	resp, _ = nc.Request(TopicDeletePort, data, 5_000_000_000)
	assertSuccess(t, resp, "delete port 1")

	// Verify switch has 2 ports now
	ls, err = mock.GetLogicalSwitch(ctx, "subnet-subnet-plc")
	if err != nil {
		t.Fatalf("get switch after port delete: %v", err)
	}
	if len(ls.Ports) != 2 {
		t.Errorf("expected 2 ports after delete, got %d", len(ls.Ports))
	}

	// Port 2 should still exist
	_, err = mock.GetLogicalSwitchPort(ctx, "port-eni-plc-2")
	if err != nil {
		t.Error("expected port 2 to still exist")
	}

	// Port 1 should be gone
	_, err = mock.GetLogicalSwitchPort(ctx, "port-eni-plc-1")
	if err == nil {
		t.Error("expected port 1 to be deleted")
	}
}

func TestTopologyHandler_NilOVN(t *testing.T) {
	_, nc := startTestNATS(t)

	// nil OVN client (OVN not connected)
	topo := NewTopologyHandler(nil)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Should fail gracefully when OVN is nil
	evt := VPCEvent{VpcId: "vpc-nil", CidrBlock: "10.0.0.0/16", VNI: 100}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicVPCCreate, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	var result struct {
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}
	_ = json.Unmarshal(resp.Data, &result)
	if result.Success {
		t.Error("expected failure when OVN is nil")
	}

	// Port create should also fail gracefully
	portEvt := PortEvent{
		NetworkInterfaceId: "eni-nil",
		SubnetId:           "subnet-nil",
		VpcId:              "vpc-nil",
		PrivateIpAddress:   "10.0.1.4",
		MacAddress:         "02:00:00:00:00:01",
	}
	data, _ = json.Marshal(portEvt)
	resp, err = nc.Request(TopicCreatePort, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request create port: %v", err)
	}
	_ = json.Unmarshal(resp.Data, &result)
	if result.Success {
		t.Error("expected create-port failure when OVN is nil")
	}

	// Port delete should also fail gracefully
	resp, err = nc.Request(TopicDeletePort, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request delete port: %v", err)
	}
	_ = json.Unmarshal(resp.Data, &result)
	if result.Success {
		t.Error("expected delete-port failure when OVN is nil")
	}
}

func TestTopologyHandler_IGWAttach(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Pre-create VPC router
	_ = mock.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name: "vpc-vpc-igw1",
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": "vpc-igw1",
			"spinifex:cidr":   "10.0.0.0/16",
		},
	})

	// Attach IGW
	evt := types.IGWEvent{InternetGatewayId: "igw-test1", VpcId: "vpc-igw1"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicIGWAttach, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.igw-attach: %v", err)
	}
	assertSuccess(t, resp, "attach IGW")

	// Verify external switch created
	extSwitch, err := mock.GetLogicalSwitch(ctx, "ext-vpc-igw1")
	if err != nil {
		t.Fatalf("expected external switch: %v", err)
	}
	if extSwitch.ExternalIDs["spinifex:role"] != "external" {
		t.Errorf("expected role=external, got %s", extSwitch.ExternalIDs["spinifex:role"])
	}
	if extSwitch.ExternalIDs["spinifex:igw_id"] != "igw-test1" {
		t.Errorf("expected igw_id=igw-test1, got %s", extSwitch.ExternalIDs["spinifex:igw_id"])
	}

	// Verify localnet port created on external switch
	_, err = mock.GetLogicalSwitchPort(ctx, "ext-port-vpc-igw1")
	if err != nil {
		t.Fatalf("expected localnet port: %v", err)
	}

	// Verify gateway router port created
	router, err := mock.GetLogicalRouter(ctx, "vpc-vpc-igw1")
	if err != nil {
		t.Fatalf("expected router: %v", err)
	}
	if len(router.Ports) < 1 {
		t.Error("expected at least 1 router port (gateway)")
	}

	// Verify switch gateway port created
	_, err = mock.GetLogicalSwitchPort(ctx, "gw-port-vpc-igw1")
	if err != nil {
		t.Fatalf("expected switch gateway port: %v", err)
	}

	// Verify NO blanket SNAT rule — only per-VM dnat_and_snat rules should
	// provide NAT (AWS parity: private subnet instances cannot route via IGW)
	if len(router.NAT) != 0 {
		t.Errorf("expected 0 NAT rules (no blanket SNAT), got %d", len(router.NAT))
	}

	// Verify default route added
	if len(router.StaticRoutes) != 1 {
		t.Errorf("expected 1 static route, got %d", len(router.StaticRoutes))
	}
}

func TestTopologyHandler_IGWAttach_WithExternalPool(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	pools := []ExternalPoolConfig{
		{
			Name:       "wan",
			RangeStart: "192.168.1.150",
			RangeEnd:   "192.168.1.250",
			Gateway:    "192.168.1.1",
			PrefixLen:  24,
		},
	}
	topo := NewTopologyHandler(mock, WithExternalNetwork("pool", pools))
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Pre-create VPC router
	_ = mock.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name: "vpc-vpc-extpool",
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": "vpc-extpool",
			"spinifex:cidr":   "10.0.0.0/16",
		},
	})

	// Attach IGW
	evt := types.IGWEvent{InternetGatewayId: "igw-ext1", VpcId: "vpc-extpool"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicIGWAttach, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.igw-attach: %v", err)
	}
	assertSuccess(t, resp, "attach IGW with external pool")

	// Verify gateway router port uses real external IP, not link-local
	router, err := mock.GetLogicalRouter(ctx, "vpc-vpc-extpool")
	if err != nil {
		t.Fatalf("expected router: %v", err)
	}
	if len(router.Ports) < 1 {
		t.Fatal("expected at least 1 router port")
	}

	gwPort, err := mock.GetLogicalRouterPort(ctx, "gw-vpc-extpool")
	if err != nil {
		t.Fatalf("expected gateway router port: %v", err)
	}
	if len(gwPort.Networks) != 1 || gwPort.Networks[0] != "192.168.1.150/24" {
		t.Errorf("expected gateway network 192.168.1.150/24, got %v", gwPort.Networks)
	}

	// Verify NO blanket SNAT rule (AWS parity)
	if len(router.NAT) != 0 {
		t.Errorf("expected 0 NAT rules (no blanket SNAT), got %d", len(router.NAT))
	}

	// Verify default route points to WAN gateway, not link-local
	if len(router.StaticRoutes) != 1 {
		t.Fatalf("expected 1 static route, got %d", len(router.StaticRoutes))
	}
	route := mock.staticRoutes[router.StaticRoutes[0]]
	if route == nil {
		t.Fatal("static route not found in mock")
	}
	if route.Nexthop != "192.168.1.1" {
		t.Errorf("expected nexthop 192.168.1.1, got %s", route.Nexthop)
	}
}

func TestTopologyHandler_IGWAttach_PoolWithGatewayIP(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	pools := []ExternalPoolConfig{
		{
			Name:       "dc1",
			RangeStart: "203.0.113.2",
			RangeEnd:   "203.0.113.14",
			Gateway:    "203.0.113.1",
			GatewayIP:  "203.0.113.2", // Explicit gateway IP
			PrefixLen:  28,
		},
	}
	topo := NewTopologyHandler(mock, WithExternalNetwork("pool", pools))
	subs, _ := topo.Subscribe(nc)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	_ = mock.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name:        "vpc-vpc-gwip",
		ExternalIDs: map[string]string{"spinifex:vpc_id": "vpc-gwip", "spinifex:cidr": "10.0.0.0/16"},
	})

	evt := types.IGWEvent{InternetGatewayId: "igw-gwip", VpcId: "vpc-gwip"}
	data, _ := json.Marshal(evt)
	resp, _ := nc.Request(TopicIGWAttach, data, 5_000_000_000)
	assertSuccess(t, resp, "attach IGW with explicit gateway IP")

	router, _ := mock.GetLogicalRouter(ctx, "vpc-vpc-gwip")
	gwPort, _ := mock.GetLogicalRouterPort(ctx, "gw-vpc-gwip")
	if gwPort.Networks[0] != "203.0.113.2/28" {
		t.Errorf("expected 203.0.113.2/28, got %s", gwPort.Networks[0])
	}
	// No blanket SNAT rule (AWS parity)
	if len(router.NAT) != 0 {
		t.Errorf("expected 0 NAT rules (no blanket SNAT), got %d", len(router.NAT))
	}
	route := mock.staticRoutes[router.StaticRoutes[0]]
	if route.Nexthop != "203.0.113.1" {
		t.Errorf("expected nexthop 203.0.113.1, got %s", route.Nexthop)
	}
}

func TestTopologyHandler_FindExternalPool(t *testing.T) {
	pools := []ExternalPoolConfig{
		{Name: "az-a", Region: "us-east-1", AZ: "us-east-1a", RangeStart: "1.1.1.1"},
		{Name: "region", Region: "us-east-1", RangeStart: "2.2.2.2"},
		{Name: "global", RangeStart: "3.3.3.3"},
	}
	topo := NewTopologyHandler(nil, WithExternalNetwork("pool", pools))

	// AZ match
	p := topo.findExternalPool("us-east-1", "us-east-1a")
	if p == nil || p.Name != "az-a" {
		t.Errorf("expected az-a pool, got %v", p)
	}

	// Region fallback (no matching AZ)
	p = topo.findExternalPool("us-east-1", "us-east-1b")
	if p == nil || p.Name != "region" {
		t.Errorf("expected region pool, got %v", p)
	}

	// Global fallback
	p = topo.findExternalPool("eu-west-1", "eu-west-1a")
	if p == nil || p.Name != "global" {
		t.Errorf("expected global pool, got %v", p)
	}

	// No pools
	topo2 := NewTopologyHandler(nil)
	p = topo2.findExternalPool("us-east-1", "us-east-1a")
	if p != nil {
		t.Errorf("expected nil pool, got %v", p)
	}
}

func TestTopologyHandler_IGWDetach(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Pre-create VPC router
	_ = mock.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name: "vpc-vpc-igw2",
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": "vpc-igw2",
			"spinifex:cidr":   "10.0.0.0/16",
		},
	})

	// Attach IGW first
	attachEvt := types.IGWEvent{InternetGatewayId: "igw-test2", VpcId: "vpc-igw2"}
	data, _ := json.Marshal(attachEvt)
	resp, err := nc.Request(TopicIGWAttach, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.igw-attach: %v", err)
	}
	assertSuccess(t, resp, "attach IGW")

	// Verify resources exist before detach
	_, err = mock.GetLogicalSwitch(ctx, "ext-vpc-igw2")
	if err != nil {
		t.Fatal("expected external switch before detach")
	}

	// Detach IGW
	detachEvt := types.IGWEvent{InternetGatewayId: "igw-test2", VpcId: "vpc-igw2"}
	data, _ = json.Marshal(detachEvt)
	resp, err = nc.Request(TopicIGWDetach, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.igw-detach: %v", err)
	}
	assertSuccess(t, resp, "detach IGW")

	// Verify external switch deleted
	_, err = mock.GetLogicalSwitch(ctx, "ext-vpc-igw2")
	if err == nil {
		t.Error("expected external switch to be deleted")
	}

	// Verify router still exists but NAT and routes are cleaned up
	router, err := mock.GetLogicalRouter(ctx, "vpc-vpc-igw2")
	if err != nil {
		t.Fatal("expected VPC router to still exist")
	}
	if len(router.NAT) != 0 {
		t.Errorf("expected 0 NAT rules after detach, got %d", len(router.NAT))
	}
	if len(router.StaticRoutes) != 0 {
		t.Errorf("expected 0 static routes after detach, got %d", len(router.StaticRoutes))
	}
	if len(router.Ports) != 0 {
		t.Errorf("expected 0 router ports after detach, got %d", len(router.Ports))
	}
}

func TestTopologyHandler_IGWAttachDetachLifecycle(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// 1. Create VPC
	vpcEvt := VPCEvent{VpcId: "vpc-igwlc", CidrBlock: "10.0.0.0/16", VNI: 400}
	data, _ := json.Marshal(vpcEvt)
	resp, _ := nc.Request(TopicVPCCreate, data, 5_000_000_000)
	assertSuccess(t, resp, "create VPC")

	// 2. Create subnet
	subEvt := SubnetEvent{SubnetId: "subnet-igwlc", VpcId: "vpc-igwlc", CidrBlock: "10.0.1.0/24"}
	data, _ = json.Marshal(subEvt)
	resp, _ = nc.Request(TopicSubnetCreate, data, 5_000_000_000)
	assertSuccess(t, resp, "create subnet")

	// 3. Attach IGW
	igwEvt := types.IGWEvent{InternetGatewayId: "igw-lc1", VpcId: "vpc-igwlc"}
	data, _ = json.Marshal(igwEvt)
	resp, _ = nc.Request(TopicIGWAttach, data, 5_000_000_000)
	assertSuccess(t, resp, "attach IGW")

	// Verify full topology: 2 switches (subnet + external), 1 router with ports+NAT+routes
	switches, _ := mock.ListLogicalSwitches(ctx)
	if len(switches) != 2 {
		t.Errorf("expected 2 switches (subnet + external), got %d", len(switches))
	}

	router, _ := mock.GetLogicalRouter(ctx, "vpc-vpc-igwlc")
	if len(router.Ports) != 2 {
		t.Errorf("expected 2 router ports (subnet + gateway), got %d", len(router.Ports))
	}

	// 4. Detach IGW
	data, _ = json.Marshal(igwEvt)
	resp, _ = nc.Request(TopicIGWDetach, data, 5_000_000_000)
	assertSuccess(t, resp, "detach IGW")

	// Only subnet switch should remain
	switches, _ = mock.ListLogicalSwitches(ctx)
	if len(switches) != 1 {
		t.Errorf("expected 1 switch after IGW detach, got %d", len(switches))
	}

	// Router should still have subnet port but no gateway port
	router, _ = mock.GetLogicalRouter(ctx, "vpc-vpc-igwlc")
	if len(router.Ports) != 1 {
		t.Errorf("expected 1 router port after IGW detach, got %d", len(router.Ports))
	}

	// 5. Delete subnet and VPC
	data, _ = json.Marshal(subEvt)
	resp, _ = nc.Request(TopicSubnetDelete, data, 5_000_000_000)
	assertSuccess(t, resp, "delete subnet")

	data, _ = json.Marshal(VPCEvent{VpcId: "vpc-igwlc"})
	resp, _ = nc.Request(TopicVPCDelete, data, 5_000_000_000)
	assertSuccess(t, resp, "delete VPC")

	// Everything should be gone
	routers, _ := mock.ListLogicalRouters(ctx)
	if len(routers) != 0 {
		t.Errorf("expected 0 routers, got %d", len(routers))
	}
	switches, _ = mock.ListLogicalSwitches(ctx)
	if len(switches) != 0 {
		t.Errorf("expected 0 switches, got %d", len(switches))
	}
}

func TestTopologyHandler_IGWNilOVN(t *testing.T) {
	_, nc := startTestNATS(t)

	topo := NewTopologyHandler(nil)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Should fail gracefully when OVN is nil
	evt := types.IGWEvent{InternetGatewayId: "igw-nil", VpcId: "vpc-nil"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicIGWAttach, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	var result struct {
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}
	_ = json.Unmarshal(resp.Data, &result)
	if result.Success {
		t.Error("expected failure when OVN is nil")
	}

	// Detach should also fail gracefully
	resp, err = nc.Request(TopicIGWDetach, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = json.Unmarshal(resp.Data, &result)
	if result.Success {
		t.Error("expected detach failure when OVN is nil")
	}
}

// --- Error path tests (Phase 3) ---

func assertFailure(t *testing.T, msg *nats.Msg, label string) {
	t.Helper()
	var result struct {
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(msg.Data, &result); err != nil {
		t.Fatalf("%s: unmarshal: %v", label, err)
	}
	if result.Success {
		t.Fatalf("%s: expected failure, got success", label)
	}
}

func TestTopologyHandler_BadJSON(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	badData := []byte("{invalid json")

	// Each topic should return an error response on bad JSON
	topics := []string{
		TopicVPCCreate, TopicVPCDelete,
		TopicSubnetCreate, TopicSubnetDelete,
		TopicCreatePort, TopicDeletePort,
		TopicIGWAttach, TopicIGWDetach,
	}
	for _, topic := range topics {
		resp, err := nc.Request(topic, badData, 5_000_000_000)
		if err != nil {
			t.Fatalf("request %s: %v", topic, err)
		}
		assertFailure(t, resp, "bad JSON on "+topic)
	}
}

func TestTopologyHandler_VPCCreate_DuplicateRouter(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Pre-create router to trigger idempotent path
	_ = mock.CreateLogicalRouter(ctx, nbdbLogicalRouter("vpc-vpc-dup", "vpc-dup"))

	evt := VPCEvent{VpcId: "vpc-dup", CidrBlock: "10.0.0.0/16", VNI: 100}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicVPCCreate, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	assertSuccess(t, resp, "idempotent VPC create")

	// Verify exactly 1 router (no duplicate created)
	routers, _ := mock.ListLogicalRouters(ctx)
	if len(routers) != 1 {
		t.Errorf("expected 1 router (idempotent), got %d", len(routers))
	}
}

func TestTopologyHandler_VPCDelete_RouterNotFound(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Delete VPC that doesn't exist — DeleteLogicalRouter will fail
	evt := VPCEvent{VpcId: "vpc-nonexistent"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicVPCDelete, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	assertFailure(t, resp, "delete nonexistent VPC")
}

func TestTopologyHandler_SubnetCreate_SwitchAlreadyExists(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Pre-create switch to trigger idempotent path
	_ = mock.CreateLogicalSwitch(ctx, nbdbLogicalSwitch("subnet-subnet-exists", "subnet-exists", "vpc-exists"))

	evt := SubnetEvent{SubnetId: "subnet-exists", VpcId: "vpc-exists", CidrBlock: "10.0.1.0/24"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicSubnetCreate, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	assertSuccess(t, resp, "idempotent subnet create")

	// Verify exactly 1 switch (no duplicate)
	switches, _ := mock.ListLogicalSwitches(ctx)
	if len(switches) != 1 {
		t.Errorf("expected 1 switch (idempotent), got %d", len(switches))
	}
}

func TestTopologyHandler_SubnetCreate_RouterPortFails(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Don't create the VPC router — CreateLogicalRouterPort will fail
	evt := SubnetEvent{SubnetId: "subnet-norouter", VpcId: "vpc-norouter", CidrBlock: "10.0.1.0/24"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicSubnetCreate, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	assertFailure(t, resp, "subnet create without router")

	// Verify switch was cleaned up (best-effort cleanup path)
	_, err = mock.GetLogicalSwitch(ctx, "subnet-subnet-norouter")
	if err == nil {
		t.Error("expected switch to be cleaned up after router port failure")
	}
}

func TestTopologyHandler_SubnetCreate_InvalidCIDR(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	evt := SubnetEvent{SubnetId: "subnet-badcidr", VpcId: "vpc-badcidr", CidrBlock: "not-a-cidr"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicSubnetCreate, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	assertFailure(t, resp, "subnet create with invalid CIDR")
}

func TestTopologyHandler_SubnetDelete_SwitchNotFound(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Delete subnet where nothing exists — switch delete fails at step 4
	evt := SubnetEvent{SubnetId: "subnet-ghost", VpcId: "vpc-ghost", CidrBlock: "10.0.99.0/24"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicSubnetDelete, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	assertFailure(t, resp, "delete nonexistent subnet")
}

func TestTopologyHandler_CreatePort_SwitchNotFound(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Create port on non-existent switch
	evt := PortEvent{
		NetworkInterfaceId: "eni-orphan",
		SubnetId:           "subnet-missing",
		VpcId:              "vpc-missing",
		PrivateIpAddress:   "10.0.1.4",
		MacAddress:         "02:00:00:00:00:01",
	}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicCreatePort, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	assertFailure(t, resp, "create port on missing switch")
}

func TestTopologyHandler_CreatePort_Idempotent(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Setup: create switch and port directly
	_ = mock.CreateLogicalSwitch(ctx, nbdbLogicalSwitch("subnet-subnet-idem", "subnet-idem", "vpc-idem"))
	_ = mock.CreateLogicalSwitchPort(ctx, "subnet-subnet-idem", &nbdb.LogicalSwitchPort{
		Name:      "port-eni-idem",
		Addresses: []string{"02:00:00:11:22:33 10.0.1.4"},
		ExternalIDs: map[string]string{
			"spinifex:eni_id": "eni-idem",
		},
	})

	// Send create for same port — should succeed (idempotent skip)
	evt := PortEvent{
		NetworkInterfaceId: "eni-idem",
		SubnetId:           "subnet-idem",
		VpcId:              "vpc-idem",
		PrivateIpAddress:   "10.0.1.4",
		MacAddress:         "02:00:00:11:22:33",
	}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicCreatePort, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	assertSuccess(t, resp, "idempotent port create")
}

func TestTopologyHandler_DeletePort_PortNotFound(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Delete port that doesn't exist
	evt := PortEvent{
		NetworkInterfaceId: "eni-ghost",
		SubnetId:           "subnet-ghost",
		VpcId:              "vpc-ghost",
		PrivateIpAddress:   "10.0.1.99",
		MacAddress:         "02:00:00:ff:ff:ff",
	}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicDeletePort, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	assertFailure(t, resp, "delete nonexistent port")
}

func TestTopologyHandler_IGWAttach_RouterNotFound(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Attach IGW without VPC router — CreateLogicalRouterPort fails
	evt := types.IGWEvent{InternetGatewayId: "igw-orphan", VpcId: "vpc-norouter"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicIGWAttach, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	assertFailure(t, resp, "IGW attach without router")

	// Verify external switch was cleaned up
	_, err = mock.GetLogicalSwitch(ctx, "ext-vpc-norouter")
	if err == nil {
		t.Error("expected external switch to be cleaned up after router port failure")
	}
}

func TestTopologyHandler_IGWAttach_Idempotent(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Pre-create external switch to trigger idempotent path
	_ = mock.CreateLogicalSwitch(ctx, &nbdb.LogicalSwitch{
		Name: "ext-vpc-igw-idem",
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": "vpc-igw-idem",
			"spinifex:role":   "external",
		},
	})

	evt := types.IGWEvent{InternetGatewayId: "igw-idem", VpcId: "vpc-igw-idem"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicIGWAttach, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	assertSuccess(t, resp, "idempotent IGW attach")
}

func TestTopologyHandler_IGWDetach_PartialCleanup(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Only create the external switch (not the full IGW topology).
	// All intermediate deletes will warn but switch delete should succeed.
	_ = mock.CreateLogicalSwitch(ctx, &nbdb.LogicalSwitch{
		Name: "ext-vpc-partial",
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": "vpc-partial",
			"spinifex:role":   "external",
		},
	})
	// Create router so NAT cleanup path is exercised (but NAT won't be found)
	_ = mock.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name: "vpc-vpc-partial",
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": "vpc-partial",
			"spinifex:cidr":   "10.0.0.0/16",
		},
	})

	evt := types.IGWEvent{InternetGatewayId: "igw-partial", VpcId: "vpc-partial"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicIGWDetach, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	// Should succeed — intermediate deletes warn but final switch delete succeeds
	assertSuccess(t, resp, "partial IGW detach")

	// Verify external switch was deleted
	_, err = mock.GetLogicalSwitch(ctx, "ext-vpc-partial")
	if err == nil {
		t.Error("expected external switch to be deleted")
	}
}

func TestTopologyHandler_IGWDetach_NoRouter(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Create external switch but no router — exercises the GetLogicalRouter warn path
	_ = mock.CreateLogicalSwitch(ctx, &nbdb.LogicalSwitch{
		Name: "ext-vpc-nortr",
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": "vpc-nortr",
			"spinifex:role":   "external",
		},
	})

	evt := types.IGWEvent{InternetGatewayId: "igw-nortr", VpcId: "vpc-nortr"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicIGWDetach, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	// Should succeed — router not found is a warn path, switch delete succeeds
	assertSuccess(t, resp, "IGW detach without router")

	_, err = mock.GetLogicalSwitch(ctx, "ext-vpc-nortr")
	if err == nil {
		t.Error("expected external switch to be deleted")
	}
}

func TestTopologyHandler_IGWDetach_SwitchNotFound(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// No external switch — final DeleteLogicalSwitch fails
	evt := types.IGWEvent{InternetGatewayId: "igw-nosw", VpcId: "vpc-nosw"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicIGWDetach, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	assertFailure(t, resp, "IGW detach without external switch")
}

func TestTopologyHandler_VPCDeleteCascade_WithPorts(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Create VPC router + subnet switch with ports (simulating pre-existing state)
	_ = mock.CreateLogicalRouter(ctx, nbdbLogicalRouter("vpc-vpc-casp", "vpc-casp"))
	_ = mock.CreateLogicalSwitch(ctx, nbdbLogicalSwitch("subnet-sub-casp", "sub-casp", "vpc-casp"))
	_ = mock.CreateLogicalSwitchPort(ctx, "subnet-sub-casp", &nbdb.LogicalSwitchPort{
		Name:      "port-eni-casp1",
		Addresses: []string{"02:00:00:11:22:33 10.0.1.4"},
		ExternalIDs: map[string]string{
			"spinifex:eni_id":    "eni-casp1",
			"spinifex:subnet_id": "sub-casp",
			"spinifex:vpc_id":    "vpc-casp",
		},
	})
	_ = mock.CreateLogicalSwitchPort(ctx, "subnet-sub-casp", &nbdb.LogicalSwitchPort{
		Name:      "port-eni-casp2",
		Addresses: []string{"02:00:00:44:55:66 10.0.1.5"},
		ExternalIDs: map[string]string{
			"spinifex:eni_id":    "eni-casp2",
			"spinifex:subnet_id": "sub-casp",
			"spinifex:vpc_id":    "vpc-casp",
		},
	})
	_, _ = mock.CreateDHCPOptions(ctx, nbdbDHCPOptions("10.0.1.0/24", "sub-casp", "vpc-casp"))

	// Verify switch has 2 ports before delete
	ls, err := mock.GetLogicalSwitch(ctx, "subnet-sub-casp")
	if err != nil {
		t.Fatalf("expected switch: %v", err)
	}
	if len(ls.Ports) != 2 {
		t.Errorf("expected 2 ports before cascade, got %d", len(ls.Ports))
	}

	// Delete VPC should cascade-delete switches (with ports) and DHCP
	evt := VPCEvent{VpcId: "vpc-casp"}
	data, _ := json.Marshal(evt)
	resp, _ := nc.Request(TopicVPCDelete, data, 5_000_000_000)
	assertSuccess(t, resp, "cascade delete VPC with ports")

	// Verify switch is gone
	_, err = mock.GetLogicalSwitch(ctx, "subnet-sub-casp")
	if err == nil {
		t.Error("expected switch to be deleted after cascade")
	}
	// Verify DHCP options gone
	dhcpList, _ := mock.ListDHCPOptions(ctx)
	if len(dhcpList) != 0 {
		t.Errorf("expected 0 DHCP options after cascade, got %d", len(dhcpList))
	}
	// Verify router gone
	_, err = mock.GetLogicalRouter(ctx, "vpc-vpc-casp")
	if err == nil {
		t.Error("expected router to be deleted after cascade")
	}
}

func TestTopologyHandler_SubnetDelete_NilOVN(t *testing.T) {
	_, nc := startTestNATS(t)

	topo := NewTopologyHandler(nil)
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	evt := SubnetEvent{SubnetId: "subnet-nil", VpcId: "vpc-nil", CidrBlock: "10.0.0.0/24"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicSubnetDelete, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	var result struct {
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}
	_ = json.Unmarshal(resp.Data, &result)
	if result.Success {
		t.Error("expected failure when OVN is nil")
	}
}

// --- Test helpers ---

func assertSuccess(t *testing.T, msg *nats.Msg, label string) {
	t.Helper()
	var result struct {
		Success bool   `json:"success"`
		Error   string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(msg.Data, &result); err != nil {
		t.Fatalf("%s: unmarshal: %v", label, err)
	}
	if !result.Success {
		t.Fatalf("%s: failed: %s", label, result.Error)
	}
}

// nbdb helper factories for tests

func nbdbLogicalRouter(name, vpcId string) *nbdb.LogicalRouter {
	return &nbdb.LogicalRouter{
		Name: name,
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": vpcId,
		},
	}
}

func nbdbLogicalSwitch(name, subnetId, vpcId string) *nbdb.LogicalSwitch {
	return &nbdb.LogicalSwitch{
		Name: name,
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": subnetId,
			"spinifex:vpc_id":    vpcId,
		},
	}
}

func nbdbLogicalRouterPort(name, subnetId, vpcId string) *nbdb.LogicalRouterPort {
	return &nbdb.LogicalRouterPort{
		Name:     name,
		MAC:      "02:00:00:aa:bb:cc",
		Networks: []string{"10.0.2.1/24"},
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": subnetId,
			"spinifex:vpc_id":    vpcId,
		},
	}
}

func nbdbLogicalSwitchPortRouter(name, routerPort, subnetId, vpcId string) *nbdb.LogicalSwitchPort {
	return &nbdb.LogicalSwitchPort{
		Name:      name,
		Type:      "router",
		Addresses: []string{"router"},
		Options: map[string]string{
			"router-port": routerPort,
		},
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": subnetId,
			"spinifex:vpc_id":    vpcId,
		},
	}
}

func nbdbDHCPOptions(cidr, subnetId, vpcId string) *nbdb.DHCPOptions {
	return &nbdb.DHCPOptions{
		CIDR: cidr,
		Options: map[string]string{
			"router":     "10.0.2.1",
			"lease_time": "3600",
		},
		ExternalIDs: map[string]string{
			"spinifex:subnet_id": subnetId,
			"spinifex:vpc_id":    vpcId,
		},
	}
}

// --- Bridge mode tests ---

func TestTopologyHandler_IGWAttach_DirectBridge_NoNatAddresses(t *testing.T) {
	// In direct bridge mode, the localnet port should NOT have nat-addresses=router
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock, WithBridgeMode(BridgeModeDirect))
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Pre-create VPC router
	_ = mock.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name: "vpc-vpc-direct1",
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": "vpc-direct1",
			"spinifex:cidr":   "10.0.0.0/16",
		},
	})

	// Attach IGW
	evt := types.IGWEvent{InternetGatewayId: "igw-d1", VpcId: "vpc-direct1"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicIGWAttach, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.igw-attach: %v", err)
	}
	assertSuccess(t, resp, "attach IGW direct bridge")

	// Verify localnet port does NOT have nat-addresses
	port, err := mock.GetLogicalSwitchPort(ctx, "ext-port-vpc-direct1")
	if err != nil {
		t.Fatalf("expected localnet port: %v", err)
	}
	if _, ok := port.Options["nat-addresses"]; ok {
		t.Errorf("direct bridge mode should NOT have nat-addresses option, got %q", port.Options["nat-addresses"])
	}
}

func TestTopologyHandler_IGWAttach_MacvlanMode_HasNatAddresses(t *testing.T) {
	// In macvlan mode (default), the localnet port SHOULD have nat-addresses=router
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock, WithBridgeMode(BridgeModeMacvlan))
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Pre-create VPC router
	_ = mock.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name: "vpc-vpc-mv1",
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": "vpc-mv1",
			"spinifex:cidr":   "10.0.0.0/16",
		},
	})

	// Attach IGW
	evt := types.IGWEvent{InternetGatewayId: "igw-mv1", VpcId: "vpc-mv1"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicIGWAttach, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.igw-attach: %v", err)
	}
	assertSuccess(t, resp, "attach IGW macvlan")

	// Verify localnet port HAS nat-addresses=router
	port, err := mock.GetLogicalSwitchPort(ctx, "ext-port-vpc-mv1")
	if err != nil {
		t.Fatalf("expected localnet port: %v", err)
	}
	if port.Options["nat-addresses"] != "router" {
		t.Errorf("macvlan mode should have nat-addresses=router, got %q", port.Options["nat-addresses"])
	}
}

func TestTopologyHandler_AddNAT_DirectBridge_DistributedNAT(t *testing.T) {
	// In direct bridge mode, DNAT rules should have ExternalMAC and LogicalPort set
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock, WithBridgeMode(BridgeModeDirect))
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Pre-create VPC router
	_ = mock.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name: "vpc-vpc-dnat1",
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": "vpc-dnat1",
			"spinifex:cidr":   "10.0.0.0/16",
		},
	})

	// Add NAT with MAC and port (simulating a VM with public IP)
	evt := NATEvent{
		VpcId:      "vpc-dnat1",
		ExternalIP: "192.168.1.201",
		LogicalIP:  "10.0.1.5",
		PortName:   "port-eni-1234",
		MAC:        "02:00:00:ab:cd:ef",
	}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicAddNAT, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.add-nat: %v", err)
	}
	assertSuccess(t, resp, "add NAT direct bridge")

	// Verify the NAT rule has ExternalMAC and LogicalPort (distributed NAT)
	router, err := mock.GetLogicalRouter(ctx, "vpc-vpc-dnat1")
	if err != nil {
		t.Fatalf("expected router: %v", err)
	}
	if len(router.NAT) != 1 {
		t.Fatalf("expected 1 NAT rule, got %d", len(router.NAT))
	}
	nat := mock.nats[router.NAT[0]]
	if nat.ExternalMAC == nil || *nat.ExternalMAC != "02:00:00:ab:cd:ef" {
		t.Errorf("expected ExternalMAC=02:00:00:ab:cd:ef for distributed NAT, got %v", nat.ExternalMAC)
	}
	if nat.LogicalPort == nil || *nat.LogicalPort != "port-eni-1234" {
		t.Errorf("expected LogicalPort=port-eni-1234 for distributed NAT, got %v", nat.LogicalPort)
	}
}

func TestTopologyHandler_AddNAT_MacvlanMode_CentralizedNAT(t *testing.T) {
	// In macvlan mode, DNAT rules should NOT have ExternalMAC/LogicalPort
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock, WithBridgeMode(BridgeModeMacvlan))
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Pre-create VPC router
	_ = mock.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name: "vpc-vpc-cnat1",
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": "vpc-cnat1",
			"spinifex:cidr":   "10.0.0.0/16",
		},
	})

	// Add NAT with MAC and port — macvlan mode should ignore them
	evt := NATEvent{
		VpcId:      "vpc-cnat1",
		ExternalIP: "192.168.1.201",
		LogicalIP:  "10.0.1.5",
		PortName:   "port-eni-5678",
		MAC:        "02:00:00:11:22:33",
	}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicAddNAT, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.add-nat: %v", err)
	}
	assertSuccess(t, resp, "add NAT macvlan")

	// Verify the NAT rule does NOT have ExternalMAC/LogicalPort (centralized NAT)
	router, err := mock.GetLogicalRouter(ctx, "vpc-vpc-cnat1")
	if err != nil {
		t.Fatalf("expected router: %v", err)
	}
	if len(router.NAT) != 1 {
		t.Fatalf("expected 1 NAT rule, got %d", len(router.NAT))
	}
	nat := mock.nats[router.NAT[0]]
	if nat.ExternalMAC != nil {
		t.Errorf("macvlan mode should NOT have ExternalMAC, got %v", *nat.ExternalMAC)
	}
	if nat.LogicalPort != nil {
		t.Errorf("macvlan mode should NOT have LogicalPort, got %v", *nat.LogicalPort)
	}
}

func TestTopologyHandler_IGWAttach_VethMode_HasNatAddresses(t *testing.T) {
	// In veth mode, the localnet port SHOULD have nat-addresses=router (centralized NAT)
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock, WithBridgeMode(BridgeModeVeth))
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	_ = mock.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name: "vpc-vpc-veth1",
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": "vpc-veth1",
			"spinifex:cidr":   "10.0.0.0/16",
		},
	})

	evt := types.IGWEvent{InternetGatewayId: "igw-veth1", VpcId: "vpc-veth1"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicIGWAttach, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.igw-attach: %v", err)
	}
	assertSuccess(t, resp, "attach IGW veth")

	port, err := mock.GetLogicalSwitchPort(ctx, "ext-port-vpc-veth1")
	if err != nil {
		t.Fatalf("expected localnet port: %v", err)
	}
	if port.Options["nat-addresses"] != "router" {
		t.Errorf("veth mode should have nat-addresses=router, got %q", port.Options["nat-addresses"])
	}
}

func TestTopologyHandler_AddNAT_VethMode_CentralizedNAT(t *testing.T) {
	// In veth mode, DNAT rules should NOT have ExternalMAC/LogicalPort (centralized NAT)
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock, WithBridgeMode(BridgeModeVeth))
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	_ = mock.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name: "vpc-vpc-vnat1",
		ExternalIDs: map[string]string{
			"spinifex:vpc_id": "vpc-vnat1",
			"spinifex:cidr":   "10.0.0.0/16",
		},
	})

	evt := NATEvent{
		VpcId:      "vpc-vnat1",
		ExternalIP: "192.168.1.201",
		LogicalIP:  "10.0.1.5",
		PortName:   "port-eni-veth1",
		MAC:        "02:00:00:aa:bb:cc",
	}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicAddNAT, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.add-nat: %v", err)
	}
	assertSuccess(t, resp, "add NAT veth")

	router, err := mock.GetLogicalRouter(ctx, "vpc-vpc-vnat1")
	if err != nil {
		t.Fatalf("expected router: %v", err)
	}
	if len(router.NAT) != 1 {
		t.Fatalf("expected 1 NAT rule, got %d", len(router.NAT))
	}
	nat := mock.nats[router.NAT[0]]
	if nat.ExternalMAC != nil {
		t.Errorf("veth mode should NOT have ExternalMAC, got %v", *nat.ExternalMAC)
	}
	if nat.LogicalPort != nil {
		t.Errorf("veth mode should NOT have LogicalPort, got %v", *nat.LogicalPort)
	}
}

func TestTopologyHandler_DefaultBridgeMode_IsMacvlan(t *testing.T) {
	// When no bridge mode is set, it should default to macvlan behavior
	topo := NewTopologyHandler(nil)
	if !topo.isMacvlanMode() {
		t.Error("expected default bridge mode to be macvlan-compatible")
	}

	topoDirect := NewTopologyHandler(nil, WithBridgeMode(BridgeModeDirect))
	if topoDirect.isMacvlanMode() {
		t.Error("expected direct bridge mode to NOT be macvlan")
	}
}

func TestTopologyHandler_AddNAT_CleansStaleRulesFromOtherVPCs(t *testing.T) {
	// When a public IP is reused across VPCs (e.g. instance terminated in the
	// default VPC, IP returned to pool, then LB allocates it in a new VPC),
	// the fire-and-forget vpc.delete-nat for the old instance may not have been
	// processed. handleAddNAT must clean up stale NAT rules from ALL routers,
	// not just the target VPC's router.
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock, WithBridgeMode(BridgeModeVeth))
	subs, err := topo.Subscribe(nc)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Create two VPC routers (simulating default VPC and a new test VPC).
	_ = mock.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name:        "vpc-vpc-default",
		ExternalIDs: map[string]string{"spinifex:vpc_id": "vpc-default", "spinifex:cidr": "10.0.0.0/16"},
	})
	_ = mock.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name:        "vpc-vpc-new",
		ExternalIDs: map[string]string{"spinifex:vpc_id": "vpc-new", "spinifex:cidr": "10.200.0.0/16"},
	})

	// Simulate: instance in default VPC got public IP 192.168.1.243.
	// Add NAT rule to default VPC's router (as if vpc.add-nat was processed).
	oldEvt := NATEvent{
		VpcId:      "vpc-default",
		ExternalIP: "192.168.1.243",
		LogicalIP:  "10.0.1.5",
		PortName:   "port-eni-old",
		MAC:        "02:00:00:aa:aa:aa",
	}
	data, _ := json.Marshal(oldEvt)
	resp, err := nc.Request(TopicAddNAT, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.add-nat (old): %v", err)
	}
	assertSuccess(t, resp, "add old NAT")

	// Verify the stale NAT exists on the default VPC's router.
	defaultRouter, _ := mock.GetLogicalRouter(ctx, "vpc-vpc-default")
	if len(defaultRouter.NAT) != 1 {
		t.Fatalf("expected 1 NAT on default router, got %d", len(defaultRouter.NAT))
	}

	// Simulate: instance terminated, IP returned to pool, and a new LB in
	// the new VPC re-allocates the same IP. vpc.delete-nat was NOT processed
	// (fire-and-forget race). Now vpc.add-nat fires for the new VPC.
	newEvt := NATEvent{
		VpcId:      "vpc-new",
		ExternalIP: "192.168.1.243",
		LogicalIP:  "10.200.1.10",
		PortName:   "port-eni-new",
		MAC:        "02:00:00:bb:bb:bb",
	}
	data, _ = json.Marshal(newEvt)
	resp, err = nc.Request(TopicAddNAT, data, 5_000_000_000)
	if err != nil {
		t.Fatalf("request vpc.add-nat (new): %v", err)
	}
	assertSuccess(t, resp, "add new NAT (should clean stale)")

	// The stale NAT rule on the default VPC's router must be gone.
	defaultRouter, _ = mock.GetLogicalRouter(ctx, "vpc-vpc-default")
	if len(defaultRouter.NAT) != 0 {
		t.Errorf("stale NAT rule was NOT cleaned from default VPC's router; got %d NAT rules", len(defaultRouter.NAT))
	}

	// The new VPC's router must have exactly 1 NAT rule with the new logical IP.
	newRouter, _ := mock.GetLogicalRouter(ctx, "vpc-vpc-new")
	if len(newRouter.NAT) != 1 {
		t.Fatalf("expected 1 NAT on new router, got %d", len(newRouter.NAT))
	}
	newNAT := mock.nats[newRouter.NAT[0]]
	if newNAT.LogicalIP != "10.200.1.10" {
		t.Errorf("new NAT logical IP = %q, want 10.200.1.10", newNAT.LogicalIP)
	}
}

// ensureLocalnetOptions retrofits options:nat-addresses on a localnet port
// whose mode no longer matches (e.g. created during a pre-Fix 1 run where
// detectBridgeMode returned direct because the veth had not been
// recreated after reboot). mulga-998.b Fix 3.

func TestEnsureLocalnetOptions_AddsNatAddressesInCentralizedMode(t *testing.T) {
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	_ = mock.CreateLogicalSwitch(ctx, &nbdb.LogicalSwitch{Name: "ext-vpc-x"})
	_ = mock.CreateLogicalSwitchPort(ctx, "ext-vpc-x", &nbdb.LogicalSwitchPort{
		Name:      "ext-port-vpc-x",
		Type:      "localnet",
		Addresses: []string{"unknown"},
		Options:   map[string]string{"network_name": "external"},
	})

	topo := NewTopologyHandler(mock, WithBridgeMode(BridgeModeVeth))
	if err := topo.ensureLocalnetOptions(ctx, "ext-port-vpc-x"); err != nil {
		t.Fatalf("ensureLocalnetOptions: %v", err)
	}
	port, _ := mock.GetLogicalSwitchPort(ctx, "ext-port-vpc-x")
	if port.Options["nat-addresses"] != "router" {
		t.Errorf("expected nat-addresses=router after retrofit, got %q", port.Options["nat-addresses"])
	}
	if port.Options["network_name"] != "external" {
		t.Errorf("network_name clobbered: got %q", port.Options["network_name"])
	}
}

func TestEnsureLocalnetOptions_RemovesNatAddressesInDirectMode(t *testing.T) {
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	_ = mock.CreateLogicalSwitch(ctx, &nbdb.LogicalSwitch{Name: "ext-vpc-y"})
	_ = mock.CreateLogicalSwitchPort(ctx, "ext-vpc-y", &nbdb.LogicalSwitchPort{
		Name:      "ext-port-vpc-y",
		Type:      "localnet",
		Addresses: []string{"unknown"},
		Options:   map[string]string{"network_name": "external", "nat-addresses": "router"},
	})

	topo := NewTopologyHandler(mock, WithBridgeMode(BridgeModeDirect))
	if err := topo.ensureLocalnetOptions(ctx, "ext-port-vpc-y"); err != nil {
		t.Fatalf("ensureLocalnetOptions: %v", err)
	}
	port, _ := mock.GetLogicalSwitchPort(ctx, "ext-port-vpc-y")
	if _, ok := port.Options["nat-addresses"]; ok {
		t.Errorf("nat-addresses should be removed in direct mode, got %q", port.Options["nat-addresses"])
	}
	if port.Options["network_name"] != "external" {
		t.Errorf("network_name clobbered: got %q", port.Options["network_name"])
	}
}

func TestEnsureLocalnetOptions_NoOpWhenAlreadyCorrect(t *testing.T) {
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	_ = mock.CreateLogicalSwitch(ctx, &nbdb.LogicalSwitch{Name: "ext-vpc-z"})
	_ = mock.CreateLogicalSwitchPort(ctx, "ext-vpc-z", &nbdb.LogicalSwitchPort{
		Name:      "ext-port-vpc-z",
		Type:      "localnet",
		Addresses: []string{"unknown"},
		Options:   map[string]string{"network_name": "external", "nat-addresses": "router"},
	})
	startUpdates := mock.UpdateLogicalSwitchPortCalls

	topo := NewTopologyHandler(mock, WithBridgeMode(BridgeModeVeth))
	if err := topo.ensureLocalnetOptions(ctx, "ext-port-vpc-z"); err != nil {
		t.Fatalf("ensureLocalnetOptions: %v", err)
	}
	if mock.UpdateLogicalSwitchPortCalls != startUpdates {
		t.Errorf("expected no UpdateLogicalSwitchPort call when already correct (idempotent); got %d new calls",
			mock.UpdateLogicalSwitchPortCalls-startUpdates)
	}
}

func TestEnsureLocalnetOptions_NoOpInDirectModeWhenAbsent(t *testing.T) {
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	_ = mock.CreateLogicalSwitch(ctx, &nbdb.LogicalSwitch{Name: "ext-vpc-d"})
	_ = mock.CreateLogicalSwitchPort(ctx, "ext-vpc-d", &nbdb.LogicalSwitchPort{
		Name:      "ext-port-vpc-d",
		Type:      "localnet",
		Addresses: []string{"unknown"},
		Options:   map[string]string{"network_name": "external"},
	})
	start := mock.UpdateLogicalSwitchPortCalls

	topo := NewTopologyHandler(mock, WithBridgeMode(BridgeModeDirect))
	if err := topo.ensureLocalnetOptions(ctx, "ext-port-vpc-d"); err != nil {
		t.Fatalf("ensureLocalnetOptions: %v", err)
	}
	if mock.UpdateLogicalSwitchPortCalls != start {
		t.Errorf("direct mode + no nat-addresses should be a no-op; got %d new calls",
			mock.UpdateLogicalSwitchPortCalls-start)
	}
}

func TestEnsureLocalnetOptions_HandlesNilOptionsMap(t *testing.T) {
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	_ = mock.CreateLogicalSwitch(ctx, &nbdb.LogicalSwitch{Name: "ext-vpc-n"})
	_ = mock.CreateLogicalSwitchPort(ctx, "ext-vpc-n", &nbdb.LogicalSwitchPort{
		Name:      "ext-port-vpc-n",
		Type:      "localnet",
		Addresses: []string{"unknown"},
	})

	topo := NewTopologyHandler(mock, WithBridgeMode(BridgeModeVeth))
	if err := topo.ensureLocalnetOptions(ctx, "ext-port-vpc-n"); err != nil {
		t.Fatalf("ensureLocalnetOptions: %v", err)
	}
	port, _ := mock.GetLogicalSwitchPort(ctx, "ext-port-vpc-n")
	if port.Options["nat-addresses"] != "router" {
		t.Errorf("expected nat-addresses=router after nil-map init, got %q", port.Options["nat-addresses"])
	}
}

func TestEnsureLocalnetOptions_RestoresNetworkName(t *testing.T) {
	// Operator (or test drill) cleared options entirely — retrofit must
	// restore both network_name=external and nat-addresses=router.
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	_ = mock.CreateLogicalSwitch(ctx, &nbdb.LogicalSwitch{Name: "ext-vpc-r"})
	_ = mock.CreateLogicalSwitchPort(ctx, "ext-vpc-r", &nbdb.LogicalSwitchPort{
		Name:      "ext-port-vpc-r",
		Type:      "localnet",
		Addresses: []string{"unknown"},
		Options:   map[string]string{},
	})

	topo := NewTopologyHandler(mock, WithBridgeMode(BridgeModeVeth))
	if err := topo.ensureLocalnetOptions(ctx, "ext-port-vpc-r"); err != nil {
		t.Fatalf("ensureLocalnetOptions: %v", err)
	}
	port, _ := mock.GetLogicalSwitchPort(ctx, "ext-port-vpc-r")
	if port.Options["network_name"] != "external" {
		t.Errorf("expected network_name=external after retrofit, got %q", port.Options["network_name"])
	}
	if port.Options["nat-addresses"] != "router" {
		t.Errorf("expected nat-addresses=router after retrofit, got %q", port.Options["nat-addresses"])
	}
}

func TestEnsureLocalnetOptions_RestoresNetworkNameDirectMode(t *testing.T) {
	// Direct mode: network_name still required, nat-addresses still absent.
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	_ = mock.CreateLogicalSwitch(ctx, &nbdb.LogicalSwitch{Name: "ext-vpc-rd"})
	_ = mock.CreateLogicalSwitchPort(ctx, "ext-vpc-rd", &nbdb.LogicalSwitchPort{
		Name:      "ext-port-vpc-rd",
		Type:      "localnet",
		Addresses: []string{"unknown"},
		Options:   map[string]string{},
	})

	topo := NewTopologyHandler(mock, WithBridgeMode(BridgeModeDirect))
	if err := topo.ensureLocalnetOptions(ctx, "ext-port-vpc-rd"); err != nil {
		t.Fatalf("ensureLocalnetOptions: %v", err)
	}
	port, _ := mock.GetLogicalSwitchPort(ctx, "ext-port-vpc-rd")
	if port.Options["network_name"] != "external" {
		t.Errorf("expected network_name=external after retrofit, got %q", port.Options["network_name"])
	}
	if _, ok := port.Options["nat-addresses"]; ok {
		t.Errorf("direct mode should not set nat-addresses, got %q", port.Options["nat-addresses"])
	}
}

func TestRetrofitAllExternalLocalnetOptions_FixesClearedOptions(t *testing.T) {
	// Walks every ext-* switch in OVN (authoritative) and retrofits options
	// on its localnet port. Covers VPCs whose IGW KV record is missing or
	// whose options were cleared manually (mulga-998.b Drill 2).
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	// Two external switches with cleared options, one subnet switch (must be
	// skipped because role != external), one external switch already correct.
	for _, vpcID := range []string{"vpc-a", "vpc-b"} {
		_ = mock.CreateLogicalSwitch(ctx, &nbdb.LogicalSwitch{
			Name:        "ext-" + vpcID,
			ExternalIDs: map[string]string{"spinifex:role": "external", "spinifex:vpc_id": vpcID},
		})
		_ = mock.CreateLogicalSwitchPort(ctx, "ext-"+vpcID, &nbdb.LogicalSwitchPort{
			Name:      "ext-port-" + vpcID,
			Type:      "localnet",
			Addresses: []string{"unknown"},
			Options:   map[string]string{},
		})
	}
	// A subnet switch that must NOT be retrofitted.
	_ = mock.CreateLogicalSwitch(ctx, &nbdb.LogicalSwitch{
		Name:        "subnet-foo",
		ExternalIDs: map[string]string{"spinifex:subnet_id": "subnet-foo"},
	})
	// An ext switch already correct — must remain a no-op.
	_ = mock.CreateLogicalSwitch(ctx, &nbdb.LogicalSwitch{
		Name:        "ext-vpc-ok",
		ExternalIDs: map[string]string{"spinifex:role": "external", "spinifex:vpc_id": "vpc-ok"},
	})
	_ = mock.CreateLogicalSwitchPort(ctx, "ext-vpc-ok", &nbdb.LogicalSwitchPort{
		Name:      "ext-port-vpc-ok",
		Type:      "localnet",
		Addresses: []string{"unknown"},
		Options:   map[string]string{"network_name": "external", "nat-addresses": "router"},
	})
	startUpdates := mock.UpdateLogicalSwitchPortCalls

	topo := NewTopologyHandler(mock, WithBridgeMode(BridgeModeVeth))
	topo.RetrofitAllExternalLocalnetOptions(ctx)

	for _, vpcID := range []string{"vpc-a", "vpc-b"} {
		port, err := mock.GetLogicalSwitchPort(ctx, "ext-port-"+vpcID)
		if err != nil {
			t.Fatalf("get port: %v", err)
		}
		if port.Options["network_name"] != "external" {
			t.Errorf("%s: expected network_name=external, got %q", vpcID, port.Options["network_name"])
		}
		if port.Options["nat-addresses"] != "router" {
			t.Errorf("%s: expected nat-addresses=router, got %q", vpcID, port.Options["nat-addresses"])
		}
	}
	// Expect exactly 2 updates (vpc-a + vpc-b); vpc-ok must be idempotent no-op.
	if got := mock.UpdateLogicalSwitchPortCalls - startUpdates; got != 2 {
		t.Errorf("expected 2 UpdateLogicalSwitchPort calls (one per stale ext port); got %d", got)
	}
}

func TestEnsureLocalnetOptions_ErrorOnMissingPort(t *testing.T) {
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock, WithBridgeMode(BridgeModeVeth))
	err := topo.ensureLocalnetOptions(ctx, "ext-port-missing")
	if err == nil {
		t.Fatal("expected error when port missing")
	}
	if !strings.Contains(err.Error(), "ext-port-missing") {
		t.Errorf("error should name the port; got: %v", err)
	}
}

// reconcileIGW must surface the CreateLogicalSwitchPort error and roll back
// the external switch when the localnet port name collides (e.g. a stale
// pre-existing port from a prior attach that leaked).
func TestTopologyHandler_ReconcileIGW_LocalnetPortCreateError(t *testing.T) {
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	ctx := context.Background()

	topo := NewTopologyHandler(mock, WithBridgeMode(BridgeModeVeth))

	// Pre-create VPC router.
	_ = mock.CreateLogicalRouter(ctx, &nbdb.LogicalRouter{
		Name:        "vpc-vpc-clash",
		ExternalIDs: map[string]string{"spinifex:vpc_id": "vpc-clash", "spinifex:cidr": "10.0.0.0/16"},
	})

	// Pre-seed a logical switch port named "ext-port-vpc-clash" on an
	// unrelated switch so reconcileIGW's CreateLogicalSwitchPort call
	// collides with the existing port.
	_ = mock.CreateLogicalSwitch(ctx, &nbdb.LogicalSwitch{Name: "stale-switch"})
	_ = mock.CreateLogicalSwitchPort(ctx, "stale-switch", &nbdb.LogicalSwitchPort{
		Name: "ext-port-vpc-clash",
		Type: "localnet",
	})

	err := topo.reconcileIGW(ctx, "vpc-clash", "igw-clash")
	if err == nil {
		t.Fatal("expected error from reconcileIGW when localnet port collides")
	}
	if !strings.Contains(err.Error(), "create localnet port") {
		t.Errorf("expected 'create localnet port' in error; got: %v", err)
	}

	// The external switch must have been rolled back.
	if _, err := mock.GetLogicalSwitch(ctx, "ext-vpc-clash"); err == nil {
		t.Error("expected external switch to be rolled back after port-create failure")
	}
}
