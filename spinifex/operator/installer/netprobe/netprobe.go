// Package netprobe identifies physical network interfaces by hardware rather
// than kernel name, so an operator choosing between eno1, ens1f0np0 and
// ens1f1np1 can tell the 1gbe Broadcom from the 25gbe Mellanox.
package netprobe

import (
	"cmp"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// sysClassNet is the kernel's per-interface attribute tree. A variable, not a
// constant, so tests can point the probe at a fixture instead of the running
// machine's hardware.
var sysClassNet = "/sys/class/net"

// The external helpers the probe leans on, indirected so tests can exercise it
// without the host's real tooling.
var (
	listInterfaces = net.Interfaces
	commandOutput  = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).Output()
	}
)

// NIC describes one physical interface as presented in the installer's
// selection table.
type NIC struct {
	Name string

	// Vendor and Model come from the udev hardware database, e.g. "Mellanox
	// Technologies" and "MT27710 Family [ConnectX-4 Lx]". Empty when the
	// database carries no entry for the card.
	Vendor string
	Model  string

	// Driver is the kernel module that claimed the port, e.g. "mlx5_core". It
	// comes straight from sysfs, so it still identifies a card the hardware
	// database misses.
	Driver string

	// DeviceID is the PCI or USB "vendor:device" pair, the last-resort identity
	// for hardware neither the database nor a driver name pins down.
	DeviceID string

	// AltName is the first alternative name systemd assigns, which is what
	// operators often see in switch and cabling documentation.
	AltName string

	// Speed is the negotiated link speed rendered for display ("25 Gbps") and
	// SpeedMbps the raw figure behind it. Both are zero on a port with no
	// carrier, which is why Probe brings links up before reading.
	Speed     string
	SpeedMbps int

	// Carrier reports a cable with link. It orders the picker so the port the
	// operator has just plugged in is the one offered first.
	Carrier bool

	// State is the link summary shown in the table: "online", "no cable" or
	// "down".
	State string

	MTU    int
	IsWiFi bool
}

// Label renders the NIC's identity line: kernel name, negotiated speed and link
// state. The hardware detail is deliberately absent — it goes on its own line
// via Hardware, where there is room to read it.
func (n NIC) Label() string {
	speed := n.Speed
	if speed == "" {
		speed = "—"
	}
	state := n.State
	if state == "" {
		state = "unknown"
	}
	return fmt.Sprintf("%s  %s  %s", n.Name, speed, state)
}

// Hardware renders the card's identity for the detail line, degrading from the
// database name to the driver and device id rather than to "unknown". An
// operator needs something to match against the card in their hand.
func (n NIC) Hardware() string {
	if hw := strings.TrimSpace(n.Vendor + " " + n.Model); hw != "" {
		return hw
	}
	switch {
	case n.Driver != "" && n.DeviceID != "":
		return n.Driver + " [" + n.DeviceID + "]"
	case n.Driver != "":
		return n.Driver
	case n.DeviceID != "":
		return n.DeviceID
	}
	return "no hardware detail"
}

// linkUpTimeout bounds how long Probe waits after bringing links up for speed
// and carrier to settle. A 25gbe port negotiating against a cold switch can
// take a second or two to report a speed.
var linkUpTimeout = 2 * time.Second

// Probe enumerates non-loopback physical interfaces and enriches each from
// sysfs and the udev hardware database, ordered so the ports worth assigning
// come first. Links are brought up first: a down port reports no speed and no
// carrier, which would leave every fast port unidentified at exactly the moment
// the operator needs to pick one.
//
// Nothing the probe depends on needs a running systemd. The live installer
// boots straight into spinifex-init with only udevd started, so networkctl —
// which has to reach systemd-networkd — enriches gaps and is never the source.
func Probe() ([]NIC, error) {
	ifaces, err := listInterfaces()
	if err != nil {
		return nil, err
	}

	var names, physical []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if len(iface.HardwareAddr) == 0 {
			continue
		}
		names = append(names, iface.Name)
		if hasDevice(iface.Name) {
			physical = append(physical, iface.Name)
		}
	}

	// Bridges, bonds and veth pairs carry a MAC like any port but have no
	// device behind them, and offering them would let a plane be bound to
	// something that does not exist until after the install. Keep them only if
	// the filter found nothing, so an unusual platform still gets a list.
	if len(physical) > 0 {
		names = physical
	}

	brought := false
	for _, name := range names {
		if _, err := commandOutput("ip", "link", "set", "dev", name, "up"); err != nil {
			slog.Debug("netprobe: could not bring link up", "iface", name, "err", err)
			continue
		}
		brought = true
	}
	if brought {
		time.Sleep(linkUpTimeout)
	}

	nics := make([]NIC, 0, len(names))
	for _, name := range names {
		nics = append(nics, describe(name))
	}
	slices.SortFunc(nics, byUsefulness)
	return nics, nil
}

