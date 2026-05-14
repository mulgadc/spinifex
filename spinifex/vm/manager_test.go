package vm

import (
	"sync"
	"sync/atomic"
	"testing"
)

func mkVM(id string) *VM { return &VM{ID: id} }

func TestManager_GetInsertDelete(t *testing.T) {
	m := NewManager()

	if v, ok := m.Get("missing"); ok || v != nil {
		t.Fatalf("Get on empty manager: got (%v, %v), want (nil, false)", v, ok)
	}

	a := mkVM("i-a")
	m.Insert(a)

	got, ok := m.Get("i-a")
	if !ok || got != a {
		t.Fatalf("Get after Insert: got (%v, %v), want (%v, true)", got, ok, a)
	}
	if m.Count() != 1 {
		t.Fatalf("Count after Insert: got %d, want 1", m.Count())
	}

	m.Delete("i-a")
	if _, ok := m.Get("i-a"); ok {
		t.Fatalf("Get after Delete: got ok=true, want false")
	}
	m.Delete("i-a") // delete is idempotent
}

func TestManager_InsertIfAbsent(t *testing.T) {
	m := NewManager()
	a := mkVM("i-a")
	a2 := mkVM("i-a")

	if !m.InsertIfAbsent(a) {
		t.Fatal("InsertIfAbsent on empty: want true")
	}
	if m.InsertIfAbsent(a2) {
		t.Fatal("InsertIfAbsent on occupied: want false")
	}
	got, _ := m.Get("i-a")
	if got != a {
		t.Fatalf("Get after collision: got %v, want %v (original)", got, a)
	}
}

func TestManager_DeleteIf(t *testing.T) {
	m := NewManager()
	a := mkVM("i-a")
	b := mkVM("i-a") // different pointer, same ID

	m.Insert(a)
	if m.DeleteIf("i-a", b) {
		t.Fatal("DeleteIf with non-matching pointer: want false")
	}
	if got, _ := m.Get("i-a"); got != a {
		t.Fatalf("DeleteIf preserved entry: got %v, want %v", got, a)
	}
	if !m.DeleteIf("i-a", a) {
		t.Fatal("DeleteIf with matching pointer: want true")
	}
	if _, ok := m.Get("i-a"); ok {
		t.Fatal("entry still present after successful DeleteIf")
	}
	if m.DeleteIf("missing", a) {
		t.Fatal("DeleteIf on missing id: want false")
	}
}

func TestManager_UpdateState(t *testing.T) {
	m := NewManager()
	a := mkVM("i-a")
	a.Status = StatePending
	m.Insert(a)

	called := false
	if !m.UpdateState("i-a", func(v *VM) {
		called = true
		v.Status = StateRunning
	}) {
		t.Fatal("UpdateState on existing id: want true")
	}
	if !called {
		t.Fatal("UpdateState fn not invoked")
	}
	if a.Status != StateRunning {
		t.Fatalf("UpdateState mutation: got %s, want %s", a.Status, StateRunning)
	}

	if m.UpdateState("missing", func(*VM) { t.Fatal("fn called on missing id") }) {
		t.Fatal("UpdateState on missing id: want false")
	}
}

func TestManager_UpdateAndPersist(t *testing.T) {
	t.Run("persists on hit", func(t *testing.T) {
		store := newFakeStateStore()
		m := NewManagerWithDeps(Deps{NodeID: "node-1", StateStore: store})
		a := mkVM("i-a")
		a.Status = StatePending
		m.Insert(a)

		called := false
		ok, err := m.UpdateAndPersist("i-a", func(v *VM) {
			called = true
			v.Status = StateRunning
		})
		if !ok {
			t.Fatal("UpdateAndPersist on existing id: want ok=true")
		}
		if err != nil {
			t.Fatalf("UpdateAndPersist err: %v", err)
		}
		if !called {
			t.Fatal("mutator not invoked")
		}
		if a.Status != StateRunning {
			t.Fatalf("mutation not applied: got %s, want %s", a.Status, StateRunning)
		}
		saved, ok := store.saved["node-1"]
		if !ok {
			t.Fatal("StateStore.SaveRunningState not called for node-1")
		}
		if _, ok := saved["i-a"]; !ok {
			t.Fatal("persisted snapshot missing i-a")
		}
	})

	t.Run("missing id is no-op", func(t *testing.T) {
		store := newFakeStateStore()
		m := NewManagerWithDeps(Deps{NodeID: "node-1", StateStore: store})

		ok, err := m.UpdateAndPersist("missing", func(*VM) {
			t.Fatal("mutator must not run on missing id")
		})
		if ok {
			t.Fatal("UpdateAndPersist on missing id: want ok=false")
		}
		if err != nil {
			t.Fatalf("UpdateAndPersist err: %v", err)
		}
		if len(store.saved) != 0 {
			t.Fatal("SaveRunningState must not be called on miss")
		}
	})

	t.Run("nil state store still mutates", func(t *testing.T) {
		m := NewManager()
		a := mkVM("i-a")
		m.Insert(a)

		ok, err := m.UpdateAndPersist("i-a", func(v *VM) { v.Status = StateRunning })
		if !ok || err != nil {
			t.Fatalf("UpdateAndPersist: ok=%v err=%v, want (true, nil)", ok, err)
		}
		if a.Status != StateRunning {
			t.Fatalf("mutation not applied: got %s", a.Status)
		}
	})
}

