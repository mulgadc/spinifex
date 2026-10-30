package workerlaunch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/firststore"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nkeys"
)

const (
	region = "ap-southeast-2"
	node   = "node-a"
	itype  = "sys.test"
)

type fakeVolumes struct {
	mu        sync.Mutex
	prepared  []string
	encrypted bool
}

func (f *fakeVolumes) Prepare(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("vol-%017x", len(f.prepared)+1)
	f.prepared = append(f.prepared, id)
	return id, nil
}

func (f *fakeVolumes) Encrypted(context.Context, string) (bool, error) { return f.encrypted, nil }

type fakeRoles struct{ fail atomic.Int32 }

func (f *fakeRoles) EnsureWorkerProfile(context.Context) (string, error) {
	if f.fail.Add(-1) >= 0 {
		return "", errors.New("iam unavailable")
	}
	return "arn:aws:iam::000000000000:instance-profile/sqsWorkerRole", nil
}

type fakeNetwork struct {
	mu     sync.Mutex
	claims []string
}

func (f *fakeNetwork) WorkerNetwork(_ context.Context, _, claimID string) (*Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims = append(f.claims, claimID)
	return &Network{SubnetID: "subnet-1", ENIID: "eni-1", ENIMac: "02:00:00:00:00:01",
		ENIIP: "10.240.0.10", CIDR: "10.240.0.10/24", Gateway: "10.240.0.1"}, nil
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

type rig struct {
	l   *Launcher
	vol *fakeVolumes
	iam *fakeRoles
	net *fakeNetwork
	vms *fakeVMs
}

func newRig(t *testing.T) rig {
	t.Helper()
	clustersize.DeclareForTest(t, 1)
	_, _, js := testutil.StartTestJetStream(t)
	ids, err := workerident.Open(context.Background(), js)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := firststore.Open(context.Background(), js)
	if err != nil {
		t.Fatal(err)
	}
	r := rig{vol: &fakeVolumes{encrypted: true}, iam: &fakeRoles{}, net: &fakeNetwork{}, vms: &fakeVMs{}}
	r.l = &Launcher{Identities: ids, FirstStore: fs, Volumes: r.vol, Roles: r.iam, Network: r.net, VMs: r.vms, NodeID: node}
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
	res, err := r.l.Ensure(ctx, region, itype)
	if err != nil || res.Generation != 1 {
		t.Fatalf("ensure = %+v, %v", res, err)
	}
	spec := r.vms.specs[0]
	if spec.StoreStatus != StoreNew || spec.StoreNKey != "" || spec.ProfileARN == "" ||
		spec.Network.ENIID != "eni-1" || spec.DataVolumeID != r.vol.prepared[0] || spec.InstanceType != itype {
		t.Fatalf("spec = %+v", spec)
	}
	id, err := r.l.Identities.Authorize(ctx, res.InstanceID)
	if err != nil || id.Generation != 1 || id.NodeID != node || id.DataVolumeID != spec.DataVolumeID {
		t.Fatalf("authorize = %+v, %v", id, err)
	}
	again, err := r.l.Ensure(ctx, region, itype)
	if err != nil || again != res || r.vms.count() != 1 || len(r.vol.prepared) != 1 {
		t.Fatalf("second ensure = %+v, %v; launched %d, prepared %d", again, err, r.vms.count(), len(r.vol.prepared))
	}
}

// A volume whose own record is not encrypted is refused before any intent.
func TestEnsure_RefusesAnUnencryptedVolume(t *testing.T) {
	r := newRig(t)
	r.vol.encrypted = false
	if _, err := r.l.Ensure(context.Background(), region, itype); !errors.Is(err, ErrUnencrypted) {
		t.Fatalf("ensure = %v, want ErrUnencrypted", err)
	}
	if _, err := r.l.Identities.Current(context.Background(), region); err == nil {
		t.Fatal("an intent was recorded for an unencrypted volume")
	}
	if r.vms.count() != 0 {
		t.Fatal("launched on an unencrypted volume")
	}
}

// A launch whose reply is lost is never sent again: every later call reports
// the unknown outcome and the record keeps the dispatched intent.
func TestEnsure_AmbiguousLaunchIsNeverResent(t *testing.T) {
	r := newRig(t)
	r.vms.fail = errors.New("nats: timeout")
	ctx := context.Background()
	if _, err := r.l.Ensure(ctx, region, itype); !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("ensure = %v, want ErrOutcomeUnknown", err)
	}
	r.vms.fail = nil
	for range 3 {
		if _, err := r.l.Ensure(ctx, region, itype); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("retry = %v, want ErrOutcomeUnknown", err)
		}
	}
	if rec := r.current(t); r.vms.count() != 1 || rec.State != workerident.StateLaunching || !rec.Dispatched {
		t.Fatalf("launched %d, record %+v", r.vms.count(), rec)
	}
}

