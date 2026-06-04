package subscribers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/netip"

	"github.com/mulgadc/spinifex/spinifex/network/external"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/nats-io/nats.go"
)

func (s *Subscriber) handleVPCCreate(msg *nats.Msg) {
	var evt VPCEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.create event", "err", err)
		respond(msg, err)
		return
	}
	spec := topology.VPCSpec{VPCID: evt.VpcId, VNI: evt.VNI}
	if evt.CidrBlock != "" {
		cidr, err := netip.ParsePrefix(evt.CidrBlock)
		if err != nil {
			slog.Error("subscribers: invalid CIDR in vpc.create event", "cidr", evt.CidrBlock, "err", err)
			respond(msg, err)
			return
		}
		spec.CIDR = cidr
	}
	ctx := context.Background()
	if err := s.topology.EnsureVPC(ctx, spec); err != nil {
		slog.Error("subscribers: EnsureVPC failed", "vpc_id", evt.VpcId, "err", err)
		respond(msg, err)
		return
	}
	respond(msg, nil)
}

func (s *Subscriber) handleVPCDelete(msg *nats.Msg) {
	var evt VPCEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.delete event", "err", err)
		respond(msg, err)
		return
	}
	ctx := context.Background()
	if err := s.topology.DeleteVPC(ctx, evt.VpcId); err != nil {
		slog.Error("subscribers: DeleteVPC failed", "vpc_id", evt.VpcId, "err", err)
		respond(msg, err)
		return
	}
	respond(msg, nil)
}

func (s *Subscriber) handleSubnetCreate(msg *nats.Msg) {
	var evt SubnetEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.create-subnet event", "err", err)
		respond(msg, err)
		return
	}
	cidr, err := netip.ParsePrefix(evt.CidrBlock)
	if err != nil {
		slog.Error("subscribers: invalid CIDR in vpc.create-subnet event", "cidr", evt.CidrBlock, "err", err)
		respond(msg, err)
		return
	}
	ctx := context.Background()
	// vpc.create and vpc.create-subnet have no ordering guarantee; pre-ensure
	// the VPC router so a tight bootstrap doesn't fail with "router not
	// found". EnsureVPC is idempotent.
	if err := s.topology.EnsureVPC(ctx, topology.VPCSpec{VPCID: evt.VpcId}); err != nil {
		slog.Error("subscribers: EnsureVPC (subnet pre-create) failed", "vpc_id", evt.VpcId, "err", err)
		respond(msg, err)
		return
	}
	if err := s.topology.EnsureSubnet(ctx, topology.SubnetSpec{
		SubnetID: evt.SubnetId,
		VPCID:    evt.VpcId,
		CIDR:     cidr,
	}); err != nil {
		slog.Error("subscribers: EnsureSubnet failed", "subnet_id", evt.SubnetId, "err", err)
		respond(msg, err)
		return
	}
	// The IMDS localport lives on every subnet switch (guests reach metadata over
	// one L2 hop). Install is best-effort — the reconciler re-ensures it
	// idempotently, so a transient OVN failure here must not fail CreateSubnet.
	if s.imds != nil {
		if _, err := s.imds.EnsureForSubnet(ctx, evt.SubnetId, cidr); err != nil {
			slog.Warn("subscribers: IMDS EnsureForSubnet failed; reconciler will converge", "subnet_id", evt.SubnetId, "err", err)
		}
	}
	respond(msg, nil)
}

func (s *Subscriber) handleSubnetDelete(msg *nats.Msg) {
	var evt SubnetEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.delete-subnet event", "err", err)
		respond(msg, err)
		return
	}
	spec := topology.SubnetSpec{SubnetID: evt.SubnetId, VPCID: evt.VpcId}
	if evt.CidrBlock != "" {
		if cidr, perr := netip.ParsePrefix(evt.CidrBlock); perr == nil {
			spec.CIDR = cidr
		}
	}
	ctx := context.Background()
	// Remove the IMDS localport before the subnet switch goes away — the
	// localport lives on subnet-{subnetID}.
	if s.imds != nil {
		if err := s.imds.RemoveForSubnet(ctx, evt.SubnetId); err != nil {
			slog.Error("subscribers: IMDS RemoveForSubnet failed", "subnet_id", evt.SubnetId, "err", err)
			respond(msg, err)
			return
		}
	}
	if err := s.topology.DeleteSubnet(ctx, spec); err != nil {
		slog.Error("subscribers: DeleteSubnet failed", "subnet_id", evt.SubnetId, "err", err)
		respond(msg, err)
		return
	}
	respond(msg, nil)
}

