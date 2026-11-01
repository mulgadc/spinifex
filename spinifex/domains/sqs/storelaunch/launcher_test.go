package storelaunch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/firststore"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storevolume"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerlaunch"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/nats-io/nkeys"
)

const (
	storeProfile = "arn:aws:iam::000000000000:instance-profile/sqsStoreRole"
	region       = "ap-southeast-2"
	node         = "node-a"
	itype        = "sys.store"
	sizeGiB      = 8
	workerInst   = "i-0000000000000000a"
)

type fakeVolumes struct {
	mu         sync.Mutex
	sizes      []int64
	encrypted  bool
	prepareErr error
}

func (f *fakeVolumes) Prepare(_ context.Context, size int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sizes = append(f.sizes, size)
	if f.prepareErr != nil {
		return "", f.prepareErr
	}
	return fmt.Sprintf("vol-%017x", len(f.sizes)), nil
}

func (f *fakeVolumes) Encrypted(context.Context, string) (bool, error) { return f.encrypted, nil }

func (f *fakeVolumes) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sizes)
}

// fakeNetwork holds one ENI per claim. A create with fail set is applied but
// reports failure; with hidden set it stays invisible to Find until shown.
type fakeNetwork struct {
	mu      sync.Mutex
	creates int
	created map[string]bool
	shown   map[string]bool
	fail    error
	hidden  bool
}

func storeNet() *workerlaunch.Network {
	return &workerlaunch.Network{SubnetID: "subnet-1", ENIID: "eni-2", ENIMac: "02:00:00:00:00:02",
		ENIIP: "10.240.0.11", CIDR: "10.240.0.11/24", Gateway: "10.240.0.1"}
}

func (f *fakeNetwork) FindStoreNetwork(_ context.Context, _, claimID string) (*workerlaunch.Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.created[claimID] && (!f.hidden || f.shown[claimID]) {
		return storeNet(), nil
	}
	return nil, nil
}

func (f *fakeNetwork) CreateStoreNetwork(_ context.Context, _, claimID string) (*workerlaunch.Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if f.created == nil {
		f.created = map[string]bool{}
	}
	f.created[claimID] = true
	if f.fail != nil {
		return nil, f.fail
	}
	return storeNet(), nil
}

func (f *fakeNetwork) show(claimID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.shown == nil {
		f.shown = map[string]bool{}
	}
	f.shown[claimID] = true
}

func (f *fakeNetwork) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

type fakeVMs struct {
	mu    sync.Mutex
	specs []Spec
	fail  error
}

func (f *fakeVMs) LaunchStore(_ context.Context, spec Spec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, spec)
	if f.fail != nil {
		return "", f.fail
	}
	return fmt.Sprintf("i-%017x", len(f.specs)+0x100), nil
}

func (f *fakeVMs) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.specs)
}

type fakeRoles struct {
	arn string
	err error
}

func (f *fakeRoles) EnsureStoreProfile(context.Context) (string, error) { return f.arn, f.err }

type fakeHost struct{ err error }

func (f *fakeHost) Ready(context.Context) error { return f.err }

type rig struct {
	l    *Launcher
	vol  *fakeVolumes
	net  *fakeNetwork
	vms  *fakeVMs
	host *fakeHost
	role *fakeRoles
	pub  string
	seed string
}

func newRig(t *testing.T) rig {
	t.Helper()
	clustersize.DeclareForTest(t, 1)
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	recs, err := Open(ctx, js)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := workerident.Open(ctx, js)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := firststore.Open(ctx, js)
	if err != nil {
		t.Fatal(err)
	}
	r := rig{vol: &fakeVolumes{encrypted: true}, net: &fakeNetwork{}, vms: &fakeVMs{}, host: &fakeHost{},
		role: &fakeRoles{arn: storeProfile}}
	r.l = &Launcher{Records: recs, Workers: ids, FirstStore: fs, Volumes: r.vol, Network: r.net,
		Roles: r.role, Host: r.host, VMs: r.vms, NodeID: node, RetainedVolume: func(err error) string {
			if v, ok := errors.AsType[*retained](err); ok {
				return v.id
			}
			return ""
		}}
	return r
}

type retained struct{ id string }

func (r *retained) Error() string { return "retained " + r.id }

