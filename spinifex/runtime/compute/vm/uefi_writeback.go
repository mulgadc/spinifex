package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// UEFIStoreRecords is the store-record bucket as a write-back reads and
// changes it. A record that is missing or deleting is uefistore.ErrNotLive.
type UEFIStoreRecords interface {
	LiveWriteBack(ctx context.Context, instanceID string) (*uefistore.UEFIWriteBack, error)
	RecordWriteBack(ctx context.Context, instanceID string, wb uefistore.UEFIWriteBack) error
	ClearWriteBack(ctx context.Context, instanceID, key string) error
	WriteBacksOwedBy(ctx context.Context, node string) ([]string, error)
}

var _ UEFIStoreRecords = (*uefistore.Store)(nil)

// UEFIVarStoreOwner is what an instance record says about who may write its
// variable store back, read at Revision.
type UEFIVarStoreOwner struct {
	Status   InstanceState
	LastNode string
	Pointer  *UEFIVarStoreRef
	Revision uint64
}

// UEFIInstancePointers reads and moves the variable store pointer on the
// cluster's instance records. Reads are of the current revision, an absent
// record is found=false, and an unreachable store is an error, never absence.
type UEFIInstancePointers interface {
	LoadUEFIVarStoreOwner(instanceID string) (owner UEFIVarStoreOwner, found bool, err error)
	// CompareAndSetUEFIVarStore points the record at ref only while it is
	// still at rev, and fails otherwise.
	CompareAndSetUEFIVarStore(instanceID string, ref *UEFIVarStoreRef, rev uint64) error
}

// errUEFIRecordWriteFailed is a stop whose record write did not land, so the
// pointer its write-back set may not be on the record.
var errUEFIRecordWriteFailed = errors.New("the stop's instance record write did not land")

// uefiWriteBack is a write-back whose put landed and whose pointer is set on
// the in-memory instance, held under the instance's write-back lock until the
// stop's record write settles it.
type uefiWriteBack struct {
	vm         *VM
	dir        string
	key        string
	seededFrom string
	since      time.Time
	previous   *UEFIVarStoreRef
	put        bool
	unlock     func()
}

// writeBackWired reports whether this node can write variable stores back.
func (d UEFIVarStoreDeps) writeBackWired() bool {
	return d.Objects != nil && d.Bucket != "" && d.DataDir != "" && d.Records != nil && d.Instances != nil
}

// writeBackUEFIVarStore writes a sealing stop's live variable store to a key
// under the instance's prefix, once QEMU is down. The returned write-back is
// settled after the stop's record write; nil means there is nothing to settle.
func (m *Manager) writeBackUEFIVarStore(ctx context.Context, instance *VM) *uefiWriteBack {
	var ref *UEFIVarStoreRef
	var status InstanceState
	fenced := false
	m.Inspect(instance, func(v *VM) {
		if v.UEFIVarStore != nil {
			cp := *v.UEFIVarStore
			ref = &cp
		}
		status = v.Status
		fenced = VolumeFenced(v.Instance)
	})
	if ref == nil || !UsesUEFIVarStore(instance) {
		return nil
	}
	switch {
	case status == StateError:
		skipUEFIWriteBack(ctx, instance.ID, "error", "the instance is in error")
		return nil
	case fenced:
		skipUEFIWriteBack(ctx, instance.ID, "fenced", "a fence stopped the instance")
		return nil
	}
	deps := m.deps.UEFIVarStore
	if !deps.writeBackWired() || m.deps.NodeID == "" {
		slog.ErrorContext(ctx, "UEFI variable store write-back is not wired on this node; the store keeps its last version", "instanceId", instance.ID)
		recordUEFIWriteBackOutcome("unwired")
		return nil
	}
	dir, err := uefiInstanceDir(deps.DataDir, instance.ID)
	if err != nil {
		slog.ErrorContext(ctx, "UEFI variable store write-back has no directory", "instanceId", instance.ID, "err", err)
		recordUEFIWriteBackOutcome("failed")
		return nil
	}

	unlock := m.uefiLocks.lock(instance.ID)
	wb := &uefiWriteBack{vm: instance, dir: dir, previous: ref, since: time.Now().UTC(), unlock: unlock}
	if !m.prepareUEFIWriteBack(ctx, wb, ref) {
		unlock()
		return nil
	}
	return wb
}

