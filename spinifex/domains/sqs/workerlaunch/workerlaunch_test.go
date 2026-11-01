package workerlaunch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storevolume"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	region = "ap-southeast-2"
	node   = "node-a"
	itype  = "sys.test"
	size   = int64(8)
)

// fakeVolumes creates one volume per Prepare. prepareErr refuses before any
// create; appliedErr creates the volume, then reports failure without its ID.
type fakeVolumes struct {
	mu         sync.Mutex
	prepared   []string
	encrypted  bool
	prepareErr error
	sizes      []int64
	appliedErr error
}

func (f *fakeVolumes) Prepare(_ context.Context, sizeGiB int64) (string, error) {
	f.mu.Lock()
	f.sizes = append(f.sizes, sizeGiB)
	f.mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prepareErr != nil {
		return "", f.prepareErr
	}
	id := fmt.Sprintf("vol-%017x", len(f.prepared)+1)
	f.prepared = append(f.prepared, id)
	if f.appliedErr != nil {
		return "", f.appliedErr
	}
	return id, nil
}

func (f *fakeVolumes) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prepared)
}

// keptVolumeError carries the ID of a volume a failed Prepare left in place.
type keptVolumeError struct{ id string }

func (e keptVolumeError) Error() string { return "fill failed for " + e.id }

func (f *fakeVolumes) Encrypted(context.Context, string) (bool, error) { return f.encrypted, nil }

type fakeRoles struct{ fail atomic.Int32 }

func (f *fakeRoles) EnsureWorkerProfile(context.Context) (string, error) {
	if f.fail.Add(-1) >= 0 {
		return "", errors.New("iam unavailable")
	}
	return "arn:aws:iam::000000000000:instance-profile/sqsWorkerRole", nil
}

// fakeNetwork holds one ENI per claim. A create with fail set is applied but
// reports failure; with hidden set it stays invisible to Find until shown.
type fakeNetwork struct {
	mu      sync.Mutex
	claims  []string
	created map[string]bool
	fail    error
	hidden  bool
	shown   map[string]bool
}

func (f *fakeNetwork) net() *Network {
	return &Network{SubnetID: "subnet-1", ENIID: "eni-1", ENIMac: "02:00:00:00:00:01",
		ENIIP: "10.240.0.10", CIDR: "10.240.0.10/24", Gateway: "10.240.0.1"}
}

func (f *fakeNetwork) FindWorkerNetwork(_ context.Context, _, claimID string) (*Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.created[claimID] && (!f.hidden || f.shown[claimID]) {
		return f.net(), nil
	}
	return nil, nil
}

func (f *fakeNetwork) CreateWorkerNetwork(_ context.Context, _, claimID string) (*Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = append(f.claims, claimID)
	if f.created == nil {
		f.created = map[string]bool{}
	}
	f.created[claimID] = true
	if f.fail != nil {
		return nil, f.fail
	}
	return f.net(), nil
}

func (f *fakeNetwork) show(claimID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.shown == nil {
		f.shown = map[string]bool{}
	}
	f.shown[claimID] = true
}

type fakeVMs struct {
	mu    sync.Mutex
	specs []Spec
	fail  error
}

func (f *fakeVMs) LaunchWorker(_ context.Context, spec Spec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, spec)
	if f.fail != nil {
		return "", f.fail
	}
	return fmt.Sprintf("i-%017x", len(f.specs)), nil
}

func (f *fakeVMs) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.specs)
}

type fakeHost struct {
	mu  sync.Mutex
	err error
}

