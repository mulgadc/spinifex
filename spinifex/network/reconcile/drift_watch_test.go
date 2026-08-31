//test:in-package — intentSource, intentBuckets and driftBackoffBase are
//unexported, and the point of these tests is what the loop watches rather than
//what it does once woken.

package reconcile

import (
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	handlers_ec2_igw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/igw"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go/jetstream"
)

// createIntentBucket makes one intent bucket the way its owning handler would,
// so a test can write to it and have the source find it.
func createIntentBucket(t *testing.T, js jetstream.JetStream, name string) jetstream.KeyValue {
	t.Helper()
	kv, err := kvutil.GetOrCreateBucket(t.Context(), js, name, 1)
	if err != nil {
		t.Fatalf("create bucket %s: %v", name, err)
	}
	return kv
}

// bucketNames is what intentSource resolved this cycle.
func bucketNames(t *testing.T, js jetstream.JetStream) []string {
	t.Helper()
	buckets, err := intentSource(js).Buckets(t.Context())
	if err != nil {
		t.Fatalf("enumerate intent buckets: %v", err)
	}
	names := make([]string, 0, len(buckets))
	for _, b := range buckets {
		names = append(names, b.Name())
	}
	return names
}

// waitForWatch writes probe keys until one of them wakes a pass, and returns the
// call count once it has. Watchers attach asynchronously and are UpdatesOnly, so
// a single write racing startup is lost for good; retrying is what makes a test
// that needs a live watcher deterministic rather than dependent on how fast the
// machine got there.
func waitForWatch(t *testing.T, rec *stubReconciler, kv jetstream.KeyValue, prefix string) int {
	t.Helper()
	before := rec.callCount()
	deadline := time.Now().Add(10 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		if _, err := kv.Put(t.Context(), prefix+strconv.Itoa(i), []byte(`{}`)); err != nil {
			t.Fatalf("probe write %d: %v", i, err)
		}
		if got := waitForCalls(t, rec, before+1, 250*time.Millisecond); got > before {
			return got
		}
	}
	t.Fatal("no intent write woke a pass within 10s, so the loop is still waiting " +
		"out DriftInterval rather than watching the bucket")
	return 0
}

// streamExists reports whether the KV bucket's backing stream is present, which
// is how a test tells "watched" from "created by being watched".
func streamExists(t *testing.T, js jetstream.JetStream, name string) bool {
	t.Helper()
	_, err := js.Stream(t.Context(), "KV_"+name)
	return err == nil
}

// The whole point of the conversion: a write to an intent bucket must reach the
// loop long before DriftInterval, because that write is the repair signal a
// dropped vpc.* event would otherwise leave for the next scan.
func TestDriftLoop_IntentWriteWakesAPass(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	kv := createIntentBucket(t, js, handlers_ec2_vpc.KVBucketVPCs)
	// The interval is long so a pass can only have come from the watch, and the
	// floor is short so the write is not merely deferred behind it.
	shrinkDriftTiming(t, time.Minute, time.Millisecond)

	rec := &stubReconciler{outcomes: []error{nil}}
	startDriftLoop(t, rec, nc, nil)

	// After the startup seed the loop is armed at DriftInterval alone, so the
	// watch has to be what brings the pass forward.
	time.Sleep(200 * time.Millisecond)
	if got := rec.callCount(); got != 0 {
		t.Fatalf("reconcile ran %d times before any write, want 0", got)
	}

	// Well inside DriftInterval, so the pass can only have come from the write.
	waitForWatch(t, rec, kv, "vpc-watch-")
}

