//go:build e2e

package reboot

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	guestShutdownKeyName    = "guest-shutdown-e2e-key"
	guestShutdownSGName     = "guest-shutdown-e2e-sg"
	guestShutdownVPCCIDR    = "10.211.0.0/16"
	guestShutdownSubnetCIDR = "10.211.1.0/24"

	// The slow guest's shutdown hook writes one fsynced line a second for this
	// long. Comfortably inside the budget the daemon gives a guest, and far
	// enough past a normal shutdown that a reset rather than a wait is
	// unmistakable in the line count that survives.
	slowShutdownSeconds = 25

	// What the file must hold afterwards. Short of the full run so a slow guest
	// under CI load is not failed for losing the last second or two, but far
	// enough above zero that a hard reset cannot reach it.
	slowShutdownMinLines = 20

	// The budget the daemon gives a guest to power down for a reboot. Not
	// imported: this is the e2e suite's own statement of the contract, and a
	// change to the constant should fail here rather than pass silently.
	guestPowerdownBudget = 60 * time.Second

	// How long a guest gets to answer on a new boot ID. The wedged variant
	// spends its whole budget before the reset and only then boots.
	guestRebootBudget = 5 * time.Minute

	// The line the daemon logs when it gives up asking and resets the guest.
	hardResetLogLine = "Guest did not power down for reboot"
)

// slowShutdownUserData installs a guest that takes its time going down and
// leaves a record of how long it was given. Each line is fsynced, so the file
// that survives is a direct measure of how much of the shutdown actually ran:
// a reset at the first poll leaves none of it.
//
// Ordinary service dependencies on purpose — the unit is stopped early in the
// shutdown transaction, while the root filesystem is still writable.
var slowShutdownUserData = fmt.Sprintf(`#!/bin/bash
set -e
cat > /usr/local/bin/slow-shutdown <<'EOS'
#!/bin/sh
i=0
while [ $i -lt %[1]d ]; do
    i=$((i+1))
    date +%%s >> %[2]s
    sync %[2]s
    sleep 1
done
EOS
chmod +x /usr/local/bin/slow-shutdown
cat > /etc/systemd/system/slow-shutdown.service <<'UNIT'
[Unit]
Description=Guest shutdown probe

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/true
ExecStop=/usr/local/bin/slow-shutdown
TimeoutStopSec=120

[Install]
WantedBy=multi-user.target
UNIT
rm -f %[2]s
systemctl daemon-reload
systemctl enable --now slow-shutdown.service
touch /run/guest-shutdown-ready
`, slowShutdownSeconds, slowShutdownProbeFile)

const slowShutdownProbeFile = "/var/lib/slow-shutdown.log"

// wedgedUserData installs a guest that never acknowledges the power button.
// systemd-logind is what turns an ACPI power-button event into a shutdown on
// these images; acpid is disabled too so the answer does not depend on which
// of the two the image happens to ship.
const wedgedUserData = `#!/bin/bash
set -e
mkdir -p /etc/systemd/logind.conf.d
cat > /etc/systemd/logind.conf.d/99-ignore-power.conf <<'EOF'
[Login]
HandlePowerKey=ignore
HandlePowerKeyLongPress=ignore
EOF
systemctl disable --now acpid 2>/dev/null || true
systemctl restart systemd-logind
touch /run/guest-shutdown-ready
`

