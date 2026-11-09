//go:build e2e

package uefistore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	store "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// varGUID is the vendor GUID every variable this suite writes is filed under,
// so none of them can be mistaken for a variable the firmware or OS owns.
const varGUID = "5a6d2e1c-3b4f-4c8a-9e7d-1f2a3b4c5d6e"

// writeBackClaim is the documented bound on a running guest's change reaching
// an object; writeBackBudget adds room for the put and pointer move under load.
const (
	writeBackClaim  = 30 * time.Second
	writeBackBudget = writeBackClaim + 30*time.Second
)

// reaperWriteBackBudget covers a writer the daemon could not resume at start,
// which the write-back reaper starts on its next 2-minute sweep.
const reaperWriteBackBudget = writeBackBudget + 3*time.Minute

const (
	guestUser      = "ubuntu"
	sshBudget      = 5 * time.Minute
	stopBudget     = 6 * time.Minute
	startBudget    = 5 * time.Minute
	guestCmdBudget = 60 * time.Second
	daemonUnit     = "spinifex-daemon"
	guestScope     = "uefistore"
)

// guest is the suite's shared UEFI instance and how to reach it now. The SSH
// target changes across a stop and start, because the public address does.
type guest struct {
	id  string
	tgt harness.SSHTarget
}

// efiVar is one variable a test wrote and the value it must keep.
type efiVar struct {
	name  string
	value string
}

func (v efiVar) path() string {
	return "/sys/firmware/efi/efivars/" + v.name + "-" + varGUID
}

// newVar names a variable with a value unique to this run, so finding the value
// in a store object can only mean this write reached it.
func newVar(t *testing.T, name string) efiVar {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random variable value: %v", err)
	}
	return efiVar{name: name, value: "spx" + hex.EncodeToString(b)}
}

// ensureUEFIGuest returns the shared UEFI guest, launching it on first use, and
// fails unless it booted through UEFI with a live store record.
func ensureUEFIGuest(t *testing.T, fix *Fixture) guest {
	t.Helper()
	instType, arch := harness.DiscoverNanoInstanceType(t, fix.Harness)
	ami := harness.DiscoverUbuntuAMI(t, fix.Harness, arch)
	requireUEFIImage(t, fix, ami)
	keyName, keyPath := harness.EnsureKeyPair(t, fix.Harness)
	vpc := harness.EnsureDefaultVPC(t, fix.Harness)
	harness.AuthorizeSSHIngress(t, fix.AWS, vpc.SGID)

	fix.launchedMu.Lock()
	if fix.launched.IsZero() {
		fix.launched = time.Now()
	}
	fix.launchedMu.Unlock()

	id := harness.EnsureInstance(t, fix.Harness, harness.InstanceSpec{
		AMIID:        ami,
		InstanceType: instType,
		KeyName:      keyName,
		SubnetID:     vpc.SubnetID,
		SGID:         vpc.SGID,
		Scope:        guestScope,
	})
	inst := harness.WaitForInstanceState(t, fix.AWS, id, "running")
	g := guest{id: id, tgt: reachGuest(t, fix, inst, keyPath)}

	if out, err := harness.GuestExecTimeout(g.tgt, "test -d /sys/firmware/efi/efivars", guestCmdBudget); err != nil {
		t.Fatalf("%s booted %s without UEFI (no /sys/firmware/efi/efivars), so it has no variable store to test: %v %s",
			id, ami, err, out)
	}
	rec, err := fix.Store.Record(t.Context(), id)
	if err != nil {
		t.Fatalf("read the store record of UEFI instance %s: %v", id, err)
	}
	if rec.State != store.StateLive || rec.Pointer == nil {
		t.Fatalf("UEFI instance %s is running with store record state %q and pointer %v; a running instance's store must be live and point at an object",
			id, rec.State, rec.Pointer)
	}
	harness.Detail(t, "instance", id, "ami", ami, "pointer", rec.Pointer.Key, "generation", rec.Generation)
	return g
}

// requireUEFIImage fails unless ami boots UEFI: a BIOS image would leave every
// assertion here with nothing to observe.
func requireUEFIImage(t *testing.T, fix *Fixture, ami string) {
	t.Helper()
	out, err := fix.AWS.EC2.DescribeImages(&ec2.DescribeImagesInput{ImageIds: []*string{aws.String(ami)}})
	if err != nil || len(out.Images) == 0 {
		t.Fatalf("describe %s: %v", ami, err)
	}
	mode := aws.StringValue(out.Images[0].BootMode)
	if mode != "uefi" && mode != "uefi-preferred" {
		t.Fatalf("the e2e Ubuntu AMI %s has boot mode %q; this suite needs a UEFI image", ami, mode)
	}
}

