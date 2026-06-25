//go:build e2e

package multinode

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// runGuestSSH is the Go port of guest SSH probes
// (run-multinode-e2e.sh:626-728). For every instance in the trio, resolve an
// SSH endpoint (PublicIpAddress or hosting-node hostfwd), wait until SSH
// answers, and assert `id` reports ubuntu (the AMI's stock default user, now
// that the seed is retired and cloud-init bootstraps from the Ec2 datasource).
// Also asserts the AWS-form hostname and runs `lsblk` as a root-device smoke.
func runGuestSSH(t *testing.T, fix *Fixture) {
	harness.Phase(t, "Multinode — Guest SSH")

	ids := needInstanceTrio(t, fix)
	_, pemPath := needKeyPair(t, fix)

	for _, id := range ids {
		harness.Step(t, "guest_ssh probe %s", id)
		host, port := harness.GuestSSHEndpoint(t, fix.AWS, fix.Cluster, id)
		harness.Detail(t, "instance", id, "ssh_host", host, "ssh_port", port)

		harness.GuestSSHReady(t, host, port, "ubuntu", pemPath,
			harness.WithTimeout(2*time.Minute), harness.WithPoll(2*time.Second))

		idOut := sshRun(t, pemPath, "ubuntu", host, port, "id")
		harness.Detail(t, "instance", id, "id", strings.TrimSpace(idOut))
		if !strings.Contains(idOut, "ubuntu") {
			t.Fatalf("instance %s id output missing ubuntu:\n%s", id, idOut)
		}

		// Cutover guard: cloud-init sets the AWS-form hostname ip-<dashed-ip> from
		// IMDS local-hostname, not the retired seed's spinifex-vm-<id>.
		hostname := strings.TrimSpace(sshRun(t, pemPath, "ubuntu", host, port, "hostname"))
		harness.Detail(t, "instance", id, "hostname", hostname)
		if !strings.HasPrefix(hostname, "ip-") {
			t.Fatalf("instance %s hostname %q is not the AWS form ip-<dashed-ip> "+
				"rendered from IMDS — seed may not be retired", id, hostname)
		}

		lsblk := sshRun(t, pemPath, "ubuntu", host, port, "lsblk")
		// lsblk header line + at least one device row.
		harness.Detail(t, "instance", id, "lsblk_lines", strings.Count(lsblk, "\n"))
	}
}

// sshRun executes cmd over SSH against (user@host:port) using pemPath as
// the private key. Returns combined stdout+stderr; fatals on non-zero
// exit. Mirrors the bash ssh invocation in phase 4 (-o flags identical).
func sshRun(t *testing.T, pemPath, user, host string, port int, cmd string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := []string{
		"-i", pemPath,
		"-p", strconv.Itoa(port),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		"-o", "LogLevel=ERROR",
		fmt.Sprintf("%s@%s", user, host),
		cmd,
	}
	out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh %s@%s:%d %q failed: %v\n%s", user, host, port, cmd, err, string(out))
	}
	return string(out)
}
