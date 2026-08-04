package handlers_rds

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Applying PendingModifiedValues is the one control-plane operation with two
// entry points that can fire at once: ApplyImmediately runs it inline in the API
// call for as long as a VM replace takes, and the reconciler sweeps every
// modifying instance every reconcileInterval looking for exactly that record
// shape. Without a lease the sweep re-enters a change still in progress and runs
// a second replaceInstanceVM against the same data volume and the same endpoint
// ENI — two VMs on one datadir, which is the state the replace exists to avoid.
// Short enough that a worker that dies is taken over within a pass or two, and
// long enough for several refresh attempts inside it.
const modifyLeaseTTL = 45 * time.Second

var errModifyLeaseLost = errors.New("rds: modify lease lost while applying pending modifications")

// Runs apply while holding the instance's modify lease, renewing it for as long
// as the work runs and releasing it whatever the outcome. Reports whether apply
// ran: a lease already held is not an error, it means another worker is inside
// this change and the caller has nothing to do.
func (s *Service) withModifyLease(
	ctx context.Context,
	kv jetstream.KeyValue,
	id string,
	apply func(context.Context) error,
) (bool, error) {
	holder := s.newModifyLeaseHolder()
	claimed, expiresAt, err := s.claimModifyLease(ctx, kv, id, holder)
	if err != nil || !claimed {
		return false, err
	}

	// The release has to outlive a cancelled ctx, or a shutdown mid-modify
	// leaves the lease to expire on its own and stalls the takeover it exists
	// to enable.
	release := context.WithoutCancel(ctx)
	applyCtx, cancelApply := context.WithCancelCause(ctx)
	renewing, stopRenewing := context.WithCancel(ctx)
	var renewals sync.WaitGroup
	renewals.Go(func() {
		s.renewModifyLease(renewing, kv, id, holder, expiresAt, cancelApply)
	})

	defer func() {
		stopRenewing()
		renewals.Wait()
		cancelApply(nil)
		if _, err := s.updateInstanceIf(release, kv, id, func(rec *DBInstanceRecord) bool {
			if rec.ModifyLease == nil || rec.ModifyLease.Holder != holder {
				return false
			}
			rec.ModifyLease = nil
			return true
		}); err != nil {
			slog.WarnContext(ctx, "rds: releasing the modify lease failed; it will expire instead",
				"dbInstance", id, "holder", holder, "err", err)
		}
	}()

	applyErr := apply(applyCtx)
	// Stop and join before inspecting the cause, or the renewer can discover a
	// takeover during deferred cleanup after a nil return has been chosen.
	stopRenewing()
	renewals.Wait()
	if cause := context.Cause(applyCtx); cause != nil {
		return true, errors.Join(cause, applyErr)
	}
	return true, applyErr
}

// The node plus a per-claim nonce: the API handler and the reconciler run on the
// same node, so the node alone would let each renew the other's lease.
func (s *Service) newModifyLeaseHolder() string {
	var nonce [8]byte
	// crypto/rand.Read is documented never to fail.
	_, _ = rand.Read(nonce[:])
	return fmt.Sprintf("%s/%x", s.deps.HolderID, nonce)
}

// Takes the lease unless a live one belongs to someone else. Re-taking our own
// is allowed, so a caller that already holds it is not deadlocked by itself.
func (s *Service) claimModifyLease(
	ctx context.Context,
	kv jetstream.KeyValue,
	id, holder string,
) (bool, time.Time, error) {
	var expiresAt time.Time
	claimed, err := s.updateInstanceIf(ctx, kv, id, func(rec *DBInstanceRecord) bool {
		if rec.ModifyLease.live() && rec.ModifyLease.Holder != holder {
			return false
		}
		expiresAt = time.Now().UTC().Add(s.modifyLeaseTTL())
		rec.ModifyLease = &ModifyLease{Holder: holder, ExpiresAt: expiresAt}
		return true
	})
	return claimed, expiresAt, err
}

// Pushes the expiry out until the work finishes or ctx is cancelled. A renewal
// that finds the lease taken over cancels the guarded work rather than
// reclaiming it. A deadline bounds each KV attempt so an outage cannot let the
// work continue after its last successful renewal expires.
func (s *Service) renewModifyLease(
	ctx context.Context,
	kv jetstream.KeyValue,
	id, holder string,
	expiresAt time.Time,
	cancelApply context.CancelCauseFunc,
) {
	ticker := time.NewTicker(s.modifyLeaseRefresh())
	defer ticker.Stop()
	expiry := time.NewTimer(time.Until(expiresAt))
	defer expiry.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-expiry.C:
			slog.WarnContext(ctx, "rds: the modify lease expired while renewal was failing",
				"dbInstance", id, "holder", holder)
			cancelApply(fmt.Errorf("%w: renewals did not succeed before expiry", errModifyLeaseLost))
			return
		case <-ticker.C:
			renewCtx, cancelRenew := context.WithDeadline(ctx, expiresAt)
			var renewedUntil time.Time
			held, err := s.updateInstanceIf(renewCtx, kv, id, func(rec *DBInstanceRecord) bool {
				if rec.ModifyLease == nil || rec.ModifyLease.Holder != holder {
					return false
				}
				renewedUntil = time.Now().UTC().Add(s.modifyLeaseTTL())
				rec.ModifyLease.ExpiresAt = renewedUntil
				return true
			})
			cancelRenew()
			if err != nil {
				if !time.Now().UTC().Before(expiresAt) {
					cancelApply(fmt.Errorf("%w: renewals did not succeed before expiry", errModifyLeaseLost))
					return
				}
				slog.WarnContext(ctx, "rds: renewing the modify lease failed; retrying",
					"dbInstance", id, "holder", holder, "err", err)
				continue
			}
			if !held {
				slog.WarnContext(ctx, "rds: the modify lease was taken over while the change was still running",
					"dbInstance", id, "holder", holder)
				cancelApply(fmt.Errorf("%w: another holder took it over", errModifyLeaseLost))
				return
			}

			expiresAt = renewedUntil
			if !expiry.Stop() {
				select {
				case <-expiry.C:
				default:
				}
			}
			expiry.Reset(time.Until(expiresAt))
		}
	}
}
