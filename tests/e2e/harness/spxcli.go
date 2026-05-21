//go:build e2e

package harness

import (
	"bytes"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// AccountInfo is the parsed result of `spx admin account create`. Fields
// match the lines emitted by cmd/spinifex/cmd/admin.go:runAccountCreate.
type AccountInfo struct {
	AccountID       string
	AccountName     string
	AccessKeyID     string
	SecretAccessKey string
	Profile         string
}

// SpxBin returns the spx binary path (SPX_BIN env var or "spx" on PATH).
func SpxBin() string {
	if v := os.Getenv("SPX_BIN"); v != "" {
		return v
	}
	return "spx"
}

// spxChildEnv returns a sanitized environment for `spx` child processes.
// spx CLI calls viper.AutomaticEnv() with prefix "SPINIFEX" inside
// config.LoadConfig (spinifex/spinifex/config/config.go:212), so any
// SPINIFEX_* env var the harness sets to wire up its own ClusterFromEnv /
// Env loaders silently overrides the matching cluster.toml field — most
// notably SPINIFEX_NODES (csv of IPs) clobbers the `nodes` map and leaves
// NATS.Host empty, surfacing as `nats: no servers available for
// connection` even though the cluster is healthy (mulga-siv-90 run
// 26195522455). Strip every SPINIFEX_* except the small set the spx root
// command actually binds via BindEnv (kept for parity with operator
// muscle-memory).
func spxChildEnv() []string {
	keep := map[string]struct{}{
		"SPINIFEX_CONFIG_PATH":  {},
		"SPINIFEX_ACCESS_KEY":   {},
		"SPINIFEX_SECRET_KEY":   {},
		"SPINIFEX_HOST":         {},
		"SPINIFEX_BASE_DIR":     {},
		"SPINIFEX_NATS_HOST":    {},
		"SPINIFEX_NATS_TOKEN":   {},
		"SPINIFEX_NATS_SUBJECT": {},
	}
	out := os.Environ()[:0]
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			out = append(out, kv)
			continue
		}
		k := kv[:eq]
		if strings.HasPrefix(k, "SPINIFEX_") {
			if _, ok := keep[k]; !ok {
				continue
			}
		}
		out = append(out, kv)
	}
	return out
}

// SpxRun runs `spx <args...>` and returns combined stdout+stderr. If wantErr
// is false the test fails on non-zero exit; if true the call returns the
// output and the test continues (caller asserts on output content).
func SpxRun(t *testing.T, wantErr bool, args ...string) string {
	t.Helper()
	var buf bytes.Buffer
	cmd := exec.Command(SpxBin(), args...)
	cmd.Env = spxChildEnv()
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := buf.String()
	if err != nil && !wantErr {
		t.Fatalf("spx %s failed: %v\noutput:\n%s", strings.Join(args, " "), err, out)
	}
	if err == nil && wantErr {
		t.Fatalf("spx %s expected non-zero exit, got success\noutput:\n%s", strings.Join(args, " "), out)
	}
	return out
}

// SpxGetNodes runs `spx get nodes`.
func SpxGetNodes(t *testing.T) string {
	t.Helper()
	return SpxRun(t, false, "get", "nodes")
}

// SpxGetVMs runs `spx get vms`.
func SpxGetVMs(t *testing.T) string {
	t.Helper()
	return SpxRun(t, false, "get", "vms")
}

// SpxRunBestEffort runs `spx <args...>` and returns combined output ignoring
// the exit code. Matches the bash `2>/dev/null` + no exit-check pattern used
// by `spx get vms` in run-multinode-e2e.sh phases 2-3 — the CLI's NATS dial
// can race the cluster join shortly after bootstrap without indicating a
// data-path fault, so a transient non-zero exit shouldn't fail the suite.
func SpxRunBestEffort(t *testing.T, args ...string) string {
	t.Helper()
	var buf bytes.Buffer
	cmd := exec.Command(SpxBin(), args...)
	cmd.Env = spxChildEnv()
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	_ = cmd.Run()
	return buf.String()
}

// SpxTopNodes runs `spx top nodes`.
func SpxTopNodes(t *testing.T) string {
	t.Helper()
	return SpxRun(t, false, "top", "nodes")
}

// admin account create output is a fixed two-column key/value list; pin each
// field to a line anchor so a layout change (added/reordered keys) surfaces
// loudly instead of silently matching the wrong value.
var (
	spxAcctRE   = regexp.MustCompile(`(?m)^\s+Account ID:\s+(\S+)`)
	spxNameRE   = regexp.MustCompile(`(?m)^\s+Account Name:\s+(.+?)\s*$`)
	spxKeyIDRE  = regexp.MustCompile(`(?m)^\s+Access Key ID:\s+(\S+)`)
	spxSecretRE = regexp.MustCompile(`(?m)^\s+Secret Access Key:\s+(\S+)`)
	spxProfRE   = regexp.MustCompile(`(?m)^\s+AWS Profile:\s+(\S+)`)
)

// SpxAdminAccountCreate creates a tenant account via the spx CLI and parses
// the printed credentials block. email is forwarded as --email when non-empty;
// `spx admin account create` ignores unknown flags today, but the CLI may
// adopt --email in future and any caller that wants to gate on it (e.g. IAM
// e-mail uniqueness checks) should pass it through unchanged.
func SpxAdminAccountCreate(t *testing.T, name, email string) AccountInfo {
	t.Helper()
	args := []string{"admin", "account", "create", "--name", name}
	if email != "" {
		args = append(args, "--email", email)
	}
	out := SpxRun(t, false, args...)

	info := AccountInfo{
		AccountID:       firstSubmatch(spxAcctRE, out),
		AccountName:     firstSubmatch(spxNameRE, out),
		AccessKeyID:     firstSubmatch(spxKeyIDRE, out),
		SecretAccessKey: firstSubmatch(spxSecretRE, out),
		Profile:         firstSubmatch(spxProfRE, out),
	}
	if info.AccountID == "" || info.AccessKeyID == "" || info.SecretAccessKey == "" {
		// Dump raw output to the artifact dir if available — otherwise the
		// formatted log block lives only in the JUnit XML.
		t.Fatalf("spx admin account create: failed to parse credentials block\n--- raw output ---\n%s\n--- end ---", out)
	}
	return info
}

func firstSubmatch(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}