// prepareUEFIWriteBack runs the skip checks, chooses the key and puts the
// store. It reports false when nothing is left to settle: a skip, or a failure
// already recorded as an obligation for the reaper.
func (m *Manager) prepareUEFIWriteBack(ctx context.Context, wb *uefiWriteBack, ref *UEFIVarStoreRef) bool {
	id := wb.vm.ID
	deps := m.deps.UEFIVarStore
	seed, err := readUEFISeed(wb.dir)
	if err != nil {
		skipUEFIWriteBack(ctx, id, "unseeded", "this node holds no record of what its copy was seeded from", "err", err)
		return false
	}
	if seed != ref.Key {
		skipUEFIWriteBack(ctx, id, "reseeded", "the record moved on after this node seeded", "seededFrom", seed, "pointer", ref.Key)
		return false
	}
	wb.seededFrom = seed

	owed, err := deps.Records.LiveWriteBack(ctx, id)
	if errors.Is(err, uefistore.ErrNotLive) {
		skipUEFIWriteBack(ctx, id, "store_not_live", "the store record is gone or deleting", "err", err)
		return false
	}
	if err != nil {
		slog.ErrorContext(ctx, "Could not read the UEFI store record; keeping this node's copy unwritten", "instanceId", id, "err", err)
		recordUEFIWriteBackOutcome("unrecorded")
		return false
	}
	// A retry of this node's own obligation reuses its key, so failed write-backs
	// leave one leftover rather than one each. The pointed key is never reused.
	if owed != nil && owed.Node == m.deps.NodeID && owed.Key != seed {
		wb.key, wb.since = owed.Key, owed.Since
	} else if wb.key, err = uefistore.NewObjectKey(id); err != nil {
		slog.ErrorContext(ctx, "Could not name a UEFI variable store object", "instanceId", id, "err", err)
		recordUEFIWriteBackOutcome("failed")
		return false
	}

	owner, found, err := deps.Instances.LoadUEFIVarStoreOwner(id)
	switch {
	case err != nil:
		m.deferUEFIWriteBack(ctx, wb, fmt.Errorf("read instance record: %w", err))
		return false
	case !found:
		m.deferUEFIWriteBack(ctx, wb, errors.New("no instance record to check ownership against"))
		return false
	case owner.LastNode != m.deps.NodeID:
		skipUEFIWriteBack(ctx, id, "superseded", "the record names another node", "lastNode", owner.LastNode)
		return false
	case owner.Pointer == nil || owner.Pointer.Key != seed:
		var pointer string
		if owner.Pointer != nil {
			pointer = owner.Pointer.Key
		}
		skipUEFIWriteBack(ctx, id, "reseeded", "the record moved on after this node seeded", "seededFrom", seed, "pointer", pointer)
		return false
	}

	start := time.Now()
	written, err := m.putUEFIVars(ctx, id, wb.dir, wb.key, ref.Firmware.Arch)
	if err != nil {
		m.deferUEFIWriteBack(ctx, wb, err)
		return false
	}
	wb.put = true
	m.Inspect(wb.vm, func(v *VM) { v.UEFIVarStore = written })
	slog.InfoContext(ctx, "Wrote the UEFI variable store back", "instanceId", id, "key", wb.key,
		"seededFrom", seed, "put_ms", otelsetup.Millis(time.Since(start)))
	return true
}

