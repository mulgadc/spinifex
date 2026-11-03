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

// A failed serving check refuses new invocations but recovery has already
// started, so an attempt launched before a restart is still cleaned up.
func TestLambdaTestLinkRecoveryStartsDespiteFailedServingCheck(t *testing.T) {
	var recovered, served bool
	refused := errors.New("hard-kill initializer digest mismatch")
	err := runLambdaTestLink(
		func() { recovered = true },
		func() error {
			if !recovered {
				t.Fatal("serving check ran before recovery started")
			}
			return refused
		},
		func() error { served = true; return nil },
	)
	if !errors.Is(err, refused) || !recovered || served {
		t.Fatalf("err=%v recovered=%v served=%v", err, recovered, served)
	}
}
