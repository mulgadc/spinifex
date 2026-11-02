package codestore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	telemetry "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
)

// ErrNotReady means the code store is not provisioned or its role is not
// assumable yet; callers report ServiceException.
var ErrNotReady = errors.New("lambda code store not ready")

const (
	initialBackoff = time.Second
	maxBackoff     = 5 * time.Minute
)

// Manager provisions and opens the store on first use, so a process that never
// uses Lambda creates nothing. After a failure, calls inside the capped backoff
// window fail fast with the last reason instead of retrying.
type Manager struct {
	provision func(context.Context) error
	open      func() (*Store, error)
	now       func() time.Time

	mu      sync.Mutex
	store   *Store
	reason  error
	backoff time.Duration
	retryAt time.Time
}

// NewManager binds the bounded provisioning step and the scoped opener. It does
// no I/O.
func NewManager(provision func(context.Context) error, open func() (*Store, error)) *Manager {
	return &Manager{provision: provision, open: open, now: time.Now}
}

// Store returns the opened store, initialising it on first use. An ErrNotOwned
// refusal is retried after backoff, so an operator's fix takes effect without a
// restart.
func (m *Manager) Store(ctx context.Context) (*Store, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store != nil {
		return m.store, nil
	}
	if now := m.now(); now.Before(m.retryAt) {
		return nil, fmt.Errorf("%w: %w", ErrNotReady, m.reason)
	}

	store, err := m.attempt(ctx)
	if err == nil {
		m.store, m.reason, m.backoff = store, nil, 0
		slog.InfoContext(ctx, "Lambda code store ready")
		return store, nil
	}
	m.backoff = min(max(m.backoff*2, initialBackoff), maxBackoff)
	m.reason, m.retryAt = err, m.now().Add(m.backoff)
	slog.WarnContext(ctx, "Lambda code store not ready", "err", err, "retry_ms", telemetry.Millis(m.backoff))
	return nil, fmt.Errorf("%w: %w", ErrNotReady, err)
}

func (m *Manager) attempt(ctx context.Context) (*Store, error) {
	if err := m.provision(ctx); err != nil {
		return nil, err
	}
	return m.open()
}
