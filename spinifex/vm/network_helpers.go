package vm

import (
	"fmt"
	"log/slog"
	"strings"

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
	return "port-" + eniID
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

// GenerateDevMAC returns the locally-administered unicast MAC for the
// dev/hostfwd NIC. The "dev:" tag disambiguates from the mgmt NIC of the
// same instance (which shares instanceID).
func GenerateDevMAC(instanceID string) string {
	return utils.HashMAC("dev:" + instanceID)
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
		instance.Config.Devices = append(instance.Config.Devices, Device{
			Value: fmt.Sprintf("virtio-net-pci,netdev=%s,mac=%s", netID, extra.ENIMac),
		})
		slog.Info("Extra VPC NIC configured",
			"tap", extraTapName, "eni", extra.ENIID, "mac", extra.ENIMac, "subnet", extra.SubnetID)
	}
	return nil
}
