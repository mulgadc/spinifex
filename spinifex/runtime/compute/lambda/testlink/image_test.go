//go:build lambdatestlink

package testlink

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

var testImageBytes = map[string][]byte{FileKernel: []byte("kernel"), FileInitrd: []byte("initramfs"), FileFirmware: []byte("firmware")}

// testImageDigest was computed independently of this package for testImageBytes.
const testImageDigest = "e4784145d0ab779740de46394eb95823159428bfdb57d3b3ee5728b387692bc1"

func writeImage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, b := range testImageBytes {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), b, 0o600))
	}
	return dir
}

// preparedImage is a prepared copy of testImageBytes, closed at test end.
func preparedImage(t *testing.T) *Image {
	t.Helper()
	im, err := PrepareImage(writeImage(t), testImageDigest)
	require.NoError(t, err)
	t.Cleanup(im.Close)
	return im
}

func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	require.NoError(t, err)
	return len(ents)
}

func sealedBytes(t *testing.T, f *os.File) []byte {
	t.Helper()
	b, err := io.ReadAll(io.NewSectionReader(f, 0, 1<<20))
	require.NoError(t, err)
	return b
}

func TestImageDigest_FixedVector(t *testing.T) {
	sums := map[string]FileSum{}
	dir := writeImage(t)
	for name, path := range ImagePaths(dir) {
		s, err := SumFile(name, path)
		require.NoError(t, err)
		sums[name] = s
	}
	got, err := ImageDigest(sums)
	require.NoError(t, err)
	require.Equal(t, testImageDigest, got)
}

func TestPrepareImage_SealsExactBytesUnderThePin(t *testing.T) {
	dir := writeImage(t)
	im, err := PrepareImage(dir, testImageDigest)
	require.NoError(t, err)
	defer im.Close()
	require.Equal(t, testImageDigest, im.Digest)
	// Every disk file replaced after preparation leaves the sealed copies alone.
	for name := range testImageBytes {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("replaced"), 0o600))
	}
	require.Equal(t, testImageBytes[FileFirmware], sealedBytes(t, im.Firmware))
	require.Equal(t, testImageBytes[FileKernel], sealedBytes(t, im.Kernel))
	require.Equal(t, testImageBytes[FileInitrd], sealedBytes(t, im.Initrd))
	for _, f := range []*os.File{im.Firmware, im.Kernel, im.Initrd} {
		_, err := f.WriteAt([]byte("x"), 0)
		require.Error(t, err, "a sealed copy must refuse writes")
	}
}

func TestPrepareImage_RefusesAndLeavesNothingOpen(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string) string{
		"wrong pin":     func(_ *testing.T, _ string) string { return strings.Repeat("0", 64) },
		"malformed pin": func(_ *testing.T, _ string) string { return "ABC" },
		"changed kernel byte": func(t *testing.T, dir string) string {
			require.NoError(t, os.WriteFile(filepath.Join(dir, FileKernel), []byte("kernex"), 0o600))
			return testImageDigest
		},
		"changed initramfs byte": func(t *testing.T, dir string) string {
			require.NoError(t, os.WriteFile(filepath.Join(dir, FileInitrd), []byte("initramfx"), 0o600))
			return testImageDigest
		},
		"changed firmware byte": func(t *testing.T, dir string) string {
			require.NoError(t, os.WriteFile(filepath.Join(dir, FileFirmware), []byte("firmwarx"), 0o600))
			return testImageDigest
		},
		"missing firmware": func(t *testing.T, dir string) string {
			require.NoError(t, os.Remove(filepath.Join(dir, FileFirmware)))
			return testImageDigest
		},
		"symlinked kernel": func(t *testing.T, dir string) string {
			p := filepath.Join(dir, FileKernel)
			require.NoError(t, os.Rename(p, p+".real"))
			require.NoError(t, os.Symlink(p+".real", p))
			return testImageDigest
		},
		"directory as initramfs": func(t *testing.T, dir string) string {
			p := filepath.Join(dir, FileInitrd)
			require.NoError(t, os.Remove(p))
			require.NoError(t, os.Mkdir(p, 0o700))
			return testImageDigest
		},
		"FIFO as initramfs": func(t *testing.T, dir string) string {
			p := filepath.Join(dir, FileInitrd)
			require.NoError(t, os.Remove(p))
			require.NoError(t, unix.Mkfifo(p, 0o600))
			return testImageDigest
		},
		"firmware over its cap": func(t *testing.T, dir string) string {
			require.NoError(t, os.Truncate(filepath.Join(dir, FileFirmware), maxImageFile[FileFirmware]+1))
			return testImageDigest
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeImage(t)
			pin := setup(t, dir)
			before := openFDs(t)
			done := make(chan error, 1)
			go func() {
				im, err := PrepareImage(dir, pin)
				im.Close()
				done <- err
			}()
			select {
			case err := <-done:
				require.ErrorIs(t, err, ErrImage)
			case <-time.After(10 * time.Second):
				t.Fatal("preparation blocked")
			}
			require.Equal(t, before, openFDs(t), "a refused preparation leaves a descriptor open")
		})
	}
}

