//go:build e2e

// Package ecr is the standalone ECR E2E suite: control-plane CRUD over the
// awsgw ECR surface plus the OCI data plane (docker/crane/skopeo push-pull)
// against a running cluster. The data-plane subtests skip when the client
// binary is missing or the registry host does not resolve, so the suite still
// runs its control-plane leg on a bare runner.
package ecr

import (
	"os"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// Package-scoped singleton fixture, built lazily by requireECRFixture and torn
// down by TestMain after the run.
var (
	pkgFixOnce sync.Once
	pkgFix     *Fixture
	pkgFixErr  error
)

// Fixture carries per-process state shared across every Test* in this package.
type Fixture struct {
	Env     *harness.Env
	AWS     *harness.AWSClient
	Account string
	TmpDir  string
}

func TestMain(m *testing.M) {
	code := m.Run()
	if pkgFix != nil && pkgFix.TmpDir != "" {
		_ = os.RemoveAll(pkgFix.TmpDir)
	}
	os.Exit(code)
}

// requireECRFixture returns the package-scoped Fixture, building it on first
// call. Skips the calling test if SPINIFEX_E2E is unset.
func requireECRFixture(t *testing.T) *Fixture {
	t.Helper()
	pkgFixOnce.Do(func() {
		if os.Getenv("SPINIFEX_E2E") == "" {
			return
		}
		env := harness.LoadEnv(t)
		awsCli := harness.NewAWSClient(t, env)
		tmpDir, terr := os.MkdirTemp("", "ecr-pkgfix-*")
		if terr != nil {
			pkgFixErr = terr
			return
		}
		pkgFix = &Fixture{
			Env:     env,
			AWS:     awsCli,
			Account: harness.IAMAccountID(t, awsCli),
			TmpDir:  tmpDir,
		}
	})
	if pkgFixErr != nil {
		t.Fatalf("ecr fixture init failed: %v", pkgFixErr)
	}
	if pkgFix == nil {
		t.Skip("ecr fixture unavailable (SPINIFEX_E2E unset)")
	}
	return pkgFix
}
