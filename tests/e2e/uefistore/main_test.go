//go:build e2e

// Package uefistore proves a real UEFI guest's variable writes reach the store
// and survive stop and start, a daemon restart and the prune. It is single-node
// and runs on the node, which it restarts the daemon of; its tests share a guest.
package uefistore

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

var (
	pkgFixOnce sync.Once
	pkgFix     *Fixture
	pkgFixErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if pkgFix != nil && pkgFix.Harness != nil {
		if err := pkgFix.Harness.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e teardown: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}

// Fixture carries per-process state shared across this package's tests.
type Fixture struct {
	Env     *harness.Env
	AWS     *harness.AWSClient
	Harness *harness.Fixture
	Store   *harness.UEFIStoreView

	// launched is when the shared guest's RunInstances was sent, which is the
	// earliest any of its store objects can have been written.
	launchedMu sync.Mutex
	launched   time.Time
}

// requireUEFIFixture returns the package-scoped Fixture, building it on first
// call. Skips when SPINIFEX_E2E is unset or the cluster is not single-node.
func requireUEFIFixture(t *testing.T) *Fixture {
	t.Helper()
	pkgFixOnce.Do(func() {
		if os.Getenv("SPINIFEX_E2E") == "" {
			return
		}
		env := harness.LoadEnv(t)
		if env.Mode != harness.ModeSingle {
			return
		}
		awsCli := harness.NewAWSClient(t, env)
		h, err := harness.NewProcessFixture(awsCli)
		if err != nil {
			pkgFixErr = err
			return
		}
		harness.EnsureDefaultSGOpen(t, awsCli)
		pkgFix = &Fixture{Env: env, AWS: awsCli, Harness: h, Store: harness.NewUEFIStoreView(t, env)}
	})
	if pkgFixErr != nil {
		t.Fatalf("uefistore fixture init: %v", pkgFixErr)
	}
	if pkgFix == nil {
		t.Skip("uefistore fixture unavailable (SPINIFEX_E2E unset or mode != single)")
	}
	return pkgFix
}
