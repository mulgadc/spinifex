//go:build e2e

// Package bedrock is the Ochre (AWS-Bedrock) E2E suite. Hermetic API-shape and
// provider-translation parity (Anthropic Messages / vLLM OpenAI wire formats)
// is covered by the gateway_bedrock unit tests; this suite exercises
// ListFoundationModels/GetFoundationModel against any running cluster and
// gates real Converse calls on backend availability (self-host GPU or a live
// Anthropic key), so CI parity holds without requiring either to be present.
//
// Model access is deny-by-default, so the cluster's test account must hold
// grants or every assertion here reduces to "empty catalog, access denied".
// awsgw seeds the platform admin account with the full catalog on first
// start, so a suite running as that account needs no setup; running as a
// tenant account needs
// `spx admin ochre access grant --account-id <id> --all-models` first.
package bedrock

import (
	"os"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// Package-scoped singleton fixture, built lazily by requireBedrockFixture and
// torn down by TestMain after the run.
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
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

// requireBedrockFixture returns the package-scoped Fixture, building it on
// first call. Skips the calling test if SPINIFEX_E2E is unset.
func requireBedrockFixture(t *testing.T) *Fixture {
	t.Helper()
	pkgFixOnce.Do(func() {
		if os.Getenv("SPINIFEX_E2E") == "" {
			return
		}
		env := harness.LoadEnv(t)
		awsCli := harness.NewAWSClient(t, env)
		pkgFix = &Fixture{
			Env:     env,
			AWS:     awsCli,
			Account: harness.IAMAccountID(t, awsCli),
		}
	})
	if pkgFixErr != nil {
		t.Fatalf("bedrock fixture init failed: %v", pkgFixErr)
	}
	if pkgFix == nil {
		t.Skip("bedrock fixture unavailable (SPINIFEX_E2E unset)")
	}
	return pkgFix
}
