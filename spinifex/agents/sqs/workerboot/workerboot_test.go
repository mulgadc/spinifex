package workerboot

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerkey"
)

const (
	testSerial     = "vol0a1b2c3d4e5f60718"
	testMajMin     = "253:16"
	testRegion     = "ap-southeast-2"
	testGatewayURL = "https://10.15.8.1:9999"
)

// guest is a fake guest: fw_cfg, sysfs, /dev and a mount table the fake disk
// edits the way a real mount would.
type guest struct {
	t    *testing.T
	root string
	cfg  Config
	disk *fakeDisk
	auth *authority
	logs []string
}

// authority stands for the worker record behind the gateway: pending until a
// receipt key fingerprint is recorded, then exactly that fingerprint.
type authority struct {
	recorded  string
	claimErrs []error
	// recordErrs fail RecordFingerprint; landed ones record before failing,
	// as a reply lost after the write.
	recordErrs []recordErr
	claims     int
	sent       []string
	builds     int
	buildErr   error
}

type recordErr struct {
	err    error
	landed bool
}

// transient is a refusal the boot may repeat, as a lost reply.
type transient struct{}

func (transient) Error() string   { return "connection reset" }
func (transient) Retryable() bool { return true }

// notBoundError is the denial a started but not yet bound worker receives.
type notBoundError struct{}

func (notBoundError) Error() string { return "AccessDenied" }
func (notBoundError) Pending() bool { return true }

func (a *authority) Claim(context.Context) (workerkey.Decision, error) {
	a.claims++
	if len(a.claimErrs) > 0 {
		err := a.claimErrs[0]
		a.claimErrs = a.claimErrs[1:]
		return workerkey.Decision{}, err
	}
	return workerkey.Decision{Pending: a.recorded == "", Fingerprint: a.recorded, HostNodes: 1}, nil
}

func (a *authority) RecordFingerprint(_ context.Context, fp string) error {
	a.sent = append(a.sent, fp)
	if a.recorded != "" && a.recorded != fp {
		return errors.New("InvalidParameterValue")
	}
	if len(a.recordErrs) > 0 {
		e := a.recordErrs[0]
		a.recordErrs = a.recordErrs[1:]
		if e.landed {
			a.recorded = fp
		}
		return e.err
	}
	a.recorded = fp
	return nil
}

type fakeDisk struct {
	g         *guest
	checkErr  error
	mountErr  error
	noopMount bool
	checks    int
	mounts    int
}

func (d *fakeDisk) Check(string) error {
	d.checks++
	return d.checkErr
}

func (d *fakeDisk) Mount(dev, dir string) error {
	d.mounts++
	if d.mountErr != nil {
		return d.mountErr
	}
	if !d.noopMount {
		d.g.appendMount(testMajMin, "/", dir, "ext4", dev)
	}
	return nil
}

func newGuest(t *testing.T) *guest {
	t.Helper()
	g := &guest{t: t, root: t.TempDir(), auth: &authority{}}
	g.disk = &fakeDisk{g: g}
	g.cfg = Config{
		FwCfgDir:      filepath.Join(g.root, "fwcfg"),
		SysBlock:      filepath.Join(g.root, "sys", "block"),
		DevDir:        filepath.Join(g.root, "dev"),
		MountInfo:     filepath.Join(g.root, "mountinfo"),
		DataDir:       filepath.Join(g.root, "data"),
		RunDir:        filepath.Join(g.root, "run"),
		Disk:          g.disk,
		IsBlockDevice: func(p string) bool { _, err := os.Stat(p); return err == nil },
		NewClaimer: func(region, url string) (workerkey.Claimer, error) {
			g.auth.builds++
			if region != testRegion || url != testGatewayURL {
				t.Errorf("claimer built for %q %q", region, url)
			}
			if g.auth.buildErr != nil {
				return nil, g.auth.buildErr
			}
			return g.auth, nil
		},
		Attempts: 3,
		Log:      func(msg string) { g.logs = append(g.logs, msg) },
	}
	for _, d := range []string{g.cfg.SysBlock, g.cfg.DevDir} {
		mustMkdir(t, d)
	}
	g.resetMounts()
	// An empty mountpoint, as the initramfs ships one.
	mustMkdir(t, g.cfg.DataDir)
	g.addDisk("vda", testSerial, testMajMin)
	return g
}