// reachGuest waits for SSH and sudo on the instance's current public address.
func reachGuest(t *testing.T, fix *Fixture, inst *ec2.Instance, keyPath string) harness.SSHTarget {
	t.Helper()
	id := aws.StringValue(inst.InstanceId)
	host, port := harness.InstancePublicSSHHost(t, inst)
	if !harness.TryGuestSSHReady(host, port, guestUser, keyPath, sshBudget) {
		t.Fatalf("guest %s SSH %s:%d not ready after %s%s", id, host, port, sshBudget, consoleTail(fix, id))
	}
	tgt := harness.SSHTarget{User: guestUser, Host: host, Port: port, KeyPath: keyPath}
	if out, err := harness.GuestExecRetrying(tgt, "sudo -n true", 30*time.Second, 3*time.Minute, 5*time.Second); err != nil {
		t.Fatalf("guest %s answers SSH but not sudo: %v %s", id, err, out)
	}
	return tgt
}

// writeVar writes v as a non-volatile, boot-service and runtime variable
// (attributes 0x7) in one write, as efivarfs requires.
func writeVar(t *testing.T, g guest, v efiVar) time.Time {
	t.Helper()
	cmd := fmt.Sprintf(`set -e; f=%s; tmp=$(mktemp); printf '\007\000\000\000%%s' %s > "$tmp"; `+
		`if [ -e "$f" ]; then sudo chattr -i "$f"; fi; `+
		`sudo dd if="$tmp" of="$f" bs=4096 conv=notrunc status=none; rm -f "$tmp"`,
		harness.ShellQuote(v.path()), harness.ShellQuote(v.value))
	issued := time.Now()
	if out, err := harness.GuestExecTimeout(g.tgt, cmd, guestCmdBudget); err != nil {
		t.Fatalf("guest %s could not write EFI variable %s: %v %s", g.id, v.name, err, out)
	}
	assertVar(t, g, v)
	harness.Step(t, "guest %s wrote EFI variable %s=%s", g.id, v.name, v.value)
	return issued
}

// assertVar fails unless the guest reads v back with attributes 0x7 and its value.
func assertVar(t *testing.T, g guest, v efiVar) {
	t.Helper()
	cmd := fmt.Sprintf("sudo od -An -tx1 -v %s | tr -d ' \\n'", harness.ShellQuote(v.path()))
	out, err := harness.GuestExecTimeout(g.tgt, cmd, guestCmdBudget)
	if err != nil {
		t.Fatalf("guest %s cannot read EFI variable %s: %v %s", g.id, v.name, err, out)
	}
	want := "07000000" + hex.EncodeToString([]byte(v.value))
	if got := strings.TrimSpace(out); got != want {
		t.Fatalf("guest %s reads EFI variable %s as %q, want %q (attributes 0x7, value %q)",
			g.id, v.name, got, want, v.value)
	}
}

// regionHolds reports whether a VARS region carries every variable: its UTF-16
// name and its value, which is unique to this run.
func regionHolds(region []byte, vars ...efiVar) bool {
	for _, v := range vars {
		if !bytes.Contains(region, utf16LE(v.name)) || !bytes.Contains(region, []byte(v.value)) {
			return false
		}
	}
	return true
}

func utf16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(out[2*i:], u)
	}
	return out
}