func TestManager_Inspect(t *testing.T) {
	m := NewManager()
	a := mkVM("i-a")
	a.Status = StatePending
	m.Insert(a)

	var seen InstanceState
	m.Inspect(a, func(v *VM) { seen = v.Status })
	if seen != StatePending {
		t.Fatalf("Inspect: got %s, want %s", seen, StatePending)
	}

	// Inspect works on VMs not in the map (e.g., during build before insert).
	loose := mkVM("i-loose")
	loose.Status = StateRunning
	m.Inspect(loose, func(v *VM) { seen = v.Status })
	if seen != StateRunning {
		t.Fatalf("Inspect on loose VM: got %s, want %s", seen, StateRunning)
	}
}

func TestManager_Status(t *testing.T) {
	m := NewManager()
	a := mkVM("i-a")
	a.Status = StatePending
	m.Insert(a)

	if got := m.Status(a); got != StatePending {
		t.Fatalf("Status: got %s, want %s", got, StatePending)
	}

	// Status reads under lock without verifying membership — orphaned VMs
	// (mid-cleanup, never-inserted) still return a consistent value.
	loose := mkVM("i-loose")
	loose.Status = StateRunning
	if got := m.Status(loose); got != StateRunning {
		t.Fatalf("Status on loose VM: got %s, want %s", got, StateRunning)
	}
}

func TestManager_ForEachAndSnapshot(t *testing.T) {
	m := NewManager()
	want := map[string]bool{"i-1": true, "i-2": true, "i-3": true}
	for id := range want {
		m.Insert(mkVM(id))
	}

	seen := map[string]bool{}
	m.ForEach(func(v *VM) { seen[v.ID] = true })
	if len(seen) != len(want) {
		t.Fatalf("ForEach saw %d, want %d", len(seen), len(want))
	}
	for id := range want {
		if !seen[id] {
			t.Fatalf("ForEach missed %s", id)
		}
	}

	snap := m.Snapshot()
	if len(snap) != len(want) {
		t.Fatalf("Snapshot len=%d, want %d", len(snap), len(want))
	}
	snapMap := m.SnapshotMap()
	if len(snapMap) != len(want) {
		t.Fatalf("SnapshotMap len=%d, want %d", len(snapMap), len(want))
	}

	// SnapshotMap is decoupled from the live map: mutating the returned map
	// must not affect the manager.
	delete(snapMap, "i-1")
	if _, ok := m.Get("i-1"); !ok {
		t.Fatal("mutating SnapshotMap result leaked into manager")
	}
}

func TestManager_Filter(t *testing.T) {
	m := NewManager()
	a, b, c := mkVM("i-a"), mkVM("i-b"), mkVM("i-c")
	a.Status = StateRunning
	b.Status = StateStopped
	c.Status = StateRunning
	m.Insert(a)
	m.Insert(b)
	m.Insert(c)

	running := m.Filter(func(v *VM) bool { return v.Status == StateRunning })
	if len(running) != 2 {
		t.Fatalf("Filter running: got %d, want 2", len(running))
	}
}

func TestManager_Replace(t *testing.T) {
	m := NewManager()
	m.Insert(mkVM("i-old"))

	src := map[string]*VM{"i-new1": mkVM("i-new1"), "i-new2": mkVM("i-new2")}
	m.Replace(src)

	if _, ok := m.Get("i-old"); ok {
		t.Fatal("old entry survived Replace")
	}
	if m.Count() != 2 {
		t.Fatalf("Count after Replace: got %d, want 2", m.Count())
	}

	// Mutating the source after Replace must not affect the manager.
	delete(src, "i-new1")
	if _, ok := m.Get("i-new1"); !ok {
		t.Fatal("Replace did not copy the source map")
	}
}

