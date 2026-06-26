package handlers_quota

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ec2_instance "github.com/mulgadc/spinifex/spinifex/gateway/ec2/instance"
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

// casVCPU applies next to accountID's counter under bounded JetStream CAS so
// concurrent writers cannot lose updates. next maps the current value to the new
// value and a skip flag; skip leaves the counter untouched and never creates a
// key. A revision conflict is retried; exhausting the bound is a hard error.
func (s *Service) casVCPU(accountID string, next func(current int) (value int, skip bool)) error {
	for range vcpuCASRetries {
		current, revision, err := s.readVCPU(accountID)
		if err != nil {
			return err
		}
		value, skip := next(current)
		if skip {
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

// AddVCPU adds delta vCPUs to accountID's counter under CAS so concurrent grows
// cannot lose updates. A non-positive delta is a no-op: shrinks are never charged
// and are left to reconcile to lower the counter.
func (s *Service) AddVCPU(accountID string, delta int) error {
	if s.Exempt(accountID) || delta <= 0 {
		return nil
	}
	return s.casVCPU(accountID, func(current int) (int, bool) {
		return current + delta, false
	})
}

// setVCPU overwrites accountID's counter to value, the only operation that lowers
// a counter. A counter already equal to value is left untouched, so a steady-state
// pass writes nothing and an account with no usage and no key is never created.
// Reconcile is the sole caller and runs under the leader lock.
func (s *Service) setVCPU(accountID string, value int) error {
	return s.casVCPU(accountID, func(current int) (int, bool) {
		return value, current == value
	})
}

// reconcileVCPU writes accountID's counter from a reconcile sweep. A complete
// sweep overwrites unconditionally (the only path that may lower a counter). An
// incomplete sweep — a node down or a failed bucket query — may only raise the
// counter: lowering from a partial view would under-count usage and lift the
// cap, so a short count is dropped and left for the next clean pass. Raising is
// always safe, since the instances actually observed do exist.
func (s *Service) reconcileVCPU(accountID string, value int, complete bool) error {
	if complete {
		return s.setVCPU(accountID, value)
	}
	current, _, err := s.readVCPU(accountID)
	if err != nil {
		return err
	}
	if value <= current {
		return nil
	}
	return s.setVCPU(accountID, value)
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

// EnforceLaunch is the check-before gate for RunInstances: it rejects when the
// worst-case charge of maxCount instances of instanceType would push accountID
// over its vCPU cap. The actual launched count is charged afterwards via
// ChargeLaunch. An unknown instance type contributes nothing and is left for the
// daemon to reject as InvalidInstanceType; the Exempt short-circuit lives in
// CheckVCPU.
func (s *Service) EnforceLaunch(accountID, instanceType string, maxCount int) error {
	perType, ok := TypeVCPUs(instanceType)
	if !ok {
		return nil
	}
	return s.CheckVCPU(accountID, perType*maxCount)
}

// ChargeLaunch is the increment-after for a successful RunInstances: it adds the
// vCPUs actually launched, summed from the returned reservation, to accountID's
// counter. The daemon may launch fewer than maxCount, so the charge is the real
// reservation rather than the checked worst case. The caller treats a write
// failure as drift for reconcile to correct, never failing the live launch.
func (s *Service) ChargeLaunch(accountID string, reservation *ec2.Reservation) error {
	return s.AddVCPU(accountID, sumReservationVCPUs([]*ec2.Reservation{reservation}))
}

// InstanceTypeResolver returns the current instance type of instanceID owned by
// accountID. ok is false when the instance does not exist for the account, in
// which case the retype gate charges nothing and leaves the modify for the daemon
// to reject. It mirrors reconcile's InstanceLister so unit tests can inject a
// static type without a NATS round trip.
type InstanceTypeResolver func(accountID, instanceID string) (instanceType string, ok bool, err error)

// NATSInstanceTypeResolver builds the production resolver: an account-filtered,
// single-instance describe returning the live type of a running or stopped
// instance. It uses the strict reconcile variant so an incomplete sweep does not
// masquerade as "instance absent" and wave a retype past the cap (see
// confirmInstanceType). expectedNodes is the configured node total, re-evaluated
// per call so a config change is reflected without a restart.
func NATSInstanceTypeResolver(natsConn *nats.Conn, expectedNodes func() int) InstanceTypeResolver {
	return func(accountID, instanceID string) (string, bool, error) {
		reservations, complete, err := gateway_ec2_instance.DescribeInstancesForReconcile(
			&ec2.DescribeInstancesInput{InstanceIds: []*string{aws.String(instanceID)}},
			natsConn, expectedNodes(), accountID)
		if err != nil {
			return "", false, err
		}
		return confirmInstanceType(reservations, instanceID, complete)
	}
}

// confirmInstanceType resolves instanceID's type from a describe sweep. A genuine
// absence on a complete sweep returns ok false with no error, so the retype gate
// charges nothing and the daemon rejects the modify. An instance that cannot be
// found on an incomplete sweep (a node or bucket missed) fails closed with
// ErrorServerInternal: a partial view must not be read as absent and let a retype
// skip its cap check. The client retries once the sweep is clean.
func confirmInstanceType(reservations []*ec2.Reservation, instanceID string, complete bool) (string, bool, error) {
	instanceType, ok := instanceTypeFromReservations(reservations, instanceID)
	if !ok && !complete {
		return "", false, errors.New(awserrors.ErrorServerInternal)
	}
	return instanceType, ok, nil
}

// EnforceRetype is the check-before gate for a ModifyInstanceAttribute that
// changes InstanceType: it rejects when retyping accountID's instance to newType
// would push its vCPU counter over the cap. It returns the vCPU delta the caller
// charges via AddVCPU once the daemon applies the retype. A retype-down or
// same-size change yields a non-positive delta that charges nothing and is left
// to reconcile. Exempt accounts, an unknown newType (the daemon rejects it), and
// an instance the resolver cannot find (the daemon rejects the modify) all return
// 0 with no check.
func (s *Service) EnforceRetype(resolve InstanceTypeResolver, accountID, instanceID, newType string) (int, error) {
	if s.Exempt(accountID) {
		return 0, nil
	}
	newVCPUs, ok := TypeVCPUs(newType)
	if !ok {
		return 0, nil
	}
	oldType, ok, err := resolve(accountID, instanceID)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	oldVCPUs, ok := TypeVCPUs(oldType)
	if !ok {
		return 0, nil
	}
	delta := newVCPUs - oldVCPUs
	if delta <= 0 {
		return delta, nil
	}
	if err := s.CheckVCPU(accountID, delta); err != nil {
		return 0, err
	}
	return delta, nil
}

// instanceTypeFromReservations finds instanceID among reservations and returns
// its type. ok is false when the instance is absent, terminal, or untyped, so the
// retype gate charges nothing and defers to the daemon.
func instanceTypeFromReservations(reservations []*ec2.Reservation, instanceID string) (string, bool) {
	for _, res := range reservations {
		if res == nil {
			continue
		}
		for _, inst := range res.Instances {
			if inst == nil || inst.InstanceType == nil || isTerminalState(inst.State) {
				continue
			}
			if aws.StringValue(inst.InstanceId) == instanceID {
				return *inst.InstanceType, true
			}
		}
	}
	return "", false
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
