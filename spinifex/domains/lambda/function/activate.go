package function

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Readiness reports why a function whose code is stored cannot run yet, or nil
// when it can. Its error text becomes the function's StateReason.
type Readiness func(ctx context.Context, rec Record) error

// SetReadiness enables activation. Without it no function ever leaves Pending.
func (s *MetaService) SetReadiness(r Readiness) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = r
	if s.nudge == nil {
		s.nudge = make(chan struct{}, 1)
	}
}

func (s *MetaService) readiness() (Readiness, chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready, s.nudge
}

// wake asks the activation loop for an early pass; it never blocks.
func (s *MetaService) wake() {
	if _, nudge := s.readiness(); nudge != nil {
		select {
		case nudge <- struct{}{}:
		default:
		}
	}
}

// RunActivation activates pending functions until ctx ends: once at start, on
// every code-stored completion, and every interval, so a crash between storing
// the code and activating it is converged by the next pass.
func (s *MetaService) RunActivation(ctx context.Context, interval time.Duration) {
	ready, nudge := s.readiness()
	if ready == nil {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.ActivatePending(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("Lambda: activation pass incomplete; retrying", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-nudge:
		case <-t.C:
		}
	}
}

// ActivatePending makes one pass over every account's functions, moving each
// Pending function with stored code to Active when readiness allows it.
func (s *MetaService) ActivatePending(ctx context.Context) error {
	ready, _ := s.readiness()
	if ready == nil {
		return nil
	}
	var accounts []string
	names := s.js.KeyValueStoreNames(ctx)
	for name := range names.Name() {
		if id, ok := strings.CutPrefix(name, KVBucketAccountPrefix); ok {
			accounts = append(accounts, id)
		}
	}
	if err := names.Error(); err != nil {
		return fmt.Errorf("lambda: list account buckets: %w", err)
	}
	var errs []error
	for _, accountID := range accounts {
		if err := s.activateAccount(ctx, accountID, ready); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *MetaService) activateAccount(ctx context.Context, accountID string, ready Readiness) error {
	kv, err := s.bucket(ctx, accountID)
	if err != nil {
		return err
	}
	keys, err := kv.ListKeysFiltered(ctx, kvFunctionKeyPrefix+">")
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lambda: list functions of account %s: %w", accountID, err)
	}
	var errs []error
	for key := range keys.Keys() {
		if err := s.activateOne(ctx, kv, key, ready); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// activateOne writes the readiness outcome by compare-and-swap at the revision
// it read, so a concurrent update or delete wins and is never overwritten.
func (s *MetaService) activateOne(ctx context.Context, kv jetstream.KeyValue, key string, ready Readiness) error {
	entry, err := kv.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lambda: read %s: %w", key, err)
	}
	rec, err := decode(entry.Value())
	if err != nil {
		return err
	}
	if rec.State != StatePending || rec.Code.State != CodeStored {
		return nil
	}
	if notReady := ready(ctx, rec); notReady != nil {
		if rec.StateReason == notReady.Error() {
			return nil
		}
		rec.StateReasonCode, rec.StateReason = ReasonCodeCreating, notReady.Error()
	} else {
		rec.State, rec.StateReasonCode, rec.StateReason = StateActive, "", ""
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := kv.Update(ctx, key, data, entry.Revision()); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return nil
		}
		return fmt.Errorf("lambda: write readiness of %s: %w", key, err)
	}
	if rec.State == StateActive {
		slog.Info("Lambda: function active", "account_id", rec.AccountID, "region", rec.Region,
			"function_name", rec.FunctionName, "function_id", rec.FunctionID)
	}
	return nil
}
