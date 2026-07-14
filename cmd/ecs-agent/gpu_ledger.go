package main

import (
	"fmt"
	"strings"
	"sync"
)

// gpuLedger is the agent's local source of truth for which of the host's
// discovered GPU UUIDs are free vs pinned to a running task's container. C4
// reads a task's pinned UUIDs to build the CDI device list for containerd.
type gpuLedger struct {
	mu     sync.Mutex
	free   []string
	pinned map[string][]string // key ("taskID/containerName") -> uuids
}

// newGPULedger seeds the ledger with the host's discovered device UUIDs. A nil
// or empty slice is a valid non-GPU host: every Pin then fails, which callers
// treat as best-effort (no device, no CDI injection later).
func newGPULedger(uuids []string) *gpuLedger {
	free := append([]string(nil), uuids...)
	return &gpuLedger{free: free, pinned: make(map[string][]string)}
}

// gpuKey builds the ledger key for one container's pin/release.
func gpuKey(taskID, containerName string) string {
	return taskID + "/" + containerName
}

// Pin reserves n free UUIDs under key, returning them. An error means fewer
// than n devices were free; the key is left unpinned so a retry (or a
// best-effort caller) can proceed without a partial reservation.
func (l *gpuLedger) Pin(key string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.free) < n {
		return nil, fmt.Errorf("gpu ledger: requested %d GPUs, %d free", n, len(l.free))
	}
	uuids := append([]string(nil), l.free[:n]...)
	l.free = l.free[n:]
	l.pinned[key] = uuids
	return uuids, nil
}

// ReleaseTask frees every UUID pinned under any container key belonging to
// taskID, called once the task's terminal STOPPED state is reported.
func (l *gpuLedger) ReleaseTask(taskID string) {
	prefix := taskID + "/"
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, uuids := range l.pinned {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		l.free = append(l.free, uuids...)
		delete(l.pinned, key)
	}
}