// TestGuestShutdownBudget proves the two paths a guest reboot can take, against
// real guests rather than a mocked QMP socket.
//
// A reset alone never reaches the guest kernel, so it syncs nothing. The
// primitive therefore asks the guest to shut down and only resets it once
// asking has failed — and the two cases that separates are a guest that is slow
// and must be waited for, and one that will never answer and must be reset
// anyway. Each is asserted on the evidence only it can produce.
func TestGuestShutdownBudget(t *testing.T) {
	env := harness.LoadEnv(t)
	requireRunnerMode(t, env)

	fix := &fixture{
		env:       env,
		artifacts: harness.ArtifactDir(t, env),
		ssh:       harness.NewPeerSSH(),
		preIPs:    map[string]string{},
		timeouts: timeouts{
			instanceRunning: durationEnv("INSTANCE_RUNNING_SECS", 120*time.Second),
		},
	}

	resolveTrust(t, fix)
	fix.aws = harness.NewAWSClient(t, env)
	t.Cleanup(func() { cleanup(t, fix) })

	harness.Phase(t, "Prerequisites")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	require.NoError(t, fix.ssh.Ping(ctx, fix.env.WANHost), "ssh to %s", fix.env.WANHost)
	cancel()

	fix.instanceType = discoverNanoInstanceType(t, fix.aws)
	fix.amiID = discoverAMI(t, fix.aws)
	harness.Detail(t, "instance_type", fix.instanceType)
	harness.Detail(t, "ami", fix.amiID)
	provisionKeyPair(t, fix, guestShutdownKeyName)

	harness.Phase(t, "VPC + subnet + security group")
	provisionNetwork(t, fix, netNames{
		sg:         guestShutdownSGName,
		sgDesc:     "Guest shutdown budget E2E",
		vpcCIDR:    guestShutdownVPCCIDR,
		subnetCIDR: guestShutdownSubnetCIDR,
	})

	t.Run("SlowGuestIsWaitedForAndKeepsWhatItWrote", func(t *testing.T) {
		id, pub := launchProbeGuest(t, fix, slowShutdownUserData, "slow")

		before := time.Now()
		bootID := rebootAndWaitForNewBootID(t, fix, id, pub)
		elapsed := time.Since(before)
		harness.Detail(t, "reboot_elapsed_secs", int(elapsed.Seconds()))
		harness.Detail(t, "post_reboot_boot_id", bootID)

		// The file is the assertion. Every line was fsynced during shutdown, so
		// its length is how much of that shutdown the guest was allowed to run.
		lines := countProbeLines(t, fix, pub)
		harness.Detail(t, "shutdown_probe_lines", lines)
		assert.GreaterOrEqualf(t, lines, slowShutdownMinLines,
			"the guest wrote %d fsynced lines during shutdown, wanted at least %d — "+
				"a guest reset instead of asked never runs its shutdown at all",
			lines, slowShutdownMinLines)

		// Assert the test tested something: a shutdown that finished in a
		// second has not exercised a slow one, whatever the line count says.
		assert.GreaterOrEqualf(t, elapsed, time.Duration(slowShutdownMinLines)*time.Second,
			"the whole reboot took %s, which is less than the shutdown hook alone should have taken — "+
				"the slow guest was not slow and this run proves nothing", elapsed)

		// The other half of the contract: a guest that went down on its own
		// must not also have been reported as one that would not.
		journal := daemonJournalFor(t, fix, id)
		assert.NotContainsf(t, journal, hardResetLogLine,
			"the daemon logged the hard-reset fallback for %s, but this guest shut down inside its budget", id)
	})

	t.Run("WedgedGuestIsResetOnceItsBudgetExpires", func(t *testing.T) {
		id, pub := launchProbeGuest(t, fix, wedgedUserData, "wedged")

		before := time.Now()
		bootID := rebootAndWaitForNewBootID(t, fix, id, pub)
		elapsed := time.Since(before)
		harness.Detail(t, "reboot_elapsed_secs", int(elapsed.Seconds()))
		harness.Detail(t, "post_reboot_boot_id", bootID)

		// A guest that will not go quietly is still rebooted — that is the AWS
		// contract, and it is what stops one wedged guest becoming an outage.
		// Reaching a new boot ID above is that assertion.
		assert.GreaterOrEqualf(t, elapsed, guestPowerdownBudget,
			"the reboot took %s, less than the %s budget, so the guest was reset without being asked first — "+
				"the whole point is that asking comes first", elapsed, guestPowerdownBudget)

		journal := daemonJournalFor(t, fix, id)
		assert.Containsf(t, journal, hardResetLogLine,
			"the daemon reset %s without logging why; a guest that never honours ACPI has to be visible, "+
				"not silently hard-reset forever", id)
	})
}

// launchProbeGuest boots one guest on the given user-data and waits until its
// setup has reported itself done, so a test never measures a shutdown against a
// guest that had not finished being configured.
func launchProbeGuest(t *testing.T, fix *fixture, userData, label string) (id, pub string) {
	t.Helper()
	harness.Step(t, "launching the %s guest", label)

	// e2e:allow-create — the guest is the subject: each variant carries its own
	// shutdown behaviour in user-data, so a memoised fixture guest is the wrong
	// shape and sharing one between the variants would erase the difference.
	out, err := fix.aws.EC2.RunInstances(&ec2.RunInstancesInput{ //nolint:staticcheck // e2e:allow-create
		ImageId:          aws.String(fix.amiID),
		InstanceType:     aws.String(fix.instanceType),
		KeyName:          aws.String(fix.keyPairName),
		SubnetId:         aws.String(fix.subnetID),
		SecurityGroupIds: []*string{aws.String(fix.sgID)},
		UserData:         aws.String(base64.StdEncoding.EncodeToString([]byte(userData))),
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
	})
	require.NoErrorf(t, err, "run-instances %s", label)
	require.NotEmpty(t, out.Instances)
	id = aws.StringValue(out.Instances[0].InstanceId)
	fix.appInstanceIDs = append(fix.appInstanceIDs, id)
	harness.Detail(t, label+"_instance", id)

	harness.WaitForInstanceRunning(t, fix.aws, id, fix.timeouts.instanceRunning)
	pub = describePublicIP(t, fix.aws, id)
	require.NotEmptyf(t, pub, "%s has no PublicIpAddress", id)

	// The sentinel is written at the end of user-data, so waiting for it waits
	// for the shutdown hook to be installed rather than for SSH alone.
	harness.Step(t, "waiting for the %s guest's setup to finish", label)
	deadline := time.Now().Add(5 * time.Minute)
	for {
		out, err := runInGuest(fix, pub, "test -e /run/guest-shutdown-ready && echo ready")
		if err == nil && strings.Contains(out, "ready") {
			return id, pub
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s guest %s never finished its user-data setup (last output %q, err: %v)",
				label, id, strings.TrimSpace(out), err)
		}
		time.Sleep(5 * time.Second)
	}
}