// bindWorker binds the Region's worker on nodeID; record also records its
// public key in the first-store decision, as the worker's boot does.
func (r *rig) bindWorker(t *testing.T, nodeID string, record bool) {
	t.Helper()
	ctx := context.Background()
	intent, err := r.l.Workers.BeginLaunch(ctx, region, workerident.LaunchRequest{InstanceType: "t3.small", ClaimID: "claim-worker-1",
		NodeID: nodeID, DataVolumeID: "vol-0000000000000000a", DataVolumeEncrypted: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.l.Workers.MarkDispatched(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if err := r.l.Workers.BindInstance(ctx, intent, workerInst); err != nil {
		t.Fatal(err)
	}
	if !record {
		return
	}
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := kp.PublicKey()
	seed, _ := kp.Seed()
	r.pub, r.seed = pub, string(seed)
	id := firststore.Identity{InstanceID: workerInst, ClaimID: "claim-worker-1", Generation: intent.Generation}
	if d, err := r.l.FirstStore.Claim(ctx, region, id); err != nil || !d.CreateKey {
		t.Fatalf("claim = %+v, %v", d, err)
	}
	if err := r.l.FirstStore.RecordPublicKey(ctx, region, id, pub); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) record(t *testing.T) Record {
	t.Helper()
	rec, err := r.l.Records.Current(context.Background(), region)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func (r *rig) nothingAllocated(t *testing.T) {
	t.Helper()
	if r.vol.count() != 0 || r.net.count() != 0 || r.vms.count() != 0 {
		t.Fatalf("allocated: %d volumes, %d ENIs, %d VMs", r.vol.count(), r.net.count(), r.vms.count())
	}
}

// The store launches once with the worker's recorded public key, a fresh
// volume of the operator's size and an ENI in the worker's subnet, and the
// record binds it. Ensuring again returns that store with nothing new.
func TestEnsure_LaunchesWithTheRecordedPublicKeyAndIsIdempotent(t *testing.T) {
	r := newRig(t)
	r.bindWorker(t, node, true)
	ctx := context.Background()
	res, err := r.l.Ensure(ctx, region, itype, sizeGiB)
	if err != nil || res.Address != storeNet().ENIIP {
		t.Fatalf("ensure = %+v, %v", res, err)
	}
	spec := r.vms.specs[0]
	if spec.ClientNKey != r.pub || !nkeys.IsValidPublicUserKey(spec.ClientNKey) || spec.InstanceType != itype ||
		spec.Network.ENIID != storeNet().ENIID || spec.DataVolumeID != "vol-00000000000000001" ||
		spec.Region != region || spec.ProfileARN != storeProfile {
		t.Fatalf("spec = %+v", spec)
	}
	if r.vol.sizes[0] != sizeGiB {
		t.Fatalf("volume size = %d", r.vol.sizes[0])
	}
	if rec := r.record(t); rec.State != StateBound || rec.InstanceID != res.InstanceID || rec.PublicKey != r.pub {
		t.Fatalf("record = %+v", rec)
	}
	again, err := r.l.Ensure(ctx, region, itype, sizeGiB)
	if err != nil || again != res || r.vms.count() != 1 || r.vol.count() != 1 || r.net.count() != 1 {
		t.Fatalf("again = %+v, %v; %d VMs, %d volumes, %d ENIs", again, err, r.vms.count(), r.vol.count(), r.net.count())
	}
}

// No store is launched, nor anything allocated, until the worker is bound on
// this node and has recorded its public key.
func TestEnsure_RequiresTheWorkerBoundHereWithItsKeyRecorded(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(*testing.T, *rig)
		wantIs error
	}{
		{"no worker", func(*testing.T, *rig) {}, ErrWorkerNotBound},
		{"worker on another node", func(t *testing.T, r *rig) { r.bindWorker(t, "node-b", true) }, ErrWorkerNotBound},
		{"key not recorded", func(t *testing.T, r *rig) { r.bindWorker(t, node, false) }, ErrKeyNotRecorded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			tc.setup(t, &r)
			_, err := r.l.Ensure(context.Background(), region, itype, sizeGiB)
			if !errors.Is(err, tc.wantIs) {
				t.Fatalf("ensure = %v, want %v", err, tc.wantIs)
			}
			r.nothingAllocated(t)
			if _, err := r.l.Records.Current(context.Background(), region); err == nil {
				t.Fatal("a refused ensure must record nothing")
			}
		})
	}
}

// A node that cannot boot the store refuses before any record or allocation,
// and a later call succeeds once it can.
func TestEnsure_HostNotReadyAllocatesNothing(t *testing.T) {
	r := newRig(t)
	r.bindWorker(t, node, true)
	r.host.err = errors.New("no store image")
	_, err := r.l.Ensure(context.Background(), region, itype, sizeGiB)
	if !errors.Is(err, ErrHostNotReady) {
		t.Fatalf("ensure = %v", err)
	}
	r.nothingAllocated(t)
	r.host.err = nil
	if _, err := r.l.Ensure(context.Background(), region, itype, sizeGiB); err != nil || r.vms.count() != 1 {
		t.Fatalf("ensure once ready = %v, %d VMs", err, r.vms.count())
	}
}

// A dispatched launch whose reply was lost is never sent again.
func TestEnsure_AmbiguousLaunchIsNeverResent(t *testing.T) {
	r := newRig(t)
	r.bindWorker(t, node, true)
	r.vms.fail = errors.New("request timed out")
	for i := range 3 {
		if _, err := r.l.Ensure(context.Background(), region, itype, sizeGiB); !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("call %d = %v", i, err)
		}
	}
	if r.vms.count() != 1 || r.vol.count() != 1 || r.net.count() != 1 {
		t.Fatalf("%d VMs, %d volumes, %d ENIs", r.vms.count(), r.vol.count(), r.net.count())
	}
}

// A volume create whose outcome is unknown is never sent again, and a volume
// it is known to have left behind is reported by ID.
func TestEnsure_AmbiguousVolumeCreateIsNeverResent(t *testing.T) {
	r := newRig(t)
	r.bindWorker(t, node, true)
	r.vol.prepareErr = &retained{id: "vol-0000000000000beef"}
	_, err := r.l.Ensure(context.Background(), region, itype, sizeGiB)
	v, ok := errors.AsType[*VolumeError](err)
	if !ok || v.VolumeID != "vol-0000000000000beef" || !errors.Is(err, ErrVolumeOutcomeUnknown) {
		t.Fatalf("ensure = %v", err)
	}
	for range 2 {
		if _, err := r.l.Ensure(context.Background(), region, itype, sizeGiB); !errors.Is(err, ErrVolumeOutcomeUnknown) {
			t.Fatalf("retry = %v", err)
		}
	}
	if r.vol.count() != 1 || r.net.count() != 0 || r.vms.count() != 0 {
		t.Fatalf("%d volumes, %d ENIs, %d VMs", r.vol.count(), r.net.count(), r.vms.count())
	}
}

// A volume whose own record is not encrypted is kept and reported, never used
// or replaced.
func TestEnsure_RefusesAnUnencryptedVolume(t *testing.T) {
	r := newRig(t)
	r.bindWorker(t, node, true)
	r.vol.encrypted = false
	for range 2 {
		_, err := r.l.Ensure(context.Background(), region, itype, sizeGiB)
		v, ok := errors.AsType[*VolumeError](err)
		if !ok || v.VolumeID != "vol-00000000000000001" || !errors.Is(err, ErrUnencrypted) {
			t.Fatalf("ensure = %v", err)
		}
	}
	if r.vol.count() != 1 || r.net.count() != 0 || r.vms.count() != 0 {
		t.Fatalf("%d volumes, %d ENIs, %d VMs", r.vol.count(), r.net.count(), r.vms.count())
	}
}

// An ENI create that timed out before its interface is visible is never sent
// again; once it appears, the next call launches exactly once.
func TestEnsure_DelayedENICreateIsNeverResent(t *testing.T) {
	r := newRig(t)
	r.bindWorker(t, node, true)
	r.net.hidden, r.net.fail = true, errors.New("timeout")
	for range 3 {
		if _, err := r.l.Ensure(context.Background(), region, itype, sizeGiB); !errors.Is(err, ErrNetworkOutcomeUnknown) {
			t.Fatalf("ensure = %v", err)
		}
	}
	if r.net.count() != 1 || r.vms.count() != 0 {
		t.Fatalf("%d ENI creates, %d VMs", r.net.count(), r.vms.count())
	}
	r.net.show(r.record(t).ClaimID)
	if _, err := r.l.Ensure(context.Background(), region, itype, sizeGiB); err != nil || r.vms.count() != 1 || r.net.count() != 1 {
		t.Fatalf("ensure once visible = %v; %d VMs, %d ENI creates", err, r.vms.count(), r.net.count())
	}
}

// A retry asking for a different volume size is refused rather than applied.
func TestEnsure_RefusesADifferentSizeOnRetry(t *testing.T) {
	r := newRig(t)
	r.bindWorker(t, node, true)
	r.vms.fail = errors.New("timeout")
	_, _ = r.l.Ensure(context.Background(), region, itype, sizeGiB)
	r.vms.fail = nil
	_, err := r.l.Ensure(context.Background(), region, itype, sizeGiB+1)
	if !errors.Is(err, ErrOutcomeUnknown) && !errors.Is(err, ErrSizeChanged) {
		t.Fatalf("ensure = %v", err)
	}
	if r.vol.count() != 1 {
		t.Fatalf("%d volumes", r.vol.count())
	}
}

// Once the store is bound, a request for another volume size is refused
// rather than silently returning the existing store.
func TestEnsure_RefusesADifferentSizeOnceBound(t *testing.T) {
	r := newRig(t)
	r.bindWorker(t, node, true)
	if _, err := r.l.Ensure(context.Background(), region, itype, sizeGiB); err != nil {
		t.Fatal(err)
	}
	if _, err := r.l.Ensure(context.Background(), region, itype, sizeGiB+1); !errors.Is(err, ErrSizeChanged) {
		t.Fatalf("bound store with another size = %v", err)
	}
	if r.vol.count() != 1 || r.vms.count() != 1 {
		t.Fatalf("%d volumes, %d VMs", r.vol.count(), r.vms.count())
	}
}

// Before any dispatch, a retry with a different size is refused at Begin.
func TestEnsure_RefusesADifferentSizeBeforeDispatch(t *testing.T) {
	r := newRig(t)
	r.bindWorker(t, node, true)
	r.net.fail = errors.New("eni quota")
	_, _ = r.l.Ensure(context.Background(), region, itype, sizeGiB)
	_, err := r.l.Ensure(context.Background(), region, itype, sizeGiB+1)
	if !errors.Is(err, ErrSizeChanged) {
		t.Fatalf("ensure = %v", err)
	}
}

// A Prepare that failed before sending any create (a missing tool, scratch or
// mkfs failure) leaves the Region retryable, and the retry launches once.
func TestEnsure_RetriesAVolumePrepareThatCreatedNothing(t *testing.T) {
	r := newRig(t)
	r.bindWorker(t, node, true)
	r.vol.prepareErr = fmt.Errorf("%w: mkfs.ext4 not found", storevolume.ErrNotCreated)
	for range 2 {
		if _, err := r.l.Ensure(context.Background(), region, itype, sizeGiB); !errors.Is(err, ErrVolumeNotCreated) {
			t.Fatalf("ensure = %v", err)
		}
		if rec := r.record(t); rec.VolumeAttempted || rec.DataVolumeID != "" {
			t.Fatalf("record after an uncreated volume: %+v", rec)
		}
	}
	r.vol.prepareErr = nil
	res, err := r.l.Ensure(context.Background(), region, itype, sizeGiB)
	if err != nil || res.InstanceID == "" || r.vms.count() != 1 || r.net.count() != 1 {
		t.Fatalf("ensure after the fix = %+v, %v; %d VMs, %d ENIs", res, err, r.vms.count(), r.net.count())
	}
}

// A retry before dispatch asking for a different instance type is refused,
// and so is a request for another type once the store is bound.
func TestEnsure_RefusesADifferentInstanceType(t *testing.T) {
	r := newRig(t)
	r.bindWorker(t, node, true)
	r.net.fail = errors.New("eni quota")
	_, _ = r.l.Ensure(context.Background(), region, itype, sizeGiB)
	if _, err := r.l.Ensure(context.Background(), region, "sys.other", sizeGiB); !errors.Is(err, ErrTypeChanged) {
		t.Fatalf("retry with another type = %v", err)
	}
	r.net.fail = nil
	res, err := r.l.Ensure(context.Background(), region, itype, sizeGiB)
	if err != nil || res.InstanceType != itype || r.vms.specs[0].InstanceType != itype {
		t.Fatalf("ensure = %+v, %v", res, err)
	}
	if _, err := r.l.Ensure(context.Background(), region, "sys.other", sizeGiB); !errors.Is(err, ErrTypeChanged) {
		t.Fatalf("bound store with another type = %v", err)
	}
	if r.vms.count() != 1 {
		t.Fatalf("%d VMs", r.vms.count())
	}
}

// The record, every error and the launch spec carry only the public key.
func TestEnsure_NeverCarriesTheSeed(t *testing.T) {
	r := newRig(t)
	r.bindWorker(t, node, true)
	r.vms.fail = errors.New("timeout")
	_, err := r.l.Ensure(context.Background(), region, itype, sizeGiB)
	if err == nil || containsSeed(err.Error(), r.seed) || containsSeed(fmt.Sprintf("%+v", r.record(t)), r.seed) ||
		containsSeed(fmt.Sprintf("%+v", r.vms.specs), r.seed) {
		t.Fatalf("seed leaked or no error: %v", err)
	}
}

func containsSeed(s, seed string) bool { return seed != "" && strings.Contains(s, seed) }

// After the worker is replaced, the store is not launched until the bound
// successor has claimed the recorded key; it then launches with that key.
func TestEnsure_WaitsForAReplacedWorkersSuccessorToClaim(t *testing.T) {
	r := newRig(t)
	r.bindWorker(t, node, true)
	ctx := context.Background()
	old := workerident.Intent{Region: region, Generation: 1, ClaimID: "claim-worker-1"}
	if err := r.l.Workers.RecordStopped(ctx, old, workerInst); err != nil {
		t.Fatal(err)
	}
	if err := r.l.FirstStore.Fence(ctx, region, 2); err != nil {
		t.Fatal(err)
	}
	next, err := r.l.Workers.BeginLaunch(ctx, region, workerident.LaunchRequest{InstanceType: "t3.small", ClaimID: "claim-worker-2",
		Supersedes: 1, NodeID: node, DataVolumeID: "vol-0000000000000000a", DataVolumeEncrypted: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.l.Workers.MarkDispatched(ctx, next); err != nil {
		t.Fatal(err)
	}
	const successor = "i-0000000000000000b"
	if err := r.l.Workers.BindInstance(ctx, next, successor); err != nil {
		t.Fatal(err)
	}

	if _, err := r.l.Ensure(ctx, region, itype, sizeGiB); !errors.Is(err, ErrKeyNotRecorded) {
		t.Fatalf("ensure before the successor claims = %v", err)
	}
	r.nothingAllocated(t)

	id := firststore.Identity{InstanceID: successor, ClaimID: "claim-worker-2", Generation: next.Generation}
	if d, err := r.l.FirstStore.Claim(ctx, region, id); err != nil || d.CreateKey || d.PublicKey != r.pub {
		t.Fatalf("successor claim = %+v, %v", d, err)
	}
	if _, err := r.l.Ensure(ctx, region, itype, sizeGiB); err != nil || r.vms.specs[0].ClientNKey != r.pub {
		t.Fatalf("ensure after the successor claims = %v", err)
	}
}

// A store whose instance profile cannot be ensured is never dispatched, so
// no VM launches without the role its bootstrap signs with.
func TestEnsure_NoProfileLaunchesNothing(t *testing.T) {
	r := newRig(t)
	r.bindWorker(t, node, true)
	r.role.err = errors.New("iam unavailable")
	if _, err := r.l.Ensure(context.Background(), region, itype, sizeGiB); err == nil {
		t.Fatal("ensure succeeded without a profile")
	}
	if r.vms.count() != 0 || r.record(t).Dispatched {
		t.Fatalf("launched %d, dispatched %v", r.vms.count(), r.record(t).Dispatched)
	}
	r.role.err = nil
	if _, err := r.l.Ensure(context.Background(), region, itype, sizeGiB); err != nil || r.vms.count() != 1 {
		t.Fatalf("retry = %v, launched %d", err, r.vms.count())
	}
}
