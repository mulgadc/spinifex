//go:build lambdatestlink

package daemon

import (
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/testlink"
)

// TestLambdaHardKillPin runs from make build-lambda-testlink with the same pin
// as the built daemon: the paired initializer passes and a changed one fails.
func TestLambdaHardKillPin(t *testing.T) {
	t.Parallel()
	lib := os.Getenv("LAMBDA_TESTLINK_INITIALIZER")
	if lambdaHardKillSHA256 == "" || lib == "" {
		t.Skip("run by make build-lambda-testlink with a pinned initializer")
	}
	if err := (&testlink.Daemon{Initializer: lib, InitializerSHA256: lambdaHardKillSHA256}).CheckInitializer(); err != nil {
		t.Fatalf("paired initializer refused: %v", err)
	}
	b, err := os.ReadFile(lib)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-1] ^= 0xff
	changed := filepath.Join(t.TempDir(), "hardkill.so")
	if err := os.WriteFile(changed, b, 0o600); err != nil {
		t.Fatal(err)
	}
	err = (&testlink.Daemon{Initializer: changed, InitializerSHA256: lambdaHardKillSHA256}).CheckInitializer()
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("changed initializer not refused by the pin: %v", err)
	}
}

// TestLambdaImageDigestPin runs from make build-lambda-testlink with the same
// ldflags as the built daemon: the pinned image digest arrives exactly as given.
func TestLambdaImageDigestPin(t *testing.T) {
	t.Parallel()
	want := os.Getenv("LAMBDA_IMAGE_DIGEST")
	if want == "" {
		t.Skip("run by make build-lambda-testlink with a pinned image digest")
	}
	if lambdaImageDigest != want || !testlink.ValidDigest(lambdaImageDigest) {
		t.Fatalf("pinned image digest %q, want %q", lambdaImageDigest, want)
	}
}

// A failed serving check refuses new invocations but recovery has already
// started, so an attempt launched before a restart is still cleaned up.
func TestLambdaTestLinkRecoveryStartsDespiteFailedServingCheck(t *testing.T) {
	t.Parallel()
	var recovered, served, released bool
	refused := errors.New("runtime image refused")
	err := runLambdaTestLink(
		func() { recovered = true },
		func() error {
			if !recovered {
				t.Fatal("serving check ran before recovery started")
			}
			return refused
		},
		func() error { served = true; return nil },
		func() { released = true },
	)
	if !errors.Is(err, refused) || !recovered || served || !released {
		t.Fatalf("err=%v recovered=%v served=%v released=%v", err, recovered, served, released)
	}
}

// What ready prepared is released when serving fails, and kept once it serves.
func TestLambdaTestLinkReleasesPreparedImageOnlyWhenNotServing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		serveErr error
	}{{"serve fails", errors.New("subscribe failed")}, {"serving", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			released := false
			err := runLambdaTestLink(func() {}, func() error { return nil },
				func() error { return tc.serveErr }, func() { released = true })
			if !errors.Is(err, tc.serveErr) || released != (tc.serveErr != nil) {
				t.Fatalf("err=%v released=%v", err, released)
			}
		})
	}
}

// readyFixture is a test-link daemon whose initializer and image both pass.
func readyFixture(t *testing.T) (*testlink.Daemon, string, string) {
	t.Helper()
	dir := t.TempDir()
	hdr := make([]byte, 64)
	copy(hdr, "\x7fELF\x02\x01\x01")
	binary.LittleEndian.PutUint16(hdr[16:], uint16(elf.ET_DYN))
	binary.LittleEndian.PutUint16(hdr[18:], uint16(elf.EM_X86_64))
	binary.LittleEndian.PutUint32(hdr[20:], 1)
	binary.LittleEndian.PutUint16(hdr[52:], 64)
	lib := filepath.Join(dir, "hardkill.so")
	if err := os.WriteFile(lib, hdr, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(hdr)
	for name, b := range map[string]string{"qboot.rom": "firmware", "vmlinuz": "kernel", "initramfs.cpio.gz": "initramfs"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pin, err := testlink.DirDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &testlink.Daemon{HostIP: "10.15.8.1", Initializer: lib, InitializerSHA256: hex.EncodeToString(sum[:])}, dir, pin
}

func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(ents)
}

// Each serving check refuses before the image is prepared, and a refused
// preparation leaves no descriptor open.
func TestLambdaTestLinkReadyRefusesWithNothingPrepared(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("the test link supports only amd64")
	}
	cases := map[string]func(tl *testlink.Daemon, dir string, pin *string, trust *error){
		"no bridge address": func(tl *testlink.Daemon, _ string, _ *string, _ *error) { tl.HostIP = "" },
		"initializer unset": func(tl *testlink.Daemon, _ string, _ *string, _ *error) { tl.InitializerSHA256 = "" },
		"download trust": func(_ *testlink.Daemon, _ string, _ *string, trust *error) {
			*trust = errors.New("cluster CA unreadable")
		},
		"image pin missing":   func(_ *testlink.Daemon, _ string, pin *string, _ *error) { *pin = "" },
		"image pin malformed": func(_ *testlink.Daemon, _ string, pin *string, _ *error) { *pin = strings.ToUpper(*pin) },
		"initializer changed": func(tl *testlink.Daemon, _ string, _ *string, _ *error) {
			tl.InitializerSHA256 = strings.Repeat("0", 64)
		},
		"image pin differs": func(_ *testlink.Daemon, _ string, pin *string, _ *error) { *pin = strings.Repeat("0", 64) },
		"image file missing": func(_ *testlink.Daemon, dir string, _ *string, _ *error) {
			if err := os.Remove(filepath.Join(dir, "vmlinuz")); err != nil {
				panic(err)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			tl, dir, pin := readyFixture(t)
			var trust error
			mutate(tl, dir, &pin, &trust)
			before := openFDs(t)
			if err := lambdaTestLinkReady(tl, dir, pin, trust); err == nil {
				t.Fatal("ready passed")
			}
			if tl.Image != nil || openFDs(t) != before {
				t.Fatalf("refusal left image=%v, fds %d -> %d", tl.Image != nil, before, openFDs(t))
			}
		})
	}
}

// A prepared image is kept while serving and fully closed when the
// subscription fails after readiness.
func TestLambdaTestLinkServeFailureClosesThePreparedImage(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("the test link supports only amd64")
	}
	for _, tc := range []struct {
		name     string
		serveErr error
	}{{"serve fails", errors.New("subscribe failed")}, {"serving", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			tl, dir, pin := readyFixture(t)
			before := openFDs(t)
			prepared := 0
			err := runLambdaTestLink(func() {},
				func() error { return lambdaTestLinkReady(tl, dir, pin, nil) },
				func() error { prepared = openFDs(t) - before; return tc.serveErr },
				func() { releaseLambdaTestLinkImage(tl) })
			if !errors.Is(err, tc.serveErr) || prepared != 3 {
				t.Fatalf("err=%v, prepared %d descriptors, want 3", err, prepared)
			}
			if tc.serveErr != nil && (tl.Image != nil || openFDs(t) != before) {
				t.Fatalf("serve failure left image=%v, fds %d -> %d", tl.Image != nil, before, openFDs(t))
			}
			if tc.serveErr == nil {
				if tl.Image == nil || openFDs(t) != before+3 {
					t.Fatalf("serving dropped the image: fds %d -> %d", before, openFDs(t))
				}
				releaseLambdaTestLinkImage(tl)
			}
		})
	}
}
