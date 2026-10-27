// Package hostnet re-creates the host network wiring setup-ovn.sh builds, on
// hosts where no systemd-networkd files persist it: the uplink veth pairs, the
// management bridge address and the encap route. spinifex-hostnet.service runs
// Apply at boot and whenever ovs-vswitchd restarts; the daemon and vpcd run
// Check before they start, so neither starts on a node left unwired.
package hostnet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
)

// Backend is how a node persists its host network wiring across reboots.
type Backend string

const (
	// BackendNetworkd persists the wiring in systemd-networkd files.
	BackendNetworkd Backend = "networkd"
	// BackendHostnet persists it in the state file, applied by this package.
	BackendHostnet Backend = "hostnet"
)

// StateVersion is the only state file version this build reads.
const StateVersion = 1

// WAN uplink modes, as setup-ovn.sh names them.
const (
	ModeExisting = "existing"
	ModeVeth     = "veth"
	ModeDirect   = "direct"
	ModeNAT      = "nat"
)

// Paths locates the files hostnet reads and writes.
type Paths struct {
	// Backend holds the backend setup-ovn.sh chose on its first run.
	Backend string
	// State is the topology setup-ovn.sh recorded for Apply.
	State string
	// Marker exists only while the last Apply succeeded.
	Marker string
	// Lock serialises Apply against setup-ovn.sh.
	Lock string
	// NetworkdDir is where setup-ovn.sh writes its systemd-networkd files.
	NetworkdDir string
}

// DefaultPaths returns the paths on a live host.
func DefaultPaths() Paths { return PathsAt("/") }

// PathsAt returns the paths with root prefixed, for tests and preflight.
func PathsAt(root string) Paths {
	return Paths{
		Backend:     filepath.Join(root, "/etc/spinifex/hostnet-backend"),
		State:       filepath.Join(root, "/etc/spinifex/hostnet.json"),
		Marker:      filepath.Join(root, "/run/spinifex/hostnet-applied"),
		Lock:        filepath.Join(root, "/run/spinifex/hostnet.lock"),
		NetworkdDir: filepath.Join(root, "/etc/systemd/network"),
	}
}

// State is the host network topology setup-ovn.sh recorded. Every name in it
// comes from setup-ovn.sh, so custom bridge names need nothing here.
type State struct {
	Version int `json:"version"`
	// Pending is set while setup-ovn.sh is changing live wiring; Apply and Check
	// refuse it, since the recorded topology may no longer match OVSDB.
	Pending    bool   `json:"pending"`
	IntBridge  string `json:"int_bridge"`
	WAN        *WAN   `json:"wan,omitempty"`
	Mgmt       *Mgmt  `json:"mgmt,omitempty"`
	EncapRoute *Route `json:"encap_route,omitempty"`
}

// WAN is the public-subnet uplink.
type WAN struct {
	Mode string `json:"mode"`
	// OVSBridge is the OVS bridge ovn-bridge-mappings names.
	OVSBridge string `json:"ovs_bridge"`
	// LinuxBridge is the host's bridge the veth pair joins (veth mode).
	LinuxBridge string `json:"linux_bridge,omitempty"`
	// NIC is the physical NIC moved into OVSBridge (direct mode).
	NIC string `json:"nic,omitempty"`
	// HostEnd and OVSEnd name the veth pair (veth and nat modes); OVSEnd is
	// the end that is a port on OVSBridge.
	HostEnd string `json:"host_end,omitempty"`
	OVSEnd  string `json:"ovs_end,omitempty"`
	// HostMAC and HostCIDR are set on HostEnd (nat mode).
	HostMAC  string `json:"host_mac,omitempty"`
	HostCIDR string `json:"host_cidr,omitempty"`
}

// Mgmt is the system-instance management bridge.
type Mgmt struct {
	Bridge string `json:"bridge"`
	CIDR   string `json:"cidr,omitempty"`
	// NIC is moved into Bridge and keeps no IPv4 address of its own.
	NIC string `json:"nic,omitempty"`
}

// Route is the Geneve encap route: Dst via Dev from Src, at Metric.
type Route struct {
	Dst    string `json:"dst"`
	Dev    string `json:"dev"`
	Src    string `json:"src"`
	Metric int    `json:"metric"`
}

// ReadBackend returns the recorded backend, or "" when there is no record.
func ReadBackend(path string) (Backend, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read backend record %s: %w", path, err)
	}
	switch b := Backend(strings.TrimSpace(string(data))); b {
	case BackendNetworkd, BackendHostnet:
		return b, nil
	default:
		return "", fmt.Errorf("backend record %s: unknown backend %q (want %q or %q)", path, b, BackendNetworkd, BackendHostnet)
	}
}

