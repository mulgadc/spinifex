package nodefacts

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
)

// Env is where Build reads from. Tests point the roots at fixtures.
type Env struct {
	Dir      string
	SysRoot  string
	ProcRoot string
	EtcRoot  string
	Node     string
	Version  string
	Commit   string
	// LiveLLDP reads neighbours now, so a re-cabled port shows without a
	// restart. Nil or an error falls back to what Collect saved.
	LiveLLDP func(context.Context) ([]byte, error)
	// Addrs returns an interface's IPv4 addresses with their prefix lengths.
	Addrs func(iface string) []net.IPNet
}

// DefaultEnv reads the live host.
func DefaultEnv(node, version, commit string) Env {
	return Env{
		Dir: DefaultDir, SysRoot: "/sys", ProcRoot: "/proc", EtcRoot: "/etc",
		Node: node, Version: version, Commit: commit,
		LiveLLDP: liveLLDP, Addrs: interfaceAddrs,
	}
}

func liveLLDP(ctx context.Context) ([]byte, error) {
	ctl := findTool(networkctlPaths)
	if ctl == "" {
		return nil, errNoFile
	}
	return run(ctx, ctl, "lldp", "--json=short")
}

func interfaceAddrs(name string) []net.IPNet {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	var out []net.IPNet
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			out = append(out, *ipn)
		}
	}
	return out
}

// Source states reported in NodeFacts.Sources.
const (
	sourceOK          = "ok"
	sourceUnavailable = "unavailable"
)

// Build assembles the facts from the collected probes and live sysfs.
func Build(ctx context.Context, env Env) clusterv1.NodeFacts {
	f := clusterv1.NodeFacts{Node: env.Node, Sources: map[string]string{}}
	if data, err := readFacts(env.Dir, fileCollected); err == nil {
		f.CollectedAt = strings.TrimSpace(string(data))
	}

	if data, err := readFacts(env.Dir, fileIdentity); err == nil {
		f.System = parseIdentity(string(data))
		f.Sources["dmi"] = sourceOK
	} else {
		f.Sources["dmi"] = sourceUnavailable
	}
	if data, err := os.ReadFile(filepath.Join(env.ProcRoot, "device-tree", "model")); err == nil {
		f.System.DeviceModel = strings.TrimRight(string(data), "\x00\n ")
	}
	if data, err := readFacts(env.Dir, fileVirt); err == nil {
		f.Virt = strings.TrimSpace(string(data))
	}
	f.FormFactor = formFactor(f.Virt, f.System)

	if data, err := readFacts(env.Dir, fileProcessor); err == nil {
		f.CPU = parseProcessors(string(data))
	}
	addCPUInfo(&f.CPU, env.ProcRoot)

	if data, err := readFacts(env.Dir, fileMemory); err == nil {
		f.Memory = parseMemory(string(data))
	}
	addMeminfo(&f.Memory, env.ProcRoot)

	f.OS = osFacts(env)

	neighbours, lldpState := lldpNeighbours(ctx, env)
	f.Sources["lldp"] = lldpState
	f.NICs = nicFacts(env, neighbours)
	f.Planes = planeFacts(env, f.NICs)
	for i := range f.NICs {
		for _, p := range f.Planes {
			if slices.Contains(p.NICs, f.NICs[i].Name) {
				f.NICs[i].Planes = append(f.NICs[i].Planes, p.Plane)
			}
		}
	}
	return f
}

// formFactor decides the picture the console draws. A hypervisor wins over
// whatever chassis the virtual firmware claims.
func formFactor(virt string, s clusterv1.SystemFacts) string {
	model := strings.ToLower(s.DeviceModel)
	switch {
	case virt != "" && virt != "none":
		return "virtual-machine"
	case strings.Contains(model, "raspberry pi"):
		return "sbc"
	case strings.Contains(model, "jetson"):
		return "jetson"
	}
	switch strings.ToLower(s.ChassisType) {
	case "rack mount chassis", "blade", "blade enclosure", "multi-system", "main server chassis":
		return "rack-server"
	case "tower", "mini tower", "desktop", "low profile desktop", "space-saving", "all in one":
		return "tower"
	case "mini pc", "compact pci", "embedded pc", "sub chassis", "stick pc":
		return "sbc"
	}
	if model != "" {
		return "sbc"
	}
	return "unknown"
}

