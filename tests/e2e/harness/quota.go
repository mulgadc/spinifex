//go:build e2e

package harness

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	handlers_quota "github.com/mulgadc/spinifex/spinifex/handlers/quota"
)

// QuotaLimits is the [quota] baseline a suite installs on the cluster. Every
// field is written, so a zero value denies that dimension outright rather than
// inheriting anything — pass a complete set.
type QuotaLimits struct {
	VCPUs         int
	VPCs          int
	Subnets       int
	EIPs          int
	Volumes       int
	VolumesGiB    int
	RDSInstances  int
	LoadBalancers int
}

// SandboxQuotaLimits mirrors the baseline shipped in the awsgw.toml template,
// which is what a self-service account gets in production. A suite asserting
// against these numbers is asserting against what a real tenant sees.
func SandboxQuotaLimits() QuotaLimits {
	return QuotaLimits{
		VCPUs: 16, VPCs: 4, Subnets: 16, EIPs: 4,
		Volumes: 16, VolumesGiB: 200, RDSInstances: 2, LoadBalancers: 2,
	}
}

// QuotaConfigPath returns the awsgw config file holding the [quota] block.
func QuotaConfigPath(env *Env) string {
	return filepath.Join(env.ConfigDir, "awsgw", "awsgw.toml")
}

// awsgwUnit is the systemd unit read the [quota] block at startup.
const awsgwUnit = "spinifex-awsgw"

// EnableQuota turns per-account quotas on across the cluster and restarts every
// gateway so the new block takes effect. The returned function puts the
// original config back and restarts again — a suite that leaves quotas on caps
// every suite that follows it on the same VM, so it must always run.
//
// Restore reports through an error rather than the *testing.T: it belongs in
// TestMain, after every test has finished, where logging to a t would panic.
func EnableQuota(t *testing.T, env *Env, limits QuotaLimits) func() error {
	t.Helper()
	if env.ConfigDir == "" {
		t.Fatalf("EnableQuota: no config dir located; set SPINIFEX_CONFIG_DIR")
	}
	path := QuotaConfigPath(env)
	hosts := quotaHosts(env)

	saved := make(map[string]string, len(hosts))
	for _, h := range hosts {
		body, err := readNodeFile(h, path)
		if err != nil {
			t.Fatalf("EnableQuota: read %s on %s: %v", path, hostLabel(h), err)
		}
		saved[h] = body
	}

	for _, h := range hosts {
		if err := writeNodeFile(h, path, withQuotaBlock(saved[h], limits)); err != nil {
			t.Fatalf("EnableQuota: write %s on %s: %v", path, hostLabel(h), err)
		}
		if err := restartGateway(env, h); err != nil {
			t.Fatalf("EnableQuota: restart gateway on %s: %v", hostLabel(h), err)
		}
	}

	return func() error {
		// Every host is attempted even after one fails: a cluster left with the
		// suite's quota block on some nodes and not others is worse than either.
		var failures []string
		for _, h := range hosts {
			if err := writeNodeFile(h, path, saved[h]); err != nil {
				failures = append(failures, fmt.Sprintf("write %s on %s: %v", path, hostLabel(h), err))
				continue
			}
			if err := restartGateway(env, h); err != nil {
				failures = append(failures, fmt.Sprintf("restart gateway on %s: %v", hostLabel(h), err))
			}
		}
		if len(failures) > 0 {
			return fmt.Errorf("restore quota config: %s", strings.Join(failures, "; "))
		}
		return nil
	}
}

// quotaHosts returns the hosts whose gateway config must be patched. The empty
// string means this machine, which is where the single-node suites run.
func quotaHosts(env *Env) []string {
	if env.Mode != ModeMultinode {
		return []string{""}
	}
	cluster, err := ClusterFromEnv()
	if err != nil {
		return []string{""}
	}
	hosts := make([]string, 0, len(cluster.Nodes))
	for _, n := range cluster.Nodes {
		hosts = append(hosts, n.Addr)
	}
	return hosts
}

func hostLabel(host string) string {
	if host == "" {
		return "localhost"
	}
	return host
}

