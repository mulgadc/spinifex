package workerboot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerkey"
	"github.com/nats-io/nkeys"
)

const (
	testSerial = "vol0a1b2c3d4e5f60718"
	testMajMin = "253:16"
)

// guest is a fake guest: fw_cfg, sysfs, /dev and a mount table the fake disk
// edits the way a real mount would.
type guest struct {
	t    *testing.T
	root string
	cfg  Config
	disk *fakeDisk
	logs []string
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
	g := &guest{t: t, root: t.TempDir()}
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
		Log:           func(msg string) { g.logs = append(g.logs, msg) },
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

func (g *guest) newStore() { g.fw("store-status", "new\n") }

func (g *guest) existingStore(pub string) {
	g.fw("store-status", "existing\n")
	g.fw("store-nkey", pub+"\n")
}

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

func TestBoot_NewStoreKeySurvivesReboot(t *testing.T) {
	g := newGuest(t)
	g.fw("data-serial", testSerial+"\n")
	g.newStore()
	pub, err := Boot(g.cfg)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if g.disk.checks != 1 || g.disk.mounts != 1 {
		t.Fatalf("checks=%d mounts=%d, want one each", g.disk.checks, g.disk.mounts)
	}
	published, err := os.ReadFile(filepath.Join(g.cfg.RunDir, "public-key"))
	if err != nil || strings.TrimSpace(string(published)) != pub {
		t.Fatalf("published key = %q, %v; want %s", published, err, pub)
	}
	seed := g.seed()
	g.assertNoSeedLeak(nil, seed)

	// Reboot into the now-existing store, and again with a stale "new".
	for _, status := range []func(){func() { g.existingStore(pub) }, g.newStore} {
		g.resetMounts()
		_ = os.Remove(filepath.Join(g.cfg.FwCfgDir, "store-nkey", "raw"))
		status()
		again, err := Boot(g.cfg)
		if err != nil {
			t.Fatalf("reboot: %v", err)
		}
		if again != pub || g.seed() != seed {
			t.Fatal("reboot changed the worker key")
		}
	}
	g.assertNoSeedLeak(nil, seed)
}

func TestBoot_EmptyMountpointCreatesNoKey(t *testing.T) {
	g := newGuest(t)
	g.fw("data-serial", testSerial)
	g.newStore()
	g.disk.noopMount = true
	_, err := Boot(g.cfg)
	if err == nil || !strings.Contains(err.Error(), "is not mounted at") {
		t.Fatalf("err = %v, want not-mounted refusal", err)
	}
	g.assertNoKey()
}

func TestBoot_DiskSelectionRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(g *guest)
		want  string
	}{
		{"wrong serial", func(g *guest) { g.fw("data-serial", "vol0ffffffffffffffff") }, "found 0"},
		{"no disk", func(g *guest) {
			g.fw("data-serial", testSerial)
			if err := os.RemoveAll(filepath.Join(g.cfg.SysBlock, "vda")); err != nil {
				t.Fatal(err)
			}
		}, "found 0"},
		{"ambiguous", func(g *guest) {
			g.fw("data-serial", testSerial)
			g.addDisk("vdb", testSerial, "253:32")
		}, "found 2"},
		{"no device node", func(g *guest) {
			g.fw("data-serial", testSerial)
			if err := os.Remove(filepath.Join(g.cfg.DevDir, "vda")); err != nil {
				t.Fatal(err)
			}
		}, "no block device node"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGuest(t)
			g.newStore()
			tc.setup(g)
			_, err := Boot(g.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if g.disk.checks != 0 || g.disk.mounts != 0 {
				t.Fatal("a refused disk was checked or mounted")
			}
			g.assertNoKey()
		})
	}
}

func TestBoot_CheckOrMountFailureCreatesNoKey(t *testing.T) {
	t.Run("check", func(t *testing.T) {
		g := newGuest(t)
		g.fw("data-serial", testSerial)
		g.newStore()
		g.disk.checkErr = errors.New("e2fsck refused")
		if _, err := Boot(g.cfg); err == nil {
			t.Fatal("boot passed a disk that failed its check")
		}
		if g.disk.mounts != 0 {
			t.Fatal("a disk that failed its check was mounted")
		}
		g.assertNoKey()
	})
	t.Run("mount", func(t *testing.T) {
		g := newGuest(t)
		g.fw("data-serial", testSerial)
		g.newStore()
		g.disk.mountErr = errors.New("invalid argument")
		if _, err := Boot(g.cfg); err == nil {
			t.Fatal("boot passed a failed mount")
		}
		g.assertNoKey()
	})
}

