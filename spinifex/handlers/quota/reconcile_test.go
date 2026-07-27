package handlers_quota

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// reservation builds a single reservation whose instances carry the given type
// and state, so a test can describe an account's running-plus-stopped shape.
func reservation(instances ...*ec2.Instance) *ec2.Reservation {
	return &ec2.Reservation{Instances: instances}
}

// instance builds one ec2.Instance with a type and state name; an empty state
// leaves State nil (treated as live).
func instance(instanceType, stateName string) *ec2.Instance {
	inst := &ec2.Instance{InstanceType: aws.String(instanceType)}
	if stateName != "" {
		inst.State = &ec2.InstanceState{Name: aws.String(stateName)}
	}
	return inst
}

// staticLister returns an InstanceLister serving a fixed per-account map as a
// complete sweep, and records which accounts were described so a test can assert
// the system account is never swept.
func staticLister(byAccount map[string][]*ec2.Reservation, seen *[]string) InstanceLister {
	return func(accountID string) ([]*ec2.Reservation, bool, error) {
		if seen != nil {
			*seen = append(*seen, accountID)
		}
		return byAccount[accountID], true, nil
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
	lister := staticLister(map[string][]*ec2.Reservation{
		testAccount: {reservation(
			instance("m5.xlarge", ec2.InstanceStateNameRunning),
			instance("t3.micro", ec2.InstanceStateNameStopped),
		)},
	}, nil)

	if err := s.Reconcile(context.Background(), accountList(testAccount), lister); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertCounter(t, s, testAccount, 6)
}

// A terminated instance still lingering in the terminated KV must not be charged:
// the counter drops to only the surviving running instance's vCPUs.
func TestReconcileCorrectsStaleTermination(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 100})

	if err := s.AddVCPU(t.Context(), testAccount, 6); err != nil {
		t.Fatalf("seed AddVCPU: %v", err)
	}
	lister := staticLister(map[string][]*ec2.Reservation{
		testAccount: {reservation(
			instance("t3.micro", ec2.InstanceStateNameRunning),      // counts: 2
			instance("m5.xlarge", ec2.InstanceStateNameTerminated),  // excluded
			instance("c5.large", ec2.InstanceStateNameShuttingDown), // excluded
		)},
	}, nil)

	if err := s.Reconcile(context.Background(), accountList(testAccount), lister); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertCounter(t, s, testAccount, 2)
}

// An account that has terminated everything is zeroed.
func TestReconcileZeroesEmptyAccount(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 100})

	if err := s.AddVCPU(t.Context(), testAccount, 8); err != nil {
		t.Fatalf("seed AddVCPU: %v", err)
	}
	lister := staticLister(map[string][]*ec2.Reservation{}, nil) // describes empty

	if err := s.Reconcile(context.Background(), accountList(testAccount), lister); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertCounter(t, s, testAccount, 0)
}

// The system account is exempt: it is skipped entirely (never described) and any
// counter parked under its key is left untouched.
func TestReconcileSkipsSystemAccount(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 100})

	// Park a non-zero value directly under the system key; AddVCPU would no-op it.
	if _, err := s.usage.PutString(t.Context(), utils.GlobalAccountID, "42"); err != nil {
		t.Fatalf("seed system counter: %v", err)
	}
	var seen []string
	lister := staticLister(map[string][]*ec2.Reservation{
		testAccount: {reservation(instance("t3.micro", ec2.InstanceStateNameRunning))},
	}, &seen)

	if err := s.Reconcile(context.Background(), accountList(utils.GlobalAccountID, testAccount), lister); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, id := range seen {
		if id == utils.GlobalAccountID {
			t.Fatalf("system account was described, want skipped")
		}
	}
	assertCounter(t, s, utils.GlobalAccountID, 42)
	assertCounter(t, s, testAccount, 2)
}

// A describe failure on one account is logged and the pass continues for the
// rest; the first error is returned and the failed account's counter is left
// untouched for the next pass to retry.
func TestReconcileContinuesOnDescribeError(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 100})

	const bad, good = "111111111111", "222222222222"
	if err := s.AddVCPU(t.Context(), bad, 8); err != nil {
		t.Fatalf("seed bad: %v", err)
	}
	sentinel := errors.New("describe boom")
	lister := func(accountID string) ([]*ec2.Reservation, bool, error) {
		if accountID == bad {
			return nil, false, sentinel
		}
		return []*ec2.Reservation{reservation(instance("m5.xlarge", ec2.InstanceStateNameRunning))}, true, nil
	}

	err := s.Reconcile(context.Background(), accountList(bad, good), lister)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Reconcile err = %v, want %v", err, sentinel)
	}
	assertCounter(t, s, bad, 8) // unchanged: left for the next pass
	assertCounter(t, s, good, 4)
}

// An incomplete sweep (a node down or a failed bucket query) must never lower a
// counter: a short count is dropped and the counter left for the next clean pass.
// A higher observed count still raises it, since those instances do exist.
func TestReconcileIncompleteSweepDoesNotLower(t *testing.T) {
	s := newVCPUService(t, Limits{Enabled: true, VCPUs: 100})

	// Counter holds 8 (two m5.xlarge across two nodes); a partial sweep sees only
	// one node's 4 vCPUs and reports complete=false.
	if err := s.AddVCPU(t.Context(), testAccount, 8); err != nil {
		t.Fatalf("seed AddVCPU: %v", err)
	}
	partial := func(accountID string) ([]*ec2.Reservation, bool, error) {
		return []*ec2.Reservation{reservation(instance("m5.xlarge", ec2.InstanceStateNameRunning))}, false, nil
	}
	if err := s.Reconcile(context.Background(), accountList(testAccount), partial); err != nil {
		t.Fatalf("Reconcile partial: %v", err)
	}
	assertCounter(t, s, testAccount, 8) // unchanged: a partial sweep cannot lower

	// A partial sweep that observes more than the counter may still raise it.
	higher := func(accountID string) ([]*ec2.Reservation, bool, error) {
		return []*ec2.Reservation{reservation(
			instance("m5.xlarge", ec2.InstanceStateNameRunning),
			instance("m5.xlarge", ec2.InstanceStateNameRunning),
			instance("m5.xlarge", ec2.InstanceStateNameRunning),
		)}, false, nil
	}
	if err := s.Reconcile(context.Background(), accountList(testAccount), higher); err != nil {
		t.Fatalf("Reconcile raise: %v", err)
	}
	assertCounter(t, s, testAccount, 12) // raised to the observed 3 x 4 = 12
}

// A disabled service never reaches the KV: Reconcile is a no-op even with a nil
// lister and account enumerator that would otherwise panic.
func TestReconcileDisabledNoop(t *testing.T) {
	s := New(Limits{Enabled: false}, nil)
	if err := s.Reconcile(context.Background(), nil, nil); err != nil {
		t.Fatalf("disabled Reconcile = %v, want nil", err)
	}
	var nilService *Service
	if err := nilService.Reconcile(context.Background(), nil, nil); err != nil {
		t.Fatalf("nil Reconcile = %v, want nil", err)
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
