package main

import (
	"sync"
	"testing"
)

func TestGPULedger_PinReturnsDistinctUUIDs(t *testing.T) {
	l := newGPULedger([]string{"GPU-a", "GPU-b", "GPU-c"})
	got, err := l.Pin("t-1/web", 2)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 UUIDs, got %v", got)
	}
	if got[0] == got[1] {
		t.Errorf("pinned UUIDs must be distinct, got %v", got)
	}
	if len(l.free) != 1 {
		t.Errorf("free = %d, want 1 remaining", len(l.free))
	}
}

func TestGPULedger_PinInsufficientFreeErrors(t *testing.T) {
	l := newGPULedger([]string{"GPU-a"})
	_, err := l.Pin("t-1/web", 2)
	if err == nil {
		t.Fatal("want error when requesting more GPUs than free")
	}
	if len(l.free) != 1 {
		t.Errorf("failed pin must not consume any device, free = %d", len(l.free))
	}
}

func TestGPULedger_PinZeroIsNoop(t *testing.T) {
	l := newGPULedger([]string{"GPU-a"})
	got, err := l.Pin("t-1/web", 0)
	if err != nil || got != nil {
		t.Errorf("Pin(0) = %v, %v; want nil, nil", got, err)
	}
}

func TestGPULedger_ReleaseTaskFreesOnlyThatTasksDevices(t *testing.T) {
	l := newGPULedger([]string{"GPU-a", "GPU-b", "GPU-c", "GPU-d"})
	if _, err := l.Pin(gpuKey("t-1", "web"), 2); err != nil {
		t.Fatalf("pin t-1: %v", err)
	}
	if _, err := l.Pin(gpuKey("t-2", "worker"), 1); err != nil {
		t.Fatalf("pin t-2: %v", err)
	}
	if len(l.free) != 1 {
		t.Fatalf("free = %d, want 1 before release", len(l.free))
	}

	l.ReleaseTask("t-1")
	if len(l.free) != 3 {
		t.Errorf("free = %d, want 3 after releasing t-1's two devices", len(l.free))
	}
	if _, ok := l.pinned[gpuKey("t-2", "worker")]; !ok {
		t.Error("t-2's pin should survive releasing t-1")
	}
	if _, ok := l.pinned[gpuKey("t-1", "web")]; ok {
		t.Error("t-1's pin should be gone after release")
	}
}

// Multi-device pinning across concurrent task assigns must hand out disjoint
// UUID sets — no two tasks may ever share a device.
func TestGPULedger_ConcurrentPinsHandOutDistinctUUIDs(t *testing.T) {
	const nDevices = 20
	uuids := make([]string, nDevices)
	for i := range uuids {
		uuids[i] = "GPU-" + string(rune('a'+i))
	}
	l := newGPULedger(uuids)

	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := make(map[string]bool)
	errs := 0
	for i := range nDevices {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := gpuKey("t-concurrent", string(rune('A'+n)))
			got, err := l.Pin(key, 1)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs++
				return
			}
			for _, u := range got {
				if seen[u] {
					t.Errorf("UUID %s handed out to more than one concurrent pin", u)
				}
				seen[u] = true
			}
		}(i)
	}
	wg.Wait()
	if errs != 0 {
		t.Errorf("want all %d pins to succeed (exactly enough devices), got %d errors", nDevices, errs)
	}
	if len(seen) != nDevices {
		t.Errorf("distinct UUIDs handed out = %d, want %d", len(seen), nDevices)
	}
	if len(l.free) != 0 {
		t.Errorf("free = %d, want 0 after all devices pinned", len(l.free))
	}
}

// Releasing a task's devices concurrently with new pins never double-hands a
// UUID: a device is only ever free or pinned to exactly one key at a time.
func TestGPULedger_ConcurrentPinAndReleaseNoDoubleAssignment(t *testing.T) {
	l := newGPULedger([]string{"GPU-a", "GPU-b"})
	var wg sync.WaitGroup
	for round := range 50 {
		wg.Add(2)
		taskID := "t-" + string(rune('a'+round%26))
		go func(id string) {
			defer wg.Done()
			_, _ = l.Pin(gpuKey(id, "web"), 1)
		}(taskID)
		go func(id string) {
			defer wg.Done()
			l.ReleaseTask(id)
		}(taskID)
	}
	wg.Wait()
	// Invariant: every currently-pinned UUID appears in exactly one pinned entry
	// and never simultaneously in free.
	l.mu.Lock()
	defer l.mu.Unlock()
	freeSet := make(map[string]bool, len(l.free))
	for _, u := range l.free {
		if freeSet[u] {
			t.Errorf("duplicate UUID %s in free list", u)
		}
		freeSet[u] = true
	}
	pinnedSet := make(map[string]bool)
	for _, uuids := range l.pinned {
		for _, u := range uuids {
			if pinnedSet[u] {
				t.Errorf("UUID %s pinned under more than one key", u)
			}
			if freeSet[u] {
				t.Errorf("UUID %s is both free and pinned", u)
			}
			pinnedSet[u] = true
		}
	}
}