func mustMkdir(t *testing.T, d string) {
	t.Helper()
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (g *guest) addDisk(name, serial, majMin string) {
	mustWrite(g.t, filepath.Join(g.cfg.SysBlock, name, "serial"), serial+"\n")
	mustWrite(g.t, filepath.Join(g.cfg.SysBlock, name, "dev"), majMin+"\n")
	mustWrite(g.t, filepath.Join(g.cfg.DevDir, name), "")
}

func (g *guest) fw(name, value string) {
	mustWrite(g.t, filepath.Join(g.cfg.FwCfgDir, name, "raw"), value)
}

// contract supplies the whole fw_cfg contract.
func (g *guest) contract() {
	g.fw("data-serial", testSerial+"\n")
	g.fw("region", testRegion+"\n")
	g.fw("gateway-url", testGatewayURL+"\n")
}

func (g *guest) boot() (string, error) {
	g.t.Helper()
	return Boot(context.Background(), g.cfg)
}

// recordKey stands for a receipt key created on this volume earlier and
// recorded by the platform; it returns that key.
func (g *guest) recordKey() []byte {
	g.t.Helper()
	key := make([]byte, workerkey.ReceiptKeyLen)
	if _, err := rand.Read(key); err != nil {
		g.t.Fatal(err)
	}
	mustMkdir(g.t, g.keyDir())
	if err := os.Chmod(g.keyDir(), 0o700); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(g.keyFile(), key, 0o600); err != nil {
		g.t.Fatal(err)
	}
	g.auth.recorded = workerkey.Fingerprint(key)
	return key
}

func (g *guest) keyFile() string { return filepath.Join(g.keyDir(), "receipt.key") }

// resetMounts is a reboot: only the initramfs root is mounted.
func (g *guest) resetMounts() {
	mustWrite(g.t, g.cfg.MountInfo, "1 0 0:2 / / rw - rootfs rootfs rw\n")
}

func (g *guest) appendMount(majMin, root, dir, fstype, source string) {
	f, err := os.OpenFile(g.cfg.MountInfo, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		g.t.Fatal(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "30 1 %s %s %s rw,nodev - %s %s rw\n", majMin, root, strings.ReplaceAll(dir, " ", `\040`), fstype, source); err != nil {
		g.t.Fatal(err)
	}
}

func (g *guest) keyDir() string { return filepath.Join(g.cfg.DataDir, keyDirName) }

func (g *guest) key() []byte {
	b, err := os.ReadFile(g.keyFile())
	if err != nil {
		g.t.Fatalf("read key: %v", err)
	}
	return b
}

func (g *guest) assertNoKey() {
	g.t.Helper()
	if _, err := os.Lstat(g.keyDir()); !errors.Is(err, os.ErrNotExist) {
		g.t.Fatalf("a key directory exists although boot refused: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(g.cfg.RunDir, receiptReady)); !errors.Is(err, os.ErrNotExist) {
		g.t.Fatal("receipt readiness was published although boot refused")
	}
}

// assertNoKeyLeak checks no error or log line carries the key's bytes.
func (g *guest) assertNoKeyLeak(err error) {
	g.t.Helper()
	b, rerr := os.ReadFile(g.keyFile())
	if rerr != nil || len(b) == 0 {
		return
	}
	for _, enc := range []string{string(b), hex.EncodeToString(b), base64.StdEncoding.EncodeToString(b)} {
		if err != nil && strings.Contains(err.Error(), enc) {
			g.t.Fatal("an error exposes the receipt key")
		}
		for _, l := range g.logs {
			if strings.Contains(l, enc) {
				g.t.Fatal("a log line exposes the receipt key")
			}
		}
	}
}

func TestBoot_NewVolumeCreatesKeyThenReusesItAcrossReboots(t *testing.T) {
	g := newGuest(t)
	g.contract()
	fp, err := g.boot()
	if err != nil {
		t.Fatal(err)
	}
	key := g.key()
	if fp != workerkey.Fingerprint(key) || g.auth.recorded != fp || len(g.auth.sent) != 1 {
		t.Fatalf("recorded %q after %d sends, want %s once", g.auth.recorded, len(g.auth.sent), fp)
	}
	// The store has since written data; a recorded key still reloads.
	mustMkdir(t, filepath.Join(g.cfg.DataDir, StoreDirName))
	for boot := 2; boot <= 3; boot++ {
		g.resetMounts()
		got, err := g.boot()
		if err != nil || got != fp || string(g.key()) != string(key) {
			t.Fatalf("boot %d = %q, %v; the receipt key must not change", boot, got, err)
		}
	}
	if len(g.auth.sent) != 1 {
		t.Fatalf("a recorded fingerprint was sent again: %d sends", len(g.auth.sent))
	}
	if g.disk.mounts != 3 {
		t.Fatalf("mounts = %d, want one per boot", g.disk.mounts)
	}
	published, err := os.ReadFile(filepath.Join(g.cfg.RunDir, receiptReady))
	if err != nil || strings.TrimSpace(string(published)) != fp {
		t.Fatalf("published fingerprint = %q, %v", published, err)
	}
	g.assertNoKeyLeak(nil)
}

// Once the platform holds a fingerprint, losing the disk's key never yields a
// new one; the boot refuses without asking again.
func TestBoot_KeyLostAfterRecordingIsNeverReplaced(t *testing.T) {
	g := newGuest(t)
	g.contract()
	g.recordKey()
	mustRemove(t, g.keyFile())
	_, err := g.boot()
	if !errors.Is(err, workerkey.ErrReceiptKeyMissing) {
		t.Fatalf("err = %v, want ErrReceiptKeyMissing", err)
	}
	if _, err := os.Lstat(g.keyFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a replacement key was created for a recorded volume")
	}
	if len(g.auth.sent) != 0 || g.auth.claims != 1 {
		t.Fatalf("sends=%d claims=%d; a final refusal must not be retried or answered", len(g.auth.sent), g.auth.claims)
	}
}

func TestBoot_RecordedKeyMismatchRefuses(t *testing.T) {
	g := newGuest(t)
	g.contract()
	key := g.recordKey()
	g.auth.recorded = workerkey.Fingerprint(append([]byte("x"), key[1:]...))
	_, err := g.boot()
	if !errors.Is(err, workerkey.ErrKeyMismatch) {
		t.Fatalf("err = %v, want ErrKeyMismatch", err)
	}
	if string(g.key()) != string(key) || len(g.auth.sent) != 0 {
		t.Fatal("a mismatch changed or sent the receipt key")
	}
	g.assertNoKeyLeak(err)
}

// A pending decision on a disk that already holds the store's directory
// creates no key: the platform and the disk disagree about a new volume.
func TestBoot_PendingWithExistingStoreDataCreatesNoKey(t *testing.T) {
	g := newGuest(t)
	g.contract()
	g.appendMount(testMajMin, "/", g.cfg.DataDir, "ext4", "/dev/vda")
	mustMkdir(t, filepath.Join(g.cfg.DataDir, StoreDirName))
	if _, err := g.boot(); !errors.Is(err, workerkey.ErrDiskHasData) {
		t.Fatalf("err = %v, want ErrDiskHasData", err)
	}
	g.assertNoKey()
}

// A reply lost after recording is retried with the same key: one key,
// recorded once.
func TestBoot_LostRecordReplyKeepsTheSameKey(t *testing.T) {
	for _, landed := range []bool{true, false} {
		t.Run(fmt.Sprintf("landed=%v", landed), func(t *testing.T) {
			g := newGuest(t)
			g.contract()
			g.auth.recordErrs = []recordErr{{err: transient{}, landed: landed}}
			fp, err := g.boot()
			if err != nil {
				t.Fatal(err)
			}
			if g.auth.recorded != fp || g.auth.claims != 2 {
				t.Fatalf("recorded=%q claims=%d", g.auth.recorded, g.auth.claims)
			}
			for _, sent := range g.auth.sent {
				if sent != fp {
					t.Fatal("a second key was recorded")
				}
			}
		})
	}
}

func TestBoot_RetriesOnlyRetryableRefusals(t *testing.T) {
	t.Run("transient then decided", func(t *testing.T) {
		g := newGuest(t)
		g.contract()
		g.auth.claimErrs = []error{transient{}, transient{}}
		if _, err := g.boot(); err != nil {
			t.Fatal(err)
		}
		if g.auth.claims != 3 {
			t.Fatalf("claims = %d, want 3", g.auth.claims)
		}
	})
	t.Run("exhausted", func(t *testing.T) {
		g := newGuest(t)
		g.contract()
		g.auth.claimErrs = []error{transient{}, transient{}, transient{}, transient{}}
		if _, err := g.boot(); err == nil {
			t.Fatal("boot passed without a decision")
		}
		if g.auth.claims != 3 {
			t.Fatalf("claims = %d, want the 3 attempts", g.auth.claims)
		}
		g.assertNoKey()
	})
	t.Run("final", func(t *testing.T) {
		g := newGuest(t)
		g.contract()
		denied := errors.New("AccessDenied")
		g.auth.claimErrs = []error{denied}
		if _, err := g.boot(); !errors.Is(err, denied) {
			t.Fatalf("err = %v, want the denial", err)
		}
		if g.auth.claims != 1 {
			t.Fatalf("claims = %d; a denial must not be retried", g.auth.claims)
		}
		g.assertNoKey()
	})
	t.Run("pending then authorized", func(t *testing.T) {
		g := newGuest(t)
		g.contract()
		g.auth.claimErrs = []error{notBoundError{}, notBoundError{}}
		if _, err := g.boot(); err != nil {
			t.Fatal(err)
		}
		if g.auth.claims != 3 {
			t.Fatalf("claims = %d, want 3", g.auth.claims)
		}
	})
	t.Run("pending never authorized", func(t *testing.T) {
		g := newGuest(t)
		g.contract()
		g.auth.claimErrs = []error{notBoundError{}, notBoundError{}, notBoundError{}, notBoundError{}}
		if _, err := g.boot(); !errors.Is(err, notBoundError{}) {
			t.Fatalf("err = %v, want the denial", err)
		}
		if g.auth.claims != 3 {
			t.Fatalf("claims = %d, want the 3 attempts", g.auth.claims)
		}
		g.assertNoKey()
	})
	t.Run("cancelled while waiting", func(t *testing.T) {
		g := newGuest(t)
		g.contract()
		g.cfg.Wait = time.Hour
		g.auth.claimErrs = []error{transient{}}
		ctx, cancel := context.WithCancel(context.Background())
		g.cfg.Log = func(string) { cancel() }
		if _, err := Boot(ctx, g.cfg); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		g.assertNoKey()
	})
}

// The gateway is asked only once the worker disk is proven mounted, so a
// disk refusal never reaches it and never creates a key.
func TestBoot_DiskRefusalsNeverAskTheGateway(t *testing.T) {
	cases := []struct {
		name  string
		setup func(g *guest)
		want  string
	}{
		{"wrong serial", func(g *guest) { g.fw("data-serial", "vol0ffffffffffffffff") }, "found 0"},
		{"no disk", func(g *guest) {
			if err := os.RemoveAll(filepath.Join(g.cfg.SysBlock, "vda")); err != nil {
				t.Fatal(err)
			}
		}, "found 0"},
		{"ambiguous", func(g *guest) { g.addDisk("vdb", testSerial, "253:32") }, "found 2"},
		{"no device node", func(g *guest) {
			if err := os.Remove(filepath.Join(g.cfg.DevDir, "vda")); err != nil {
				t.Fatal(err)
			}
		}, "no block device node"},
		{"check fails", func(g *guest) { g.disk.checkErr = errors.New("e2fsck refused") }, "e2fsck refused"},
		{"mount fails", func(g *guest) { g.disk.mountErr = errors.New("invalid argument") }, "invalid argument"},
		{"empty mountpoint", func(g *guest) { g.disk.noopMount = true }, "is not mounted at"},
		{"foreign device", func(g *guest) { g.appendMount("253:48", "/", g.cfg.DataDir, "ext4", "x") }, "not the worker disk"},
		{"bind of a subdirectory", func(g *guest) { g.appendMount(testMajMin, "/sub", g.cfg.DataDir, "ext4", "x") }, "not as a whole ext4 filesystem"},
		{"tmpfs", func(g *guest) { g.appendMount("0:40", "/", g.cfg.DataDir, "tmpfs", "x") }, "not as a whole ext4 filesystem"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGuest(t)
			g.contract()
			tc.setup(g)
			_, err := g.boot()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if g.auth.builds != 0 || g.auth.claims != 0 {
				t.Fatal("the gateway was asked before the disk was proven")
			}
			g.assertNoKey()
		})
	}
}

func TestBoot_AlreadyMountedWorkerDiskIsReused(t *testing.T) {
	g := newGuest(t)
	g.contract()
	g.recordKey()
	g.appendMount(testMajMin, "/", g.cfg.DataDir, "ext4", "/dev/vda")
	if _, err := g.boot(); err != nil {
		t.Fatal(err)
	}
	if g.disk.checks != 0 || g.disk.mounts != 0 {
		t.Fatal("a mounted worker disk was checked or mounted again")
	}
}

func TestBoot_ContractRefusalsTouchNothing(t *testing.T) {
	cases := []struct {
		name  string
		setup func(g *guest)
		want  string
	}{
		{"no serial", func(g *guest) { mustRemove(t, filepath.Join(g.cfg.FwCfgDir, "data-serial")) }, "data-serial not supplied"},
		{"bad serial", func(g *guest) { g.fw("data-serial", "vol-0a1b2c3d4e5f6071") }, "not a volume serial"},
		{"no region", func(g *guest) { mustRemove(t, filepath.Join(g.cfg.FwCfgDir, "region")) }, "region not supplied"},
		{"empty region", func(g *guest) { g.fw("region", "") }, "exactly one value"},
		{"bad region", func(g *guest) { g.fw("region", "Sydney") }, "not a Region name"},
		{"no gateway", func(g *guest) { mustRemove(t, filepath.Join(g.cfg.FwCfgDir, "gateway-url")) }, "gateway-url not supplied"},
		{"plain http gateway", func(g *guest) { g.fw("gateway-url", "http://10.15.8.1:9999") }, "not an https URL"},
		{"no claimer", func(g *guest) { g.cfg.NewClaimer = nil }, "no gateway client"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGuest(t)
			g.contract()
			tc.setup(g)
			_, err := g.boot()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if g.auth.claims != 0 {
				t.Fatal("a refused contract asked the gateway")
			}
			g.assertNoKey()
		})
	}
}

func TestBoot_ClaimerBuildFailureCreatesNoKey(t *testing.T) {
	g := newGuest(t)
	g.contract()
	g.auth.buildErr = errors.New("no IMDS credentials")
	if _, err := g.boot(); err == nil || !strings.Contains(err.Error(), "no IMDS credentials") {
		t.Fatalf("err = %v", err)
	}
	g.assertNoKey()
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

func TestMountedDevice_UsesTopmostMountAndUnescapes(t *testing.T) {
	g := newGuest(t)
	dir := filepath.Join(g.root, "data dir")
	g.appendMount("253:48", "/", dir, "ext4", "/dev/vdb")
	g.appendMount(testMajMin, "/", dir, "ext4", "/dev/vda")
	got, err := mountedDevice(g.cfg.MountInfo, dir)
	if err != nil || got != testMajMin {
		t.Fatalf("mountedDevice = %q, %v; want %s", got, err, testMajMin)
	}
}