// readResult is a reader whose single read returns n bytes and err.
type readResult struct {
	n   int
	err error
}

func (r readResult) Read(p []byte) (int, error) { return r.n, r.err }

// copyExact passes only on exactly n bytes followed by a clean end of file.
func TestCopyExact_OnlyExactLengthAndCleanEndPass(t *testing.T) {
	require.NoError(t, copyExact(io.Discard, strings.NewReader("abc"), 3))
	require.Error(t, copyExact(io.Discard, strings.NewReader("abcd"), 3), "file grew")
	require.Error(t, copyExact(io.Discard, strings.NewReader("ab"), 3), "file shrank")
	require.Error(t, copyExact(io.Discard, io.MultiReader(strings.NewReader("abc"), readResult{1, io.EOF}), 3))
	require.Error(t, copyExact(io.Discard, io.MultiReader(strings.NewReader("abc"), readResult{0, errors.New("eio")}), 3))
	require.Error(t, copyExact(io.Discard, io.MultiReader(strings.NewReader("abc"), readResult{0, nil}), 3))
}

// The digest tool and the daemon select the same three files in one directory,
// so firmware outside it cannot change what boots.
func TestImagePaths_PairDigestToolAndDaemon(t *testing.T) {
	dir := writeImage(t)
	sums := map[string]FileSum{}
	for name, path := range ImagePaths(dir) {
		require.Equal(t, dir, filepath.Dir(path))
		s, err := SumFile(name, path)
		require.NoError(t, err)
		sums[name] = s
	}
	pin, err := ImageDigest(sums)
	require.NoError(t, err)
	im, err := PrepareImage(dir, pin)
	require.NoError(t, err)
	im.Close()
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(dir), FileFirmware), []byte("shared"), 0o600))
	im, err = PrepareImage(dir, pin)
	require.NoError(t, err, "a firmware file beside the directory is not part of the image")
	im.Close()
}

// The image fds are 3 to 5 and the hard-kill ack and initializer 6 and 7; an
// inherited file that would shift them is refused, and every launch reuses the
// daemon's still-open sealed copies.
func TestArmLaunch_DescriptorOrder(t *testing.T) {
	im := preparedImage(t)
	lib, err := os.CreateTemp(t.TempDir(), "lib")
	require.NoError(t, err)
	defer lib.Close()
	for range 2 {
		cmd := exec.Command("true")
		hk, err := armLaunch(cmd, im, lib, time.Hour)
		require.NoError(t, err)
		require.Equal(t, []*os.File{im.Firmware, im.Kernel, im.Initrd, hk.w, lib}, cmd.ExtraFiles)
		require.Contains(t, cmd.Env, "LD_PRELOAD=/proc/self/fd/7")
		require.Contains(t, cmd.Env, "SPX_HARDKILL_ACK_FD=6")
		require.NoError(t, cmd.Run())
		hk.close()
		for _, f := range []*os.File{im.Firmware, im.Kernel, im.Initrd} {
			var st unix.Stat_t
			require.NoError(t, unix.Fstat(int(f.Fd()), &st), "the daemon's sealed copy was closed by a launch")
		}
	}
	cmd := exec.Command("true")
	cmd.ExtraFiles = []*os.File{lib}
	_, err = armLaunch(cmd, im, lib, time.Hour)
	require.Error(t, err)
	_, err = armLaunch(exec.Command("true"), nil, lib, time.Hour)
	require.Error(t, err)
}

