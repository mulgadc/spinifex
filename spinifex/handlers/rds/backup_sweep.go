package handlers_rds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go/jetstream"
)

// The retention sweep is a cluster-wide vm.Reaper rather than a reconciler tick,
// because it is the one part of rds-9 that deletes customer data. The shared GC
// backstop buys it the two properties that matters most for: it is skipped
// entirely while KV is unhealthy, so it can never reap against a desired state it
// cannot read, and cluster-wide scope is leader-gated by the framework. Backup
// *creation* stays in the RDS reconciler, where the per-instance state it reads
// already lives.
type BackupRetentionReaper struct {
	svc *Service
	// The per-pass bound on deletions. Under-collecting for one interval is free;
	// a sweep that walks the whole object store every two minutes is not.
	limit int
}

var _ vm.Reaper = (*BackupRetentionReaper)(nil)

func (s *Service) NewBackupRetentionReaper() *BackupRetentionReaper {
	return &BackupRetentionReaper{svc: s, limit: s.sweepDeleteLimit()}
}

func (r *BackupRetentionReaper) Class() string         { return "rds-backup-retention" }
func (r *BackupRetentionReaper) Scope() vm.ReaperScope { return vm.ScopeClusterWide }

// Sweeps every account's automated backups past their retention, and reclaims the
// retained data volumes nothing references any more. Drives from the RDS KV index
// rather than from a snapshot scan: a bucket-wide ListObjectsV2 per pass would
// grow without bound with the fleet, and the same cost lands on viperblock's own
// GC safety scan.
func (r *BackupRetentionReaper) Sweep(ctx context.Context) (int, error) {
	js, err := r.svc.js()
	if err != nil {
		return 0, err
	}
	// A truncated listing must never read as "no accounts": that would leave every
	// tenant's retention unenforced while looking like a clean pass.
	buckets, err := AccountBucketNames(ctx, js)
	if err != nil {
		return 0, fmt.Errorf("rds: enumerate account buckets: %w", err)
	}

	reaped := 0
	var failures []error
	for _, bucket := range buckets {
		if reaped >= r.limit {
			break
		}
		if err := ctx.Err(); err != nil {
			return reaped, err
		}
		kv, err := js.KeyValue(ctx, bucket)
		if err != nil {
			failures = append(failures, fmt.Errorf("open %s: %w", bucket, err))
			continue
		}
		accountID := AccountIDFromBucketName(bucket)
		swept, err := r.sweepAccount(ctx, kv, accountID, r.limit-reaped)
		reaped += swept
		if err != nil {
			failures = append(failures, fmt.Errorf("sweep %s: %w", bucket, err))
		}
	}
	return reaped, errors.Join(failures...)
}

// One account: the over-retention automated snapshots of every instance, then the
// retained volumes whose last snapshot is gone.
func (r *BackupRetentionReaper) sweepAccount(ctx context.Context, kv jetstream.KeyValue,
	accountID string, budget int) (int, error) {
	indexed, err := ListAutomatedBackups(ctx, kv)
	if err != nil {
		return 0, err
	}

	reaped := 0
	var failures []error
	for _, id := range slices.Sorted(maps.Keys(indexed)) {
		if reaped >= budget {
			return reaped, errors.Join(failures...)
		}
		swept, err := r.svc.sweepInstanceBackups(ctx, kv, accountID, id, indexed[id], budget-reaped)
		reaped += swept
		if err != nil {
			failures = append(failures, fmt.Errorf("DB instance %s: %w", id, err))
		}
	}

	// The backstop for a crash between rds-8's last DeleteDBSnapshot and the
	// inline volume delete that follows it: nothing else references the volume by
	// then, so without this it is orphaned permanently.
	volumes, err := r.svc.reclaimOrphanedVolumes(ctx, kv)
	if err != nil {
		failures = append(failures, err)
	}
	return reaped + volumes, errors.Join(failures...)
}