// LoadState reads and validates the state file. Every error names the file.
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("state file %s: %w", path, err)
	}
	// Read the version alone first, so a future layout is reported as an
	// unknown version rather than as whichever field it renamed.
	var head struct {
		Version *int `json:"version"`
		Pending bool `json:"pending"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("state file %s: not valid JSON (truncated?): %w", path, err)
	}
	if head.Version == nil {
		return nil, fmt.Errorf("state file %s: no version field", path)
	}
	if *head.Version != StateVersion {
		return nil, fmt.Errorf("state file %s: unknown version %d (this build reads version %d)", path, *head.Version, StateVersion)
	}
	// A pending file may describe a topology setup-ovn.sh never finished
	// building, so it is refused before its contents are judged.
	if head.Pending {
		return nil, fmt.Errorf("state file %s is pending: setup-ovn.sh stopped part-way through changing the host network; rerun it with the same arguments", path)
	}
	var s State
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("state file %s: %w", path, err)
	}
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("state file %s: %w", path, err)
	}
	return &s, nil
}

func (s *State) validate() error {
	if err := checkName("int_bridge", s.IntBridge); err != nil {
		return err
	}
	if w := s.WAN; w != nil {
		if err := checkName("wan.ovs_bridge", w.OVSBridge); err != nil {
			return err
		}
		switch w.Mode {
		case ModeExisting:
		case ModeVeth:
			if err := checkNames(map[string]string{"wan.linux_bridge": w.LinuxBridge, "wan.host_end": w.HostEnd, "wan.ovs_end": w.OVSEnd}); err != nil {
				return err
			}
		case ModeDirect:
			if err := checkName("wan.nic", w.NIC); err != nil {
				return err
			}
		case ModeNAT:
			if err := checkNames(map[string]string{"wan.host_end": w.HostEnd, "wan.ovs_end": w.OVSEnd}); err != nil {
				return err
			}
			if _, err := net.ParseMAC(w.HostMAC); err != nil {
				return fmt.Errorf("wan.host_mac: %w", err)
			}
			if _, err := netip.ParsePrefix(w.HostCIDR); err != nil {
				return fmt.Errorf("wan.host_cidr: %w", err)
			}
		default:
			return fmt.Errorf("wan.mode: unknown mode %q", w.Mode)
		}
	}
	if m := s.Mgmt; m != nil {
		if err := checkName("mgmt.bridge", m.Bridge); err != nil {
			return err
		}
		if m.CIDR != "" {
			if _, err := netip.ParsePrefix(m.CIDR); err != nil {
				return fmt.Errorf("mgmt.cidr: %w", err)
			}
		}
		if m.NIC != "" {
			if err := checkName("mgmt.nic", m.NIC); err != nil {
				return err
			}
		}
	}
	if r := s.EncapRoute; r != nil {
		if _, err := netip.ParsePrefix(r.Dst); err != nil {
			return fmt.Errorf("encap_route.dst: %w", err)
		}
		if err := checkName("encap_route.dev", r.Dev); err != nil {
			return err
		}
		if _, err := netip.ParseAddr(r.Src); err != nil {
			return fmt.Errorf("encap_route.src: %w", err)
		}
		if r.Metric < 0 {
			return fmt.Errorf("encap_route.metric: %d is negative", r.Metric)
		}
	}
	return nil
}

func checkNames(fields map[string]string) error {
	for field, name := range fields {
		if err := checkName(field, name); err != nil {
			return err
		}
	}
	return nil
}

// checkName applies the kernel's interface name rules (IFNAMSIZ, no '/' or
// whitespace), so a bad name is reported against its field, not by ip.
func checkName(field, name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%s: missing", field)
	case len(name) > 15:
		return fmt.Errorf("%s: %q is longer than 15 characters", field, name)
	case name == "." || name == ".." || strings.ContainsAny(name, "/: \t\n"):
		return fmt.Errorf("%s: %q is not a valid interface name", field, name)
	}
	return nil
}

// hasNetworkdFiles reports whether setup-ovn.sh has written systemd-networkd
// files here, which every node set up before the backend record existed has.
func hasNetworkdFiles(dir string) (bool, error) {
	for _, pattern := range []string{"*-spinifex-*.network", "*-spinifex-*.netdev"} {
		matches, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			return false, err
		}
		if len(matches) > 0 {
			return true, nil
		}
	}
	return false, nil
}
