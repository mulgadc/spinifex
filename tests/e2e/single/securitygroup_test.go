//go:build e2e

package single

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

// pingReceivedRE matches the busybox/iputils summary line for 1–3 echo replies
// (`3 packets received` or `1 received`). Mirrors the bash grep pattern.
var pingReceivedRE = regexp.MustCompile(`[1-3] (packets )?received`)

// pingDroppedRE matches the summary for a fully dropped probe — either
// `0 received` or `100% packet loss`. Mirrors the bash grep pattern.
var pingDroppedRE = regexp.MustCompile(`0 (packets )?received|100% packet loss`)

// phase5f_SecurityGroupEgress flips the default SG's allow-all egress rule and
// verifies vpcd programs OVN ACLs that actually drop traffic. Egress is tested
// because in dev_networking mode ingress SSH bypasses OVN via QEMU hostfwd —
// only egress hits the ACL. Maps to run-e2e.sh ~846–928.
func phase5f_SecurityGroupEgress(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Phase 5f — Security Group Enforcement (egress ACL)")
	require.NotEmpty(t, fix.InstanceID, "Phase 5 must populate fix.InstanceID")
	require.NotEmpty(t, fix.SSHHost, "Phase 5a-ii must populate fix.SSHHost")
	require.NotZero(t, fix.SSHPort, "Phase 5a-ii must populate fix.SSHPort")
	require.NotEmpty(t, fix.KeyPath, "Phase 3 must populate fix.KeyPath")

	def := harness.EnsureDefaultVPC(t, fix.Harness)
	require.NotEmpty(t, def.SGID, "default SG ID required")

	tgt := harness.SSHTarget{User: "ec2-user", Host: fix.SSHHost, Port: fix.SSHPort, KeyPath: fix.KeyPath}

	// Restore allow-all egress no matter what — ignore Duplicate so a clean
	// finish (test left the rule in place) doesn't poison later phases.
	t.Cleanup(func() {
		if err := authorizeAllowAllEgress(fix.AWS, def.SGID); err != nil &&
			!harness.ErrorCodeIs(err, "InvalidPermission.Duplicate") {
			t.Logf("WARNING: cleanup failed to restore allow-all egress on %s: %v",
				def.SGID, err)
		}
	})

	harness.Step(t, "discover default gateway inside VM")
	gwOut, gwErr := runSSHCombined(tgt, `ip route show default | awk '{print $3}' | head -1`)
	gw := strings.TrimSpace(strings.Map(func(r rune) rune {
		// Strip all whitespace, matching `tr -d '[:space:]'` from bash.
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, gwOut))
	if gwErr != nil || gw == "" {
		t.Skipf("Phase 5f: could not discover default gateway inside VM (err=%v, out=%q)",
			gwErr, gwOut)
	}
	harness.Detail(t, "probe_gateway", gw)

	probeICMP := func() string {
		out, _ := runSSHCombined(tgt, fmt.Sprintf("ping -c 3 -W 2 %s", gw))
		return out
	}

	// Test 5f-1: Baseline — allow-all egress should let ICMP through. If the
	// environment blocks ICMP between the VM and gateway regardless of SG,
	// skip the rest of the phase: we can't distinguish enforcement from a
	// network that never carried the probe in the first place.
	harness.Step(t, "5f-1 baseline egress (allow-all)")
	baseline := probeICMP()
	if !pingReceivedRE.MatchString(baseline) {
		t.Skipf("Phase 5f: baseline ICMP did not succeed; env may block ICMP regardless of SG\nOutput:\n%s",
			baseline)
	}
	harness.Detail(t, "baseline", "icmp_ok")

	// Test 5f-2: Revoke egress → ICMP must be dropped.
	harness.Step(t, "5f-2 revoke egress -> expect drop")
	_, err := fix.AWS.EC2.RevokeSecurityGroupEgress(&ec2.RevokeSecurityGroupEgressInput{
		GroupId:       aws.String(def.SGID),
		IpPermissions: []*ec2.IpPermission{allowAllEgressPermission()},
	})
	require.NoError(t, err, "revoke-security-group-egress")

	// ACL propagation: bash uses a flat sleep 3 — poll the probe instead so a
	// slow OVN flow install still gets bounded, fast environments don't waste
	// the full budget. Total budget kept tight: each ping is ~6s (3 echoes,
	// 2s timeout) so a 30s ceiling tolerates ~4 retries.
	var lastRevoke string
	harness.EventuallyErr(t, func() error {
		lastRevoke = probeICMP()
		if pingDroppedRE.MatchString(lastRevoke) {
			return nil
		}
		return fmt.Errorf("ICMP still succeeding after revoke; output:\n%s", lastRevoke)
	}, 30*time.Second, 1*time.Second)
	harness.Detail(t, "revoke", "icmp_dropped")

	// Test 5f-3: Re-authorize → ICMP works again.
	harness.Step(t, "5f-3 re-authorize egress -> expect allow")
	err = authorizeAllowAllEgress(fix.AWS, def.SGID)
	require.NoError(t, err, "authorize-security-group-egress")

	var lastRestore string
	harness.EventuallyErr(t, func() error {
		lastRestore = probeICMP()
		if pingReceivedRE.MatchString(lastRestore) {
			return nil
		}
		return fmt.Errorf("ICMP still dropped after re-authorize; output:\n%s", lastRestore)
	}, 30*time.Second, 1*time.Second)
	harness.Detail(t, "restore", "icmp_ok")
}

// allowAllEgressPermission returns the SDK shape for the default SG's stock
// `IpProtocol=-1, CidrIp=0.0.0.0/0` rule.
func allowAllEgressPermission() *ec2.IpPermission {
	return &ec2.IpPermission{
		IpProtocol: aws.String("-1"),
		IpRanges:   []*ec2.IpRange{{CidrIp: aws.String("0.0.0.0/0")}},
	}
}

// authorizeAllowAllEgress is the idempotent re-add used both by the happy
// path and t.Cleanup. Returns the raw SDK error so the caller can decide
// whether `InvalidPermission.Duplicate` is fatal in its context.
func authorizeAllowAllEgress(c *harness.AWSClient, sgID string) error {
	_, err := c.EC2.AuthorizeSecurityGroupEgress(&ec2.AuthorizeSecurityGroupEgressInput{
		GroupId:       aws.String(sgID),
		IpPermissions: []*ec2.IpPermission{allowAllEgressPermission()},
	})
	return err
}

// runSSHCombined runs `command` over SSH against tgt and returns combined
// stdout+stderr regardless of exit status. Unlike runSSH (instance_test.go)
// it does not t.Fatal on non-zero exit — needed because `ping` legitimately
// exits non-zero when packets are dropped, which is the expected outcome of
// the revoke test.
func runSSHCombined(tgt harness.SSHTarget, command string) (string, error) {
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		"-p", strconv.Itoa(tgt.Port),
		"-i", tgt.KeyPath,
		tgt.User + "@" + tgt.Host,
		command,
	}
	var combined bytes.Buffer
	cmd := exec.Command("ssh", args...)
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	err := cmd.Run()
	return combined.String(), err
}