func (s *Subscriber) handleCreatePort(msg *nats.Msg) {
	var evt PortEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.create-port event", "err", err)
		respond(msg, err)
		return
	}
	ip, err := netip.ParseAddr(evt.PrivateIpAddress)
	if err != nil {
		slog.Error("subscribers: invalid private IP in vpc.create-port event", "ip", evt.PrivateIpAddress, "err", err)
		respond(msg, err)
		return
	}
	mac, err := net.ParseMAC(evt.MacAddress)
	if err != nil {
		slog.Error("subscribers: invalid MAC in vpc.create-port event", "mac", evt.MacAddress, "err", err)
		respond(msg, err)
		return
	}
	if err := s.topology.EnsurePort(context.Background(), topology.PortSpec{
		PortID:    evt.NetworkInterfaceId,
		SubnetID:  evt.SubnetId,
		VPCID:     evt.VpcId,
		PrivateIP: ip,
		MAC:       mac,
		SGIDs:     evt.SecurityGroupIds,
	}); err != nil {
		slog.Error("subscribers: EnsurePort failed", "eni_id", evt.NetworkInterfaceId, "err", err)
		respond(msg, err)
		return
	}
	respond(msg, nil)
}

func (s *Subscriber) handleDeletePort(msg *nats.Msg) {
	var evt PortEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.delete-port event", "err", err)
		respond(msg, err)
		return
	}
	if err := s.topology.DeletePort(context.Background(), topology.PortSpec{
		PortID:   evt.NetworkInterfaceId,
		SubnetID: evt.SubnetId,
		VPCID:    evt.VpcId,
	}); err != nil {
		slog.Error("subscribers: DeletePort failed", "eni_id", evt.NetworkInterfaceId, "err", err)
		respond(msg, err)
		return
	}
	respond(msg, nil)
}

// handleUpdatePortSGs reconciles the LSP's PG memberships against the
// declarative SG list; the manager computes the diff.
func (s *Subscriber) handleUpdatePortSGs(msg *nats.Msg) {
	var evt UpdatePortSGsEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.update-port-sgs event", "err", err)
		respond(msg, err)
		return
	}
	if err := s.topology.SetPortSecurityGroups(context.Background(), evt.NetworkInterfaceId, evt.SecurityGroupIds); err != nil {
		slog.Error("subscribers: SetPortSecurityGroups failed",
			"eni_id", evt.NetworkInterfaceId, "sgs", evt.SecurityGroupIds, "err", err)
		respond(msg, err)
		return
	}
	respond(msg, nil)
}

func (s *Subscriber) handleIGWAttach(msg *nats.Msg) {
	var evt types.IGWEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.igw-attach event", "err", err)
		respond(msg, err)
		return
	}
	if err := s.igw.AttachIGW(context.Background(), external.IGWSpec{
		VPCID:             evt.VpcId,
		InternetGatewayID: evt.InternetGatewayId,
	}); err != nil {
		slog.Error("subscribers: AttachIGW failed",
			"vpc_id", evt.VpcId, "igw_id", evt.InternetGatewayId, "err", err)
		respond(msg, err)
		return
	}
	respond(msg, nil)
}

func (s *Subscriber) handleIGWDetach(msg *nats.Msg) {
	var evt types.IGWEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.igw-detach event", "err", err)
		respond(msg, err)
		return
	}
	if err := s.igw.DetachIGW(context.Background(), evt.VpcId); err != nil {
		slog.Error("subscribers: DetachIGW failed", "vpc_id", evt.VpcId, "err", err)
		respond(msg, err)
		return
	}
	slog.Info("subscribers: detached internet gateway from VPC",
		"igw_id", evt.InternetGatewayId, "vpc_id", evt.VpcId)
	respond(msg, nil)
}