// describe reads everything about one interface that needs no daemon, then lets
// networkctl fill the gaps if it happens to be answering.
func describe(name string) NIC {
	nic := NIC{
		Name:     name,
		Driver:   sysLinkBase(name, "device/driver"),
		DeviceID: deviceID(name),
		MTU:      sysInt(name, "mtu"),
		IsWiFi:   isWiFi(name),
	}

	// The kernel reports -1 for a port that cannot state its speed; treat that
	// as unknown rather than letting it sort below every real figure.
	nic.SpeedMbps = max(sysInt(name, "speed"), 0)
	nic.Speed = formatSpeed(nic.SpeedMbps)
	nic.Carrier = sysAttr(name, "carrier") == "1"
	nic.State = linkState(name, nic.Carrier)

	props := udevProperties(name)
	nic.Vendor = props["ID_VENDOR_FROM_DATABASE"]
	nic.Model = props["ID_MODEL_FROM_DATABASE"]

	// udev's predictable names are the ones that appear in cabling notes; the
	// path-based name is the one networkctl lists first.
	for _, key := range []string{"ID_NET_NAME_PATH", "ID_NET_NAME_ONBOARD", "ID_NET_NAME_SLOT"} {
		if alt := props[key]; alt != "" && alt != name {
			nic.AltName = alt
			break
		}
	}

	if out, err := commandOutput("networkctl", "status", "--no-pager", name); err == nil {
		fill(&nic, ParseStatus(string(out)))
	} else {
		slog.Debug("netprobe: networkctl unavailable, using sysfs only", "iface", name, "err", err)
	}
	return nic
}

// fill copies parsed hardware detail onto a NIC without overwriting anything
// sysfs already established. State is never taken: the carrier bit is a truer
// answer to "is this cable live" than networkd's configuration status.
func fill(dst *NIC, src NIC) {
	if dst.Vendor == "" {
		dst.Vendor = src.Vendor
	}
	if dst.Model == "" {
		dst.Model = src.Model
	}
	if dst.AltName == "" {
		dst.AltName = src.AltName
	}
	if dst.Speed == "" {
		dst.Speed = src.Speed
	}
	if dst.MTU == 0 {
		dst.MTU = src.MTU
	}
}

// byUsefulness orders the picker so the interfaces an operator would actually
// assign come first: a live cable, then wired over wireless, then the fastest
// port. The rest still appear, just below the plausible choices.
func byUsefulness(a, b NIC) int {
	if a.Carrier != b.Carrier {
		return boolOrder(a.Carrier)
	}
	if a.IsWiFi != b.IsWiFi {
		return boolOrder(b.IsWiFi)
	}
	if a.SpeedMbps != b.SpeedMbps {
		return cmp.Compare(b.SpeedMbps, a.SpeedMbps)
	}
	return cmp.Compare(a.Name, b.Name)
}

// boolOrder sorts true ahead of false.
func boolOrder(first bool) int {
	if first {
		return -1
	}
	return 1
}

// formatSpeed renders the kernel's Mbit/s figure the way a datasheet does.
func formatSpeed(mbps int) string {
	switch {
	case mbps <= 0:
		return ""
	case mbps%1000 == 0:
		return fmt.Sprintf("%d Gbps", mbps/1000)
	default:
		return fmt.Sprintf("%d Mbps", mbps)
	}
}

// linkState collapses the kernel's flags into the three outcomes an operator
// acts on: the port is live, the port is enabled but unplugged, or the port
// never came up at all. operstate reports "down" for the latter two alike.
func linkState(name string, carrier bool) string {
	switch {
	case carrier:
		return "online"
	case adminUp(name):
		return "no cable"
	default:
		return "down"
	}
}

