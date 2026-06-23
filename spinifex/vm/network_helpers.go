package vm

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// TapSpec parameterises a single tap-on-OVS-bridge plumbing operation.
// VPC taps populate ExternalIDs (iface-id, attached-mac) for OVN binding;
// management taps leave it nil since br-mgmt is a plain L2 standalone bridge.
type TapSpec struct {
	Name        string
	Bridge      string
	ExternalIDs map[string]string
}

// TapDeviceName returns the Linux tap device name for an ENI.
// Linux IFNAMSIZ limits interface names to 15 characters; long ENI IDs are
// truncated to fit.
func TapDeviceName(eniID string) string {
	id := strings.TrimPrefix(eniID, "eni-")
	name := "tap" + id
	if len(name) > 15 {
		name = name[:15]
	}
	return name
}

// MgmtTapName returns the Linux TAP device name for a management NIC.
// Uses "mg" prefix + truncated instance ID to stay within 15-char IFNAMSIZ.
func MgmtTapName(instanceID string) string {
	id := strings.TrimPrefix(instanceID, "i-")
	name := "mg" + id
	if len(name) > 15 {
		name = name[:15]
	}
	return name
}

// OVSIfaceID returns the OVS external_ids:iface-id value for an ENI.
// This must match the OVN LogicalSwitchPort name for ovn-controller binding.
func OVSIfaceID(eniID string) string {
	return topology.Port(eniID)
}

// VPCTapSpec returns the TapSpec for a VPC ENI's tap on br-int. The
// external_ids carry the OVN binding (iface-id) and the kernel-attached MAC.
func VPCTapSpec(eniID, mac string) TapSpec {
	return TapSpec{
		Name:   TapDeviceName(eniID),
		Bridge: "br-int",
		ExternalIDs: map[string]string{
			"iface-id":     OVSIfaceID(eniID),
			"attached-mac": mac,
		},
	}
}

// IMDSBridgeName is the dedicated, OVN-unmanaged bridge that carries the per-tap
// IMDS datapath. Kept in sync with network/host.IMDSBridge (host imports vm, not
// the reverse); a cross-package test in network/host asserts they match.
const IMDSBridgeName = "br-imds"

// IMDSPrimaryTapSpec returns the TapSpec for a primary ENI's tap on br-imds. The
// tap lives on br-imds so its egress meets the IMDS demux flows on the same
// bridge; it carries no external_ids because the br-imds<->br-int patch's br-int
// end (installed by AttachIMDSDatapath) carries the OVN iface-id binding instead.
func IMDSPrimaryTapSpec(eniID string) TapSpec {
	return TapSpec{
		Name:   TapDeviceName(eniID),
		Bridge: IMDSBridgeName,
	}
}

// GenerateDevMAC returns the locally-administered unicast MAC for the
// dev/hostfwd NIC. The "dev:" tag disambiguates from the mgmt NIC of the
// same instance (which shares instanceID).
func GenerateDevMAC(instanceID string) string {
	return utils.HashMAC("dev:" + instanceID)
}

// GenerateMgmtMAC returns the locally-administered unicast MAC for the
// management NIC. The "mgmt:" tag disambiguates from the dev NIC of the
// same instance (which shares instanceID).
func GenerateMgmtMAC(instanceID string) string {
	return utils.HashMAC("mgmt:" + instanceID)
}

// attachPrimaryIMDSDatapath installs the per-tap IMDS datapath for the instance's
// primary ENI once its tap is up, before QEMU starts the guest. Best-effort: a
// failure is logged and never fails the launch, since IMDS readiness must not
// gate boot. Only the primary ENI serves IMDS, so extra ENIs are skipped.
// Serving is wired separately by the IMDS responder's reconcile-from-taps.
func (m *Manager) attachPrimaryIMDSDatapath(instance *VM) {
	if m.deps.NetworkPlumber == nil || instance.ENIId == "" {
		return
	}
	subnetID := ""
	if instance.Instance != nil {
		subnetID = aws.StringValue(instance.Instance.SubnetId)
	}
	if subnetID == "" {
		slog.Debug("IMDS: no subnet for primary ENI, skipping per-tap datapath",
			"instance", instance.ID, "eni", instance.ENIId)
		return
	}
	if err := m.deps.NetworkPlumber.AttachIMDSDatapath(instance.ENIId, instance.ENIMac, subnetID); err != nil {
		slog.Warn("IMDS: per-tap datapath attach failed (continuing)",
			"instance", instance.ID, "eni", instance.ENIId, "err", err)
	}
}

// setupExtraENINICs creates tap devices on br-int and appends matching QEMU
// virtio-net device entries to instance.Config for each additional ENI a
// system VM spans. The primary ENI is handled separately by the launch
// caller. Cloud-init brings the guest interfaces up via per-MAC DHCP blocks
// written by generateNetworkConfig.
func (m *Manager) setupExtraENINICs(instance *VM) error {
	if m.deps.NetworkPlumber == nil {
		return nil
	}
	for idx, extra := range instance.ExtraENIs {
		spec := VPCTapSpec(extra.ENIID, extra.ENIMac)
		if err := m.deps.NetworkPlumber.SetupTap(spec); err != nil {
			slog.Error("Failed to set up tap device for extra ENI", "eni", extra.ENIID, "err", err)
			return fmt.Errorf("setup tap device for extra ENI %s: %w", extra.ENIID, err)
		}
		extraTapName := spec.Name
		netID := fmt.Sprintf("net%d", idx+1)
		instance.Config.NetDevs = append(instance.Config.NetDevs, NetDev{
			Value: fmt.Sprintf("tap,id=%s,ifname=%s,script=no,downscript=no", netID, extraTapName),
		})
		instance.Config.Devices = append(instance.Config.Devices, NetDevice(instance.Config.MachineType, netID, extra.ENIMac))
		slog.Info("Extra VPC NIC configured",
			"tap", extraTapName, "eni", extra.ENIID, "mac", extra.ENIMac, "subnet", extra.SubnetID)
	}
	return nil
}