func (s *Subscriber) handleAddNAT(msg *nats.Msg) {
	var evt NATEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.add-nat event", "err", err)
		respond(msg, err)
		return
	}
	if err := s.eip.AttachEIP(context.Background(), policy.EIPSpec{
		VPCID:      evt.VpcId,
		ExternalIP: evt.ExternalIP,
		LogicalIP:  evt.LogicalIP,
		PortName:   evt.PortName,
		MAC:        evt.MAC,
	}); err != nil {
		slog.Error("subscribers: AddEIP failed",
			"vpc_id", evt.VpcId, "external_ip", evt.ExternalIP,
			"logical_ip", evt.LogicalIP, "err", err)
		respond(msg, err)
		return
	}
	slog.Info("subscribers: added dnat_and_snat rule",
		"vpc_id", evt.VpcId, "external_ip", evt.ExternalIP,
		"logical_ip", evt.LogicalIP, "port", evt.PortName)
	respond(msg, nil)
}

func (s *Subscriber) handleDeleteNAT(msg *nats.Msg) {
	var evt NATEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.delete-nat event", "err", err)
		respond(msg, err)
		return
	}
	if err := s.eip.DetachEIP(context.Background(), evt.VpcId, evt.LogicalIP); err != nil {
		slog.Error("subscribers: DeleteEIP failed",
			"vpc_id", evt.VpcId, "logical_ip", evt.LogicalIP, "err", err)
		respond(msg, err)
		return
	}
	slog.Info("subscribers: deleted dnat_and_snat rule",
		"vpc_id", evt.VpcId, "logical_ip", evt.LogicalIP)
	respond(msg, nil)
}

func (s *Subscriber) handleAddNATGateway(msg *nats.Msg) {
	var evt NATGatewayEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.add-nat-gateway event", "err", err)
		return
	}
	ctx := context.Background()
	if err := s.natgw.AttachNATGateway(ctx, policy.NATGWSpec{
		VPCID:        evt.VpcId,
		NATGatewayID: evt.NatGatewayId,
		PublicIP:     evt.PublicIp,
		SubnetCIDR:   evt.SubnetCidr,
	}); err != nil {
		slog.Error("subscribers: AddNATGateway failed",
			"vpc_id", evt.VpcId, "natgw_id", evt.NatGatewayId,
			"public_ip", evt.PublicIp, "subnet_cidr", evt.SubnetCidr, "err", err)
		return
	}
	slog.Info("subscribers: NAT Gateway SNAT rule added",
		"vpc_id", evt.VpcId, "natgw_id", evt.NatGatewayId,
		"public_ip", evt.PublicIp, "subnet_cidr", evt.SubnetCidr)

	// SNAT rewrites src IP but the LR still needs a route to egress. Install
	// a per-subnet LR policy at NATGW priority so private-subnet packets are
	// rerouted out the IGW gateway port (NATGW SNAT happens on the same LR
	// before egress).
	if evt.SubnetId == "" {
		return
	}
	destCidr := evt.DestinationCidr
	if destCidr == "" {
		destCidr = "0.0.0.0/0"
	}
	prefix, err := netip.ParsePrefix(destCidr)
	if err != nil {
		slog.Error("subscribers: invalid destination CIDR in vpc.add-nat-gateway event",
			"cidr", destCidr, "err", err)
		return
	}
	if err := s.igw.EnsureNATGatewaySubnetEgress(ctx, evt.VpcId, evt.SubnetId, prefix); err != nil {
		slog.Error("subscribers: EnsureNATGatewaySubnetEgress failed",
			"vpc_id", evt.VpcId, "subnet_id", evt.SubnetId, "cidr", destCidr,
			"natgw_id", evt.NatGatewayId, "err", err)
		return
	}
	slog.Info("subscribers: installed NATGW route policy",
		"vpc_id", evt.VpcId, "subnet_id", evt.SubnetId, "cidr", destCidr,
		"natgw_id", evt.NatGatewayId)
}