// adminUp reads the IFF_UP bit out of the interface flags.
func adminUp(name string) bool {
	flags, err := strconv.ParseUint(strings.TrimPrefix(sysAttr(name, "flags"), "0x"), 16, 64)
	return err == nil && flags&0x1 != 0
}

// deviceID reads the PCI or USB "vendor:device" pair. A NIC's device link
// points at the PCI function directly; on USB it points at the interface, whose
// parent carries the ids.
func deviceID(name string) string {
	dev := filepath.Join(sysClassNet, name, "device")
	if id := idPair(dev, "vendor", "device"); id != "" {
		return id
	}
	return idPair(filepath.Join(dev, ".."), "idVendor", "idProduct")
}

func idPair(dir, vendorAttr, productAttr string) string {
	vendor := readTrimmed(filepath.Join(dir, vendorAttr))
	product := readTrimmed(filepath.Join(dir, productAttr))
	if vendor == "" || product == "" {
		return ""
	}
	return strings.TrimPrefix(vendor, "0x") + ":" + strings.TrimPrefix(product, "0x")
}

// udevProperties reads the interface's udev database entry, which is where the
// hardware-database vendor and model names live. udevd populates it during the
// installer's device enumeration, so this works with no systemd running.
func udevProperties(name string) map[string]string {
	out, err := commandOutput("udevadm", "info", "--query=property", "--path="+filepath.Join(sysClassNet, name))
	if err != nil {
		slog.Debug("netprobe: udevadm info failed", "iface", name, "err", err)
		return nil
	}

	props := make(map[string]string)
	for line := range strings.SplitSeq(string(out), "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[key] = value
		}
	}
	return props
}

// ParseStatus extracts the hardware fields from `networkctl status <iface>`
// output. The format is a set of right-aligned "Key: value" lines, with
// Alternative Names continuing across subsequent indented lines.
func ParseStatus(out string) NIC {
	var nic NIC
	var inAltNames bool

	for line := range strings.SplitSeq(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			inAltNames = false
			continue
		}

		key, value, found := strings.Cut(trimmed, ": ")
		if !found {
			// A continuation line under "Alternative Names:" — take the first
			// only, which is the one operators see in cabling notes.
			if inAltNames && nic.AltName == "" {
				nic.AltName = trimmed
			}
			continue
		}
		inAltNames = false

		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "Vendor":
			nic.Vendor = value
		case "Model":
			nic.Model = value
		case "Speed":
			nic.Speed = value
		case "Online state":
			// Rendered as "online" or "online (configured)".
			if f := strings.Fields(value); len(f) > 0 {
				nic.State = f[0]
			}
		case "MTU":
			// Rendered as "9000 (min: 68, max: 9978)".
			if f := strings.Fields(value); len(f) > 0 {
				if mtu, err := strconv.Atoi(f[0]); err == nil {
					nic.MTU = mtu
				}
			}
		case "Alternative Names":
			inAltNames = true
			if f := strings.Fields(value); len(f) > 0 && nic.AltName == "" {
				nic.AltName = f[0]
			}
		}
	}
	return nic
}

// hasDevice reports whether the interface is backed by real hardware, which
// sysfs shows as a device link to the PCI, USB or platform node behind it.
func hasDevice(name string) bool {
	_, err := os.Stat(filepath.Join(sysClassNet, name, "device"))
	return err == nil
}

// isWiFi reports whether the interface has a wireless subdirectory in sysfs.
func isWiFi(name string) bool {
	_, err := os.Stat(filepath.Join(sysClassNet, name, "wireless"))
	return err == nil
}

// sysAttr reads one sysfs attribute. A missing or unreadable attribute is not
// an error worth surfacing: speed and carrier both fail on a port with no link,
// which the state column already conveys.
func sysAttr(name, attr string) string {
	return readTrimmed(filepath.Join(sysClassNet, name, attr))
}

func sysInt(name, attr string) int {
	n, err := strconv.Atoi(sysAttr(name, attr))
	if err != nil {
		return 0
	}
	return n
}

// sysLinkBase resolves a sysfs symlink and returns its final element, which is
// how the driver name is exposed.
func sysLinkBase(name, attr string) string {
	target, err := os.Readlink(filepath.Join(sysClassNet, name, attr))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
