//go:build e2e

// Package harness provides shared E2E test primitives for the Spinifex scenario
// suites (cert, lb, multinode, baremetal, reboot, tofu). Each scenario package
// imports this harness; bash drivers under spinifex/tests/e2e/ are being
// retired in favour of these Go scenarios — see
// docs/development/improvements/e2e-go-harness.md.
package harness

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type Mode string

const (
	ModeSingle    Mode = "single"
	ModePseudo    Mode = "pseudo"
	ModeMultinode Mode = "multinode"
	ModeBaremetal Mode = "baremetal"
)

type Env struct {
	Mode    Mode
	NodeIPs []string
	// WANHost is the externally-reachable host used by runner-resident
	// scenarios (e.g. the reboot suite, which executes on the GitHub Actions
	// runner because systemctl-reboot kills any in-VM process). Distinct from
	// ServiceIPs[0]: ServiceIPs[0] is the awsgw bind IP and may be loopback
	// when the test runs in-VM, whereas WANHost must always be the SSH/HTTP
	// target IP reachable from outside the cluster.
	WANHost        string
	ServiceIPs     []string
	ConfigDir      string
	AWSGWPort      int
	UIPort         int
	ArtifactDir    string
	DefaultTimeout time.Duration
	DefaultPoll    time.Duration
}

func LoadEnv(t *testing.T) *Env {
	t.Helper()
	if os.Getenv("SPINIFEX_E2E") == "" {
		t.Skip("SPINIFEX_E2E not set; skipping E2E scenario")
	}

	mode := Mode(getenv("SPINIFEX_MODE", string(ModeSingle)))

	configDir := getenv("SPINIFEX_CONFIG_DIR", "")
	if configDir == "" {
		for _, c := range []string{"/etc/spinifex", os.ExpandEnv("$HOME/spinifex/config")} {
			if stat, err := os.Stat(c); err == nil && stat.IsDir() {
				configDir = c
				break
			}
		}
	}

	nodeIPs := splitCSV(os.Getenv("SPINIFEX_NODE_IPS"))
	if len(nodeIPs) == 0 {
		// Single-node parity with run-cert-e2e.sh: loopback + every
		// non-loopback global IPv4 so SAN checks see all addresses the
		// cert is expected to cover.
		if mode == ModeSingle {
			nodeIPs = discoverSingleNodeIPs()
		} else {
			nodeIPs = []string{"127.0.0.1"}
		}
	}
	serviceIPs := splitCSV(os.Getenv("SPINIFEX_SERVICE_IPS"))
	if len(serviceIPs) == 0 {
		// awsgw.host in spinifex.toml is the only IP the gateway actually
		// listens on; cert TLS checks must hit that IP, not the loopback.
		if bind := awsgwBindIP(configDir); bind != "" && bind != "0.0.0.0" {
			serviceIPs = []string{bind}
		} else {
			serviceIPs = nodeIPs
		}
	}

	wanHost := getenv("SPINIFEX_WAN_IP", "")
	if wanHost == "" && len(nodeIPs) > 0 {
		wanHost = nodeIPs[0]
	}

	return &Env{
		Mode:           mode,
		NodeIPs:        nodeIPs,
		WANHost:        wanHost,
		ServiceIPs:     serviceIPs,
		ConfigDir:      configDir,
		AWSGWPort:      atoiOr("SPINIFEX_AWSGW_PORT", 9999),
		UIPort:         atoiOr("SPINIFEX_UI_PORT", 3000),
		ArtifactDir:    getenv("ARTIFACT_DIR", "/tmp/spinifex-e2e-artifacts"),
		DefaultTimeout: durationOr("SPINIFEX_DEFAULT_TIMEOUT", 30*time.Second),
		DefaultPoll:    durationOr("SPINIFEX_DEFAULT_POLL", 500*time.Millisecond),
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func atoiOr(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func durationOr(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func discoverSingleNodeIPs() []string {
	ips := []string{"127.0.0.1"}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipNet.IP.To4()
		if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
			continue
		}
		ips = append(ips, ip4.String())
	}
	return ips
}

var awsgwHostLine = regexp.MustCompile(`(?i)^\s*host\s*=\s*"([^":]+)`)

func awsgwBindIP(configDir string) string {
	if configDir == "" {
		return ""
	}
	f, err := os.Open(filepath.Join(configDir, "spinifex.toml"))
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	inAwsgw := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "[") {
			inAwsgw = strings.Contains(line, ".awsgw]") || line == "[awsgw]"
			continue
		}
		if !inAwsgw {
			continue
		}
		if m := awsgwHostLine.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}