// The VM command's firmware, kernel and initramfs arguments name only the
// sealed-copy fds; whether QEMU boots them is shown only by a KVM run.
func TestPrepare_ImageArgumentsNameOnlyTheSealedFDs(t *testing.T) {
	req := baseRequest("https://example.invalid/code", []byte("zip"))
	req.Grant = execution.Grant{GrantID: "g-1", AttemptID: req.AttemptID, EnvironmentID: "env-x", Generation: 1, Node: testNode}
	cfg, _, ln, err := (&Daemon{HostIP: "127.0.0.1"}).prepare(req, []byte("zip"), filepath.Join(t.TempDir(), "a"), "tap0", "02:00:00:00:00:01", "10.0.0.2")
	require.NoError(t, err)
	defer ln.Close()
	cmd, err := cfg.Execute()
	require.NoError(t, err)
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"-bios /proc/self/fd/3", "-kernel /proc/self/fd/4", "-initrd /proc/self/fd/5"} {
		require.Contains(t, args, want)
	}
}

// Without a prepared image a request is refused before the slot or the grant
// is touched.
func TestHandle_NoPreparedImageFailsClosed(t *testing.T) {
	code := []byte("zip bytes")
	srv := codeServer(t, code)
	g := grant(t, code)
	ips := &fakeIPs{}
	d := g.daemon(&Daemon{IPs: ips, Taps: &fakeTaps{}, HTTP: srv.Client(), WorkDir: t.TempDir()})
	d.Image = nil
	rep := d.Handle(context.Background(), g.request(srv.URL, code), testCred)
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Contains(t, rep.Error, "runtime image not prepared")
	require.False(t, d.busy.Load())
	require.Empty(t, ips.allocated)
	require.Equal(t, g.att.State, g.state(t))
}

// With the boot files removed after preparation and hardkill.so kept beside
// them, Handle still checks the initializer and builds the VM command; the
// fixed deadline stops it before QEMU would start.
func TestHandle_BootFilesRemovedAfterPreparation(t *testing.T) {
	code := []byte("zip bytes")
	srv := codeServer(t, code)
	g := grant(t, code)
	dir := writeImage(t)
	lib, err := os.ReadFile(g.lib)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hardkill.so"), lib, 0o600))
	im, err := PrepareImage(dir, testImageDigest)
	require.NoError(t, err)
	t.Cleanup(im.Close)
	for _, path := range ImagePaths(dir) {
		require.NoError(t, os.Remove(path))
	}
	ips, taps := &fakeIPs{}, &fakeTaps{}
	d := g.daemon(&Daemon{IPs: ips, Taps: taps, HTTP: srv.Client(), WorkDir: t.TempDir(), HostIP: "127.0.0.1"})
	d.Image, d.Initializer = im, filepath.Join(dir, "hardkill.so")
	req := g.request(srv.URL, code)
	var parsed Request
	require.NoError(t, json.Unmarshal(req, &parsed))
	stepBoot(t, time.Hour, hardDeadlineFrom(time.Hour, parsed))

	rep := d.Handle(context.Background(), req, testCred)
	require.Equal(t, OutcomeRefused, rep.Outcome)
	require.Equal(t, ErrDeadlineExpired.Error(), rep.Error, "refused for a reason other than the deadline")
	require.Len(t, taps.removed, 1)
	require.Equal(t, execution.StateEnded, g.state(t))
}