func (s *Subscriber) handleAddSystemEgress(msg *nats.Msg) {
	var evt SystemEgressEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.add-system-egress event", "err", err)
		return
	}
	ctx := context.Background()
	if err := s.igw.EnsureSystemInstanceEgress(ctx, evt.VpcId, evt.SubnetId, evt.InstanceIp, evt.ExternalIp); err != nil {
		slog.Error("subscribers: EnsureSystemInstanceEgress failed",
			"vpc_id", evt.VpcId, "subnet_id", evt.SubnetId,
			"instance_ip", evt.InstanceIp, "external_ip", evt.ExternalIp, "err", err)
		return
	}
	slog.Info("subscribers: system instance egress installed",
		"vpc_id", evt.VpcId, "subnet_id", evt.SubnetId,
		"instance_ip", evt.InstanceIp, "external_ip", evt.ExternalIp)
}

func (s *Subscriber) handleDeleteSystemEgress(msg *nats.Msg) {
	var evt SystemEgressEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.delete-system-egress event", "err", err)
		return
	}
	ctx := context.Background()
	if err := s.igw.RemoveSystemInstanceEgress(ctx, evt.VpcId, evt.SubnetId, evt.InstanceIp, evt.ExternalIp); err != nil {
		slog.Error("subscribers: RemoveSystemInstanceEgress failed",
			"vpc_id", evt.VpcId, "subnet_id", evt.SubnetId,
			"instance_ip", evt.InstanceIp, "external_ip", evt.ExternalIp, "err", err)
		return
	}
	slog.Info("subscribers: system instance egress removed",
		"vpc_id", evt.VpcId, "subnet_id", evt.SubnetId, "instance_ip", evt.InstanceIp)
}

func (s *Subscriber) handleDeleteNATGateway(msg *nats.Msg) {
	var evt NATGatewayEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.delete-nat-gateway event", "err", err)
		return
	}
	ctx := context.Background()
	// Remove the per-subnet policy first so no packets are routed to the LR
	// uplink after the SNAT rule is gone (which would leak un-NAT'd private IPs).
	if evt.SubnetId != "" {
		destCidr := evt.DestinationCidr
		if destCidr == "" {
			destCidr = "0.0.0.0/0"
		}
		if prefix, err := netip.ParsePrefix(destCidr); err != nil {
			slog.Error("subscribers: invalid destination CIDR in vpc.delete-nat-gateway event",
				"cidr", destCidr, "err", err)
		} else if err := s.igw.RemoveNATGatewaySubnetEgress(ctx, evt.VpcId, evt.SubnetId, prefix); err != nil {
			slog.Warn("subscribers: RemoveNATGatewaySubnetEgress failed",
				"vpc_id", evt.VpcId, "subnet_id", evt.SubnetId, "cidr", destCidr, "err", err)
		} else {
			slog.Info("subscribers: removed NATGW route policy",
				"vpc_id", evt.VpcId, "subnet_id", evt.SubnetId, "cidr", destCidr)
		}
	}
	if err := s.natgw.DetachNATGateway(ctx, evt.VpcId, evt.SubnetCidr); err != nil {
		slog.Warn("subscribers: DeleteNATGateway failed",
			"vpc_id", evt.VpcId, "subnet_cidr", evt.SubnetCidr, "err", err)
		return
	}
	slog.Info("subscribers: NAT Gateway SNAT rule removed",
		"vpc_id", evt.VpcId, "natgw_id", evt.NatGatewayId, "subnet_cidr", evt.SubnetCidr)
}

