//go:build e2e

package uefistore

import (
	"fmt"
	"slices"
	"testing"
	"time"

	store "github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// TestUEFIGuestVariableSurvivesStopStart writes a variable from the guest,
// requires the store's pointer to move to an object holding it within the
// write-back bound, then requires the guest to read it back after a stop/start.
func TestUEFIGuestVariableSurvivesStopStart(t *testing.T) {
	fix := requireUEFIFixture(t)
	harness.Phase(t, "UEFI store: a guest write reaches the store and survives stop/start")
	g := ensureUEFIGuest(t, fix)

	rec, err := fix.Store.Record(t.Context(), g.id)
	if err != nil || rec.Pointer == nil {
		t.Fatalf("read the store record of %s: %v (record %+v)", g.id, err, rec)
	}
	before := rec.Pointer.Key

	v := newVar(t, "SpxE2EStopStart")
	writeVar(t, g, v)
	landed := waitForStoreToHold(t, fix, g.id, writeBackBudget,
		fmt.Sprintf("a variable the guest writes while running reaches an object within %d s", int(writeBackClaim.Seconds())), v)
	if landed == before {
		t.Fatalf("the object holding %s is %s, the key the store pointed at before the write; a write-back goes to a fresh key",
			v.name, landed)
	}

	g = stopStart(t, fix, g, v)
	assertVar(t, g, v)
	harness.Step(t, "%s=%s survived a stop and start", v.name, v.value)
}

// TestUEFIWriterResumesAfterDaemonRestart restarts the daemon under the running
// guest, writes a second variable, and requires both the variable written before
// the restart and the one written after it to land and survive a stop/start.
func TestUEFIWriterResumesAfterDaemonRestart(t *testing.T) {
	fix := requireUEFIFixture(t)
	requireOnNode(t)
	harness.Phase(t, "UEFI store: the writer resumes after a daemon restart")
	g := ensureUEFIGuest(t, fix)

	first := newVar(t, "SpxE2EBeforeRestart")
	writeVar(t, g, first)
	waitForStoreToHold(t, fix, g.id, writeBackBudget, "the write before the daemon restart lands", first)

	bootBefore := bootID(t, g)
	since := restartDaemon(t)
	adopted := writerAdopted(t, g.id, since, 3*time.Minute)
	harness.WaitForInstanceState(t, fix.AWS, g.id, "running", harness.WithTimeout(3*time.Minute))
	if after := bootID(t, g); after != bootBefore {
		t.Fatalf("%s rebooted across a daemon restart (boot ID %s, now %s); guests must outlive the daemon, so this test's premise fails",
			g.id, bootBefore, after)
	}

	budget := writeBackBudget
	if !adopted {
		harness.Step(t, "the restarted daemon did not log adopting %s's writer; allowing for the write-back reaper to start one", g.id)
		budget = reaperWriteBackBudget
	}
	second := newVar(t, "SpxE2EAfterRestart")
	writeVar(t, g, second)
	waitForStoreToHold(t, fix, g.id, budget, "a write after the daemon restart lands", first, second)

	g = stopStart(t, fix, g, first, second)
	assertVar(t, g, first)
	assertVar(t, g, second)
	harness.Step(t, "%s and %s survived a daemon restart and a stop/start", first.name, second.name)
}

// pruneVersions is how many write-backs the prune test lands before it watches,
// so the prefix holds superseded objects for the prune to delete.
const pruneVersions = 3

// pruneSettle is how long past PruneMinAge the prefix has to return to one
// object: one 2-minute garbage-collection sweep plus margin for a loaded node.
const pruneSettle = 5 * time.Minute

// pruneWatchCap bounds the whole watch, however often the pointer moves.
const pruneWatchCap = 25 * time.Minute

// TestS7_UEFIStorePruneNeverDeletesThePointedObject lands several versions of a
// variable, then watches the prefix until the prune returns it to one object,
// failing if the pointed object or any object under 10 minutes old disappears.
func TestS7_UEFIStorePruneNeverDeletesThePointedObject(t *testing.T) {
	fix := requireUEFIFixture(t)
	harness.Phase(t, "UEFI store: the prune keeps the pointer and every young object (ADR-0003:S7)")
	g := ensureUEFIGuest(t, fix)

	type version struct {
		key    string
		issued time.Time
	}
	var versions []version
	var v efiVar
	for i := range pruneVersions {
		v = newVar(t, "SpxE2EPrune")
		issued := writeVar(t, g, v)
		key := waitForStoreToHold(t, fix, g.id, writeBackBudget, fmt.Sprintf("version %d of %s lands", i+1, v.name), v)
		versions = append(versions, version{key: key, issued: issued})
	}

	keys, err := fix.Store.Objects(t.Context(), g.id)
	if err != nil {
		t.Fatalf("list the store objects of %s: %v", g.id, err)
	}
	for _, ver := range versions {
		if !slices.Contains(keys, ver.key) {
			t.Fatalf("ADR-0003:S7: %s, written %d s ago, is gone from %s's prefix; a prune keeps every object younger than 10 minutes (prefix: %v)",
				ver.key, int(time.Since(ver.issued).Seconds()), g.id, keys)
		}
	}
	harness.Detail(t, "instance", g.id, "objects", len(keys), "versions", len(versions))

	start := time.Now()
	pointer, lastMove := versions[len(versions)-1].key, time.Now()
	maxObjects := len(keys)
	var lastErr error
	lastOK := time.Now()
	for {
		if time.Since(start) > pruneWatchCap {
			t.Fatalf("the prune watch on %s ran past %d minutes without the prefix settling (objects: %v)",
				g.id, int(pruneWatchCap.Minutes()), keys)
		}
		time.Sleep(10 * time.Second)

		rec, err := fix.Store.Record(t.Context(), g.id)
		if err == nil {
			keys, err = fix.Store.Objects(t.Context(), g.id)
		}
		if err != nil {
			lastErr = err
			if time.Since(lastOK) > 2*time.Minute {
				t.Fatalf("cannot read %s's store for 2 minutes during the prune watch: %v", g.id, lastErr)
			}
			continue
		}
		lastOK = time.Now()
		if rec.State != store.StateLive || rec.Pointer == nil {
			t.Fatalf("%s's store record went to state %q with pointer %v during the prune watch", g.id, rec.State, rec.Pointer)
		}
		if rec.Pointer.Key != pointer {
			pointer, lastMove = rec.Pointer.Key, time.Now()
			harness.Step(t, "the pointer moved to %s during the watch; the settle deadline restarts", pointer)
		}
		if !slices.Contains(keys, pointer) {
			// The pointer may have moved between the two reads, and its old object
			// been pruned, so only a pointer that still names the key is a deletion.
			if again, rerr := fix.Store.Record(t.Context(), g.id); rerr == nil && again.Pointer != nil && again.Pointer.Key == pointer {
				t.Fatalf("ADR-0003:S7: the object %s's store record points at, %s, is gone from its prefix (prefix: %v)",
					g.id, pointer, keys)
			}
			continue
		}
		for _, ver := range versions {
			if time.Since(ver.issued) < store.PruneMinAge && !slices.Contains(keys, ver.key) {
				t.Fatalf("ADR-0003:S7: %s was deleted %d s after its write; a prune keeps every object younger than 10 minutes (prefix: %v)",
					ver.key, int(time.Since(ver.issued).Seconds()), keys)
			}
		}
		maxObjects = max(maxObjects, len(keys))
		if len(keys) == 1 {
			break
		}
		if time.Since(lastMove) > store.PruneMinAge+pruneSettle {
			t.Fatalf("%s's prefix still holds %d objects %d minutes after its pointer last moved; a prefix returns to one object after its next prune (objects: %v)",
				g.id, len(keys), int(time.Since(lastMove).Minutes()), keys)
		}
	}

	fix.launchedMu.Lock()
	age := time.Since(fix.launched)
	fix.launchedMu.Unlock()
	if age < store.PruneMinAge {
		t.Fatalf("the prune completed on %s only %d s after launch, younger than the 10-minute prune age; this is not evidence on an instance older than every timeout",
			g.id, int(age.Seconds()))
	}
	harness.Detail(t, "instance", g.id, "kept", pointer, "max_objects", maxObjects,
		"instance_age_s", int(age.Seconds()), "watch_s", int(time.Since(start).Seconds()))

	g = stopStart(t, fix, g, v)
	assertVar(t, g, v)
	harness.Step(t, "%s=%s survived the prune and a stop/start", v.name, v.value)
}