// addCPUInfo fills what DMI cannot tell a VM, and the features worth naming.
func addCPUInfo(c *clusterv1.CPUFacts, procRoot string) {
	data, err := os.ReadFile(filepath.Join(procRoot, "cpuinfo"))
	if err != nil {
		return
	}
	var (
		model   string
		flags   []string
		threads int
		socket  string
		sockets = map[string]bool{}
		cores   = map[string]bool{}
	)
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "processor":
			threads++
		case "model name":
			model = value
		case "physical id":
			socket = value
			sockets[value] = true
		case "core id":
			cores[socket+"/"+value] = true
		case "flags", "Features": // ARM names the same list Features
			if flags == nil {
				flags = strings.Fields(value)
			}
		}
	}
	if model != "" {
		c.Model = strings.Join(strings.Fields(model), " ")
	}
	if c.Threads == 0 {
		c.Threads = threads
	}
	if c.Sockets == 0 {
		c.Sockets = max(len(sockets), 1)
	}
	if c.Cores == 0 {
		c.Cores = cmp.Or(len(cores), threads)
	}
	switch {
	case slices.Contains(flags, "svm"):
		c.Virtualization = "AMD-V"
	case slices.Contains(flags, "vmx"):
		c.Virtualization = "VT-x"
	}
	c.Flags = flags
	for _, want := range []string{"avx512f", "avx2", "aes", "sev", "sme"} {
		if slices.Contains(flags, want) {
			c.Features = append(c.Features, want)
		}
	}
	if runtime.GOARCH == "arm64" && c.Model == "" {
		c.Model = "ARM64"
	}
}

func addMeminfo(m *clusterv1.MemoryFacts, procRoot string) {
	data, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		kb, err := strconv.ParseUint(strings.Fields(value + " 0")[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			m.OSTotalBytes = kb << 10
		case "MemAvailable":
			m.OSAvailBytes = kb << 10
		case "Hugetlb":
			m.HugePageBytes = kb << 10
		}
	}
}