// settleUEFIWriteBack completes wb once the stop's record write is known. A
// record write that failed reverts the in-memory pointer and leaves the
// obligation for the reaper, which puts to the same key.
func (m *Manager) settleUEFIWriteBack(ctx context.Context, wb *uefiWriteBack, recordErr error) {
	if wb == nil {
		return
	}
	defer wb.unlock()
	if recordErr != nil {
		m.Inspect(wb.vm, func(v *VM) {
			if v.UEFIVarStore != nil && v.UEFIVarStore.Key == wb.key {
				v.UEFIVarStore = wb.previous
			}
		})
		m.deferUEFIWriteBack(ctx, wb, recordErr)
		return
	}
	if m.finishUEFIWriteBack(ctx, wb.vm.ID, wb.dir, wb.key, wb.seededFrom, "") {
		recordUEFIWriteBackOutcome("written")
	}
}

// deferUEFIWriteBack records wb as owed by this node and keeps vars.fd, so
// the stop completes and the reaper converges. A store that is no longer live
// owes nothing, so a put already made is deleted.
func (m *Manager) deferUEFIWriteBack(ctx context.Context, wb *uefiWriteBack, cause error) {
	id := wb.vm.ID
	err := m.deps.UEFIVarStore.Records.RecordWriteBack(ctx, id, uefistore.UEFIWriteBack{
		Node: m.deps.NodeID, Key: wb.key, SeededFrom: wb.seededFrom, Since: wb.since,
	})
	switch {
	case errors.Is(err, uefistore.ErrNotLive):
		if wb.put {
			m.deleteUEFIObject(ctx, id, wb.key)
		}
		slog.InfoContext(ctx, "UEFI store record is gone or deleting; no write-back is owed", "instanceId", id, "cause", cause, "err", err)
		recordUEFIWriteBackOutcome("dropped")
	case err != nil:
		slog.ErrorContext(ctx, "UEFI variable store write-back failed and could not be recorded; only a start on this node keeps the changes",
			"instanceId", id, "key", wb.key, "cause", cause, "err", err)
		recordUEFIWriteBackOutcome("unrecorded")
	default:
		slog.WarnContext(ctx, "UEFI variable store write-back failed; recorded for retry", "instanceId", id, "key", wb.key,
			"seededFrom", wb.seededFrom, "cause", cause)
		recordUEFIWriteBackOutcome("deferred")
	}
}

// finishUEFIWriteBack runs once a record points at key: seeded.json names it,
// the obligation is cleared and the prefix pruned. It reports whether the
// store was still live; when not, key is deleted.
func (m *Manager) finishUEFIWriteBack(ctx context.Context, id, dir, key, seededFrom, clearKey string) bool {
	deps := m.deps.UEFIVarStore
	if err := writeUEFISeed(dir, key); err != nil {
		slog.WarnContext(ctx, "Could not record the written-back UEFI store locally; the next start here fetches it", "instanceId", id, "key", key, "err", err)
	}
	err := deps.Records.ClearWriteBack(ctx, id, clearKey)
	if errors.Is(err, uefistore.ErrNotLive) {
		m.deleteUEFIObject(ctx, id, key)
		slog.InfoContext(ctx, "UEFI store record is gone or deleting; deleted the store just written", "instanceId", id, "key", key, "err", err)
		recordUEFIWriteBackOutcome("dropped")
		return false
	}
	if err != nil {
		// An obligation left on the record names a key a retry reuses, so the
		// prune waits for the next write rather than guess which to keep.
		slog.WarnContext(ctx, "Could not clear the UEFI write-back obligation; not pruning", "instanceId", id, "err", err)
		return true
	}
	deleted, err := uefistore.Prune(ctx, deps.Objects, deps.Bucket, id, key, seededFrom)
	if len(deleted) > 0 {
		slog.InfoContext(ctx, "Pruned old UEFI variable store versions", "instanceId", id, "deleted", deleted, "pointer", key)
	}
	if err != nil {
		slog.WarnContext(ctx, "Could not prune old UEFI variable store versions", "instanceId", id, "err", err)
	}
	return true
}