// TestManager_ConcurrentSoak runs many goroutines doing Insert/Delete/ForEach/
// UpdateState concurrently. Run with `go test -race ./vm/...` to validate.
func TestManager_ConcurrentSoak(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping soak in short mode")
	}

	m := NewManager()
	const writers = 8
	const iters = 2000

	var writersWG sync.WaitGroup
	var readersWG sync.WaitGroup
	stop := make(chan struct{})
	var foreachCount atomic.Int64

	readersWG.Add(2)
	go func() {
		defer readersWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.ForEach(func(v *VM) { _ = v.ID })
			foreachCount.Add(1)
		}
	}()
	go func() {
		defer readersWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = m.Snapshot()
		}
	}()

	for w := range writers {
		writersWG.Add(1)
		go func(seed int) {
			defer writersWG.Done()
			for i := range iters {
				id := mkID(seed, i)
				m.Insert(mkVM(id))
				m.UpdateState(id, func(v *VM) { v.Status = StateRunning })
				m.Delete(id)
			}
		}(w)
	}

	writersWG.Wait()
	close(stop)
	readersWG.Wait()

	if foreachCount.Load() == 0 {
		t.Fatal("readers never observed any state — possible reader starvation")
	}
}

// TestManager_SlotOccupancyRace simulates the production race the plan calls
// out at daemon_handlers_instance.go:594-626 and daemon.go:2140/2149: a
// terminate handler running DeleteIf(id, original) concurrently with a start
// handler running InsertIfAbsent(replacement) for the same slot, plus the
// rollback InsertIfAbsent(original) on a write failure. The invariant is that
// the slot never contains a torn or unexpected pointer — at any moment Get(id)
// returns either the original, the replacement, or nothing.
func TestManager_SlotOccupancyRace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping race stress in short mode")
	}

	m := NewManager()
	const iters = 5000
	const id = "i-slot"

	original := &VM{ID: id}
	replacement := &VM{ID: id}

	// Pre-seed: original is in the slot.
	m.Insert(original)

	var wg sync.WaitGroup
	wg.Add(3)

	// Terminator: DeleteIf(original); on the next round, re-Insert original to
	// reset the slot for the next iteration.
	go func() {
		defer wg.Done()
		for range iters {
			m.DeleteIf(id, original)
			m.InsertIfAbsent(original)
		}
	}()

	// Starter: tries to occupy the slot with replacement after a delete; rolls
	// back via DeleteIf to keep the test loop in steady state.
	go func() {
		defer wg.Done()
		for range iters {
			if m.InsertIfAbsent(replacement) {
				m.DeleteIf(id, replacement)
			}
		}
	}()

	// Observer: at every observation, Get must return original, replacement,
	// or nothing. Anything else means the slot is corrupted.
	go func() {
		defer wg.Done()
		for range iters {
			v, ok := m.Get(id)
			if !ok {
				continue
			}
			if v != original && v != replacement {
				t.Errorf("slot held an unexpected pointer: %p", v)
				return
			}
		}
	}()

	wg.Wait()
}

// TestManager_ForEachIsRaceSafeForMutableFields documents the safe contract:
// ForEach holds the lock for the entire iteration, so concurrent mutators
// using Inspect/UpdateState are serialized against the iteration. This is the
// race-clean alternative when callers need to read fields like Status that
// can change concurrently. (Snapshot, by contrast, returns shared *VM
// pointers — reading mutable fields after release is unsynchronized.)
func TestManager_ForEachIsRaceSafeForMutableFields(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping race stress in short mode")
	}

	m := NewManager()
	v := &VM{ID: "i-1", Status: StatePending}
	m.Insert(v)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		toggle := true
		for {
			select {
			case <-stop:
				return
			default:
			}
			if toggle {
				m.UpdateState("i-1", func(v *VM) { v.Status = StateRunning })
			} else {
				m.UpdateState("i-1", func(v *VM) { v.Status = StatePending })
			}
			toggle = !toggle
		}
	})

	for range 5000 {
		m.ForEach(func(v *VM) {
			_ = v.Status
		})
	}
	close(stop)
	wg.Wait()
}

func mkID(w, i int) string {
	const hexDigits = "0123456789abcdef"
	buf := []byte("i-")
	x := uint64(w)<<32 | uint64(i)
	for shift := 60; shift >= 0; shift -= 4 {
		buf = append(buf, hexDigits[(x>>uint(shift))&0xF])
	}
	return string(buf)
}