// waitForStoreToHold polls until the store record points at an object holding
// every variable, and returns that object's key. It reads each pointed object
// once, so a pointer that moves for another reason is not mistaken for this one.
func waitForStoreToHold(t *testing.T, fix *Fixture, id string, budget time.Duration, why string, vars ...efiVar) string {
	t.Helper()
	start := time.Now()
	checked := map[string]bool{}
	var lastErr error
	for time.Since(start) < budget {
		rec, err := fix.Store.Record(t.Context(), id)
		switch {
		case err != nil:
			lastErr = err
		case rec.Pointer == nil:
			lastErr = fmt.Errorf("store record state %q names no pointer", rec.State)
		case !checked[rec.Pointer.Key]:
			region, rerr := fix.Store.Region(t.Context(), rec.Pointer.Key)
			if rerr != nil {
				lastErr = rerr
				break
			}
			checked[rec.Pointer.Key] = true
			if regionHolds(region, vars...) {
				harness.Detail(t, "instance", id, "pointer", rec.Pointer.Key, "writer", rec.Pointer.WriterNode,
					"landed_ms", time.Since(start).Milliseconds())
				return rec.Pointer.Key
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s: no object the store record of %s pointed at held %s within %d s (pointers checked: %v, last error: %v)",
		why, id, varNames(vars), int(budget.Seconds()), mapKeys(checked), lastErr)
	return ""
}

// stopStart stops the guest, checks the stop left the store holding every
// variable, starts it, and returns how to reach the new boot.
func stopStart(t *testing.T, fix *Fixture, g guest, vars ...efiVar) guest {
	t.Helper()
	before := bootID(t, g)

	harness.Step(t, "stopping %s", g.id)
	if _, err := fix.AWS.EC2.StopInstances(&ec2.StopInstancesInput{InstanceIds: []*string{aws.String(g.id)}}); err != nil {
		t.Fatalf("StopInstances %s: %v", g.id, err)
	}
	harness.WaitForInstanceState(t, fix.AWS, g.id, "stopped", harness.WithTimeout(stopBudget), harness.WithPoll(5*time.Second))
	waitForStoreToHold(t, fix, g.id, writeBackBudget,
		"a stop writes the store back before it completes", vars...)

	harness.Step(t, "starting %s", g.id)
	if _, err := fix.AWS.EC2.StartInstances(&ec2.StartInstancesInput{InstanceIds: []*string{aws.String(g.id)}}); err != nil {
		t.Fatalf("StartInstances %s: %v", g.id, err)
	}
	inst := harness.WaitForInstanceState(t, fix.AWS, g.id, "running", harness.WithTimeout(startBudget), harness.WithPoll(5*time.Second))
	next := guest{id: g.id, tgt: reachGuest(t, fix, inst, g.tgt.KeyPath)}
	if after := bootID(t, next); after == before {
		t.Fatalf("%s kept boot ID %s across a stop and start, so the guest never rebooted and the variables were not read back from the store",
			g.id, before)
	}
	return next
}

func bootID(t *testing.T, g guest) string {
	t.Helper()
	out, err := harness.GuestExecTimeout(g.tgt, "cat /proc/sys/kernel/random/boot_id", guestCmdBudget)
	if err != nil {
		t.Fatalf("read boot ID of %s: %v %s", g.id, err, out)
	}
	return strings.TrimSpace(out)
}

// requireOnNode fails unless this process runs on the node and can drive its
// units, which the daemon restart needs.
func requireOnNode(t *testing.T) {
	t.Helper()
	if out, err := hostCmd(t, 30*time.Second, "sudo", "-n", "systemctl", "cat", daemonUnit); err != nil {
		t.Fatalf("this suite runs on the node it tests and restarts %s with sudo; cannot read the unit here: %v %s",
			daemonUnit, err, out)
	}
}

// restartDaemon restarts the daemon unit and waits for it to be active again.
// It returns the time just before the restart, for reading the journal since.
func restartDaemon(t *testing.T) time.Time {
	t.Helper()
	since := time.Now().Add(-time.Second)
	harness.Step(t, "restarting %s under the running guest", daemonUnit)
	if out, err := hostCmd(t, 2*time.Minute, "sudo", "-n", "systemctl", "restart", daemonUnit); err != nil {
		t.Fatalf("systemctl restart %s: %v %s", daemonUnit, err, out)
	}
	harness.EventuallyErr(t, func() error {
		out, err := hostCmd(t, 10*time.Second, "systemctl", "is-active", daemonUnit)
		if strings.TrimSpace(out) != "active" {
			return fmt.Errorf("%s is %q after restart: %v", daemonUnit, strings.TrimSpace(out), err)
		}
		return nil
	}, 2*time.Minute, 2*time.Second)
	return since
}

// writerAdopted waits for the restarted daemon to log that it adopted the
// guest's writer. A false return is not a failure: the reaper starts it later.
func writerAdopted(t *testing.T, id string, since time.Time, budget time.Duration) bool {
	t.Helper()
	const adopted = "Adopted a running instance's UEFI variable store writer"
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		out, _ := hostCmd(t, 20*time.Second, "sudo", "-n", "journalctl", "-u", daemonUnit, "--no-pager",
			"--since", fmt.Sprintf("@%d", since.Unix()))
		for line := range strings.SplitSeq(out, "\n") {
			if strings.Contains(line, adopted) && strings.Contains(line, id) {
				return true
			}
		}
		time.Sleep(5 * time.Second)
	}
	return false
}

func hostCmd(t *testing.T, timeout time.Duration, name string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

func consoleTail(fix *Fixture, id string) string {
	console, err := harness.InstanceConsole(fix.AWS, id)
	if err != nil {
		return fmt.Sprintf("\nconsole unavailable: %v", err)
	}
	lines := strings.Split(strings.TrimRight(console, "\n"), "\n")
	if len(lines) > 60 {
		lines = lines[len(lines)-60:]
	}
	return "\nconsole tail:\n" + strings.Join(lines, "\n")
}

func varNames(vars []efiVar) string {
	names := make([]string, 0, len(vars))
	for _, v := range vars {
		names = append(names, v.name+"="+v.value)
	}
	return strings.Join(names, ", ")
}

func mapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
