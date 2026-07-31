package handlers_rds

import (
	"context"
	"crypto/rand"
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
const (
	// Short enough that a worker that dies is taken over within a pass or two,
	// and long enough for a renewal to be retried several times inside it.
	modifyLeaseTTL     = 45 * time.Second
	modifyLeaseRefresh = 15 * time.Second
)

// Runs apply while holding the instance's modify lease, renewing it for as long
// as the work runs and releasing it whatever the outcome. Reports whether apply
// ran: a lease already held is not an error, it means another worker is inside
// this change and the caller has nothing to do.
func (s *Service) withModifyLease(ctx context.Context, kv jetstream.KeyValue, id string, apply func() error) (bool, error) {
	holder := s.newModifyLeaseHolder()
	claimed, err := s.claimModifyLease(ctx, kv, id, holder)
	if err != nil || !claimed {
		return false, err
	}

	// The release has to outlive a cancelled ctx, or a shutdown mid-modify
	// leaves the lease to expire on its own and stalls the takeover it exists
	// to enable.
	release := context.WithoutCancel(ctx)
	renewing, stopRenewing := context.WithCancel(ctx)
	var renewals sync.WaitGroup
	renewals.Go(func() { s.renewModifyLease(renewing, kv, id, holder) })

	defer func() {
		stopRenewing()
		renewals.Wait()
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
	return true, apply()
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
func (s *Service) claimModifyLease(ctx context.Context, kv jetstream.KeyValue, id, holder string) (bool, error) {
	return s.updateInstanceIf(ctx, kv, id, func(rec *DBInstanceRecord) bool {
		if rec.ModifyLease.live() && rec.ModifyLease.Holder != holder {
			return false
		}
		rec.ModifyLease = &ModifyLease{Holder: holder, ExpiresAt: time.Now().UTC().Add(modifyLeaseTTL)}
		return true
	})
}

// Pushes the expiry out until the work finishes or ctx is cancelled. A renewal
// that finds the lease taken over stops rather than reclaiming it: another
// worker is already inside the change, and two holders is the state this
// prevents.
func (s *Service) renewModifyLease(ctx context.Context, kv jetstream.KeyValue, id, holder string) {
	ticker := time.NewTicker(modifyLeaseRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			held, err := s.updateInstanceIf(ctx, kv, id, func(rec *DBInstanceRecord) bool {
				if rec.ModifyLease == nil || rec.ModifyLease.Holder != holder {
					return false
				}
				rec.ModifyLease.ExpiresAt = time.Now().UTC().Add(modifyLeaseTTL)
				return true
			})
			if err != nil {
				slog.WarnContext(ctx, "rds: renewing the modify lease failed; retrying",
					"dbInstance", id, "holder", holder, "err", err)
				continue
			}
			if !held {
				slog.WarnContext(ctx, "rds: the modify lease was taken over while the change was still running",
					"dbInstance", id, "holder", holder)
				return
			}
		}
	}
}
