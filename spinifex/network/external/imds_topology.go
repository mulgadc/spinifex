package external

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	handlers_imds "github.com/mulgadc/spinifex/spinifex/handlers/imds"
	"github.com/mulgadc/spinifex/spinifex/network/host"
	"github.com/mulgadc/spinifex/spinifex/network/ovn"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// imdsLRPNetwork is the /30 the IMDS LRP terminates on the VPC router. .253 is
// the LRP, .254 is the host-owned IMDS LSP (MetaDataServerIP); the /30 makes
// the route to .254 directly-connected on the IMDS switch.
const imdsLRPNetwork = "169.254.169.253/30"

// metaDataServerRoute is the /32 the VPC router uses to send IMDS traffic out
// the IMDS LRP instead of the WAN default.
var metaDataServerRoute = netip.PrefixFrom(netip.MustParseAddr(handlers_imds.MetaDataServerIP), 32)

// IMDSVPCSpec is the resolved set of OVN + host names for a VPC's IMDS
// plumbing. Every field is a deterministic function of the VPC ID, so callers
// can rebuild it from the VPC ID alone (BindManager does this per bucket entry).
type IMDSVPCSpec struct {
	VPCID         string
	ShortVPCID    string // last 8 chars of VPCID
	LSName        string // imds-ls-{vpcID}
	LRPName       string // imds-lrp-{vpcID}
	RouterLSPName string // imds-rtr-{vpcID} (type=router)
	LSPName       string // imds-port-{vpcID} (localport; host veth binds here)
	LSPMAC        string // 02:.. — HashMAC("imds-"+vpcID)
	LRPNetwork    string // imdsLRPNetwork
	OVSEndName    string // imds-o-{shortVpcID}
	HostEndName   string // imds-h-{shortVpcID}
}

// IMDSTopologyManager installs and removes the per-VPC OVN topology that makes
// 169.254.169.254 routable to the host-owned IMDS localport. Idempotent. Lives in
// external (not policy): it composes L2 creation with L3 routing (ADR-0006 S5).
type IMDSTopologyManager interface {
	EnsureForVPC(ctx context.Context, vpcID string) (IMDSVPCSpec, error)
	RemoveForVPC(ctx context.Context, vpcID string) error
}

type imdsTopologyManager struct {
	ovn    ovn.Client
	routes policy.RouteManager
	store  *handlers_imds.VethStore
}

var _ IMDSTopologyManager = (*imdsTopologyManager)(nil)

// NewIMDSTopologyManager constructs an IMDSTopologyManager. store is the
// vpc-veth KV bucket the installer publishes records to.
func NewIMDSTopologyManager(client ovn.Client, routes policy.RouteManager, store *handlers_imds.VethStore) (IMDSTopologyManager, error) {
	switch {
	case client == nil:
		return nil, errors.New("IMDSTopologyManager: OVN client required")
	case routes == nil:
		return nil, errors.New("IMDSTopologyManager: RouteManager required")
	case store == nil:
		return nil, errors.New("IMDSTopologyManager: VethStore required")
	}
	return &imdsTopologyManager{ovn: client, routes: routes, store: store}, nil
}

// IMDSSpecForVPC returns the deterministic name/MAC set for a VPC's IMDS
// plumbing without touching OVN or the bucket.
func IMDSSpecForVPC(vpcID string) IMDSVPCSpec {
	return IMDSVPCSpec{
		VPCID:         vpcID,
		ShortVPCID:    host.IMDSShortVPCID(vpcID),
		LSName:        topology.IMDSSwitch(vpcID),
		LRPName:       topology.IMDSRouterPort(vpcID),
		RouterLSPName: topology.IMDSSwitchRouterPort(vpcID),
		LSPName:       topology.IMDSPort(vpcID),
		LSPMAC:        utils.HashMAC("imds-" + vpcID),
		LRPNetwork:    imdsLRPNetwork,
		OVSEndName:    host.IMDSOVSPortName(vpcID),
		HostEndName:   host.IMDSHostVethName(vpcID),
	}
}

