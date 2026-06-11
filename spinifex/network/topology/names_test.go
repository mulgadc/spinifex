package topology

import "testing"

func TestNameHelpers(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"VPCRouter", VPCRouter("vpc-abc"), "vpc-vpc-abc"},
		{"SubnetSwitch", SubnetSwitch("subnet-1"), "subnet-subnet-1"},
		{"SubnetRouterPort", SubnetRouterPort("subnet-1"), "rtr-subnet-1"},
		{"SubnetSwitchRouterPort", SubnetSwitchRouterPort("subnet-1"), "rtr-port-subnet-1"},
		{"Port", Port("eni-1"), "port-eni-1"},
		{"GatewayRouterPort", GatewayRouterPort("vpc-abc"), "gw-vpc-abc"},
		{"GatewaySwitchPort", GatewaySwitchPort("vpc-abc"), "gw-port-vpc-abc"},
		{"GatewayChassisRedirectPort", GatewayChassisRedirectPort("vpc-abc"), "cr-gw-vpc-abc"},
		{"ExternalSwitch", ExternalSwitch("vpc-abc"), "ext-vpc-abc"},
		{"ExternalLocalnetPort", ExternalLocalnetPort("vpc-abc"), "ext-port-vpc-abc"},
		{"SecurityGroupPortGroup", SecurityGroupPortGroup("sg-abc-123"), "sg_abc_123"},
		{"TransitSwitch", TransitSwitch("az-1", "vpc-abc"), "ts-az-1-vpc-abc"},
		{"TransitRouterPort", TransitRouterPort("az-1", "vpc-abc"), "trp-az-1-vpc-abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}
