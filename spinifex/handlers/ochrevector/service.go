package handlers_ochrevector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// reconcileStuckAfter bounds how long a CREATING/DELETING record may sit
// before Reconcile treats it as abandoned by a crashed writer and resumes it.
const reconcileStuckAfter = 5 * time.Minute

// rollbackTimeout bounds a rollback's own cleanup, run with the request
// context already cancelled or expired.
const rollbackTimeout = 30 * time.Second

// CreateIndexParams is the caller-supplied half of a new index: everything
// the registry does not derive itself.
type CreateIndexParams struct {
	Name           string
	Dimension      int
	EmbeddingModel string
}

// Service orchestrates the Registry and a VectorBackend into the
// create/delete/list/reconcile index lifecycle (D4): the registry is always
// mutated first on create and last on delete, so a crash never leaves an
// orphan Postgres table with no registry entry, nor a registry entry that
// silently outlives its table's success.
type Service struct {
	Registry *Registry
	Backend  VectorBackend
}

// NewService constructs a Service over registry and backend.
func NewService(registry *Registry, backend VectorBackend) *Service {
	return &Service{Registry: registry, Backend: backend}
}

// CreateIndex reserves indexID in the registry (state CREATING), provisions
// the account's schema/role and the index's table in the backend, then flips
// the record to READY. A backend failure rolls back both the backend state
// and the reservation, so no half-created index survives the call.
func (s *Service) CreateIndex(ctx context.Context, accountID, indexID string, params CreateIndexParams) (*Record, error) {
	if err := validateIndexID(indexID); err != nil {
		return nil, err
	}
	if params.Dimension <= 0 {
		return nil, fmt.Errorf("ochrevector: index %s: dimension must be positive, got %d", indexID, params.Dimension)
	}

	now := time.Now().UTC()
	rec := Record{
		ID:             indexID,
		Name:           params.Name,
		Dimension:      params.Dimension,
		EmbeddingModel: params.EmbeddingModel,
		State:          StateCreating,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.Registry.Reserve(ctx, accountID, rec); err != nil {
		return nil, err
	}
	rec.AccountID = accountID

	if err := s.provisionIndex(ctx, accountID, indexID, params.Dimension); err != nil {
		s.rollbackCreate(ctx, accountID, indexID)
		return nil, err
	}
	if err := s.Registry.SetState(ctx, accountID, indexID, StateReady); err != nil {
		s.rollbackCreate(ctx, accountID, indexID)
		return nil, err
	}

	rec.State = StateReady
	rec.UpdatedAt = time.Now().UTC()
	return &rec, nil
}

// provisionIndex runs the two backend calls a create needs: the account's
// schema/role (idempotent, safe even if it already exists) and the index's
// table. Both are safe to retry, so Reconcile calls this same path to resume
// a create a crash interrupted rather than rolling it back.
func (s *Service) provisionIndex(ctx context.Context, accountID, indexID string, dimension int) error {
	if err := s.Backend.EnsureAccount(ctx, accountID); err != nil {
		return fmt.Errorf("ochrevector: ensure account %s: %w", accountID, err)
	}
	if err := s.Backend.CreateIndex(ctx, accountID, IndexSpec{ID: indexID, Dimension: dimension}); err != nil {
		return fmt.Errorf("ochrevector: create index %s: %w", indexID, err)
	}
	return nil
}

// rollbackCreate unwinds a failed create: drop whatever table might have
// been created, then withdraw the reservation, so no half-state — a table
// without a registry entry, or a registry entry without a real table —
// survives. Best-effort: failures are logged, not propagated, since the
// caller has already failed and there is nothing further to roll back to.
func (s *Service) rollbackCreate(ctx context.Context, accountID, indexID string) {
	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if err := s.Backend.DropIndex(rbCtx, accountID, indexID); err != nil {
		slog.WarnContext(rbCtx, "ochrevector: rollback drop index failed", "index", indexID, "account", accountID, "err", err)
	}
	if err := s.Registry.Delete(rbCtx, accountID, indexID); err != nil {
		slog.WarnContext(rbCtx, "ochrevector: rollback delete registry record failed", "index", indexID, "account", accountID, "err", err)
	}
}

// DeleteIndex drops indexID's table in Postgres first, then removes its
// registry record (D4) — the ordering that guarantees an orphan table with no
// registry entry never happens, at the cost of a possible orphan *record*
// (state DELETING) if the process crashes between the two steps, which
// Reconcile finishes. Idempotent: deleting an already-absent index is a
// no-op success.
func (s *Service) DeleteIndex(ctx context.Context, accountID, indexID string) error {
	rec, err := s.Registry.Get(ctx, accountID, indexID)
	if err != nil {
		return err
	}
	if rec == nil {
		return nil
	}
	if err := s.Registry.SetState(ctx, accountID, indexID, StateDeleting); err != nil {
		return err
	}
	if err := s.Backend.DropIndex(ctx, accountID, indexID); err != nil {
		return fmt.Errorf("ochrevector: drop index %s: %w", indexID, err)
	}
	if err := s.Registry.Delete(ctx, accountID, indexID); err != nil {
		return err
	}
	return nil
}

// ListIndexes returns every index record accountID owns.
func (s *Service) ListIndexes(ctx context.Context, accountID string) ([]Record, error) {
	return s.Registry.List(ctx, accountID)
}

// Reconcile completes every record stuck in CREATING or DELETING for longer
// than reconcileStuckAfter — the re-entrant crash-recovery primitive a
// reaper calls on a schedule (no scheduler here). A record still within the
// grace period is left alone, so a create/delete in normal-latency flight is
// never disturbed by a concurrent Reconcile pass.
func (s *Service) Reconcile(ctx context.Context) error {
	recs, err := s.Registry.ListAll(ctx)
	if err != nil {
		return fmt.Errorf("ochrevector: reconcile: list all: %w", err)
	}
	now := time.Now().UTC()
	var errs []error
	for _, rec := range recs {
		if now.Sub(rec.UpdatedAt) < reconcileStuckAfter {
			continue
		}
		switch rec.State {
		case StateCreating:
			if err := s.reconcileCreating(ctx, rec); err != nil {
				errs = append(errs, err)
			}
		case StateDeleting:
			if err := s.reconcileDeleting(ctx, rec); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// reconcileCreating resumes a stuck CREATING record: EnsureAccount and
// CreateIndex are both idempotent, so this retries the same forward path
// rather than rolling back a create that may have mostly succeeded before
// the crash.
func (s *Service) reconcileCreating(ctx context.Context, rec Record) error {
	if err := s.provisionIndex(ctx, rec.AccountID, rec.ID, rec.Dimension); err != nil {
		return fmt.Errorf("ochrevector: reconcile creating index %s: %w", rec.ID, err)
	}
	if err := s.Registry.SetState(ctx, rec.AccountID, rec.ID, StateReady); err != nil {
		return fmt.Errorf("ochrevector: reconcile creating index %s: %w", rec.ID, err)
	}
	return nil
}

// reconcileDeleting resumes a stuck DELETING record: drop (idempotent), then
// remove the registry entry.
func (s *Service) reconcileDeleting(ctx context.Context, rec Record) error {
	if err := s.Backend.DropIndex(ctx, rec.AccountID, rec.ID); err != nil {
		return fmt.Errorf("ochrevector: reconcile deleting index %s: %w", rec.ID, err)
	}
	if err := s.Registry.Delete(ctx, rec.AccountID, rec.ID); err != nil {
		return fmt.Errorf("ochrevector: reconcile deleting index %s: %w", rec.ID, err)
	}
	return nil
}