// A burst is one pass, not one per key: the debounce coalesces it and the floor
// keeps a launch storm from turning a five-minute OVN scan into a running one.
func TestDriftLoop_BurstOfWritesIsOnePass(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	kv := createIntentBucket(t, js, handlers_ec2_vpc.KVBucketENIs)
	shrinkDriftTiming(t, time.Minute, 2*time.Second)

	rec := &stubReconciler{outcomes: []error{nil}}
	startDriftLoop(t, rec, nc, nil)

	// The burst has to land on a watcher that is already attached, or the writes
	// it is meant to coalesce are simply never seen.
	base := waitForWatch(t, rec, kv, "eni-probe-")

	for i := range 20 {
		if _, err := kv.Put(t.Context(), "eni-burst-"+strconv.Itoa(i), []byte(`{}`)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if got := waitForCalls(t, rec, base+1, 10*time.Second); got < base+1 {
		t.Fatal("a burst of intent writes never woke a pass")
	}
	// The burst is long spent, so a further pass here could only come from the
	// loop tracking writes rather than coalescing them.
	time.Sleep(500 * time.Millisecond)
	if got := rec.callCount() - base; got != 1 {
		t.Errorf("reconcile ran %d times for one burst of 20 writes, want 1", got)
	}
}

// Watching must not create. These buckets belong to the EC2 handlers, which set
// their history depth; a bucket vpcd created first would fix the wrong depth in
// place, and the handler's own get-or-create would then return it rather than
// correct it.
func TestIntentSource_DoesNotCreateMissingBuckets(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)

	if got := bucketNames(t, js); len(got) != 0 {
		t.Fatalf("intentSource resolved %v against an empty JetStream, want none", got)
	}
	for _, name := range intentBuckets {
		if streamExists(t, js, name) {
			t.Errorf("bucket %s exists after enumeration, want it left absent", name)
		}
	}
}

// A cluster that has never had an IGW has no IGW bucket, so the source has to
// pick one up when it appears rather than only at startup. That is why it is a
// Dynamic source: JetStream has no bucket-created event, so discovery rides the
// resync.
func TestIntentSource_PicksUpABucketThatAppearsLater(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	createIntentBucket(t, js, handlers_ec2_vpc.KVBucketVPCs)

	if got := bucketNames(t, js); len(got) != 1 || got[0] != handlers_ec2_vpc.KVBucketVPCs {
		t.Fatalf("intentSource resolved %v, want just %s", got, handlers_ec2_vpc.KVBucketVPCs)
	}

	createIntentBucket(t, js, handlers_ec2_igw.KVBucketIGW)
	got := bucketNames(t, js)
	if len(got) != 2 {
		t.Fatalf("intentSource resolved %v after a second bucket appeared, want both", got)
	}
}

// bucketConstants is every handler KV bucket constant referenced by a file.
func bucketConstants(t *testing.T, path string) map[string]struct{} {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]struct{}{}
	for _, m := range regexp.MustCompile(`handlers_ec2_\w+\.KVBucket\w+`).FindAllString(string(src), -1) {
		out[m] = struct{}{}
	}
	return out
}

// Every bucket LoadIntentFromKV reads must be watched, or a change to it waits
// out the resync while every other kind of change is repaired in seconds. The
// two lists are compared at the source rather than by name, so a load added to
// intent.go and not to intentBuckets fails here instead of going quiet.
func TestIntentSource_CoversEveryBucketTheIntentLoadReads(t *testing.T) {
	read := bucketConstants(t, "intent.go")
	if len(read) == 0 {
		t.Fatal("no bucket constants found in intent.go — the scan is not matching what it should")
	}
	watched := bucketConstants(t, "drift.go")
	for name := range read {
		if _, ok := watched[name]; !ok {
			t.Errorf("%s is read by LoadIntentFromKV but absent from intentBuckets, "+
				"so a change to it is only repaired on the resync", name)
		}
	}
}

// The floor is what stops a busy cluster scanning continuously, so it is pinned
// on the pass rather than inferred from the loop's timing.
func TestDriftPass_FloorsTheChangeDrivenRate(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	shrinkDriftTiming(t, time.Minute, time.Hour)

	rec := &stubReconciler{outcomes: []error{nil}}
	pass := driftPass(rec, nc, js, "us-east-1a", "node-1", nil)

	// The seed pass does no work: the bootstrap ReconcileApplyOnly was the
	// startup scan and this one must not repeat it.
	if revisit, err := pass(t.Context()); err != nil || revisit != 0 {
		t.Fatalf("seed pass = (%v, %v), want (0, nil) for a converged startup", revisit, err)
	}
	if got := rec.callCount(); got != 0 {
		t.Fatalf("seed pass ran reconcile %d times, want 0", got)
	}

	if _, err := pass(t.Context()); err != nil {
		t.Fatalf("first real pass: %v", err)
	}
	if got := rec.callCount(); got != 1 {
		t.Fatalf("reconcile ran %d times, want 1", got)
	}

	revisit, err := pass(t.Context())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := rec.callCount(); got != 1 {
		t.Errorf("reconcile ran %d times inside the floor, want it deferred to 1", got)
	}
	if revisit <= 0 || revisit > driftBackoffBase {
		t.Errorf("deferred pass asked to be revisited in %v, want a positive wait "+
			"no longer than the floor (%v)", revisit, driftBackoffBase)
	}
}