func (f *fakeHost) Ready(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

type rig struct {
	l    *Launcher
	host *fakeHost
	vol  *fakeVolumes
	iam  *fakeRoles
	net  *fakeNetwork
	vms  *fakeVMs
}

func newRig(t *testing.T) rig {
	t.Helper()
	clustersize.DeclareForTest(t, 1)
	_, _, js := testutil.StartTestJetStream(t)
	ids, err := workerident.Open(context.Background(), js)
	if err != nil {
		t.Fatal(err)
	}
	r := rig{vol: &fakeVolumes{encrypted: true}, iam: &fakeRoles{}, net: &fakeNetwork{}, vms: &fakeVMs{}, host: &fakeHost{}}
	r.l = &Launcher{Identities: ids, Volumes: r.vol, Roles: r.iam, Network: r.net, Host: r.host, VMs: r.vms, NodeID: node}
	return r
}

func (r rig) current(t *testing.T) workerident.Record {
	t.Helper()
	rec, err := r.l.Identities.Current(context.Background(), region)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// The first launch prepares one volume, launches once with the real profile,
// network and a new-store status, and binds the instance so the gateway's
// authorization accepts it. Ensuring again returns that worker unchanged.
func TestEnsure_FirstLaunchBindsAndIsIdempotent(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	res, err := r.l.Ensure(ctx, region, itype, size)
	if err != nil || res.Generation != 1 {
		t.Fatalf("ensure = %+v, %v", res, err)
	}
	spec := r.vms.specs[0]
	if spec.Region != region || spec.ProfileARN == "" ||
		spec.Network.ENIID != "eni-1" || spec.DataVolumeID != r.vol.prepared[0] || spec.InstanceType != itype {
		t.Fatalf("spec = %+v", spec)
	}
	id, err := r.l.Identities.Authorize(ctx, res.InstanceID)
	if err != nil || id.Generation != 1 || id.NodeID != node || id.DataVolumeID != spec.DataVolumeID {
		t.Fatalf("authorize = %+v, %v", id, err)
	}
	again, err := r.l.Ensure(ctx, region, itype, size)
	if err != nil || again != res || r.vms.count() != 1 || len(r.vol.prepared) != 1 {
		t.Fatalf("second ensure = %+v, %v; launched %d, prepared %d", again, err, r.vms.count(), len(r.vol.prepared))
	}
}

// A volume whose own record is not encrypted is recorded and reported, never
// launched on, and later calls refuse it rather than preparing another.
func TestEnsure_RefusesAnUnencryptedVolume(t *testing.T) {
	r := newRig(t)
	r.vol.encrypted = false
	for range 3 {
		_, err := r.l.Ensure(context.Background(), region, itype, size)
		if !errors.Is(err, ErrUnencrypted) || !strings.Contains(err.Error(), r.vol.prepared[0]) {
			t.Fatalf("ensure = %v, want ErrUnencrypted naming %s", err, r.vol.prepared[0])
		}
	}
	if rec := r.current(t); rec.DataVolumeID != r.vol.prepared[0] || rec.Dispatched {
		t.Fatalf("record = %+v", rec)
	}
	if r.vol.count() != 1 || r.vms.count() != 0 || len(r.net.claims) != 0 {
		t.Fatalf("volumes=%d launches=%d network=%d", r.vol.count(), r.vms.count(), len(r.net.claims))
	}
}

// A Prepare proven to have sent no create withdraws its mark, so a retry may
// prepare the volume; the failed call allocated nothing.
func TestEnsure_ProvenPreCreateFailureIsRetried(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.vol.prepareErr = fmt.Errorf("%w: %w: no storage key", storevolume.ErrNotCreated, ErrUnencrypted)
	for range 2 {
		_, err := r.l.Ensure(ctx, region, itype, size)
		if !errors.Is(err, ErrVolumeNotCreated) || !errors.Is(err, ErrUnencrypted) {
			t.Fatalf("ensure = %v, want ErrVolumeNotCreated", err)
		}
		if _, ok := errors.AsType[*VolumeError](err); ok {
			t.Fatalf("ensure = %v reports a volume it never created", err)
		}
	}
	if rec := r.current(t); rec.VolumeAttempted || rec.DataVolumeID != "" || rec.Allocated() {
		t.Fatalf("record after a proven pre-create failure = %+v", rec)
	}
	r.vol.prepareErr = nil
	if _, err := r.l.Ensure(ctx, region, itype, size); err != nil {
		t.Fatal(err)
	}
	if r.vol.count() != 1 || r.vms.count() != 1 {
		t.Fatalf("volumes=%d launches=%d", r.vol.count(), r.vms.count())
	}
}

// A claim that cannot be minted refuses with nothing recorded or prepared.
func TestEnsure_NoClaimRecordsNothing(t *testing.T) {
	r := newRig(t)
	r.l.newClaimID = func() (string, error) { return "", errors.New("no entropy") }
	if _, err := r.l.Ensure(context.Background(), region, itype, size); err == nil {
		t.Fatal("ensure succeeded without a claim ID")
	}
	if r.vol.count() != 0 {
		t.Fatalf("prepared %v before a refusal that needs no volume", r.vol.prepared)
	}
	if _, err := r.l.Identities.Current(context.Background(), region); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Fatalf("record after refusal: %v", err)
	}
}

// A create that applied but returned no ID is never sent again: every later
// call reports the unknown outcome, even once the volume exists, and nothing
// further is allocated or launched.
func TestEnsure_AmbiguousVolumeCreateIsNeverResent(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.vol.appliedErr = errors.New("nats: timeout")
	if _, err := r.l.Ensure(ctx, region, itype, size); !errors.Is(err, ErrVolumeOutcomeUnknown) {
		t.Fatalf("ensure = %v, want ErrVolumeOutcomeUnknown", err)
	}
	r.vol.appliedErr = nil
	for range 3 {
		if _, err := r.l.Ensure(ctx, region, itype, size); !errors.Is(err, ErrVolumeOutcomeUnknown) {
			t.Fatalf("retry = %v, want ErrVolumeOutcomeUnknown", err)
		}
	}
	rec := r.current(t)
	if r.vol.count() != 1 || !rec.VolumeAttempted || rec.DataVolumeID != "" || r.vms.count() != 0 || len(r.net.claims) != 0 {
		t.Fatalf("volumes=%d record=%+v launches=%d network=%d", r.vol.count(), rec, r.vms.count(), len(r.net.claims))
	}
	if _, err := r.l.Ensure(ctx, region, "sys.other", size); !errors.Is(err, workerident.ErrTypeChanged) {
		t.Fatalf("re-type after a sent create = %v, want ErrTypeChanged", err)
	}
}

// A failed Prepare that names the volume it left behind reports that ID, and
// the create is still never sent again.
func TestEnsure_RetainedVolumeIDStaysVisible(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.l.RetainedVolume = func(err error) string {
		if k, ok := errors.AsType[keptVolumeError](err); ok {
			return k.id
		}
		return ""
	}
	r.vol.appliedErr = keptVolumeError{id: "vol-00000000000000001"}
	_, err := r.l.Ensure(ctx, region, itype, size)
	kept, ok := errors.AsType[*VolumeError](err)
	if !ok || kept.VolumeID != "vol-00000000000000001" || !errors.Is(err, ErrVolumeOutcomeUnknown) {
		t.Fatalf("ensure = %v, want the kept volume reported", err)
	}
	r.vol.appliedErr = nil
	if _, err := r.l.Ensure(ctx, region, itype, size); !errors.Is(err, ErrVolumeOutcomeUnknown) {
		t.Fatalf("retry = %v, want ErrVolumeOutcomeUnknown", err)
	}
	if r.vol.count() != 1 || r.vms.count() != 0 {
		t.Fatalf("volumes=%d launches=%d", r.vol.count(), r.vms.count())
	}
}

// An intent that has sent nothing may be re-typed; one that has may not.
func TestEnsure_UnallocatedIntentMayBeRetyped(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.vol.prepareErr = fmt.Errorf("%w: image writer missing", storevolume.ErrNotCreated)
	if _, err := r.l.Ensure(ctx, region, itype, size); !errors.Is(err, ErrVolumeNotCreated) {
		t.Fatalf("ensure = %v, want ErrVolumeNotCreated", err)
	}
	r.vol.prepareErr = nil
	res, err := r.l.Ensure(ctx, region, "sys.other", size)
	if err != nil || res.InstanceType != "sys.other" || res.Generation != 1 || r.vms.specs[0].InstanceType != "sys.other" {
		t.Fatalf("re-typed ensure = %+v, %v", res, err)
	}
}

// A launch whose reply is lost is never sent again: every later call reports
// the unknown outcome and the record keeps the dispatched intent.
func TestEnsure_AmbiguousLaunchIsNeverResent(t *testing.T) {
	r := newRig(t)
	r.vms.fail = errors.New("nats: timeout")
	ctx := context.Background()
	if _, err := r.l.Ensure(ctx, region, itype, size); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("ensure = %v, want ErrOutcomeUnknown", err)
	}
	r.vms.fail = nil
	for range 3 {
		if _, err := r.l.Ensure(ctx, region, itype, size); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("retry = %v, want ErrOutcomeUnknown", err)
		}
	}
	if rec := r.current(t); r.vms.count() != 1 || rec.State != workerident.StateLaunching || !rec.Dispatched {
		t.Fatalf("launched %d, record %+v", r.vms.count(), rec)
	}
}

