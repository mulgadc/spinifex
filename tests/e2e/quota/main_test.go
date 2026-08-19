//go:build e2e

// Package quota is the per-account service-quota E2E suite. It turns quotas on
// for the duration of the run, gives the super-admin account an unlimited
// override so nothing else on the box is capped, then drives each enforced
// dimension from a freshly created tenant account: the account a self-service
// signup produces, holding the baseline a production sandbox user gets.
//
// Every test resolves the account's current usage before it sets a limit, so an
// assertion is about the boundary rather than about how much the account
// happened to hold when the suite started.
package quota

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// Package-scoped singleton fixture, built lazily by requireQuotaFixture so a
// run with SPINIFEX_E2E unset neither dials AWS nor rewrites any config.
var (
	pkgFixOnce sync.Once
	pkgFix     *Fixture
	pkgFixErr  error
)

// TestMain owns process-level lifecycle. Teardown unwinds LIFO, and the order
// is load-bearing: turning quotas back off closes the override bucket, so an
// override still to be removed must go first or its removal cannot be stored.
func TestMain(m *testing.M) {
	code := m.Run()
	if pkgFix != nil {
		if pkgFix.Harness != nil {
			if err := pkgFix.Harness.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "e2e teardown: %v\n", err)
				code = 1
			}
		}
		for i := len(pkgFix.restore) - 1; i >= 0; i-- {
			if err := pkgFix.restore[i](); err != nil {
				fmt.Fprintf(os.Stderr, "e2e teardown: %v\n", err)
				code = 1
			}
		}
	}
	os.Exit(code)
}

// Fixture carries the per-process state shared by every Test* in the package.
type Fixture struct {
	Env *harness.Env

	// Admin is the super-admin account: the credential the /admin surface
	// accepts, and the account whose quota is lifted for the run.
	Admin          *harness.AWSClient
	AdminAccountID string

	// Harness memoizes the AMI, instance type and AZ discovery. Bound to the
	// admin client because gold images live in the super-admin account.
	Harness *harness.Fixture

	// Baseline is the [quota] block installed on the cluster, which is what an
	// account with no override of its own resolves to.
	Baseline harness.QuotaLimits

	// Tenant is the account under test; Peer proves an override applies to one
	// account and not to its neighbour.
	Tenant *harness.Profile
	Peer   *harness.Profile

	// restore undoes the cluster-level changes the fixture made, in the order
	// registered. Each reports an error rather than failing a test: they run
	// from TestMain, after the last test has finished.
	restore []func() error
}

// requireQuotaFixture returns the package-scoped Fixture, building it on first
// call. Skips when SPINIFEX_E2E is unset; runs in both single and multinode
// topologies, because the enforcement path is the same gateway code either way.
func requireQuotaFixture(t *testing.T) *Fixture {
	t.Helper()
	pkgFixOnce.Do(func() {
		if os.Getenv("SPINIFEX_E2E") == "" {
			return
		}
		env := harness.LoadEnv(t)
		admin := harness.NewAWSClient(t, env)

		ident, err := admin.STS.GetCallerIdentity(&sts.GetCallerIdentityInput{})
		if err != nil {
			pkgFixErr = fmt.Errorf("resolve super-admin account: %w", err)
			return
		}
		fx, err := harness.NewProcessFixture(admin)
		if err != nil {
			pkgFixErr = err
			return
		}

		baseline := harness.SandboxQuotaLimits()
		fix := &Fixture{
			Env:            env,
			Admin:          admin,
			AdminAccountID: aws.StringValue(ident.Account),
			Harness:        fx,
			Baseline:       baseline,
		}

		// Registered before anything can fail below, so a half-built fixture
		// still puts the cluster back the way it found it.
		fix.restore = append(fix.restore, harness.EnableQuota(t, env, baseline))
		waitAdminSurface(t, fix)

		// Suites share this VM and most of them run as the super-admin account,
		// which the baseline would cap at 16 vCPUs. Lifting it is also how a
		// production operator exempts an account, so the path is worth using.
		liftAdminQuota(t, fix)

		fix.Tenant = newTenant(t, env, "quota-tenant")
		fix.Peer = newTenant(t, env, "quota-peer")
		pkgFix = fix
	})
	if pkgFixErr != nil {
		t.Fatalf("quota fixture init failed: %v", pkgFixErr)
	}
	if pkgFix == nil {
		t.Skip("quota fixture unavailable (SPINIFEX_E2E unset)")
	}
	return pkgFix
}

// waitAdminSurface polls until the restarted gateway answers a signed request.
// A TLS handshake only proves the listener is back; the quota buckets are
// opened during startup and the first call must not race them.
func waitAdminSurface(t *testing.T, fix *Fixture) {
	t.Helper()
	harness.EventuallyErr(t, func() error {
		_, err := fix.Admin.EC2.DescribeVpcs(&ec2.DescribeVpcsInput{})
		return err
	}, 90*time.Second, 2*time.Second)
}

// liftAdminQuota gives the super-admin account an unlimited override on every
// dimension and registers its removal.
func liftAdminQuota(t *testing.T, fix *Fixture) {
	t.Helper()
	harness.SpxAdminQuotaSet(t, fix.AdminAccountID,
		"--vcpus", "-1", "--vpcs", "-1", "--subnets", "-1", "--eips", "-1",
		"--volumes", "-1", "--volumes-gib", "-1", "--rds-instances", "-1",
		"--load-balancers", "-1")
	fix.restore = append(fix.restore, func() error {
		return harness.ClearAccountQuota(fix.AdminAccountID)
	})
}

// newTenant creates an account the way self-service signup does and returns a
// client scoped to it. Its quota is whatever the account inherits, which is the
// point: a sandbox user is never configured, only defaulted.
func newTenant(t *testing.T, env *harness.Env, label string) *harness.Profile {
	t.Helper()
	name := fmt.Sprintf("E2E %s %d", label, time.Now().UnixNano())
	info := harness.SpxAdminAccountCreate(t, name, "")
	return harness.NewAccountCarousel().Add(t, env, label, info)
}