func osFacts(env Env) clusterv1.OSFacts {
	o := clusterv1.OSFacts{Arch: runtime.GOARCH, Version: env.Version, Commit: env.Commit}
	if data, err := os.ReadFile(filepath.Join(env.EtcRoot, "os-release")); err == nil {
		for line := range strings.SplitSeq(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
				o.Name = strings.Trim(v, `"`)
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(env.ProcRoot, "sys", "kernel", "osrelease")); err == nil {
		o.Kernel = strings.TrimSpace(string(data))
	}
	if data, err := os.ReadFile(filepath.Join(env.ProcRoot, "uptime")); err == nil {
		if fields := strings.Fields(string(data)); len(fields) > 0 {
			secs, _ := strconv.ParseFloat(fields[0], 64)
			o.UptimeSeconds = int64(secs)
		}
	}
	if data, err := os.ReadFile(filepath.Join(env.ProcRoot, "sys", "kernel", "hostname")); err == nil {
		o.Hostname = strings.TrimSpace(string(data))
	}
	return o
}

// lldpNeighbours prefers a live read and falls back to the start-up probe.
func lldpNeighbours(ctx context.Context, env Env) (map[string]clusterv1.LLDPNeighbor, string) {
	if env.LiveLLDP != nil {
		if data, err := env.LiveLLDP(ctx); err == nil {
			if n, err := parseLLDP(data); err == nil {
				return n, sourceOK
			}
		}
	}
	data, err := readFacts(env.Dir, fileLLDP)
	if err != nil {
		return nil, sourceUnavailable
	}
	n, err := parseLLDP(data)
	if err != nil {
		return nil, sourceUnavailable
	}
	return n, sourceOK
}

func readSys(sysRoot, nic, name string) string {
	data, err := os.ReadFile(filepath.Join(sysRoot, "class", "net", nic, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func nicFacts(env Env, neighbours map[string]clusterv1.LLDPNeighbor) []clusterv1.NICFacts {
	var out []clusterv1.NICFacts
	for _, name := range physicalNICs(env.SysRoot) {
		n := clusterv1.NICFacts{
			Name:  name,
			MAC:   readSys(env.SysRoot, name, "address"),
			State: readSys(env.SysRoot, name, "operstate"),
		}
		n.MTU, _ = strconv.Atoi(readSys(env.SysRoot, name, "mtu"))
		if speed, err := strconv.Atoi(readSys(env.SysRoot, name, "speed")); err == nil && speed > 0 {
			n.SpeedMbps = speed
		}
		if target, err := os.Readlink(filepath.Join(env.SysRoot, "class", "net", name, "master")); err == nil {
			n.Master = filepath.Base(target)
		}
		if data, err := readFacts(env.Dir, nicPrefix+name+".txt"); err == nil {
			e := parseEthtool(string(data))
			n.Driver, n.Firmware, n.BusInfo, n.Duplex, n.Port = e.driver, e.firmware, e.busInfo, e.duplex, e.port
			n.MaxMbps = e.maxMbps
			if n.SpeedMbps == 0 {
				n.SpeedMbps = e.speedMbps
			}
			n.Model = parseUdevModel(string(data))
			if br := ovsBridge(string(data)); br != "" && (n.Master == "" || n.Master == "ovs-system") {
				n.Master = br
			}
		}
		if n.Driver == "" {
			if target, err := os.Readlink(filepath.Join(env.SysRoot, "class", "net", name, "device", "driver")); err == nil {
				n.Driver = filepath.Base(target)
			}
		}
		if nb, ok := neighbours[name]; ok {
			n.Neighbor = &nb
		}
		out = append(out, n)
	}
	return out
}

func ovsBridge(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "OVS_BRIDGE="); ok {
			return v
		}
	}
	return ""
}

// planeFacts finds each plane's bridge and address. A host without br-vpc
// carries the overlay on whichever bridge holds the encap address.
func planeFacts(env Env, nics []clusterv1.NICFacts) []clusterv1.PlaneFacts {
	addrs := env.Addrs
	if addrs == nil {
		addrs = func(string) []net.IPNet { return nil }
	}
	var encap string
	if data, err := readFacts(env.Dir, fileEncapIP); err == nil {
		encap = strings.TrimSpace(string(data))
	}

	bridges := map[string]string{
		clusterv1.PlaneWAN: "br-wan",
		clusterv1.PlaneLAN: "br-lan",
		clusterv1.PlaneVPC: "br-vpc",
	}
	if encap != "" {
		if br := bridgeHolding(encap, addrs); br != "" {
			bridges[clusterv1.PlaneVPC] = br
		}
	}

	var out []clusterv1.PlaneFacts
	for _, plane := range []string{clusterv1.PlaneWAN, clusterv1.PlaneLAN, clusterv1.PlaneVPC} {
		br := bridges[plane]
		if _, err := os.Stat(filepath.Join(env.SysRoot, "class", "net", br)); err != nil {
			continue
		}
		p := clusterv1.PlaneFacts{Plane: plane, Bridge: br}
		p.MTU, _ = strconv.Atoi(readSys(env.SysRoot, br, "mtu"))
		list := addrs(br)
		for _, a := range list {
			if plane == clusterv1.PlaneVPC && encap != "" && a.IP.String() != encap {
				continue
			}
			ones, _ := a.Mask.Size()
			p.Address, p.Prefix = a.IP.String(), ones
			break
		}
		for _, n := range nics {
			if n.Master == br {
				p.NICs = append(p.NICs, n.Name)
			}
		}
		out = append(out, p)
	}
	return out
}

func bridgeHolding(ip string, addrs func(string) []net.IPNet) string {
	for _, br := range []string{"br-vpc", "br-lan", "br-wan"} {
		for _, a := range addrs(br) {
			if a.IP.String() == ip {
				return br
			}
		}
	}
	return ""
}
