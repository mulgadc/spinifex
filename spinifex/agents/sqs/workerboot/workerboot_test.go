package workerboot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerkey"
	"github.com/nats-io/nkeys"
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

// authority stands for the domain's first-store record behind the gateway:
// it authorizes creation until a key is recorded, then only that key.
type authority struct {
	recorded  string
	claimErrs []error
	// recordErrs fail RecordPublicKey; landed ones record before failing,
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

func (a *authority) Claim(context.Context) (bool, string, error) {
	a.claims++
	if len(a.claimErrs) > 0 {
		err := a.claimErrs[0]
		a.claimErrs = a.claimErrs[1:]
		return false, "", err
	}
	return a.recorded == "", a.recorded, nil
}

func (a *authority) RecordPublicKey(_ context.Context, pub string) error {
	a.sent = append(a.sent, pub)
	if a.recorded != "" && a.recorded != pub {
		return errors.New("InvalidParameterValue")
	}
	if len(a.recordErrs) > 0 {
		e := a.recordErrs[0]
		a.recordErrs = a.recordErrs[1:]
		if e.landed {
			a.recorded = pub
		}
		return e.err
	}
	a.recorded = pub
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

// assertSentOnlyPublicKeys checks nothing but valid public keys left the guest.
func (g *guest) assertSentOnlyPublicKeys(seed string) {
	g.t.Helper()
	for _, v := range g.auth.sent {
		if !nkeys.IsValidPublicUserKey(v) || (seed != "" && strings.Contains(v, seed)) {
			g.t.Fatal("the guest sent something other than a public key")
		}
	}
}

// seedKey stands for a worker key created when the store was first made and
// recorded by the domain; it returns that key's public half.
func (g *guest) seedKey() string {
	g.t.Helper()
	kp, err := workerkey.Store{Dir: g.keyDir()}.ForNewStore()
	if err != nil {
		g.t.Fatalf("seed worker key: %v", err)
	}
	defer kp.Wipe()
	pub, err := kp.PublicKey()
	if err != nil {
		g.t.Fatal(err)
	}
	g.auth.recorded = pub
	return pub
}

func (g *guest) keyFile() string { return filepath.Join(g.keyDir(), "store-client.nk") }

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

func (g *guest) seed() string {
	b, err := os.ReadFile(filepath.Join(g.keyDir(), "store-client.nk"))
	if err != nil {
		g.t.Fatalf("read key: %v", err)
	}
	return string(b)
}

func (g *guest) assertNoKey() {
	g.t.Helper()
	if _, err := os.Lstat(g.keyDir()); !errors.Is(err, os.ErrNotExist) {
		g.t.Fatalf("a key directory exists although boot refused: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(g.cfg.RunDir, "public-key")); !errors.Is(err, os.ErrNotExist) {
		g.t.Fatal("a public key was published although boot refused")
	}
}

func (g *guest) assertNoSeedLeak(err error, seed string) {
	g.t.Helper()
	if seed == "" {
		return
	}
	if err != nil && strings.Contains(err.Error(), seed) {
		g.t.Fatal("an error exposes the seed")
	}
	for _, l := range g.logs {
		if strings.Contains(l, seed) {
			g.t.Fatal("a log line exposes the seed")
		}
	}
}

// otherPub is a valid store key the worker does not hold.
func otherPub(t *testing.T) string {
	t.Helper()
	pub, _ := userKey(t)
	return pub
}

func userKey(t *testing.T) (pub, seed string) {
	t.Helper()
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	p, err := kp.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	s, err := kp.Seed()
	if err != nil {
		t.Fatal(err)
	}
	return p, string(s)
}

func TestBoot_FirstStoreCreatesKeyThenReusesItAcrossReboots(t *testing.T) {
	g := newGuest(t)
	g.contract()
	pub, err := g.boot()
	if err != nil {
		t.Fatal(err)
	}
	seed := g.seed()
	if g.auth.recorded != pub || len(g.auth.sent) != 1 {
		t.Fatalf("recorded %q after %d sends, want %s once", g.auth.recorded, len(g.auth.sent), pub)
	}
	for boot := 2; boot <= 3; boot++ {
		g.resetMounts()
		got, err := g.boot()
		if err != nil || got != pub || g.seed() != seed {
			t.Fatalf("boot %d = %q, %v; the worker key must not change", boot, got, err)
		}
	}
	if len(g.auth.sent) != 1 {
		t.Fatalf("a recorded key was sent again: %d sends", len(g.auth.sent))
	}
	if g.disk.mounts != 3 {
		t.Fatalf("mounts = %d, want one per boot", g.disk.mounts)
	}
	published, err := os.ReadFile(filepath.Join(g.cfg.RunDir, "public-key"))
	if err != nil || strings.TrimSpace(string(published)) != pub {
		t.Fatalf("published key = %q, %v", published, err)
	}
	g.assertSentOnlyPublicKeys(seed)
	g.assertNoSeedLeak(nil, seed)
}

// Once the domain holds a key, losing the guest's copy never yields a new
// one, whatever a replayed first-start fw_cfg says.
func TestBoot_KeyLostAfterRecordingIsNeverReplaced(t *testing.T) {
	g := newGuest(t)
	g.contract()
	g.seedKey()
	if err := os.Remove(g.keyFile()); err != nil {
		t.Fatal(err)
	}
	g.fw("store-status", "new\n")
	g.resetMounts()
	_, err := g.boot()
	if !errors.Is(err, workerkey.ErrKeyMissing) {
		t.Fatalf("err = %v, want ErrKeyMissing", err)
	}
	if _, err := os.Lstat(g.keyFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a replacement key was created for an existing store")
	}
	if len(g.auth.sent) != 0 || g.auth.claims != 1 {
		t.Fatalf("sends=%d claims=%d; a final refusal must not be retried or answered", len(g.auth.sent), g.auth.claims)
	}
	if _, err := os.Lstat(filepath.Join(g.cfg.RunDir, "public-key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a public key was published")
	}
}

func TestBoot_RecordedKeyMismatchRefuses(t *testing.T) {
	g := newGuest(t)
	g.contract()
	g.seedKey()
	seed := g.seed()
	g.auth.recorded = otherPub(t)
	_, err := g.boot()
	if !errors.Is(err, workerkey.ErrKeyMismatch) {
		t.Fatalf("err = %v, want ErrKeyMismatch", err)
	}
	if g.seed() != seed || len(g.auth.sent) != 0 {
		t.Fatal("a mismatch changed or sent the worker key")
	}
	g.assertNoSeedLeak(err, seed)
}

// A reply lost after the key was recorded is retried, and the retry is
// answered with that same key: one key, recorded once.
func TestBoot_LostRecordReplyKeepsTheSameKey(t *testing.T) {
	for _, landed := range []bool{true, false} {
		t.Run(fmt.Sprintf("landed=%v", landed), func(t *testing.T) {
			g := newGuest(t)
			g.contract()
			g.auth.recordErrs = []recordErr{{err: transient{}, landed: landed}}
			pub, err := g.boot()
			if err != nil {
				t.Fatal(err)
			}
			if g.auth.recorded != pub || g.auth.claims != 2 {
				t.Fatalf("recorded=%q claims=%d", g.auth.recorded, g.auth.claims)
			}
			for _, sent := range g.auth.sent {
				if sent != pub {
					t.Fatal("a second key was sent")
				}
			}
			g.assertSentOnlyPublicKeys(g.seed())
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
	g.seedKey()
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
