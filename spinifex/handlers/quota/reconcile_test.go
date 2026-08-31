package handlers_quota

import (
	"context"
	"errors"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// staticTotals serves a fixed per-account total as one scan, and records how
// many times it was called so a test can assert the sweep reads the record
// space once rather than once per account.
func staticTotals(byAccount map[string]int, complete bool, calls *int) InstanceVCPULister {
	return func(context.Context) (map[string]int, bool, error) {
		if calls != nil {
			*calls++
		}
		return byAccount, complete, nil
	}
}

func accountList(ids ...string) AccountLister {
	return func() ([]string, error) { return ids, nil }
}

func TestReconcileCorrectsOverCount(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 100})

	// Counter drifted high (e.g. a stale increment); the account truly holds one
	// m5.xlarge (4) plus one t3.micro (2) = 6.
	if err := s.AddVCPU(t.Context(), testAccount, 16); err != nil {
		t.Fatalf("seed AddVCPU: %v", err)
	}

	if err := s.Reconcile(context.Background(), accountList(testAccount),
		staticTotals(map[string]int{testAccount: 6}, true, nil)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertCounter(t, s, testAccount, 6)
}

// An account that has terminated everything is absent from the scan entirely,
// which is the only signal that it now holds nothing, so it must be zeroed
// rather than skipped.
func TestReconcileZeroesAccountAbsentFromScan(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 100})

	if err := s.AddVCPU(t.Context(), testAccount, 8); err != nil {
		t.Fatalf("seed AddVCPU: %v", err)
	}

	if err := s.Reconcile(context.Background(), accountList(testAccount),
		staticTotals(map[string]int{}, true, nil)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertCounter(t, s, testAccount, 0)
}

// One scan covers every account, where the fan-out cost one describe each.
func TestReconcileScansOnceForEveryAccount(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 100})

	const a, b, c = "111111111111", "222222222222", "333333333333"
	calls := 0
	if err := s.Reconcile(context.Background(), accountList(a, b, c),
		staticTotals(map[string]int{a: 2, b: 4, c: 8}, true, &calls)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if calls != 1 {
		t.Fatalf("scans = %d, want 1 for three accounts", calls)
	}
	assertCounter(t, s, a, 2)
	assertCounter(t, s, b, 4)
	assertCounter(t, s, c, 8)
}

// The system account is exempt: any counter parked under its key is left
// untouched even when the scan reports a total against it.
func TestReconcileSkipsSystemAccount(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 100})

	// Park a non-zero value directly under the system key; AddVCPU would no-op it.
	if _, err := s.usage.PutString(t.Context(), utils.GlobalAccountID, "42"); err != nil {
		t.Fatalf("seed system counter: %v", err)
	}

	if err := s.Reconcile(context.Background(), accountList(utils.GlobalAccountID, testAccount),
		staticTotals(map[string]int{utils.GlobalAccountID: 99, testAccount: 2}, true, nil)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertCounter(t, s, utils.GlobalAccountID, 42)
	assertCounter(t, s, testAccount, 2)
}

// The scan is one read covering every account, so a failed read is not a
// per-account outage that the pass can continue past: no counter may move, and
// the error is returned for the next pass to retry.
func TestReconcileScanFailureLeavesEveryCounterUntouched(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 100})

	const a, b = "111111111111", "222222222222"
	if err := s.AddVCPU(t.Context(), a, 8); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if err := s.AddVCPU(t.Context(), b, 4); err != nil {
		t.Fatalf("seed b: %v", err)
	}
	sentinel := errors.New("scan boom")
	failing := func(context.Context) (map[string]int, bool, error) { return nil, false, sentinel }

	err := s.Reconcile(context.Background(), accountList(a, b), failing)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Reconcile err = %v, want %v", err, sentinel)
	}
	assertCounter(t, s, a, 8)
	assertCounter(t, s, b, 4)
}