// Removes this instance's automated snapshots older than its retention. Two rules
// bound it: an in-use snapshot is skipped rather than failed, and the newest
// available snapshot is never deleted while automated backups are on — if backup
// creation has been failing for a week, strict retention would delete the whole
// backup set at the exact moment a backup matters most.
func (s *Service) sweepInstanceBackups(ctx context.Context, kv jetstream.KeyValue,
	accountID, dbInstanceIdentifier string, stamps []string, budget int) (int, error) {
	var rec DBInstanceRecord
	found, err := getJSON(ctx, kv, DBInstanceKey(dbInstanceIdentifier), &rec)
	if err != nil {
		return 0, err
	}
	// An index outliving its instance is a teardown that did not finish. Everything
	// goes, newest included: nothing can restore an instance that no longer exists,
	// and the alternative is a data volume pinned for good.
	retention := time.Duration(0)
	if found {
		retention = time.Duration(rec.BackupRetentionPeriod) * oneDay
	} else if err := confirmDBInstanceGone(ctx, kv, dbInstanceIdentifier); err != nil {
		return 0, err
	}

	entries, err := s.loadAutomatedBackups(ctx, kv, dbInstanceIdentifier, stamps)
	if err != nil {
		return 0, err
	}
	keep := ""
	// Retention 0 means automated backups are off, and the point of turning them
	// off is to make the volume GC-eligible again — so the last one goes too.
	if retention > 0 {
		keep = newestAvailableBackup(entries)
	}

	cutoff := time.Now().UTC().Add(-retention)
	reaped := 0
	var failures []error
	for _, entry := range entries {
		if reaped >= budget {
			break
		}
		if entry.DBSnapshotIdentifier == keep || entry.CreatedAt.After(cutoff) {
			continue
		}
		deleted, err := s.deleteAutomatedBackup(ctx, kv, accountID, entry)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if deleted {
			reaped++
		}
	}
	return reaped, errors.Join(failures...)
}

// The corroboration the purge-everything branch needs before it runs. A single
// absent key is not proof the instance is gone — a read served by a lagging
// replica reads the same way — and that branch removes a live instance's whole
// backup set, newest included. The bucket listing is a second, independent read
// path, so an identifier it still names fails this instance's sweep instead.
func confirmDBInstanceGone(ctx context.Context, kv jetstream.KeyValue, dbInstanceIdentifier string) error {
	ids, err := ListDBInstanceIDs(ctx, kv)
	if err != nil {
		return err
	}
	if slices.Contains(ids, dbInstanceIdentifier) {
		return fmt.Errorf("rds: the record of %s could not be read while its bucket still names it; "+
			"its automated backups are left alone", dbInstanceIdentifier)
	}
	slog.WarnContext(ctx, "rds: removing the whole automated backup set of a DB instance that no longer exists",
		"dbInstance", dbInstanceIdentifier)
	return nil
}

// The index entries of one instance, oldest first. An entry whose snapshot record
// is gone is a half-finished delete: the entry is removed so the index stops
// naming it, and it counts as nothing reaped because no data went with it.
func (s *Service) loadAutomatedBackups(ctx context.Context, kv jetstream.KeyValue,
	dbInstanceIdentifier string, stamps []string) ([]automatedBackup, error) {
	slices.Sort(stamps)
	entries := make([]automatedBackup, 0, len(stamps))
	for _, stamp := range stamps {
		var entry AutomatedBackupRecord
		found, err := getJSON(ctx, kv, AutomatedBackupKey(dbInstanceIdentifier, stamp), &entry)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		var snapshot DBSnapshotRecord
		found, err = getJSON(ctx, kv, DBSnapshotKey(entry.DBSnapshotIdentifier), &snapshot)
		if err != nil {
			return nil, err
		}
		if !found {
			if err := s.dropAutomatedBackupIndex(ctx, kv, dbInstanceIdentifier, stamp); err != nil {
				return nil, err
			}
			continue
		}
		// The snapshot record is the authority on age, not the index key: the key is
		// a naming convention, and the record is what a restore reads.
		entry.CreatedAt = snapshot.CreatedAt
		entries = append(entries, automatedBackup{AutomatedBackupRecord: entry, stamp: stamp, snapshot: &snapshot})
	}
	return entries, nil
}

// An index entry paired with the snapshot record it points at.
type automatedBackup struct {
	AutomatedBackupRecord

	stamp    string
	snapshot *DBSnapshotRecord
}

// The newest snapshot retention must not remove. Only an available one counts: a
// snapshot still being cut is not yet something the customer could restore from,
// so keeping it instead would leave them with nothing.
func newestAvailableBackup(entries []automatedBackup) string {
	newest := ""
	var at time.Time
	for _, entry := range entries {
		if entry.snapshot.Status != SnapshotStatusAvailable {
			continue
		}
		if newest == "" || entry.CreatedAt.After(at) {
			newest, at = entry.DBSnapshotIdentifier, entry.CreatedAt
		}
	}
	return newest
}