// rebootAndWaitForNewBootID reboots the guest and blocks until it answers with a
// boot ID other than the one it held, which is the only positive proof that the
// guest actually rebooted rather than never having gone down.
func rebootAndWaitForNewBootID(t *testing.T, fix *fixture, id, pub string) string {
	t.Helper()

	previous := readBootID(t, fix, pub)
	require.NotEmptyf(t, previous, "%s returned an empty boot ID before the reboot", id)
	harness.Detail(t, "pre_reboot_boot_id", previous)

	harness.Step(t, "reboot-instances %s", id)
	_, err := fix.aws.EC2.RebootInstances(&ec2.RebootInstancesInput{
		InstanceIds: []*string{aws.String(id)},
	})
	require.NoErrorf(t, err, "reboot-instances %s", id)

	deadline := time.Now().Add(guestRebootBudget)
	var lastID string
	var lastErr error
	for {
		out, err := runInGuest(fix, pub, "cat /proc/sys/kernel/random/boot_id")
		lastErr = err
		if err == nil {
			lastID = lastNonEmptyLine(out)
			if lastID != "" && lastID != previous {
				return lastID
			}
		}
		if time.Now().After(deadline) {
			// "Never came back" and "came back on the same boot" are different
			// faults, and one message for both would name neither.
			if lastID == previous {
				t.Fatalf("%s still holds boot ID %s after %s, so the reboot never reached the guest kernel",
					id, previous, guestRebootBudget)
			}
			t.Fatalf("%s never answered SSH with a boot ID within %s (last error: %v)",
				id, guestRebootBudget, lastErr)
		}
		time.Sleep(3 * time.Second)
	}
}

func readBootID(t *testing.T, fix *fixture, pub string) string {
	t.Helper()
	out, err := runInGuest(fix, pub, "cat /proc/sys/kernel/random/boot_id")
	require.NoError(t, err, "read boot ID from the guest")
	return lastNonEmptyLine(out)
}

// countProbeLines reads how many fsynced lines the shutdown hook managed to
// write. A missing file means none of it ran, which is the same finding as an
// empty one and is reported as zero rather than as an error.
func countProbeLines(t *testing.T, fix *fixture, pub string) int {
	t.Helper()
	out, err := runInGuest(fix, pub, "wc -l < "+slowShutdownProbeFile+" 2>/dev/null || echo 0")
	require.NoError(t, err, "read the shutdown probe file")
	n, err := strconv.Atoi(lastNonEmptyLine(out))
	require.NoErrorf(t, err, "parse the shutdown probe line count from %q", out)
	return n
}

// daemonJournalFor returns the daemon's log lines mentioning one instance. The
// suite runs on the CI runner, so this hops through the node.
func daemonJournalFor(t *testing.T, fix *fixture, id string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := "sudo journalctl -u spinifex-daemon --since '-15 min' --no-pager 2>/dev/null | grep -F " + id + " || true"
	out, err := fix.ssh.Run(ctx, fix.env.WANHost, cmd)
	require.NoErrorf(t, err, "read the daemon journal for %s", id)
	harness.DumpFile(t, fix.artifacts, fmt.Sprintf("journal-%s.txt", id), out)
	return string(out)
}

// runInGuest runs one command inside a guest, hopping through the node: the
// runner has no route to a guest's address and the node does.
func runInGuest(fix *fixture, pub, command string) (string, error) {
	keyData, err := os.ReadFile(fix.privateKeyPath)
	if err != nil {
		return "", fmt.Errorf("read guest key: %w", err)
	}
	remoteKey := "/tmp/guest-shutdown-e2e.pem"
	outer := fmt.Sprintf(`set -e
umask 077 && printf '%%s' '%s' | base64 -d > %s && chmod 600 %s
ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
    -o ConnectTimeout=8 -o BatchMode=yes ubuntu@%s bash -s <<'EOFCMD'
%s
EOFCMD
`, base64.StdEncoding.EncodeToString(keyData), remoteKey, remoteKey, remoteKey, pub, command)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := fix.ssh.Run(ctx, fix.env.WANHost, outer)
	return string(out), err
}

// lastNonEmptyLine takes the final line with content, so an SSH banner or a
// warning ahead of the answer does not become the answer.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if trimmed := strings.TrimSpace(lines[i]); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
