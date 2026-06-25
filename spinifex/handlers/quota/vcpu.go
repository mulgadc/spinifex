package handlers_quota

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/instancetypes"
	"github.com/nats-io/nats.go"
)

// vcpuCASRetries bounds AddVCPU's retry on a revision conflict. Each retry
// implies another writer committed, so a single grow can conflict at most as
// many times as there are concurrent grows on the account; the bound sits well
// above any realistic in-flight launch burst and trips only as a circuit
// breaker, never on genuine contention.
const vcpuCASRetries = 100

// TypeVCPUs returns the vCPU count charged for an instance type, wrapping the
// static instancetypes catalog so the gate handlers and reconcile share one
// source of truth. ok is false for an unknown type.
func TypeVCPUs(instanceType string) (int, bool) {
	return instancetypes.VCPUsForType(instanceType)
}

// CheckVCPU rejects with ResourceLimitExceeded when charging want more vCPUs to
// accountID would exceed the configured cap. It only reads the counter; the
// caller increments via AddVCPU after the grow succeeds. Two concurrent checks
// can both pass before either increments (the documented soft-cap window); the
// per-node physical gate still backstops real overcommit.
func (s *Service) CheckVCPU(accountID string, want int) error {
	if s.Exempt(accountID) {
		return nil
	}
	current, _, err := s.readVCPU(accountID)
	if err != nil {
		return err
	}
	if current+want > s.limits.VCPUs {
		return errors.New(awserrors.ErrorResourceLimitExceeded)
	}
	return nil
}

// AddVCPU adds delta vCPUs to accountID's counter under JetStream CAS so
// concurrent grows cannot lose updates. A non-positive delta is a no-op: shrinks
// are never charged and are left to reconcile to lower the counter.
func (s *Service) AddVCPU(accountID string, delta int) error {
	if s.Exempt(accountID) || delta <= 0 {
		return nil
	}
	for range vcpuCASRetries {
		current, revision, err := s.readVCPU(accountID)
		if err != nil {
			return err
		}
		data, err := json.Marshal(current + delta)
		if err != nil {
			return err
		}
		if revision == 0 {
			_, err = s.usage.Create(accountID, data)
		} else {
			_, err = s.usage.Update(accountID, data, revision)
		}
		if err == nil {
			return nil
		}
		if !isCASConflict(err) {
			return err
		}
	}
	return fmt.Errorf("vcpu counter CAS exhausted for %s after %d attempts", accountID, vcpuCASRetries)
}

// setVCPU overwrites accountID's counter to value under bounded CAS, the only
// operation that lowers a counter. A counter already equal to value is left
// untouched, so a steady-state pass writes nothing and an account with no usage
// and no key is never created. Reconcile is the sole caller and runs under the
// leader lock; contention is limited to an in-flight grow on the same account,
// which a CAS conflict retries and the next pass reconciles.
func (s *Service) setVCPU(accountID string, value int) error {
	for range vcpuCASRetries {
		current, revision, err := s.readVCPU(accountID)
		if err != nil {
			return err
		}
		if current == value {
			return nil
		}
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if revision == 0 {
			_, err = s.usage.Create(accountID, data)
		} else {
			_, err = s.usage.Update(accountID, data, revision)
		}
		if err == nil {
			return nil
		}
		if !isCASConflict(err) {
			return err
		}
	}
	return fmt.Errorf("vcpu counter CAS exhausted for %s after %d attempts", accountID, vcpuCASRetries)
}

// isCASConflict reports whether err is a lost optimistic-concurrency race: a
// Create on an existing key or an Update against a stale revision. Both map to
// JetStream's wrong-last-sequence code and are retryable; any other error is a
// genuine failure the caller must surface.
func isCASConflict(err error) bool {
	if errors.Is(err, nats.ErrKeyExists) {
		return true
	}
	var apiErr *nats.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode == nats.JSErrCodeStreamWrongLastSequence
	}
	return false
}

// readVCPU returns accountID's current reserved vCPU count and the KV revision
// it was read at. A missing key is the zero counter at revision 0, which AddVCPU
// treats as a create.
func (s *Service) readVCPU(accountID string) (count int, revision uint64, err error) {
	entry, err := s.usage.Get(accountID)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	if err := json.Unmarshal(entry.Value(), &count); err != nil {
		return 0, 0, fmt.Errorf("unmarshal vcpu counter for %s: %w", accountID, err)
	}
	return count, entry.Revision(), nil
}