// runNodeShell runs script on host, or on this machine when host is empty.
func runNodeShell(host, script string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if host != "" {
		return NewPeerSSH().Run(ctx, host, script)
	}
	out, err := exec.CommandContext(ctx, "bash", "-c", script).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("local shell: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func readNodeFile(host, path string) (string, error) {
	out, err := runNodeShell(host, "sudo -n cat -- "+ShellQuote(path))
	return string(out), err
}

// writeNodeFile replaces path's contents. The body travels base64-encoded so a
// single command string carries it intact over SSH, quoting and newlines alike.
func writeNodeFile(host, path, body string) error {
	script := fmt.Sprintf("printf %%s %s | base64 -d | sudo -n tee -- %s >/dev/null",
		ShellQuote(base64.StdEncoding.EncodeToString([]byte(body))), ShellQuote(path))
	_, err := runNodeShell(host, script)
	return err
}

func restartGateway(env *Env, host string) error {
	if _, err := runNodeShell(host, "sudo -n systemctl restart "+awsgwUnit); err != nil {
		return err
	}
	addr := host
	if addr == "" {
		addr = gatewayDialHost(env)
	}
	return waitGatewayListening(addr, env.AWSGWPort, 90*time.Second)
}

// gatewayDialHost picks an address the local gateway answers on. ServiceIPs[0]
// is the awsgw bind IP, so it is correct even where the listener is not on
// loopback.
func gatewayDialHost(env *Env) string {
	if len(env.ServiceIPs) > 0 && env.ServiceIPs[0] != "" {
		return env.ServiceIPs[0]
	}
	return "127.0.0.1"
}

// waitGatewayListening polls until the gateway completes a TLS handshake. The
// certificate is not checked: this is a readiness probe, and the cert suite is
// what asserts the chain.
func waitGatewayListening(host string, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	target := net.JoinHostPort(host, strconv.Itoa(port))
	var last error
	for time.Now().Before(deadline) {
		conn, err := tls.DialWithDialer(dialer, "tcp", target,
			&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // readiness probe, not a trust decision
		if err == nil {
			_ = conn.Close()
			return nil
		}
		last = err
		time.Sleep(time.Second)
	}
	return fmt.Errorf("gateway %s did not accept connections within %s: %w", target, timeout, last)
}

// withQuotaBlock returns cfg with its [quota] table replaced by limits. The old
// table is dropped whole — editing keys in place would leave a stale key behind
// whenever the block being replaced carries one the new block does not.
func withQuotaBlock(cfg string, limits QuotaLimits) string {
	quotaTable := regexp.MustCompile(`^\s*\[quota(\.[^\]]+)?\]\s*$`)

	var kept []string
	skipping := false
	for _, line := range strings.Split(cfg, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			skipping = quotaTable.MatchString(line)
		}
		if !skipping {
			kept = append(kept, line)
		}
	}

	return strings.TrimRight(strings.Join(kept, "\n"), "\n") + "\n\n" + fmt.Sprintf(
		`[quota]
enabled = true
vcpus = %d
vpcs = %d
subnets = %d
eips = %d
volumes = %d
volumes_gib = %d
rds_instances = %d
load_balancers = %d
`,
		limits.VCPUs, limits.VPCs, limits.Subnets, limits.EIPs,
		limits.Volumes, limits.VolumesGiB, limits.RDSInstances, limits.LoadBalancers)
}

// spxAdminEnv adds the AWS profile the /admin surface signs with. The quota
// subcommands reach the gateway over SigV4 rather than NATS, so they need a
// credential where the account commands need none.
func spxAdminEnv() []string {
	profile := os.Getenv("AWS_PROFILE")
	if profile == "" {
		profile = "spinifex"
	}
	return append(spxChildEnv(), "AWS_PROFILE="+profile)
}

// SpxAdminQuotaSet runs `spx admin account quota set` for one account. args are
// the dimension flags, e.g. "--vcpus", "8".
func SpxAdminQuotaSet(t *testing.T, accountID string, args ...string) string {
	t.Helper()
	return spxAdminQuota(t, append([]string{"set", accountID}, args...)...)
}

// SpxAdminQuotaClear drops every override, returning the account to the
// configured baseline.
func SpxAdminQuotaClear(t *testing.T, accountID string) string {
	t.Helper()
	return spxAdminQuota(t, "set", accountID, "--clear")
}

// ClearAccountQuota is SpxAdminQuotaClear for teardown that outlives the test
// that registered it, where reporting through a *testing.T would panic.
func ClearAccountQuota(accountID string) error {
	_, err := runSpxAdminQuota("set", accountID, "--clear")
	return err
}

// quotaRow matches one line of the table `spx admin account quota get` prints:
// dimension, limit, and the layer the limit came from.
var quotaRow = regexp.MustCompile(`^([a-z_]+)\s+(\S+)\s+(config|override)\s*$`)

// SpxAdminQuotaGet returns the limits in force for an account and, per
// dimension, whether each was inherited from the config or set as an override.
func SpxAdminQuotaGet(t *testing.T, accountID string) (limits map[string]int, source map[string]string) {
	t.Helper()
	out := spxAdminQuota(t, "get", accountID)

	limits, source = map[string]int{}, map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		m := quotaRow.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		value := handlers_quota.Unlimited
		if m[2] != "unlimited" {
			parsed, err := strconv.Atoi(m[2])
			if err != nil {
				t.Fatalf("quota get %s: unparseable limit %q for %s", accountID, m[2], m[1])
			}
			value = parsed
		}
		limits[m[1]], source[m[1]] = value, m[3]
	}
	if len(limits) == 0 {
		t.Fatalf("quota get %s: no dimensions parsed from:\n%s", accountID, out)
	}
	return limits, source
}

func spxAdminQuota(t *testing.T, args ...string) string {
	t.Helper()
	out, err := runSpxAdminQuota(args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}

func runSpxAdminQuota(args ...string) (string, error) {
	full := append([]string{"admin", "account", "quota"}, args...)
	cmd := exec.Command(SpxBin(), full...)
	cmd.Env = spxAdminEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("spx %s failed: %w\noutput:\n%s",
			strings.Join(full, " "), err, out)
	}
	return string(out), nil
}
