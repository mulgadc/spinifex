// Package netprobe identifies physical network interfaces by hardware rather
// than kernel name, so an operator choosing between eno1, ens1f0np0 and
// ens1f1np1 can tell the 1gbe Broadcom from the 25gbe Mellanox.
package netprobe

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// NIC describes one physical interface as presented in the installer's
// selection table.
type NIC struct {
	Name string

	// Vendor and Model come from networkctl's hardware database lookup, e.g.
	// "Mellanox Technologies" and "MT27710 Family [ConnectX-4 Lx]".
	Vendor string
	Model  string

	// AltName is the first alternative name systemd assigns, which is what
	// operators often see in switch and cabling documentation.
	AltName string

	// Speed is the negotiated link speed as networkctl reports it ("25Gbps").
	// Empty when the link has no carrier, which is why Probe brings links up
	// before reading.
	Speed string

	// State is networkctl's online state ("online", "off", "degraded").
	State string

	MTU    int
	IsWiFi bool
}

// Label renders the NIC for a single table row.
func (n NIC) Label() string {
	hw := strings.TrimSpace(n.Vendor + " " + n.Model)
	if hw == "" {
		hw = "unknown"
	}
	speed := n.Speed
	if speed == "" {
		speed = "—"
	}
	return fmt.Sprintf("%s  %s  %s  %s", n.Name, speed, n.State, hw)
}

// linkUpTimeout bounds how long Probe waits after bringing links up for speed
// and carrier to settle. A 25gbe port negotiating against a cold switch can
// take a second or two to report a speed.
const linkUpTimeout = 2 * time.Second

// Probe enumerates non-loopback physical interfaces and enriches each with
// networkctl hardware detail. Links are brought up first: networkctl reports
// no Speed and an "off" state for a down interface, which would render every
// fast port as unknown at exactly the moment the operator needs to identify it.
func Probe() ([]NIC, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	var names []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if len(iface.HardwareAddr) == 0 {
			continue
		}
		names = append(names, iface.Name)
	}

	brought := false
	for _, name := range names {
		if err := exec.Command("ip", "link", "set", "dev", name, "up").Run(); err != nil {
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
		nic := NIC{Name: name, IsWiFi: isWiFi(name)}
		out, err := exec.Command("networkctl", "status", "--no-pager", name).Output()
		if err != nil {
			slog.Debug("netprobe: networkctl status failed", "iface", name, "err", err)
			nics = append(nics, nic)
			continue
		}
		merge(&nic, ParseStatus(string(out)))
		nics = append(nics, nic)
	}
	return nics, nil
}

// merge copies the parsed hardware detail onto a NIC, preserving the fields
// Probe established itself.
func merge(dst *NIC, src NIC) {
	dst.Vendor = src.Vendor
	dst.Model = src.Model
	dst.AltName = src.AltName
	dst.Speed = src.Speed
	dst.State = src.State
	dst.MTU = src.MTU
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

// isWiFi reports whether the interface has a wireless subdirectory in sysfs.
func isWiFi(name string) bool {
	_, err := os.Stat("/sys/class/net/" + name + "/wireless")
	return err == nil
}
