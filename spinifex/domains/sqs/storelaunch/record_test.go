package storelaunch

import (
	"context"
	"errors"
	"strings"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nkeys"
)

func newRecords(t *testing.T) (Records, string) {
	t.Helper()
	clustersize.DeclareForTest(t, 1)
	_, _, js := testutil.StartTestJetStream(t)
	r, err := Open(context.Background(), js)
	if err != nil {
		t.Fatal(err)
	}
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := kp.PublicKey()
	return r, pub
}

// Begin is idempotent for the same claim, node, key and size, and refuses any
// other caller, key or size rather than overwriting the record.
func TestBegin_IsIdempotentAndRefusesChanges(t *testing.T) {
	ctx := context.Background()
	r, pub := newRecords(t)
	for range 2 {
		if err := r.Begin(ctx, region, "store-aaaa", node, pub, "m5.large", 8); err != nil {
			t.Fatal(err)
		}
	}
	kp, _ := nkeys.CreateUser()
	pub2, _ := kp.PublicKey()
	cases := []struct {
		name          string
		claim, nodeID string
		key           string
		size          int64
		want          error
	}{
		{"another claim", "store-bbbb", node, pub, 8, ErrSuperseded},
		{"another node", "store-aaaa", "node-b", pub, 8, ErrSuperseded},
		{"another key", "store-aaaa", node, pub2, 8, ErrKeyChanged},
		{"another size", "store-aaaa", node, pub, 9, ErrSizeChanged},
		{"a seed", "store-aaaa", node, "SUAEXAMPLE", 8, ErrInvalidRecording},
	}
	for _, tc := range cases {
		if err := r.Begin(ctx, region, tc.claim, tc.nodeID, tc.key, "m5.large", tc.size); !errors.Is(err, tc.want) {
			t.Fatalf("%s: begin = %v, want %v", tc.name, err, tc.want)
		}
	}
	if err := r.Begin(ctx, region, "store-aaaa", node, pub, "m5.xlarge", 8); !errors.Is(err, ErrTypeChanged) {
		t.Fatalf("another type: begin = %v", err)
	}
	rec, err := r.Current(ctx, region)
	if err != nil || rec.ClaimID != "store-aaaa" || rec.PublicKey != pub || rec.SizeGiB != 8 || rec.InstanceType != "m5.large" {
		t.Fatalf("record = %+v, %v", rec, err)
	}
}

// The volume mark may be withdrawn only while it is set and no volume is
// recorded; once a volume is recorded it stays one-shot.
func TestClearVolumeAttempt_OnlyBeforeAVolumeIsRecorded(t *testing.T) {
	ctx := context.Background()
	r, pub := newRecords(t)
	if err := r.Begin(ctx, region, "store-aaaa", node, pub, "m5.large", 8); err != nil {
		t.Fatal(err)
	}
	if err := r.ClearVolumeAttempt(ctx, region, "store-aaaa"); !errors.Is(err, ErrConflict) {
		t.Fatalf("clear without a mark = %v", err)
	}
	if err := r.MarkVolumeAttempted(ctx, region, "store-aaaa"); err != nil {
		t.Fatal(err)
	}
	if err := r.ClearVolumeAttempt(ctx, region, "store-aaaa"); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkVolumeAttempted(ctx, region, "store-aaaa"); err != nil {
		t.Fatalf("mark after clear = %v", err)
	}
	if err := r.RecordVolume(ctx, region, "store-aaaa", "vol-00000000000000001"); err != nil {
		t.Fatal(err)
	}
	if err := r.ClearVolumeAttempt(ctx, region, "store-aaaa"); !errors.Is(err, ErrConflict) {
		t.Fatalf("clear after a recorded volume = %v", err)
	}
	if rec, err := r.Current(ctx, region); err != nil || !rec.VolumeAttempted || rec.DataVolumeID == "" {
		t.Fatalf("record = %+v, %v", rec, err)
	}
}

