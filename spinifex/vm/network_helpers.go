package vm

import (
	"errors"
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
// primary ENI once its tap is up, before QEMU starts the guest. Only the primary
// ENI serves IMDS, so extra ENIs are skipped; serving is wired separately by the
// IMDS responder's reconcile-from-taps.
//
// The primary tap is already on the secure br-imds, so the attach is NOT
// best-effort: a serving-only failure (vm.ErrIMDSServingDegraded) leaves the guest
// fully connected and is logged-and-continued, but a connectivity-critical failure
// would strand the guest with no L2 path to OVN, so the tap is rolled back to
// br-int. A nil error means the datapath (or the rollback) is in a connected state.
func (m *Manager) attachPrimaryIMDSDatapath(instance *VM) error {
	if m.deps.NetworkPlumber == nil || instance.ENIId == "" {
		return nil
	}
	subnetID := ""
	if instance.Instance != nil {
		subnetID = aws.StringValue(instance.Instance.SubnetId)
	}
	if subnetID == "" {
		slog.Debug("IMDS: no subnet for primary ENI, skipping per-tap datapath",
			"instance", instance.ID, "eni", instance.ENIId)
		return nil
	}
	err := m.deps.NetworkPlumber.AttachIMDSDatapath(instance.ENIId, instance.ENIMac, subnetID)
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrIMDSServingDegraded) {
		// Connectivity is intact (the patch + forward flows installed); only the
		// IMDS demux/egress/reply stage failed. The guest keeps full VPC
		// connectivity, so log and continue — boot is not gated on IMDS readiness.
		slog.Error("IMDS: per-tap serving install failed; guest keeps connectivity but IMDS is unavailable",
			"instance", instance.ID, "eni", instance.ENIId, "err", err)
		return nil
	}
	// Connectivity-critical failure: the primary tap is stranded on the secure
	// br-imds with no patch/forward path to OVN, so the guest would boot
	// black-holed (no gateway, DHCP renewal, or off-subnet traffic). Roll the tap
	// back to br-int — losing IMDS but restoring full connectivity, the pre-cutover
	// behaviour — and fail the launch only if even that fails.
	slog.Error("IMDS: per-tap connectivity install failed; rolling primary tap back to br-int",
		"instance", instance.ID, "eni", instance.ENIId, "err", err)
	return m.rollbackPrimaryTapToBrInt(instance)
}

// rollbackPrimaryTapToBrInt moves the primary tap off the secure br-imds back onto
// br-int after a connectivity-critical IMDS datapath failure, restoring the guest's
// L2 path to OVN at the cost of IMDS. It tears down any partial datapath (freeing
// the patch's br-int iface-id so the tap can reclaim it), removes the stranded tap
// from br-imds, then re-plumbs it on br-int with the OVN binding. Detach and
// cleanup are best-effort, but a failure to re-plumb onto br-int leaves the guest
// with no connected path, so that fails the launch.
func (m *Manager) rollbackPrimaryTapToBrInt(instance *VM) error {
	if err := m.deps.NetworkPlumber.DetachIMDSDatapath(instance.ENIId); err != nil {
		slog.Warn("IMDS: rollback detach failed (continuing)",
			"instance", instance.ID, "eni", instance.ENIId, "err", err)
	}
	tapName := TapDeviceName(instance.ENIId)
	if err := m.deps.NetworkPlumber.CleanupTap(tapName); err != nil {
		slog.Warn("IMDS: rollback tap cleanup failed (continuing)",
			"instance", instance.ID, "tap", tapName, "err", err)
	}
	if err := m.deps.NetworkPlumber.SetupTap(VPCTapSpec(instance.ENIId, instance.ENIMac)); err != nil {
		return fmt.Errorf("roll primary tap %s back to br-int: %w", tapName, err)
	}
	slog.Warn("IMDS: primary tap rolled back to br-int; guest connected, IMDS unavailable",
		"instance", instance.ID, "tap", tapName, "eni", instance.ENIId)
	return nil
}

// detachPrimaryIMDSDatapath removes the per-tap IMDS datapath for the instance's
// primary ENI at terminate, the inverse of attachPrimaryIMDSDatapath. Best-effort:
// a failure is logged and never fails teardown. Only the primary ENI carries the
// datapath; teardown keys off the ENI-derived port names, so no subnet is needed.
func (m *Manager) detachPrimaryIMDSDatapath(instance *VM) {
	if m.deps.NetworkPlumber == nil || instance.ENIId == "" {
		return
	}
	if err := m.deps.NetworkPlumber.DetachIMDSDatapath(instance.ENIId); err != nil {
		slog.Warn("IMDS: per-tap datapath detach failed (continuing)",
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