// EnsureForVPC installs the IMDS OVN topology for vpcID and publishes the vpc-veth
// record. Idempotent: a present record short-circuits, and each OVN object is created
// only when absent so a lost record still converges. The VPC router must already exist.
func (m *imdsTopologyManager) EnsureForVPC(ctx context.Context, vpcID string) (IMDSVPCSpec, error) {
	if vpcID == "" {
		return IMDSVPCSpec{}, errors.New("EnsureForVPC: vpcID required")
	}
	spec := IMDSSpecForVPC(vpcID)

	// The bucket record is the publish gate: present means topology is
	// installed and BindManagers have (or will) materialise host state.
	rec, err := m.store.Get(vpcID)
	if err != nil {
		return IMDSVPCSpec{}, fmt.Errorf("read imds veth record for %s: %w", vpcID, err)
	}
	if rec != nil {
		return spec, nil
	}

	router := topology.VPCRouter(vpcID)
	extIDs := map[string]string{"spinifex:vpc_id": vpcID, "spinifex:role": "imds"}

	if _, err := m.ovn.EnsureLogicalSwitch(ctx, &nbdb.LogicalSwitch{Name: spec.LSName, ExternalIDs: extIDs}); err != nil {
		return IMDSVPCSpec{}, fmt.Errorf("ensure imds switch %s: %w", spec.LSName, err)
	}

	if _, err := m.ovn.GetLogicalRouterPort(ctx, spec.LRPName); err != nil {
		if cErr := m.ovn.CreateLogicalRouterPort(ctx, router, &nbdb.LogicalRouterPort{
			Name:        spec.LRPName,
			MAC:         utils.HashMAC(spec.LRPName),
			Networks:    []string{spec.LRPNetwork},
			ExternalIDs: extIDs,
		}); cErr != nil {
			return IMDSVPCSpec{}, fmt.Errorf("create imds router port %s: %w", spec.LRPName, cErr)
		}
	}

	if _, err := m.ovn.GetLogicalSwitchPort(ctx, spec.RouterLSPName); err != nil {
		if cErr := m.ovn.CreateLogicalSwitchPort(ctx, spec.LSName, &nbdb.LogicalSwitchPort{
			Name:        spec.RouterLSPName,
			Type:        "router",
			Addresses:   []string{"router"},
			Options:     map[string]string{"router-port": spec.LRPName},
			ExternalIDs: extIDs,
		}); cErr != nil {
			return IMDSVPCSpec{}, fmt.Errorf("create imds router LSP %s: %w", spec.RouterLSPName, cErr)
		}
	}

	// Host-owned localport: no port_security (the host is trusted to source
	// 169.254.169.254 frames, and port_security would make ovn-controller drop
	// reply frames from the host veth's MAC). localport binds on every chassis.
	if _, err := m.ovn.GetLogicalSwitchPort(ctx, spec.LSPName); err != nil {
		if cErr := m.ovn.CreateLogicalSwitchPort(ctx, spec.LSName, &nbdb.LogicalSwitchPort{
			Name:        spec.LSPName,
			Type:        "localport",
			Addresses:   []string{spec.LSPMAC + " " + handlers_imds.MetaDataServerIP},
			ExternalIDs: extIDs,
		}); cErr != nil {
			return IMDSVPCSpec{}, fmt.Errorf("create imds localport %s: %w", spec.LSPName, cErr)
		}
	}

	if err := m.routes.AddStaticRoute(ctx, vpcID, policy.RouteSpec{
		Prefix:     metaDataServerRoute,
		Nexthop:    handlers_imds.MetaDataServerIP,
		OutputPort: spec.LRPName,
	}); err != nil {
		return IMDSVPCSpec{}, fmt.Errorf("add imds static route on %s: %w", router, err)
	}

	if err := m.store.Put(handlers_imds.VPCVethRecord{
		VPCID:       vpcID,
		ShortVPCID:  spec.ShortVPCID,
		IMDSPortMAC: spec.LSPMAC,
		LRPNetwork:  spec.LRPNetwork,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return IMDSVPCSpec{}, fmt.Errorf("publish imds veth record for %s: %w", vpcID, err)
	}

	slog.Info("external: installed IMDS topology",
		"vpc_id", vpcID, "imds_switch", spec.LSName, "imds_lrp", spec.LRPName, "imds_port", spec.LSPName)
	return spec, nil
}

// RemoveForVPC tears down the IMDS OVN topology and the vpc-veth record.
// Idempotent and drift-tolerant: missing OVN objects are logged and skipped so the
// record delete always runs. Must run before the VPC router (it holds the LRP/route).
func (m *imdsTopologyManager) RemoveForVPC(ctx context.Context, vpcID string) error {
	if vpcID == "" {
		return errors.New("RemoveForVPC: vpcID required")
	}
	spec := IMDSSpecForVPC(vpcID)
	router := topology.VPCRouter(vpcID)

	if err := m.routes.DeleteStaticRoute(ctx, vpcID, metaDataServerRoute); err != nil {
		slog.Warn("external: delete imds static route failed", "vpc_id", vpcID, "err", err)
	}
	if err := m.ovn.DeleteLogicalSwitchPort(ctx, spec.LSName, spec.LSPName); err != nil {
		slog.Warn("external: delete imds localport failed", "port", spec.LSPName, "err", err)
	}
	if err := m.ovn.DeleteLogicalSwitchPort(ctx, spec.LSName, spec.RouterLSPName); err != nil {
		slog.Warn("external: delete imds router LSP failed", "port", spec.RouterLSPName, "err", err)
	}
	if err := m.ovn.DeleteLogicalRouterPort(ctx, router, spec.LRPName); err != nil {
		slog.Warn("external: delete imds router port failed", "port", spec.LRPName, "err", err)
	}
	if err := m.ovn.DeleteLogicalSwitch(ctx, spec.LSName); err != nil {
		slog.Warn("external: delete imds switch failed", "switch", spec.LSName, "err", err)
	}
	if err := m.store.Delete(vpcID); err != nil {
		return fmt.Errorf("delete imds veth record for %s: %w", vpcID, err)
	}

	slog.Info("external: removed IMDS topology", "vpc_id", vpcID)
	return nil
}