// An oversized boot file is refused from its fstat size before any memfd is
// created, so a huge or sparse file is never copied into memory.
func TestPrepare_OversizedFileRefusedBeforeAnyMemfd(t *testing.T) {
	dir := writeImage(t)
	require.NoError(t, os.Truncate(ImagePaths(dir)[FileFirmware], maxImageFile[FileFirmware]+1))
	calls := 0
	old := memfdCreate
	t.Cleanup(func() { memfdCreate = old })
	memfdCreate = func(name string, flags int) (int, error) {
		calls++
		return old(name, flags)
	}
	_, err := PrepareImage(dir, testImageDigest)
	require.ErrorIs(t, err, ErrImage)
	require.Contains(t, err.Error(), "outside 1..")
	require.Zero(t, calls, "memfd created before the size cap refused the file")
}

// The digest tool and the daemon select and hash the same three files from
// one directory, so the printed digest is the pin the daemon accepts.
func TestDirDigest_PairsWithPrepare(t *testing.T) {
	dir := writeImage(t)
	got, err := DirDigest(dir)
	require.NoError(t, err)
	require.Equal(t, testImageDigest, got)
	im, err := PrepareImage(dir, got)
	require.NoError(t, err)
	im.Close()

	require.NoError(t, os.Remove(ImagePaths(dir)[FileFirmware]))
	_, err = DirDigest(dir)
	require.ErrorIs(t, err, ErrImage)
	_, err = PrepareImage(dir, got)
	require.ErrorIs(t, err, ErrImage)
}

// Firmware comes only from the image directory: a firmware file beside it,
// where the shared microvm qboot.rom lives, changes nothing.
func TestDirDigest_IgnoresFirmwareOutsideTheDirectory(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "lambda-testlink")
	require.NoError(t, os.Mkdir(dir, 0o700))
	for name, b := range testImageBytes {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), b, 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(parent, FileFirmware), []byte("other firmware"), 0o600))
	got, err := DirDigest(dir)
	require.NoError(t, err)
	require.Equal(t, testImageDigest, got)
	im, err := PrepareImage(dir, testImageDigest)
	require.NoError(t, err)
	im.Close()
}

// Each loader step that can fail on a real kernel fails the kernel file, the
// second one sealed, so the firmware copy already made must be closed too.
func TestPrepareImage_LoaderStepFailuresLeaveNothingOpen(t *testing.T) {
	orig := memfdCreate
	cases := map[string]struct {
		create func(t *testing.T, dir, name string, flags int) (int, error)
		want   string
	}{
		"file grows after fstat": {func(t *testing.T, dir, name string, flags int) (int, error) {
			f, err := os.OpenFile(filepath.Join(dir, FileKernel), os.O_APPEND|os.O_WRONLY, 0)
			require.NoError(t, err)
			_, err = f.Write([]byte("x"))
			require.NoError(t, errors.Join(err, f.Close()))
			return orig(name, flags)
		}, "file longer than its size"},
		"file shrinks after fstat": {func(t *testing.T, dir, name string, flags int) (int, error) {
			require.NoError(t, os.Truncate(filepath.Join(dir, FileKernel), 1))
			return orig(name, flags)
		}, "copied"},
		"copy into the memfd fails": {func(t *testing.T, _, _ string, _ int) (int, error) {
			return unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		}, "copied"},
		"sealing refused": {func(t *testing.T, _, name string, _ int) (int, error) {
			return orig(name, unix.MFD_CLOEXEC)
		}, "seal memfd"},
		"sealed size differs": {func(t *testing.T, _, name string, flags int) (int, error) {
			fd, err := orig(name, flags)
			require.NoError(t, err)
			_, err = unix.Write(fd, []byte("x"))
			require.NoError(t, err)
			return fd, nil
		}, "sealed size differs"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeImage(t)
			calls := 0
			t.Cleanup(func() { memfdCreate = orig })
			memfdCreate = func(n string, flags int) (int, error) {
				calls++
				if calls == 2 {
					return tc.create(t, dir, n, flags)
				}
				return orig(n, flags)
			}
			before := openFDs(t)
			im, err := PrepareImage(dir, testImageDigest)
			require.ErrorIs(t, err, ErrImage)
			require.Nil(t, im)
			require.Contains(t, err.Error(), FileKernel)
			require.Contains(t, err.Error(), tc.want)
			require.Equal(t, 2, calls, "failure did not come from the kernel file")
			require.Equal(t, before, openFDs(t), "a failed step leaves a descriptor open")
		})
	}
}
