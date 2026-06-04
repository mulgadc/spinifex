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
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// IMDSSubnetSpec is the resolved set of OVN + host names for a subnet's IMDS
// localport. Every field is a deterministic function of the subnet ID, so
// callers can rebuild it from the subnet ID alone (BindManager does this per
// bucket entry).
type IMDSSubnetSpec struct {
	SubnetID      string
	ShortSubnetID string // last 8 chars of SubnetID
	LSName        string // subnet-{subnetID} — the guest's own switch
	LSPName       string // imds-port-{subnetID} (localport; host veth binds here)
	LSPMAC        string // 02:.. — HashMAC("imds-"+subnetID)
	OVSEndName    string // imds-o-{shortSubnetID}
	HostEndName   string // imds-h-{shortSubnetID}
}

// IMDSTopologyManager installs and removes the per-subnet localport that claims
// 169.254.169.254 directly on the guest's subnet switch, so metadata is reached
// over a single L2 hop on the guest's own broadcast domain — no router in the
// path. Idempotent.
type IMDSTopologyManager interface {
	EnsureForSubnet(ctx context.Context, subnetID, vpcID string, cidr netip.Prefix) (IMDSSubnetSpec, error)
	RemoveForSubnet(ctx context.Context, subnetID string) error
}

type imdsTopologyManager struct {
	ovn   ovn.Client
	store *handlers_imds.VethStore
}

var _ IMDSTopologyManager = (*imdsTopologyManager)(nil)

// NewIMDSTopologyManager constructs an IMDSTopologyManager. store is the
// subnet-veth KV bucket the installer publishes records to.
func NewIMDSTopologyManager(client ovn.Client, store *handlers_imds.VethStore) (IMDSTopologyManager, error) {
	switch {
	case client == nil:
		return nil, errors.New("IMDSTopologyManager: OVN client required")
	case store == nil:
		return nil, errors.New("IMDSTopologyManager: VethStore required")
	}
	return &imdsTopologyManager{ovn: client, store: store}, nil
}

// IMDSSpecForSubnet returns the deterministic name/MAC set for a subnet's IMDS
// localport without touching OVN or the bucket.
func IMDSSpecForSubnet(subnetID string) IMDSSubnetSpec {
	return IMDSSubnetSpec{
		SubnetID:      subnetID,
		ShortSubnetID: host.IMDSShortSubnetID(subnetID),
		LSName:        topology.SubnetSwitch(subnetID),
		LSPName:       topology.IMDSPort(subnetID),
		LSPMAC:        utils.HashMAC("imds-" + subnetID),
		OVSEndName:    host.IMDSOVSPortName(subnetID),
		HostEndName:   host.IMDSHostVethName(subnetID),
	}
}

// EnsureForSubnet installs the IMDS localport on subnet-{subnetID} and publishes
// the subnet-veth record. Idempotent: a present record short-circuits, and the
// localport is created only when absent so a lost record still converges. The
// subnet switch must already exist (the subnet lifecycle owns it). cidr is
// persisted in the record so the host reply path resolves the guest on-link;
// vpcID is persisted so the IMDS handler can resolve the subnet→VPC mapping the
// eni-by-vpc-ip index needs (the index stays keyed vpcID/ip).
func (m *imdsTopologyManager) EnsureForSubnet(ctx context.Context, subnetID, vpcID string, cidr netip.Prefix) (IMDSSubnetSpec, error) {
	if subnetID == "" {
		return IMDSSubnetSpec{}, errors.New("EnsureForSubnet: subnetID required")
	}
	if vpcID == "" {
		return IMDSSubnetSpec{}, errors.New("EnsureForSubnet: vpcID required")
	}
	if !cidr.IsValid() {
		return IMDSSubnetSpec{}, errors.New("EnsureForSubnet: cidr required")
	}
	spec := IMDSSpecForSubnet(subnetID)

	// The bucket record is the publish gate: present means the localport is
	// installed and BindManagers have (or will) materialise host state.
	rec, err := m.store.Get(subnetID)
	if err != nil {
		return IMDSSubnetSpec{}, fmt.Errorf("read imds veth record for %s: %w", subnetID, err)
	}
	if rec != nil {
		return spec, nil
	}

	extIDs := map[string]string{"spinifex:subnet_id": subnetID, "spinifex:role": "imds"}

	// Host-owned localport on the guest's subnet switch: no port_security (the
	// host sources 169.254.169.254 frames, and port_security would make
	// ovn-controller drop reply frames from the host veth's MAC) and no SG
	// membership (so the guest's SG ACLs never match it). localport binds on
	// every chassis.
	if _, err := m.ovn.GetLogicalSwitchPort(ctx, spec.LSPName); err != nil {
		if cErr := m.ovn.CreateLogicalSwitchPort(ctx, spec.LSName, &nbdb.LogicalSwitchPort{
			Name:        spec.LSPName,
			Type:        "localport",
			Addresses:   []string{spec.LSPMAC + " " + handlers_imds.MetaDataServerIP},
			ExternalIDs: extIDs,
		}); cErr != nil {
			return IMDSSubnetSpec{}, fmt.Errorf("create imds localport %s on %s: %w", spec.LSPName, spec.LSName, cErr)
		}
	}

	if err := m.store.Put(handlers_imds.SubnetVethRecord{
		SubnetID:      subnetID,
		ShortSubnetID: spec.ShortSubnetID,
		VPCID:         vpcID,
		IMDSPortMAC:   spec.LSPMAC,
		SubnetCIDR:    cidr.String(),
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return IMDSSubnetSpec{}, fmt.Errorf("publish imds veth record for %s: %w", subnetID, err)
	}

	slog.Info("external: installed IMDS localport",
		"subnet_id", subnetID, "subnet_switch", spec.LSName, "imds_port", spec.LSPName)
	return spec, nil
}

// RemoveForSubnet deletes the IMDS localport and the subnet-veth record. The
// subnet switch is left in place (the subnet lifecycle owns it). Idempotent and
// drift-tolerant: a missing localport is logged and skipped so the record delete
// always runs. Must run before the subnet switch goes away.
func (m *imdsTopologyManager) RemoveForSubnet(ctx context.Context, subnetID string) error {
	if subnetID == "" {
		return errors.New("RemoveForSubnet: subnetID required")
	}
	spec := IMDSSpecForSubnet(subnetID)

	if err := m.ovn.DeleteLogicalSwitchPort(ctx, spec.LSName, spec.LSPName); err != nil {
		slog.Warn("external: delete imds localport failed", "port", spec.LSPName, "err", err)
	}
	if err := m.store.Delete(subnetID); err != nil {
		return fmt.Errorf("delete imds veth record for %s: %w", subnetID, err)
	}

	slog.Info("external: removed IMDS localport", "subnet_id", subnetID)
	return nil
}
