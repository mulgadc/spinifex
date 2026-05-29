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
	// IMDS topology rides on every VPC (not just IGW-attached ones): private
	// instances still need instance metadata + role credentials.
	if s.imds != nil {
		if _, err := s.imds.EnsureForVPC(ctx, evt.VpcId); err != nil {
			slog.Error("subscribers: IMDS EnsureForVPC failed", "vpc_id", evt.VpcId, "err", err)
			respond(msg, err)
			return
		}
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
	// Remove IMDS topology before the router goes away — the IMDS LRP and
	// static route live on vpc-{vpcID}.
	if s.imds != nil {
		if err := s.imds.RemoveForVPC(ctx, evt.VpcId); err != nil {
			slog.Error("subscribers: IMDS RemoveForVPC failed", "vpc_id", evt.VpcId, "err", err)
			respond(msg, err)
			return
		}
	}
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
	if err := s.topology.DeleteSubnet(context.Background(), spec); err != nil {
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
	if err := s.natgw.AttachNATGateway(context.Background(), policy.NATGWSpec{
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
}

func (s *Subscriber) handleDeleteNATGateway(msg *nats.Msg) {
	var evt NATGatewayEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("subscribers: failed to unmarshal vpc.delete-nat-gateway event", "err", err)
		return
	}
	if err := s.natgw.DetachNATGateway(context.Background(), evt.VpcId, evt.SubnetCidr); err != nil {
		slog.Warn("subscribers: DeleteNATGateway failed",
			"vpc_id", evt.VpcId, "subnet_cidr", evt.SubnetCidr, "err", err)
		return
	}
	slog.Info("subscribers: NAT Gateway SNAT rule removed",
		"vpc_id", evt.VpcId, "natgw_id", evt.NatGatewayId, "subnet_cidr", evt.SubnetCidr)
}