func TestBoot_ForeignMountAtDataDirRefuses(t *testing.T) {
	cases := []struct {
		name, majMin, root, fstype, want string
	}{
		{"other device", "253:48", "/", "ext4", "not the worker disk"},
		{"bind of a subdirectory", testMajMin, "/sub", "ext4", "not as a whole ext4 filesystem"},
		{"tmpfs", "0:40", "/", "tmpfs", "not as a whole ext4 filesystem"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGuest(t)
			g.fw("data-serial", testSerial)
			g.newStore()
			g.appendMount(tc.majMin, tc.root, g.cfg.DataDir, tc.fstype, "x")
			_, err := Boot(g.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if g.disk.mounts != 0 {
				t.Fatal("mounted over a foreign mount")
			}
		})
	}
}

func TestBoot_AlreadyMountedWorkerDiskIsReused(t *testing.T) {
	g := newGuest(t)
	g.fw("data-serial", testSerial)
	g.newStore()
	g.appendMount(testMajMin, "/", g.cfg.DataDir, "ext4", "/dev/vda")
	if _, err := Boot(g.cfg); err != nil {
		t.Fatal(err)
	}
	if g.disk.checks != 0 || g.disk.mounts != 0 {
		t.Fatal("a mounted worker disk was checked or mounted again")
	}
}

func TestBoot_ContractRefusalsTouchNoDisk(t *testing.T) {
	pub, seed := userKey(t)
	cases := []struct {
		name  string
		setup func(g *guest)
		want  string
	}{
		{"no serial", func(g *guest) { g.newStore() }, "data-serial not supplied"},
		{"bad serial", func(g *guest) { g.fw("data-serial", "vol-0a1b2c3d4e5f6071"); g.newStore() }, "not a volume serial"},
		{"status absent", func(g *guest) { g.fw("data-serial", testSerial) }, "store-status not supplied"},
		{"status empty", func(g *guest) { g.fw("data-serial", testSerial); g.fw("store-status", "") }, "exactly one value"},
		{"status unknown", func(g *guest) { g.fw("data-serial", testSerial); g.fw("store-status", "unknown") }, `"new" or "existing"`},
		{"existing without key", func(g *guest) { g.fw("data-serial", testSerial); g.fw("store-status", "existing") }, "store-nkey not supplied"},
		{"existing with empty key", func(g *guest) {
			g.fw("data-serial", testSerial)
			g.fw("store-status", "existing")
			g.fw("store-nkey", "")
		}, "exactly one value"},
		{"existing with seed as key", func(g *guest) { g.fw("data-serial", testSerial); g.existingStore(seed) }, "not a user public NKey"},
		{"new with a key", func(g *guest) {
			g.fw("data-serial", testSerial)
			g.newStore()
			g.fw("store-nkey", pub)
		}, "must not be supplied"},
		{"new with an empty key", func(g *guest) {
			g.fw("data-serial", testSerial)
			g.newStore()
			g.fw("store-nkey", "")
		}, "must not be supplied"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGuest(t)
			tc.setup(g)
			_, err := Boot(g.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			g.assertNoSeedLeak(err, seed)
			if g.disk.checks != 0 || g.disk.mounts != 0 {
				t.Fatal("a refused contract still touched the disk")
			}
			g.assertNoKey()
		})
	}
}

func TestBoot_ExistingStoreKeyMustBePresentAndMatch(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		g := newGuest(t)
		pub, _ := userKey(t)
		g.fw("data-serial", testSerial)
		g.existingStore(pub)
		if _, err := Boot(g.cfg); !errors.Is(err, workerkey.ErrKeyMissing) {
			t.Fatalf("err = %v, want ErrKeyMissing", err)
		}
		if _, err := os.Lstat(filepath.Join(g.keyDir(), "store-client.nk")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("a key was generated for an existing store")
		}
	})
	t.Run("mismatch", func(t *testing.T) {
		g := newGuest(t)
		g.fw("data-serial", testSerial)
		g.newStore()
		if _, err := Boot(g.cfg); err != nil {
			t.Fatal(err)
		}
		seed := g.seed()
		other, _ := userKey(t)
		g.resetMounts()
		g.existingStore(other)
		_, err := Boot(g.cfg)
		if !errors.Is(err, workerkey.ErrKeyMismatch) {
			t.Fatalf("err = %v, want ErrKeyMismatch", err)
		}
		g.assertNoSeedLeak(err, seed)
		if g.seed() != seed {
			t.Fatal("a mismatch replaced the worker key")
		}
	})
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
