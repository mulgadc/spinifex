package iam

import "sync"

// recordLocks queues this process's writers of one IAM record behind each
// other. CAS alone keeps a write from being lost, but a caller looping a cheap
// self-scoped write can win every revision race and starve an administrator's
// DetachUserPolicy until its CAS budget is spent. The zero value is ready to use.
type recordLocks struct {
	mu    sync.Mutex
	locks map[string]*recordLock
}

type recordLock struct {
	mu   sync.Mutex
	refs int
}

// lock blocks until the caller holds key and returns its release. Entries are
// dropped once no writer holds or waits on them, so the map tracks only
// records being written right now.
func (l *recordLocks) lock(key string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]*recordLock)
	}
	rl := l.locks[key]
	if rl == nil {
		rl = &recordLock{}
		l.locks[key] = rl
	}
	rl.refs++
	l.mu.Unlock()

	rl.mu.Lock()
	return func() {
		rl.mu.Unlock()
		l.mu.Lock()
		rl.refs--
		if rl.refs == 0 {
			delete(l.locks, key)
		}
		l.mu.Unlock()
	}
}