// Concurrent first launches prepare one volume and start exactly one VM.
func TestEnsure_ConcurrentCallsLaunchOnce(t *testing.T) {
	r := newRig(t)
	var wg sync.WaitGroup
	var ok atomic.Int32
	for range 8 {
		wg.Go(func() {
			if _, err := r.l.Ensure(context.Background(), region, itype, size); err == nil {
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	if r.vms.count() != 1 || r.vol.count() != 1 || ok.Load() < 1 {
		t.Fatalf("launched %d VMs on %d volumes, %d calls succeeded", r.vms.count(), r.vol.count(), ok.Load())
	}
	if rec := r.current(t); rec.State != workerident.StateBound {
		t.Fatalf("record = %+v, want bound", rec)
	}
}

// A failure before dispatch leaves an intent the next call resumes with the
// same claim, volume and generation.
func TestEnsure_ResumesAnUndispatchedIntent(t *testing.T) {
	r := newRig(t)
	r.iam.fail.Store(1)
	ctx := context.Background()
	if _, err := r.l.Ensure(ctx, region, itype, size); err == nil {
		t.Fatal("ensure succeeded with IAM failing")
	}
	if rec := r.current(t); rec.State != workerident.StateLaunching || rec.Dispatched {
		t.Fatalf("record = %+v, want an undispatched intent", rec)
	}
	res, err := r.l.Ensure(ctx, region, itype, size)
	if err != nil || res.Generation != 1 || r.vms.count() != 1 || len(r.vol.prepared) != 1 {
		t.Fatalf("resume = %+v, %v; launched %d, prepared %d", res, err, r.vms.count(), len(r.vol.prepared))
	}
	if len(r.net.claims) != 1 || r.current(t).ClaimID != r.net.claims[0] {
		t.Fatalf("network claims %v, record claim %q", r.net.claims, r.current(t).ClaimID)
	}
}

// A new volume is recorded pending its receipt key at the requested size;
// after a confirmed stop the replacement reuses the pinned volume and its
// receipt key state, and the replaced instance is no longer authorized.
func TestEnsure_ReplacementReusesTheVolumeAndItsReceiptKey(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	first, err := r.l.Ensure(ctx, region, itype, size)
	if err != nil {
		t.Fatal(err)
	}
	if cur := r.current(t); cur.ReceiptInit != workerident.ReceiptPending || cur.SizeGiB != size || r.vol.sizes[0] != size {
		t.Fatalf("new volume record = %+v, sizes %v", cur, r.vol.sizes)
	}
	old, err := r.l.Identities.Authorize(ctx, first.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	fp := strings.Repeat("c", 64)
	if err := r.l.Identities.RecordReceiptKey(ctx, old, fp); err != nil {
		t.Fatal(err)
	}
	intent := workerident.Intent{Region: region, Generation: 1, ClaimID: old.ClaimID, SizeGiB: size}
	if err := r.l.Identities.RecordStopped(ctx, intent, first.InstanceID); err != nil {
		t.Fatal(err)
	}

	next, err := r.l.Ensure(ctx, region, itype, size)
	if err != nil || next.Generation != 2 || next.InstanceID == first.InstanceID {
		t.Fatalf("replacement = %+v, %v", next, err)
	}
	spec := r.vms.specs[1]
	if spec.DataVolumeID != r.vol.prepared[0] || len(r.vol.prepared) != 1 || spec.Region != region {
		t.Fatalf("replacement spec = %+v", spec)
	}
	if cur := r.current(t); cur.ReceiptInit != workerident.ReceiptRecorded || cur.ReceiptFingerprint != fp {
		t.Fatalf("replacement lost the receipt key state: %+v", cur)
	}
	if _, err := r.l.Identities.Authorize(ctx, first.InstanceID); err == nil {
		t.Fatal("the replaced instance is still authorized")
	}
}

// The worker is pinned: a launcher on another node launches nothing.
func TestEnsure_RefusesAnotherNode(t *testing.T) {
	r := newRig(t)
	if _, err := r.l.Ensure(context.Background(), region, itype, size); err != nil {
		t.Fatal(err)
	}
	other := *r.l
	other.NodeID = "node-b"
	if _, err := other.Ensure(context.Background(), region, itype, size); !errors.Is(err, workerident.ErrWrongNode) {
		t.Fatalf("other node = %v, want ErrWrongNode", err)
	}
	if r.vms.count() != 1 {
		t.Fatalf("launched %d", r.vms.count())
	}
}

func TestEnsure_RefusesWithoutEveryCapability(t *testing.T) {
	r := newRig(t)
	for name, mutate := range map[string]func(*Launcher){
		"volumes": func(l *Launcher) { l.Volumes = nil },
		"roles":   func(l *Launcher) { l.Roles = nil },
		"network": func(l *Launcher) { l.Network = nil },
		"host":    func(l *Launcher) { l.Host = nil },
		"vms":     func(l *Launcher) { l.VMs = nil },
		"node":    func(l *Launcher) { l.NodeID = "" },
	} {
		l := *r.l
		mutate(&l)
		if _, err := l.Ensure(context.Background(), region, itype, size); !errors.Is(err, ErrIncomplete) {
			t.Errorf("%s: %v, want ErrIncomplete", name, err)
		}
	}
	if r.vms.count() != 0 || len(r.vol.prepared) != 0 {
		t.Fatal("an incomplete launcher did work")
	}
}

// A host that cannot boot the worker refuses the first launch before a volume,
// intent, network or dispatch exists, and the next ready call launches.
func TestEnsure_HostNotReadyRefusesBeforeAllocating(t *testing.T) {
	r := newRig(t)
	r.host.err = errors.New("image missing")
	if _, err := r.l.Ensure(context.Background(), region, itype, size); !errors.Is(err, ErrHostNotReady) {
		t.Fatalf("Ensure = %v, want ErrHostNotReady", err)
	}
	if len(r.vol.prepared) != 0 || len(r.net.claims) != 0 || r.vms.count() != 0 {
		t.Fatalf("allocated: volumes=%d claims=%d launches=%d", len(r.vol.prepared), len(r.net.claims), r.vms.count())
	}
	if _, err := r.l.Identities.Current(context.Background(), region); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Fatalf("record after refusal: %v", err)
	}
	r.host.err = nil
	if _, err := r.l.Ensure(context.Background(), region, itype, size); err != nil {
		t.Fatal(err)
	}
	if r.vms.count() != 1 || len(r.vol.prepared) != 1 {
		t.Fatalf("ready retry: launches=%d volumes=%d", r.vms.count(), len(r.vol.prepared))
	}
}

// An undispatched intent is not dispatched while the host is not ready, and
// a stopped worker is not replaced.
func TestEnsure_HostNotReadyLeavesRecordsUntouched(t *testing.T) {
	r := newRig(t)
	r.iam.fail.Store(1)
	if _, err := r.l.Ensure(context.Background(), region, itype, size); err == nil {
		t.Fatal("want IAM failure")
	}
	before := r.current(t)
	r.host.err = errors.New("image missing")
	if _, err := r.l.Ensure(context.Background(), region, itype, size); !errors.Is(err, ErrHostNotReady) {
		t.Fatalf("resume = %v, want ErrHostNotReady", err)
	}
	if after := r.current(t); after != before || r.vms.count() != 0 {
		t.Fatalf("resume changed the record or launched: %+v", after)
	}

	r.host.err = nil
	first, err := r.l.Ensure(context.Background(), region, itype, size)
	if err != nil {
		t.Fatal(err)
	}
	cur := r.current(t)
	intent := workerident.Intent{Region: region, Generation: cur.Generation, ClaimID: cur.ClaimID, SizeGiB: cur.SizeGiB}
	if err := r.l.Identities.RecordStopped(context.Background(), intent, first.InstanceID); err != nil {
		t.Fatal(err)
	}
	stopped := r.current(t)
	r.host.err = errors.New("image missing")
	if _, err := r.l.Ensure(context.Background(), region, itype, size); !errors.Is(err, ErrHostNotReady) {
		t.Fatalf("replace = %v, want ErrHostNotReady", err)
	}
	if after := r.current(t); after != stopped || r.vms.count() != 1 {
		t.Fatalf("replacement changed the record or launched: %+v", after)
	}
}

// A create whose reply is lost and whose ENI is not yet visible is never sent
// again: the retry fails closed and dispatches nothing, and once the ENI
// becomes visible the next call reuses it and launches once.
func TestEnsure_UncertainNetworkCreateIsNotRepeated(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.net.fail = errors.New("nats: timeout")
	r.net.hidden = true
	if _, err := r.l.Ensure(ctx, region, itype, size); !errors.Is(err, ErrNetworkOutcomeUnknown) {
		t.Fatalf("first = %v, want ErrNetworkOutcomeUnknown", err)
	}
	r.net.fail = nil
	for range 3 {
		if _, err := r.l.Ensure(ctx, region, itype, size); !errors.Is(err, ErrNetworkOutcomeUnknown) {
			t.Fatalf("retry = %v, want ErrNetworkOutcomeUnknown", err)
		}
	}
	rec := r.current(t)
	if len(r.net.claims) != 1 || rec.Dispatched || !rec.NetworkAttempted || r.vms.count() != 0 {
		t.Fatalf("creates=%d record=%+v launches=%d", len(r.net.claims), rec, r.vms.count())
	}
	r.net.show(rec.ClaimID)
	if _, err := r.l.Ensure(ctx, region, itype, size); err != nil {
		t.Fatal(err)
	}
	if len(r.net.claims) != 1 || r.vms.count() != 1 {
		t.Fatalf("after visible: creates=%d launches=%d", len(r.net.claims), r.vms.count())
	}
}

// A failure after the ENI is visible resumes with that ENI, not a new create.
func TestEnsure_VisibleNetworkIsReusedOnResume(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.net.fail = errors.New("nats: timeout")
	if _, err := r.l.Ensure(ctx, region, itype, size); !errors.Is(err, ErrNetworkOutcomeUnknown) {
		t.Fatalf("first = %v", err)
	}
	r.net.fail = nil
	if _, err := r.l.Ensure(ctx, region, itype, size); err != nil {
		t.Fatal(err)
	}
	if len(r.net.claims) != 1 || r.vms.count() != 1 {
		t.Fatalf("creates=%d launches=%d", len(r.net.claims), r.vms.count())
	}
}

// A request for another type neither resumes an undispatched intent nor
// reports the bound VM as that type; only a replacement may change it.
func TestEnsure_InstanceTypeIsFixedUntilReplacement(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.iam.fail.Store(1)
	if _, err := r.l.Ensure(ctx, region, itype, size); err == nil {
		t.Fatal("ensure succeeded with IAM failing")
	}
	if _, err := r.l.Ensure(ctx, region, "sys.other", size); !errors.Is(err, workerident.ErrTypeChanged) {
		t.Fatalf("resume with another type = %v, want ErrTypeChanged", err)
	}
	first, err := r.l.Ensure(ctx, region, itype, size)
	if err != nil || first.InstanceType != itype || r.vms.count() != 1 {
		t.Fatalf("resume = %+v, %v; launched %d", first, err, r.vms.count())
	}
	if _, err := r.l.Ensure(ctx, region, "sys.other", size); !errors.Is(err, workerident.ErrTypeChanged) {
		t.Fatalf("bound with another type = %v, want ErrTypeChanged", err)
	}
	if r.vms.count() != 1 {
		t.Fatalf("a type mismatch launched: %d", r.vms.count())
	}

	intent := workerident.Intent{Region: region, Generation: 1, ClaimID: r.current(t).ClaimID}
	if err := r.l.Identities.RecordStopped(ctx, intent, first.InstanceID); err != nil {
		t.Fatal(err)
	}
	next, err := r.l.Ensure(ctx, region, "sys.other", size)
	if err != nil || next.Generation != 2 || next.InstanceType != "sys.other" || r.vms.specs[1].InstanceType != "sys.other" {
		t.Fatalf("replacement = %+v, %v", next, err)
	}
	if rec := r.current(t); rec.InstanceType != "sys.other" {
		t.Fatalf("replacement record type = %q", rec.InstanceType)
	}
}
