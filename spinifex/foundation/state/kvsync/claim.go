package kvsync

import (
	"context"
	"errors"
	"fmt"
)

// Claim is ownership of a key with no expiry: it is held until its holder
// unlocks it or another holder takes it over. It keeps no state, so it gives no
// exclusion between goroutines of one holder; its callers serialise their use.
type Claim struct {
	store  KVStore
	key    string
	holder string
}

// NewClaim returns the claim on key for holder. It panics if key is invalid or
// already constructed against store, or if holder is empty.
func NewClaim(store KVStore, key, holder string) *Claim {
	if holder == "" {
		panic("kvsync: empty holder") //nolint:forbidigo // Constructing a lock wrongly is a wiring error that must fail at startup, not at the first election.
	}
	registerKey(store, key)
	return &Claim{store: store, key: key, holder: holder}
}

// TryLock takes the key when it is absent, and adopts it without a write when
// it already names this holder, keeping its generation. Otherwise it returns
// *HeldError naming the holder.
func (c *Claim) TryLock(ctx context.Context) (Token, error) {
	var err error
	for range casAttempts {
		var rev uint64
		rev, err = c.store.CompareAndSwap(ctx, c.key, encode(c.holder, 0), 0)
		if err == nil {
			return Token{Holder: c.holder, Generation: rev}, nil
		}
		if !errors.Is(err, ErrRevisionMismatch) {
			return Token{}, fmt.Errorf("kvsync: claim %s: %w", c.key, err)
		}
		cur, _, ok, rerr := read(ctx, c.store, c.key)
		if rerr != nil {
			return Token{}, rerr
		}
		if !ok {
			// Unlocked between the create and the read; try again.
			continue
		}
		if cur.Holder == c.holder {
			return cur, nil
		}
		return Token{}, &HeldError{Current: cur}
	}
	return Token{}, fmt.Errorf("kvsync: claim %s: %w", c.key, err)
}

// Unlock deletes the key while it still holds held, and returns ErrLost when
// it does not.
func (c *Claim) Unlock(ctx context.Context, held Token) error {
	for range casAttempts {
		cur, rev, ok, err := read(ctx, c.store, c.key)
		if err != nil {
			return err
		}
		if !ok || cur != held {
			return ErrLost
		}
		err = c.store.CompareAndDelete(ctx, c.key, rev)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrRevisionMismatch) {
			return fmt.Errorf("kvsync: unlock %s: %w", c.key, err)
		}
	}
	return ErrLost
}

// TakeOver moves the key to this holder only while it still holds from, the
// token the caller judged dead from its own evidence. It returns *HeldError
// when the key no longer holds from.
func (c *Claim) TakeOver(ctx context.Context, from Token) (Token, error) {
	cur, rev, ok, err := read(ctx, c.store, c.key)
	if err != nil {
		return Token{}, err
	}
	if !ok || cur != from {
		return Token{}, &HeldError{Current: cur}
	}
	next, err := c.store.CompareAndSwap(ctx, c.key, encode(c.holder, 0), rev)
	if errors.Is(err, ErrRevisionMismatch) {
		return Token{}, heldAfterConflict(ctx, c.store, c.key)
	}
	if err != nil {
		return Token{}, fmt.Errorf("kvsync: take over %s: %w", c.key, err)
	}
	return Token{Holder: c.holder, Generation: next}, nil
}

// Holder reports the token of whoever holds the key, and false when nobody does.
func (c *Claim) Holder(ctx context.Context) (Token, bool, error) {
	cur, _, ok, err := read(ctx, c.store, c.key)
	return cur, ok, err
}

// ClaimGroup hands out per-resource claims under one registered prefix.
type ClaimGroup struct {
	store  KVStore
	prefix string
	holder string
}

// NewClaimGroup registers prefix, which ends in ".", against store. It panics
// on an invalid or overlapping prefix, or an empty holder.
func NewClaimGroup(store KVStore, prefix, holder string) *ClaimGroup {
	if holder == "" {
		panic("kvsync: empty holder") //nolint:forbidigo // Constructing a lock wrongly is a wiring error that must fail at startup, not at the first election.
	}
	registerPrefix(store, prefix)
	return &ClaimGroup{store: store, prefix: prefix, holder: holder}
}

// Get returns the claim on prefix+name. A Claim keeps no state, so a fresh
// value for the same name is equivalent to an earlier one.
func (g *ClaimGroup) Get(name string) *Claim {
	if err := validName("name", name); err != nil {
		panic(err) //nolint:forbidigo // Constructing a lock wrongly is a wiring error that must fail at startup, not at the first election.
	}
	return &Claim{store: g.store, key: g.prefix + name, holder: g.holder}
}
