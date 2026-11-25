//go:build e2e

package single

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/domains/dns"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

const (
	northstarUnit       = "spinifex-northstar.service"
	northstarConfigPath = "/etc/spinifex/northstar/northstar.toml"

	// silentForwarder never answers (TEST-NET-1); refusingForwarder is an
	// authoritative-only server that refuses recursion for names it does not own.
	silentForwarder   = "192.0.2.1:53"
	refusingForwarder = "216.239.32.10:53"

	// northstar's per-forwarder budget: a first lookup faster than this never
	// waited on the silent forwarder, so the fault was not exercised.
	silentWait = 1400 * time.Millisecond
)

var upstreamNameservers = regexp.MustCompile(`(?m)^nameservers\s*=\s*\[(.*)\]\s*$`)

// runUpstreamFailover puts a silent and a refusing forwarder ahead of the
// node's real one, then times guest lookups through the shim: the first must
// wait out the silent one and still answer inside a guest's 5 s attempt, and
// the second must skip both.
func runUpstreamFailover(t *testing.T, fix *Fixture, tgt harness.SSHTarget, privIP string) {
	harness.RequireDNSEnabled(t, fix.Env)
	require.NoError(t, restoreNorthstarConfig(), "restore a config left by an interrupted run")
	original := readNorthstarConfig(t)
	m := upstreamNameservers.FindStringSubmatch(original)
	require.NotNilf(t, m, "no [upstream] nameservers line in %s", northstarConfigPath)
	working := strings.TrimSpace(strings.Split(m[1], ",")[0])
	if working == "" {
		t.Skip("the node has no upstream forwarder to put behind the failing ones")
	}
	line := fmt.Sprintf("nameservers = [%q, %q, %s]", silentForwarder, refusingForwarder, working)
	harness.Detail(t, "upstream", line)

	require.NoError(t, sudoRun("cp", "-p", northstarConfigPath, northstarConfigPath+backupSuffix), "stage the original config")
	t.Cleanup(func() {
		if err := restoreNorthstarConfig(); err != nil {
			t.Errorf("restore %s: %v", northstarConfigPath, err)
		}
	})
	require.NoError(t, writeNorthstarConfig(strings.Replace(original, m[0], line, 1)))

	internalDomain := harness.NorthstarInternalDomain(fix.Env)
	region := aws.StringValue(fix.AWS.EC2Conf.Config.Region)
	privateName := dns.EC2PrivateName(privIP, region, internalDomain)
	harness.Step(t, "wait for northstar to answer %s after the restart", privateName)
	harness.EventuallyErr(t, func() error {
		_, err := sshCapture(tgt, "getent ahostsv4 "+privateName)
		return err
	}, 60*time.Second, 2*time.Second)

	harness.Step(t, "resolve example.org from the guest: waits out the silent forwarder, answers within 2 s")
	took, out := guestLookupTime(t, tgt, "example.org")
	harness.Detail(t, "name", "example.org", "took_ms", took.Milliseconds())
	require.GreaterOrEqualf(t, took, silentWait,
		"the first lookup took %s, under the silent forwarder's budget: %s was not silent here, so the fault was not exercised", took, silentForwarder)
	require.LessOrEqualf(t, took, 2*time.Second, "the first lookup took %s with a silent and a refusing forwarder first\n%s", took, out)

	harness.Step(t, "resolve example.net from the guest: both bad forwarders skipped, within 1 s")
	took, out = guestLookupTime(t, tgt, "example.net")
	harness.Detail(t, "name", "example.net", "took_ms", took.Milliseconds())
	require.LessOrEqualf(t, took, time.Second, "the second lookup took %s: the failing forwarders were not skipped\n%s", took, out)
}

const backupSuffix = ".e2e-orig"

// guestLookupTime times one lookup inside the guest, so SSH setup is not
// counted, and fails the test unless it returned an IPv4 address.
func guestLookupTime(t *testing.T, tgt harness.SSHTarget, name string) (time.Duration, string) {
	t.Helper()
	cmd := fmt.Sprintf(`s=$(date +%%s%%N); getent ahostsv4 %s; rc=$?; e=$(date +%%s%%N); echo "rc=$rc took_ns=$((e-s))"`, name)
	out, err := sshCapture(tgt, cmd)
	require.NoErrorf(t, err, "ssh to the guest\n%s", out)
	m := regexp.MustCompile(`rc=(\d+) took_ns=(\d+)\s*$`).FindStringSubmatch(out)
	require.NotNilf(t, m, "no timing from the guest\n%s", out)
	require.Equalf(t, "0", m[1], "getent %s failed through northstar\n%s", name, out)
	ns, err := strconv.ParseInt(m[2], 10, 64)
	require.NoError(t, err)
	require.Regexpf(t, `\d{1,3}(\.\d{1,3}){3}`, out, "%s returned no IPv4 address\n%s", name, out)
	return time.Duration(ns), out
}

// readNorthstarConfig returns the config; it holds credentials, so a failure
// reports only the exit status.
func readNorthstarConfig(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("sudo", "-n", "cat", northstarConfigPath).Output()
	require.NoErrorf(t, err, "read %s", northstarConfigPath)
	return string(out)
}

// writeNorthstarConfig writes a copy that keeps the original's owner and mode,
// renames it over the config, and restarts northstar. No output is captured,
// because the content holds credentials.
func writeNorthstarConfig(content string) error {
	staged := northstarConfigPath + ".e2e-new"
	if err := sudoRun("cp", "-p", northstarConfigPath, staged); err != nil {
		return fmt.Errorf("stage: %w", err)
	}
	tee := exec.Command("sudo", "-n", "tee", staged)
	tee.Stdin = strings.NewReader(content)
	if err := tee.Run(); err != nil {
		return fmt.Errorf("write %s: %w", staged, err)
	}
	if err := sudoRun("mv", "-f", staged, northstarConfigPath); err != nil {
		return fmt.Errorf("replace: %w", err)
	}
	return restartNorthstar()
}

// restoreNorthstarConfig moves a staged original back, if there is one, and
// restarts northstar on it.
func restoreNorthstarConfig() error {
	backup := northstarConfigPath + backupSuffix
	if exec.Command("sudo", "-n", "test", "-e", backup).Run() != nil {
		return nil
	}
	if err := sudoRun("mv", "-f", backup, northstarConfigPath); err != nil {
		return err
	}
	return restartNorthstar()
}

func restartNorthstar() error {
	if err := sudoRun("systemctl", "restart", northstarUnit); err != nil {
		return err
	}
	out, err := exec.Command("systemctl", "is-active", northstarUnit).Output()
	if state := strings.TrimSpace(string(out)); err != nil || state != "active" {
		return fmt.Errorf("%s is %q after restart", northstarUnit, state)
	}
	return nil
}

func sudoRun(argv ...string) error {
	if out, err := exec.Command("sudo", append([]string{"-n"}, argv...)...).CombinedOutput(); err != nil {
		return fmt.Errorf("sudo %s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
