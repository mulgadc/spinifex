package kvsync

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// RunElection holds l across many passes until ctx is cancelled, retrying
// every ttl/3 while another holder leads. onGained fires once per acquisition
// and onLost once per loss, neither under a lock; either may be nil.
//
// An onGained that fails stands the lease down, because a leader that cannot
// set up its work is worse than none; onLost does not fire for it. On return
// the lease is unlocked.
func RunElection(ctx context.Context, l *Lease, onGained func(context.Context) error, onLost func()) {
	// lost is nil while the lease is not held.
	var (
		tok  Token
		lost <-chan struct{}
	)
	try := func() {
		t, err := l.TryLock(ctx)
		if err != nil {
			if he, ok := errors.AsType[*HeldError](err); ok {
				slog.DebugContext(ctx, "kvsync: election held elsewhere", "key", l.key, "node", l.holder, "holder", he.Current.Holder)
			} else {
				slog.WarnContext(ctx, "kvsync: election attempt failed", "key", l.key, "node", l.holder, "err", err)
			}
			return
		}
		ch := l.Lost()
		slog.InfoContext(ctx, "kvsync: elected", "key", l.key, "node", l.holder, "generation", t.Generation)
		if onGained != nil {
			if err := onGained(ctx); err != nil {
				slog.ErrorContext(ctx, "kvsync: onGained failed, standing down", "key", l.key, "node", l.holder, "err", err)
				l.Unlock(ctx, t)
				return
			}
		}
		tok, lost = t, ch
	}
	stepDown := func() {
		lost = nil
		if onLost != nil {
			onLost()
		}
	}

	try()
	for {
		var retry <-chan time.Time
		if lost == nil {
			retry = l.clock.After(l.ttl / 3)
		}
		select {
		case <-ctx.Done():
			if lost != nil {
				l.Unlock(ctx, tok)
				stepDown()
			}
			return
		case <-lost:
			slog.WarnContext(ctx, "kvsync: election lost", "key", l.key, "node", l.holder, "generation", tok.Generation)
			stepDown()
		case <-retry:
			try()
		}
	}
}
