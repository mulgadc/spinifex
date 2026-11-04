//go:build lambdatestlink

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/testlink"
)

// TestLambdaHardKillPin runs from make build-lambda-testlink with the same pin
// as the built daemon: the paired initializer passes and a changed one fails.
func TestLambdaHardKillPin(t *testing.T) {
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
