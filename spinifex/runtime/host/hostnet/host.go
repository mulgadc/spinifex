package hostnet

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// Link is the part of a kernel link Apply compares against the state file.
type Link struct {
	Name string
	// Kind is the rtnetlink link kind ("veth", "bridge", "openvswitch"), or
	// "" for a physical device.
	Kind string
	// Peer is the other end of a veth in the same namespace.
	Peer   string
	Master string
	MAC    string
	Up     bool
}

// RouteEntry is one IPv4 route to a destination.
type RouteEntry struct {
	Dev    string
	Src    string
	Metric int
}

// Host reads and changes kernel network state. It has no way to delete a
// link: Apply only creates and adjusts, so a link it does not name, or one it
// wrongly matched, is never destroyed.
type Host interface {
	// Link returns nil, nil when no link has this name.
	Link(ctx context.Context, name string) (*Link, error)
	AddVethPair(ctx context.Context, name, peer string) error
	SetUp(ctx context.Context, name string) error
	SetMaster(ctx context.Context, name, master string) error
	SetMAC(ctx context.Context, name, mac string) error
	// IPv4Addrs returns the link's IPv4 addresses as CIDR prefixes.
	IPv4Addrs(ctx context.Context, name string) ([]string, error)
	AddAddr(ctx context.Context, name, cidr string) error
	DelAddr(ctx context.Context, name, cidr string) error
	Routes(ctx context.Context, dst string) ([]RouteEntry, error)
	ReplaceRoute(ctx context.Context, r Route) error
}

// Runner runs a host command and returns its stdout.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// IPHost is the Host backed by iproute2's JSON output.
type IPHost struct {
	Runner Runner
}

var _ Host = (*IPHost)(nil)

// NewIPHost returns an IPHost that runs ip directly; Apply runs as root.
func NewIPHost() *IPHost { return &IPHost{Runner: execRunner{}} }

func (h *IPHost) ip(ctx context.Context, args ...string) ([]byte, error) {
	return h.Runner.Run(ctx, "ip", args...)
}

type ipLink struct {
	IfName   string   `json:"ifname"`
	Link     string   `json:"link"`
	Master   string   `json:"master"`
	Address  string   `json:"address"`
	Flags    []string `json:"flags"`
	LinkInfo struct {
		InfoKind string `json:"info_kind"`
	} `json:"linkinfo"`
}

// Link lists every link rather than asking for one by name, so a missing link
// is told apart from a failed ip by the output, not by matching error text.
func (h *IPHost) Link(ctx context.Context, name string) (*Link, error) {
	out, err := h.ip(ctx, "-j", "-d", "link", "show")
	if err != nil {
		return nil, err
	}
	var links []ipLink
	if err := json.Unmarshal(out, &links); err != nil {
		return nil, fmt.Errorf("parse ip link output: %w", err)
	}
	for _, l := range links {
		if l.IfName != name {
			continue
		}
		return &Link{
			Name:   l.IfName,
			Kind:   l.LinkInfo.InfoKind,
			Peer:   l.Link,
			Master: l.Master,
			MAC:    strings.ToLower(l.Address),
			Up:     slices.Contains(l.Flags, "UP"),
		}, nil
	}
	return nil, nil
}

func (h *IPHost) AddVethPair(ctx context.Context, name, peer string) error {
	_, err := h.ip(ctx, "link", "add", name, "type", "veth", "peer", "name", peer)
	return err
}

func (h *IPHost) SetUp(ctx context.Context, name string) error {
	_, err := h.ip(ctx, "link", "set", "dev", name, "up")
	return err
}

func (h *IPHost) SetMaster(ctx context.Context, name, master string) error {
	_, err := h.ip(ctx, "link", "set", "dev", name, "master", master)
	return err
}

func (h *IPHost) SetMAC(ctx context.Context, name, mac string) error {
	_, err := h.ip(ctx, "link", "set", "dev", name, "address", mac)
	return err
}

func (h *IPHost) IPv4Addrs(ctx context.Context, name string) ([]string, error) {
	out, err := h.ip(ctx, "-j", "-4", "addr", "show", "dev", name)
	if err != nil {
		return nil, err
	}
	var links []struct {
		AddrInfo []struct {
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal(out, &links); err != nil {
		return nil, fmt.Errorf("parse ip addr output for %s: %w", name, err)
	}
	var addrs []string
	for _, l := range links {
		for _, a := range l.AddrInfo {
			addrs = append(addrs, a.Local+"/"+strconv.Itoa(a.PrefixLen))
		}
	}
	return addrs, nil
}

func (h *IPHost) AddAddr(ctx context.Context, name, cidr string) error {
	_, err := h.ip(ctx, "addr", "add", cidr, "dev", name)
	return err
}

func (h *IPHost) DelAddr(ctx context.Context, name, cidr string) error {
	_, err := h.ip(ctx, "addr", "del", cidr, "dev", name)
	return err
}

func (h *IPHost) Routes(ctx context.Context, dst string) ([]RouteEntry, error) {
	out, err := h.ip(ctx, "-j", "-4", "route", "show", "exact", dst)
	if err != nil {
		return nil, err
	}
	var routes []struct {
		Dev      string `json:"dev"`
		PrefSrc  string `json:"prefsrc"`
		Metric   int    `json:"metric"`
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal(out, &routes); err != nil {
		return nil, fmt.Errorf("parse ip route output for %s: %w", dst, err)
	}
	entries := make([]RouteEntry, 0, len(routes))
	for _, r := range routes {
		entries = append(entries, RouteEntry{Dev: r.Dev, Src: r.PrefSrc, Metric: r.Metric})
	}
	return entries, nil
}

func (h *IPHost) ReplaceRoute(ctx context.Context, r Route) error {
	_, err := h.ip(ctx, "route", "replace", r.Dst, "dev", r.Dev, "src", r.Src, "metric", strconv.Itoa(r.Metric))
	return err
}

// ovnRemote returns the node's external_ids:ovn-remote, or "" when it is unset
// or OVS is not installed.
func ovnRemote(ctx context.Context, r Runner) (string, error) {
	if !onPath("ovs-vsctl") {
		return "", nil
	}
	out, err := r.Run(ctx, "ovs-vsctl", "--timeout=5", "--if-exists", "get", "Open_vSwitch", ".", "external_ids:ovn-remote")
	if err != nil {
		return "", fmt.Errorf("read external_ids:ovn-remote: %w", err)
	}
	return strings.Trim(strings.TrimSpace(string(out)), `"`), nil
}

func onPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