// Each one-shot mark is accepted once; dispatch requires a recorded volume,
// and bind requires dispatch and is idempotent for the same instance.
func TestMarks_AreOneShotAndOrdered(t *testing.T) {
	ctx := context.Background()
	r, pub := newRecords(t)
	if err := r.Begin(ctx, region, "store-aaaa", node, pub, "m5.large", 8); err != nil {
		t.Fatal(err)
	}
	if err := r.RecordVolume(ctx, region, "store-aaaa", "vol-00000000000000001"); !errors.Is(err, ErrConflict) {
		t.Fatalf("volume before mark = %v", err)
	}
	if err := r.MarkDispatched(ctx, region, "store-aaaa"); !errors.Is(err, ErrConflict) {
		t.Fatalf("dispatch before volume = %v", err)
	}
	for _, mark := range []func(context.Context, string, string) error{r.MarkVolumeAttempted, r.MarkNetworkAttempted} {
		if err := mark(ctx, region, "store-aaaa"); err != nil {
			t.Fatal(err)
		}
		if err := mark(ctx, region, "store-aaaa"); !errors.Is(err, ErrMarked) {
			t.Fatalf("second mark = %v", err)
		}
	}
	for range 2 {
		if err := r.RecordVolume(ctx, region, "store-aaaa", "vol-00000000000000001"); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.RecordVolume(ctx, region, "store-aaaa", "vol-00000000000000002"); !errors.Is(err, ErrConflict) {
		t.Fatalf("another volume = %v", err)
	}
	if err := r.Bind(ctx, region, "store-aaaa", "i-00000000000000001", "10.0.0.5"); !errors.Is(err, ErrConflict) {
		t.Fatalf("bind before dispatch = %v", err)
	}
	if err := r.MarkDispatched(ctx, region, "store-aaaa"); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkDispatched(ctx, region, "store-aaaa"); !errors.Is(err, ErrMarked) {
		t.Fatalf("second dispatch = %v", err)
	}
	for range 2 {
		if err := r.Bind(ctx, region, "store-aaaa", "i-00000000000000001", "10.0.0.5"); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Bind(ctx, region, "store-aaaa", "i-00000000000000002", "10.0.0.5"); !errors.Is(err, ErrConflict) {
		t.Fatalf("bind another instance = %v", err)
	}
	if err := r.MarkNetworkAttempted(ctx, region, "store-bbbb"); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("mark by another claim = %v", err)
	}
}

// A malformed or private-key-bearing record is unreadable and authorizes
// nothing.
func TestRead_RefusesMalformedRecords(t *testing.T) {
	ctx := context.Background()
	r, _ := newRecords(t)
	kp, _ := nkeys.CreateUser()
	seed, _ := kp.Seed()
	for _, raw := range []string{"{", `{"region":"ap-southeast-2","state":"launching","claimID":"store-aaaa","nodeID":"node-a","publicKey":"` + string(seed) + `","sizeGiB":8}`} {
		if _, err := r.KV.Put(ctx, key(region), []byte(raw)); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Current(ctx, region); !errors.Is(err, ErrUnreadable) {
			t.Fatalf("read %q = %v", raw[:1], err)
		}
	}
}

// Only the bound instance records a pin, only for its own claim and volume,
// and only once: the same pin again succeeds and a different one is refused
// without changing the record.
func TestRecordServerKey_IsCreateOnceForTheBoundStore(t *testing.T) {
	ctx := context.Background()
	r, pub := newRecords(t)
	const claim, vol, inst = "store-aaaa", "vol-00000000000000001", "i-00000000000000001"
	pin, other := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	if err := r.RecordServerKey(ctx, region, inst, claim, vol, pin); !errors.Is(err, ErrNotBound) {
		t.Fatalf("before any record = %v", err)
	}
	steps := []func() error{
		func() error { return r.Begin(ctx, region, claim, node, pub, "m5.large", 8) },
		func() error { return r.MarkVolumeAttempted(ctx, region, claim) },
		func() error { return r.RecordVolume(ctx, region, claim, vol) },
		func() error { return r.MarkNetworkAttempted(ctx, region, claim) },
		func() error { return r.MarkDispatched(ctx, region, claim) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.RecordServerKey(ctx, region, inst, claim, vol, pin); !errors.Is(err, ErrNotBound) {
		t.Fatalf("before bind = %v", err)
	}
	if err := r.Bind(ctx, region, claim, inst, "10.0.0.5"); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"another instance": func() error { return r.RecordServerKey(ctx, region, "i-00000000000000002", claim, vol, pin) },
		"another claim":    func() error { return r.RecordServerKey(ctx, region, inst, "store-bbbb", vol, pin) },
		"another volume":   func() error { return r.RecordServerKey(ctx, region, inst, claim, "vol-00000000000000002", pin) },
	} {
		if err := call(); !errors.Is(err, ErrNotStore) {
			t.Fatalf("%s = %v", name, err)
		}
	}
	if _, err := r.ServerKeyOf(ctx, region, "i-00000000000000002"); !errors.Is(err, ErrNotStore) {
		t.Fatalf("describe by another instance = %v", err)
	}
	for range 2 {
		if err := r.RecordServerKey(ctx, region, inst, claim, vol, pin); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.RecordServerKey(ctx, region, inst, claim, vol, other); !errors.Is(err, ErrPinChanged) {
		t.Fatalf("a different pin = %v", err)
	}
	rec, err := r.ServerKeyOf(ctx, region, inst)
	if err != nil || rec.ServerKeyPin != pin || rec.ClaimID != claim || rec.DataVolumeID != vol {
		t.Fatalf("record = %+v, %v", rec, err)
	}
}
