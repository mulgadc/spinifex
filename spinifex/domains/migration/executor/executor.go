// Package executor is the per-node migration executor. In this release it
// runs no jobs: it drains records marked for deletion and expires the cached
// AWS sessions on a timer.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/migration/admission"
	"github.com/mulgadc/spinifex/spinifex/domains/migration/issuer"
	"github.com/mulgadc/spinifex/spinifex/domains/migration/store"
	"github.com/nats-io/nats.go/jetstream"
)

// SweepInterval is how often each node's executor runs a sweep.
const SweepInterval = 30 * time.Second

// Objects deletes the platform objects stored under a prefix. Deleting a
// prefix that holds nothing is success.
type Objects interface {
	DeletePrefix(ctx context.Context, prefix string) error
}

// NoObjects is the Objects of a release whose migration writes no object.
type NoObjects struct{}

var _ Objects = NoObjects{}

// DeletePrefix has nothing to delete.
func (NoObjects) DeletePrefix(context.Context, string) error { return nil }

// SessionCache is the in-memory AWS session cache of this process.
type SessionCache interface {
	Sweep()
	Accounts() []string
	DropAccount(accountID string)
}

var _ SessionCache = (*issuer.Cache)(nil)

// AccountPrefix is the object prefix of every object an account's migration
// records name.
func AccountPrefix(accountID string) string { return "migration/" + accountID + "/" }

// RecordPrefix is the object prefix of one record's objects.
func RecordPrefix(accountID, kind, id string) string {
	return AccountPrefix(accountID) + kind + "/" + id + "/"
}

// Executor is one node's executor.
type Executor struct {
	js      jetstream.KeyValueManager
	node    string
	objects Objects
	cache   SessionCache
	seals   *admission.Seals
}

// New returns the executor of node. cache may be nil while this process holds
// no session cache.
func New(js jetstream.KeyValueManager, node string, objects Objects, cache SessionCache) (*Executor, error) {
	if js == nil || node == "" || objects == nil {
		return nil, errors.New("migration executor needs JetStream, a node ID and an object store")
	}
	return &Executor{js: js, node: node, objects: objects, cache: cache, seals: admission.NewSeals(js)}, nil
}

// Node is the node this executor runs on.
func (x *Executor) Node() string { return x.node }

// Run sweeps once per tick until ctx ends. A failed sweep is logged and the
// next tick retries it.
func (x *Executor) Run(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
		if err := x.Sweep(ctx); err != nil {
			slog.WarnContext(ctx, "Migration executor sweep incomplete", "node", x.node, "err", err)
		}
	}
}

// Sweep expires cached sessions, drops the sessions of sealed accounts, and
// resumes the deletion of every record marked for deletion.
func (x *Executor) Sweep(ctx context.Context) error {
	var errs []error
	if x.cache != nil {
		x.cache.Sweep()
		for _, account := range x.cache.Accounts() {
			sealed, err := x.seals.Sealed(ctx, account)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if sealed {
				x.cache.DropAccount(account)
			}
		}
	}
	accounts, err := store.AccountIDs(ctx, x.js)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, account := range accounts {
		if _, err := x.Drain(ctx, account); err != nil && !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, fmt.Errorf("drain account %s: %w", account, err))
		}
	}
	return errors.Join(errs...)
}

// DrainResult is what one drain pass left, per kind.
type DrainResult struct {
	// Waiting counts marked records whose owner has not recorded it stopped.
	Waiting map[string]int
	// Deleted counts records this pass removed.
	Deleted map[string]int
}

// Drain makes one deletion pass over an account's records marked for
// deletion. It deletes a record's objects, then the record, only once no
// executor holds it unstopped. It never waits for an owner.
func (x *Executor) Drain(ctx context.Context, accountID string) (DrainResult, error) {
	res := DrainResult{Waiting: map[string]int{}, Deleted: map[string]int{}}
	s, err := store.Open(ctx, x.js, accountID)
	if err != nil {
		return res, err
	}
	listing, err := s.List(ctx)
	if err != nil {
		return res, err
	}
	var errs []error
	for _, e := range listing.Entries {
		if !e.Record.DeleteRequested {
			continue
		}
		deleted, err := x.drainRecord(ctx, s, e)
		switch {
		case err != nil:
			errs = append(errs, err)
		case deleted:
			res.Deleted[e.Record.Kind]++
		default:
			res.Waiting[e.Record.Kind]++
		}
	}
	return res, errors.Join(errs...)
}

// drainRecord deletes one marked record when its owner allows. This executor
// runs no job, so a claim it holds for itself is a stale one and is stopped.
func (x *Executor) drainRecord(ctx context.Context, s *store.Store, e store.Entry) (bool, error) {
	rec := e.Record
	if rec.Owner != nil && !rec.Owner.Stopped {
		if rec.Owner.Node != x.node {
			return false, nil
		}
		var err error
		if e, err = s.RecordStopped(ctx, rec.Kind, rec.ID, x.node); err != nil {
			return false, err
		}
		rec = e.Record
	}
	if err := x.objects.DeletePrefix(ctx, RecordPrefix(s.AccountID(), rec.Kind, rec.ID)); err != nil {
		return false, fmt.Errorf("delete record objects: %w", err)
	}
	if err := s.Delete(ctx, rec.Kind, rec.ID, e.Revision); err != nil {
		return false, err
	}
	return true, nil
}