// Removes one automated snapshot and its index entry. Reports false when the
// snapshot is still being read from — an instance restored from it is still
// running — which is a skip rather than a failure: the next pass retries, and the
// entry stays so it is not lost from the index.
func (s *Service) deleteAutomatedBackup(ctx context.Context, kv jetstream.KeyValue,
	accountID string, entry automatedBackup) (bool, error) {
	err := s.removeDBSnapshot(ctx, kv, accountID, entry.snapshot)
	switch {
	// Both readings of "not now": a snapshot an instance restored from it is still
	// reading, and one still being cut. Neither is a failure — the next pass
	// retries, and the index entry stays so the backup is not lost from it.
	case awserrors.IsErrorCode(err, awserrors.ErrorDBSnapshotInvalidState):
		slog.DebugContext(ctx, "rds: an over-retention automated backup cannot be removed yet; leaving it",
			"dbSnapshot", entry.DBSnapshotIdentifier, "accountId", accountID, "status", entry.snapshot.Status)
		return false, nil
	case awserrors.IsErrorCode(err, awserrors.ErrorDBSnapshotNotFound):
		// Deleted between the index read and here; only the entry is left to clear.
	case err != nil:
		return false, fmt.Errorf("rds: delete the automated backup %s: %w", entry.DBSnapshotIdentifier, err)
	}

	if err := s.dropAutomatedBackupIndex(ctx, kv, entry.DBInstanceIdentifier, entry.stamp); err != nil {
		return false, err
	}
	slog.InfoContext(ctx, "rds: automated backup swept past its retention",
		"dbSnapshot", entry.DBSnapshotIdentifier, "dbInstance", entry.DBInstanceIdentifier,
		"accountId", accountID, "createdAt", entry.CreatedAt)
	return true, nil
}

// Last, always: while the entry exists the snapshot is still findable by the
// sweep, and every step above tolerates work it has already done.
func (s *Service) dropAutomatedBackupIndex(ctx context.Context, kv jetstream.KeyValue,
	dbInstanceIdentifier, stamp string) error {
	key := AutomatedBackupKey(dbInstanceIdentifier, stamp)
	if err := kv.Delete(ctx, key); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("rds: clear the automated-backup index entry %s: %w", key, err)
	}
	return nil
}

// Removes every automated backup of one instance whatever its retention says.
// Two callers, both of which mean "there is nothing left to retain for": a
// BackupRetentionPeriod=0 modify, which turns the feature off, and the teardown
// of the instance itself, whose automated backups would otherwise pin its data
// volume after the instance is gone (D10).
func (s *Service) purgeAutomatedBackups(ctx context.Context, kv jetstream.KeyValue,
	accountID, dbInstanceIdentifier string) error {
	stamps, err := ListAutomatedBackupStamps(ctx, kv, dbInstanceIdentifier)
	if err != nil {
		return err
	}
	entries, err := s.loadAutomatedBackups(ctx, kv, dbInstanceIdentifier, stamps)
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if _, err := s.deleteAutomatedBackup(ctx, kv, accountID, entry); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// Deletes the data volumes retained for snapshots that no longer exist. rds-8
// deletes the volume inline on the last DeleteDBSnapshot, so this only ever fires
// for a crash between those two steps — which is precisely what a KV-health-gated
// cluster-wide sweep is for.
func (s *Service) reclaimOrphanedVolumes(ctx context.Context, kv jetstream.KeyValue) (int, error) {
	ids, err := ListRetainedVolumeIDs(ctx, kv)
	if err != nil {
		return 0, err
	}
	reclaimed := 0
	var failures []error
	for _, id := range ids {
		var retained RetainedVolumeRecord
		found, err := getJSON(ctx, kv, RetainedVolumeKey(id), &retained)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		// Every record is re-checked against the volume store rather than trusted:
		// the holders it was written with are a past reading, and a volume whose last
		// snapshot went in a crashed delete still names it.
		if !found {
			continue
		}
		released, err := s.reclaimRetainedVolume(ctx, kv, &retained)
		if err != nil {
			failures = append(failures, fmt.Errorf("reclaim %s: %w", id, err))
			continue
		}
		if released {
			reclaimed++
		}
	}
	return reclaimed, errors.Join(failures...)
}