// putUEFIVars puts this node's vars.fd to key under this node's firmware for
// arch, returning the pointer that names it.
func (m *Manager) putUEFIVars(ctx context.Context, id, dir, key, arch string) (*UEFIVarStoreRef, error) {
	deps := m.deps.UEFIVarStore
	fw, err := NodeFirmwareFor(arch)
	if err != nil {
		return nil, err
	}
	region, err := os.ReadFile(filepath.Join(dir, uefiVarsFileName))
	if err != nil {
		return nil, fmt.Errorf("read UEFI variable store for %s: %w", id, err)
	}
	if err := uefistore.PutObject(ctx, deps.Objects, deps.Bucket, key, region, uefistore.ObjectMeta{
		OwnerID:    id,
		WriterNode: m.deps.NodeID,
		Firmware:   fw.Identity,
		CodePath:   fw.Code,
		Source:     uefistore.SourceWriteBack,
	}); err != nil {
		return nil, err
	}
	return &UEFIVarStoreRef{Key: key, Firmware: fw.Identity, WriterNode: m.deps.NodeID, WrittenAt: time.Now().UTC()}, nil
}

// deleteUEFIObject deletes one object this node wrote and no record points at.
func (m *Manager) deleteUEFIObject(ctx context.Context, id, key string) {
	deps := m.deps.UEFIVarStore
	ctx, cancel := context.WithTimeout(ctx, uefistore.PutTimeout)
	defer cancel()
	_, err := deps.Objects.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(deps.Bucket), Key: aws.String(key)})
	if err != nil && !objectstore.IsNoSuchKeyError(err) {
		slog.WarnContext(ctx, "Could not delete an unpointed UEFI variable store object; a later prune removes it", "instanceId", id, "key", key, "err", err)
	}
}

// clearForeignUEFIWriteBack drops another node's obligation once this node has
// started the instance from the pointer: that node's copy is no longer the
// instance's store, and its changes since launch are lost as with the node.
func (m *Manager) clearForeignUEFIWriteBack(ctx context.Context, id string) {
	records := m.deps.UEFIVarStore.Records
	if records == nil {
		return
	}
	owed, err := records.LiveWriteBack(ctx, id)
	if err != nil || owed == nil || owed.Node == m.deps.NodeID {
		return
	}
	if err := records.ClearWriteBack(ctx, id, owed.Key); err != nil {
		slog.WarnContext(ctx, "Could not clear another node's UEFI write-back obligation", "instanceId", id, "node", owed.Node, "err", err)
		return
	}
	slog.InfoContext(ctx, "Cleared another node's UEFI write-back obligation; started from the pointer", "instanceId", id,
		"node", owed.Node, "key", owed.Key, "since", owed.Since)
}

// readUEFISeed returns the key seeded.json in dir names.
func readUEFISeed(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, uefiSeededFileName))
	if err != nil {
		return "", err
	}
	var seed uefiSeed
	if err := json.Unmarshal(data, &seed); err != nil {
		return "", fmt.Errorf("decode %s: %w", uefiSeededFileName, err)
	}
	if seed.Key == "" {
		return "", fmt.Errorf("%s names no key", uefiSeededFileName)
	}
	return seed.Key, nil
}

// writeUEFISeed records that vars.fd in dir holds the store at key.
func writeUEFISeed(dir, key string) error {
	seed, err := json.Marshal(uefiSeed{Key: key, SeededAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	return writeFileAtomic(dir, uefiSeededFileName, seed)
}

func skipUEFIWriteBack(ctx context.Context, id, reason, why string, attrs ...any) {
	uefiStoreMetrics()
	if uefiWriteBackSkips != nil {
		uefiWriteBackSkips.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", reason)))
	}
	slog.InfoContext(ctx, "Skipping UEFI variable store write-back: "+why, append([]any{"instanceId", id, "reason", reason}, attrs...)...)
}

func recordUEFIWriteBackOutcome(outcome string) {
	uefiStoreMetrics()
	if uefiWriteBacks != nil {
		uefiWriteBacks.Add(context.Background(), 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	}
}
