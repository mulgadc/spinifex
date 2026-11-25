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
)

var upstreamNameservers = regexp.MustCompile(`(?m)^nameservers\s*=\s*\[(.*)\]\s*$`)

// runUpstreamFailover puts a silent and a refusing forwarder ahead of the
// node's real one, then times guest lookups through the shim: the first must
// fail over inside a guest's 5 s attempt and the second skip the bad two.
func runUpstreamFailover(t *testing.T, fix *Fixture, tgt harness.SSHTarget, privIP string) {
	harness.RequireDNSEnabled(t, fix.Env)
	original := sudoOutput(t, "cat", northstarConfigPath)
	m := upstreamNameservers.FindStringSubmatch(original)
	require.NotNilf(t, m, "no [upstream] nameservers line in %s", northstarConfigPath)
	entries := strings.Split(m[1], ",")
	working := strings.TrimSpace(entries[0])
	if working == "" {
		t.Skip("the node has no upstream forwarder to put behind the failing ones")
	}
	line := fmt.Sprintf("nameservers = [%q, %q, %s]", silentForwarder, refusingForwarder, working)
	harness.Detail(t, "upstream", line)

	t.Cleanup(func() {
		if err := writeNorthstarConfig(original); err != nil {
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

	for _, c := range []struct {
		name  string
		limit time.Duration
	}{
		{"example.org", 2 * time.Second},
		{"example.net", time.Second},
	} {
		harness.Step(t, "resolve %s from the guest within %s", c.name, c.limit)
		took, out := guestLookupTime(t, tgt, c.name)
		harness.Detail(t, "name", c.name, "took_ms", took.Milliseconds())
		require.Regexpf(t, `\d{1,3}(\.\d{1,3}){3}`, out, "%s did not resolve through northstar\n%s", c.name, out)
		require.LessOrEqualf(t, took, c.limit, "%s took %s with a silent and a refusing forwarder first", c.name, took)
	}
}

// guestLookupTime times one lookup inside the guest, so SSH setup is not counted.
func guestLookupTime(t *testing.T, tgt harness.SSHTarget, name string) (time.Duration, string) {
	t.Helper()
	cmd := fmt.Sprintf(`s=$(date +%%s%%N); getent ahostsv4 %s; e=$(date +%%s%%N); echo "took_ns=$((e-s))"`, name)
	out, _ := sshCapture(tgt, cmd)
	i := strings.LastIndex(out, "took_ns=")
	require.GreaterOrEqualf(t, i, 0, "no timing from the guest\n%s", out)
	ns, err := strconv.ParseInt(strings.TrimSpace(out[i+len("took_ns="):]), 10, 64)
	require.NoErrorf(t, err, "timing from the guest\n%s", out)
	return time.Duration(ns), out[:i]
}

// writeNorthstarConfig rewrites the file in place, so its owner and mode stay,
// then restarts northstar and waits for it to be active.
func writeNorthstarConfig(content string) error {
	tee := exec.Command("sudo", "-n", "tee", northstarConfigPath)
	tee.Stdin = strings.NewReader(content)
	if out, err := tee.CombinedOutput(); err != nil {
		return fmt.Errorf("write: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("sudo", "-n", "systemctl", "restart", northstarUnit).CombinedOutput(); err != nil {
		return fmt.Errorf("restart %s: %v: %s", northstarUnit, err, strings.TrimSpace(string(out)))
	}
	out, err := exec.Command("systemctl", "is-active", northstarUnit).CombinedOutput()
	if state := strings.TrimSpace(string(out)); err != nil || state != "active" {
		return fmt.Errorf("%s is %q after restart", northstarUnit, state)
	}
	return nil
}

func sudoOutput(t *testing.T, argv ...string) string {
	t.Helper()
	out, err := exec.Command("sudo", append([]string{"-n"}, argv...)...).CombinedOutput()
	require.NoErrorf(t, err, "sudo %s: %s", strings.Join(argv, " "), strings.TrimSpace(string(out)))
	return string(out)
}