func (s *Subscriber) handleAddIGWRoute(msg *nats.Msg) {
	var evt IGWRouteEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.add-igw-route event", "err", err)
		respond(msg, err)
		return
	}
	prefix, err := netip.ParsePrefix(evt.DestinationCidr)
	if err != nil {
		slog.Error("subscribers: invalid destination CIDR in vpc.add-igw-route event",
			"cidr", evt.DestinationCidr, "err", err)
		respond(msg, err)
		return
	}
	if err := s.igw.EnsureSubnetEgress(context.Background(), evt.VpcId, evt.SubnetId, prefix); err != nil {
		slog.Error("subscribers: EnsureSubnetEgress failed",
			"vpc_id", evt.VpcId, "subnet_id", evt.SubnetId, "cidr", evt.DestinationCidr,
			"igw_id", evt.InternetGatewayId, "err", err)
		respond(msg, err)
		return
	}
	slog.Info("subscribers: installed IGW route policy",
		"vpc_id", evt.VpcId, "subnet_id", evt.SubnetId, "cidr", evt.DestinationCidr,
		"igw_id", evt.InternetGatewayId)
	respond(msg, nil)
}

func (s *Subscriber) handleDeleteIGWRoute(msg *nats.Msg) {
	var evt IGWRouteEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.delete-igw-route event", "err", err)
		respond(msg, err)
		return
	}
	prefix, err := netip.ParsePrefix(evt.DestinationCidr)
	if err != nil {
		slog.Error("subscribers: invalid destination CIDR in vpc.delete-igw-route event",
			"cidr", evt.DestinationCidr, "err", err)
		respond(msg, err)
		return
	}
	if err := s.igw.RemoveSubnetEgress(context.Background(), evt.VpcId, evt.SubnetId, prefix); err != nil {
		slog.Warn("subscribers: RemoveSubnetEgress failed",
			"vpc_id", evt.VpcId, "subnet_id", evt.SubnetId, "cidr", evt.DestinationCidr, "err", err)
		respond(msg, err)
		return
	}
	slog.Info("subscribers: removed IGW route policy",
		"vpc_id", evt.VpcId, "subnet_id", evt.SubnetId, "cidr", evt.DestinationCidr)
	respond(msg, nil)
}

func (s *Subscriber) handleGateSubnetEgress(msg *nats.Msg) {
	var evt SubnetEgressGateEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.gate-subnet-egress event", "err", err)
		respond(msg, err)
		return
	}
	prefix, err := netip.ParsePrefix(evt.DestinationCidr)
	if err != nil {
		slog.Error("subscribers: invalid destination CIDR in vpc.gate-subnet-egress event",
			"cidr", evt.DestinationCidr, "err", err)
		respond(msg, err)
		return
	}
	if err := s.igw.EnsureSubnetEgressDrop(context.Background(), evt.VpcId, evt.SubnetId, prefix); err != nil {
		slog.Error("subscribers: EnsureSubnetEgressDrop failed",
			"vpc_id", evt.VpcId, "subnet_id", evt.SubnetId, "cidr", evt.DestinationCidr, "err", err)
		respond(msg, err)
		return
	}
	slog.Info("subscribers: gated subnet egress",
		"vpc_id", evt.VpcId, "subnet_id", evt.SubnetId, "cidr", evt.DestinationCidr)
	respond(msg, nil)
}

func (s *Subscriber) handleUngateSubnetEgress(msg *nats.Msg) {
	var evt SubnetEgressUngateEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.ungate-subnet-egress event", "err", err)
		respond(msg, err)
		return
	}
	prefix, err := netip.ParsePrefix(evt.DestinationCidr)
	if err != nil {
		slog.Error("subscribers: invalid destination CIDR in vpc.ungate-subnet-egress event",
			"cidr", evt.DestinationCidr, "err", err)
		respond(msg, err)
		return
	}
	if err := s.igw.RemoveSubnetEgressDrop(context.Background(), evt.VpcId, evt.SubnetId, prefix); err != nil {
		slog.Warn("subscribers: RemoveSubnetEgressDrop failed",
			"vpc_id", evt.VpcId, "subnet_id", evt.SubnetId, "cidr", evt.DestinationCidr, "err", err)
		respond(msg, err)
		return
	}
	slog.Info("subscribers: ungated subnet egress",
		"vpc_id", evt.VpcId, "subnet_id", evt.SubnetId, "cidr", evt.DestinationCidr)
	respond(msg, nil)
}