// Concurrent first launches start exactly one VM.
func TestEnsure_ConcurrentCallsLaunchOnce(t *testing.T) {
	r := newRig(t)
	var wg sync.WaitGroup
	var ok atomic.Int32
	for range 8 {
		wg.Go(func() {
			if _, err := r.l.Ensure(context.Background(), region, itype); err == nil {
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	if r.vms.count() != 1 || ok.Load() < 1 {
		t.Fatalf("launched %d VMs, %d calls succeeded", r.vms.count(), ok.Load())
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
	if _, err := r.l.Ensure(ctx, region, itype); err == nil {
		t.Fatal("ensure succeeded with IAM failing")
	}
	if rec := r.current(t); rec.State != workerident.StateLaunching || rec.Dispatched {
		t.Fatalf("record = %+v, want an undispatched intent", rec)
	}
	res, err := r.l.Ensure(ctx, region, itype)
	if err != nil || res.Generation != 1 || r.vms.count() != 1 || len(r.vol.prepared) != 1 {
		t.Fatalf("resume = %+v, %v; launched %d, prepared %d", res, err, r.vms.count(), len(r.vol.prepared))
	}
	if len(r.net.claims) != 1 || r.current(t).ClaimID != r.net.claims[0] {
		t.Fatalf("network claims %v, record claim %q", r.net.claims, r.current(t).ClaimID)
	}
}

// After a confirmed stop the replacement fences the old generation out of
// first-store, reuses the pinned volume, and launches with the recorded key.
func TestEnsure_ReplacementFencesAndReusesTheVolume(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	first, err := r.l.Ensure(ctx, region, itype)
	if err != nil {
		t.Fatal(err)
	}
	old := workerident.Identity{InstanceID: first.InstanceID, Generation: 1, ClaimID: r.current(t).ClaimID}
	oldFS := firststore.Identity{InstanceID: old.InstanceID, ClaimID: old.ClaimID, Generation: 1}
	kp, _ := nkeys.CreateUser()
	pub, _ := kp.PublicKey()
	if _, err := r.l.FirstStore.Claim(ctx, region, oldFS); err != nil {
		t.Fatal(err)
	}
	if err := r.l.FirstStore.RecordPublicKey(ctx, region, oldFS, pub); err != nil {
		t.Fatal(err)
	}
	intent := workerident.Intent{Region: region, Generation: 1, ClaimID: old.ClaimID}
	if err := r.l.Identities.RecordStopped(ctx, intent, first.InstanceID); err != nil {
		t.Fatal(err)
	}

	next, err := r.l.Ensure(ctx, region, itype)
	if err != nil || next.Generation != 2 || next.InstanceID == first.InstanceID {
		t.Fatalf("replacement = %+v, %v", next, err)
	}
	spec := r.vms.specs[1]
	if spec.DataVolumeID != r.vol.prepared[0] || len(r.vol.prepared) != 1 || spec.StoreStatus != StoreExisting || spec.StoreNKey != pub {
		t.Fatalf("replacement spec = %+v", spec)
	}
	if f, _ := r.l.FirstStore.FencedAt(ctx, region); f != 2 {
		t.Fatalf("fence = %d, want 2", f)
	}
	if _, err := r.l.FirstStore.Claim(ctx, region, oldFS); !errors.Is(err, firststore.ErrStaleGeneration) {
		t.Fatalf("old generation claim = %v, want ErrStaleGeneration", err)
	}
	if _, err := r.l.Identities.Authorize(ctx, first.InstanceID); err == nil {
		t.Fatal("the replaced instance is still authorized")
	}
}

// The worker is pinned: a launcher on another node launches nothing.
func TestEnsure_RefusesAnotherNode(t *testing.T) {
	r := newRig(t)
	if _, err := r.l.Ensure(context.Background(), region, itype); err != nil {
		t.Fatal(err)
	}
	other := *r.l
	other.NodeID = "node-b"
	if _, err := other.Ensure(context.Background(), region, itype); !errors.Is(err, workerident.ErrWrongNode) {
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
		"vms":     func(l *Launcher) { l.VMs = nil },
		"node":    func(l *Launcher) { l.NodeID = "" },
	} {
		l := *r.l
		mutate(&l)
		if _, err := l.Ensure(context.Background(), region, itype); !errors.Is(err, ErrIncomplete) {
			t.Errorf("%s: %v, want ErrIncomplete", name, err)
		}
	}
	if r.vms.count() != 0 || len(r.vol.prepared) != 0 {
		t.Fatal("an incomplete launcher did work")
	}
}
