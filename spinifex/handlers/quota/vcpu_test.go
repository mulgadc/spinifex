package handlers_quota

import (
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const testAccount = "123456789012"

// newVCPUService starts an embedded JetStream, creates the account-usage bucket
// the way the gateway does, and returns a quota Service bound to it.
func newVCPUService(t *testing.T, limits Limits) *Service {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:  KVBucketAccountUsage,
		History: 1,
	})
	if err != nil {
		t.Fatalf("create usage bucket: %v", err)
	}
	return New(limits, kv)
}

func TestCheckVCPUBoundaries(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 8})

	// Empty counter: at-limit passes, over-limit rejects.
	if err := s.CheckVCPU(testAccount, 8); err != nil {
		t.Fatalf("CheckVCPU(8) on empty = %v, want nil", err)
	}
	if err := s.CheckVCPU(testAccount, 9); err == nil || err.Error() != awserrors.ErrorResourceLimitExceeded {
		t.Fatalf("CheckVCPU(9) on empty = %v, want %q", err, awserrors.ErrorResourceLimitExceeded)
	}

	// Reserve 6, then the remaining headroom is 2.
	if err := s.AddVCPU(testAccount, 6); err != nil {
		t.Fatalf("AddVCPU(6) = %v", err)
	}
	if err := s.CheckVCPU(testAccount, 2); err != nil {
		t.Fatalf("CheckVCPU(2) at 6/8 = %v, want nil", err)
	}
	if err := s.CheckVCPU(testAccount, 3); err == nil || err.Error() != awserrors.ErrorResourceLimitExceeded {
		t.Fatalf("CheckVCPU(3) at 6/8 = %v, want %q", err, awserrors.ErrorResourceLimitExceeded)
	}
}

// Exempt callers (disabled config, system account) must short-circuit before
// touching the counter, so the checks return nil even with a nil KV handle.
func TestVCPUExemptShortCircuits(t *testing.T) {
	disabled := New(Limits{Enabled: false}, nil)
	enabled := New(Limits{Enabled: true, VCPUs: 8}, nil)
	cases := []struct {
		name string
		fn   func() error
	}{
		{"check disabled", func() error { return disabled.CheckVCPU(testAccount, 1000) }},
		{"add disabled", func() error { return disabled.AddVCPU(testAccount, 1000) }},
		{"check system account", func() error { return enabled.CheckVCPU(utils.GlobalAccountID, 1000) }},
		{"add system account", func() error { return enabled.AddVCPU(utils.GlobalAccountID, 1000) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); err != nil {
				t.Fatalf("exempt path returned %v, want nil", err)
			}
		})
	}
}

func TestAddVCPUAccumulatesAndIgnoresShrinks(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 100})

	for _, delta := range []int{4, 2, 0, -3} {
		if err := s.AddVCPU(testAccount, delta); err != nil {
			t.Fatalf("AddVCPU(%d) = %v", delta, err)
		}
	}
	// 4 + 2 land; the 0 and -3 are no-ops left to reconcile.
	got, _, err := s.readVCPU(testAccount)
	if err != nil {
		t.Fatalf("readVCPU: %v", err)
	}
	if got != 6 {
		t.Fatalf("counter = %d, want 6", got)
	}
}

// Concurrent grows under CAS must not lose updates: the final counter equals the
// sum of every increment.
func TestAddVCPUConcurrentNoLostUpdates(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 1000})

	const goroutines, perGoroutine = 10, 5
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range perGoroutine {
				if err := s.AddVCPU(testAccount, 1); err != nil {
					t.Errorf("AddVCPU: %v", err)
				}
			}
		})
	}
	wg.Wait()

	got, _, err := s.readVCPU(testAccount)
	if err != nil {
		t.Fatalf("readVCPU: %v", err)
	}
	if want := goroutines * perGoroutine; got != want {
		t.Fatalf("counter = %d, want %d (lost updates under CAS)", got, want)
	}
}