// An incomplete sweep must never lower a counter: a short count is dropped and
// the counter left for the next clean pass. A higher observed count still
// raises it, since those instances do exist.
func TestReconcileIncompleteSweepDoesNotLower(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 100})

	if err := s.AddVCPU(t.Context(), testAccount, 8); err != nil {
		t.Fatalf("seed AddVCPU: %v", err)
	}
	if err := s.Reconcile(context.Background(), accountList(testAccount),
		staticTotals(map[string]int{testAccount: 4}, false, nil)); err != nil {
		t.Fatalf("Reconcile partial: %v", err)
	}
	assertCounter(t, s, testAccount, 8) // unchanged: a partial sweep cannot lower

	if err := s.Reconcile(context.Background(), accountList(testAccount),
		staticTotals(map[string]int{testAccount: 12}, false, nil)); err != nil {
		t.Fatalf("Reconcile raise: %v", err)
	}
	assertCounter(t, s, testAccount, 12)
}

// The guard that makes an absolute overwrite safe: a charge landing between the
// revision read and the write moves the revision, so the charge stands and the
// scan is dropped. Without it the overwrite would silently undo the launch.
func TestReconcileDoesNotClobberAChargeLandingMidScan(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 100})

	if err := s.AddVCPU(t.Context(), testAccount, 4); err != nil {
		t.Fatalf("seed AddVCPU: %v", err)
	}
	// The scan sees the account holding only the seeded 4 vCPUs; a launch is
	// charged while it runs, so the counter it would write is already stale.
	racing := func(context.Context) (map[string]int, bool, error) {
		if err := s.AddVCPU(context.Background(), testAccount, 2); err != nil {
			return nil, false, err
		}
		return map[string]int{testAccount: 4}, true, nil
	}

	if err := s.Reconcile(context.Background(), accountList(testAccount), racing); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertCounter(t, s, testAccount, 6) // the charge, not the scan's stale 4
}

// ReconcileAccount is the per-key entry point: it settles the account it was
// given and leaves every other account alone, which is what makes a change to
// one instance cost one account's work.
func TestReconcileAccountTouchesOnlyThatAccount(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 100})

	const mine, other = "111111111111", "222222222222"
	if err := s.AddVCPU(t.Context(), mine, 16); err != nil {
		t.Fatalf("seed mine: %v", err)
	}
	if err := s.AddVCPU(t.Context(), other, 8); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	if err := s.ReconcileAccount(context.Background(), mine,
		staticTotals(map[string]int{mine: 6, other: 0}, true, nil)); err != nil {
		t.Fatalf("ReconcileAccount: %v", err)
	}
	assertCounter(t, s, mine, 6)
	assertCounter(t, s, other, 8) // untouched, though the scan saw it hold nothing
}

func TestReconcileAccountSkipsSystemAccount(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 100})

	if _, err := s.usage.PutString(t.Context(), utils.GlobalAccountID, "42"); err != nil {
		t.Fatalf("seed system counter: %v", err)
	}
	if err := s.ReconcileAccount(context.Background(), utils.GlobalAccountID,
		staticTotals(map[string]int{utils.GlobalAccountID: 0}, true, nil)); err != nil {
		t.Fatalf("ReconcileAccount: %v", err)
	}
	assertCounter(t, s, utils.GlobalAccountID, 42)
}

// A disabled service never reaches the KV: both entry points are a no-op even
// with a nil lister and account enumerator that would otherwise panic.
func TestReconcileDisabledNoop(t *testing.T) {
	s := New(Limits{Enabled: false}, nil)
	if err := s.Reconcile(context.Background(), nil, nil); err != nil {
		t.Fatalf("disabled Reconcile = %v, want nil", err)
	}
	if err := s.ReconcileAccount(context.Background(), testAccount, nil); err != nil {
		t.Fatalf("disabled ReconcileAccount = %v, want nil", err)
	}
	var nilService *Service
	if err := nilService.Reconcile(context.Background(), nil, nil); err != nil {
		t.Fatalf("nil Reconcile = %v, want nil", err)
	}
	if err := nilService.ReconcileAccount(context.Background(), testAccount, nil); err != nil {
		t.Fatalf("nil ReconcileAccount = %v, want nil", err)
	}
}

func assertCounter(t *testing.T, s *Service, accountID string, want int) {
	t.Helper()
	got, _, err := s.readVCPU(t.Context(), accountID)
	if err != nil {
		t.Fatalf("readVCPU(%s): %v", accountID, err)
	}
	if got != want {
		t.Fatalf("counter[%s] = %d, want %d", accountID, got, want)
	}
}
