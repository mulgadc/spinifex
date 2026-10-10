package daemon

import (
	"context"
	"errors"
	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	"net/netip"
	"sync"

	"github.com/aws/aws-sdk-go/service/ec2"
	ec2eip "github.com/mulgadc/spinifex/spinifex/domains/ec2/eip"
	"github.com/mulgadc/spinifex/spinifex/domains/network/external"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

var errInjected = errors.New("injected failure")

// failingKV wraps a real bucket and fails the methods named in failOn with
// errInjected, so tests can reach the KV error branches.
type failingKV struct {
	jetstream.KeyValue

	mu     sync.Mutex
	failOn map[string]bool
}

func newFailingKV(kv jetstream.KeyValue, methods ...string) *failingKV {
	f := &failingKV{KeyValue: kv, failOn: map[string]bool{}}
	for _, m := range methods {
		f.failOn[m] = true
	}
	return f
}

func (f *failingKV) fails(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failOn[method]
}

func (f *failingKV) setFail(method string, fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOn[method] = fail
}

func (f *failingKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if f.fails("Get") {
		return nil, errInjected
	}
	return f.KeyValue.Get(ctx, key)
}

func (f *failingKV) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	if f.fails("Put") {
		return 0, errInjected
	}
	return f.KeyValue.Put(ctx, key, value)
}

func (f *failingKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if f.fails("Create") {
		return 0, errInjected
	}
	return f.KeyValue.Create(ctx, key, value, opts...)
}

func (f *failingKV) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	if f.fails("Update") {
		return 0, errInjected
	}
	return f.KeyValue.Update(ctx, key, value, last)
}

func (f *failingKV) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if f.fails("Delete") {
		return errInjected
	}
	return f.KeyValue.Delete(ctx, key, opts...)
}

func (f *failingKV) Purge(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if f.fails("Purge") {
		return errInjected
	}
	return f.KeyValue.Purge(ctx, key, opts...)
}

func (f *failingKV) Keys(ctx context.Context, opts ...jetstream.WatchOpt) ([]string, error) {
	if f.fails("Keys") {
		return nil, errInjected
	}
	return f.KeyValue.Keys(ctx, opts...)
}

func (f *failingKV) ListKeys(ctx context.Context, opts ...jetstream.WatchOpt) (jetstream.KeyLister, error) {
	if f.fails("ListKeys") {
		return nil, errInjected
	}
	return f.KeyValue.ListKeys(ctx, opts...)
}

// fakeEIPService answers only the EIP calls a test configures; any other
// method panics through the nil embedded interface.
type fakeEIPService struct {
	ec2eip.EIPService

	mu sync.Mutex

	allocateOut *ec2.AllocateAddressOutput
	allocateErr error

	associateOut *ec2.AssociateAddressOutput
	associateErr error

	disassociateErr error
	releaseErr      error

	released      []string
	disassociated []string
}

func (f *fakeEIPService) AllocateAddress(context.Context, *ec2.AllocateAddressInput, string) (*ec2.AllocateAddressOutput, error) {
	if f.allocateErr != nil {
		return nil, f.allocateErr
	}
	return f.allocateOut, nil
}

func (f *fakeEIPService) AssociateAddress(context.Context, *ec2.AssociateAddressInput, string) (*ec2.AssociateAddressOutput, error) {
	if f.associateErr != nil {
		return nil, f.associateErr
	}
	if f.associateOut != nil {
		return f.associateOut, nil
	}
	return &ec2.AssociateAddressOutput{}, nil
}

func (f *fakeEIPService) DisassociateAddress(_ context.Context, in *ec2.DisassociateAddressInput, _ string) (*ec2.DisassociateAddressOutput, error) {
	f.mu.Lock()
	if in != nil && in.AssociationId != nil {
		f.disassociated = append(f.disassociated, *in.AssociationId)
	}
	f.mu.Unlock()
	if f.disassociateErr != nil {
		return nil, f.disassociateErr
	}
	return &ec2.DisassociateAddressOutput{}, nil
}

func (f *fakeEIPService) ReleaseAddress(_ context.Context, in *ec2.ReleaseAddressInput, _ string) (*ec2.ReleaseAddressOutput, error) {
	f.mu.Lock()
	if in != nil && in.AllocationId != nil {
		f.released = append(f.released, *in.AllocationId)
	}
	f.mu.Unlock()
	if f.releaseErr != nil {
		return nil, f.releaseErr
	}
	return &ec2.ReleaseAddressOutput{}, nil
}

// fakeAllocator is an external.Allocator whose Release returns releaseErr and
// records each release.
type fakeAllocator struct {
	mu sync.Mutex

	allocateAddr netip.Addr
	allocateErr  error
	releaseErr   error

	releases []netip.Addr
}

var _ external.Allocator = (*fakeAllocator)(nil)

func (f *fakeAllocator) Allocate(context.Context, external.AllocateRequest) (netip.Addr, error) {
	return f.allocateAddr, f.allocateErr
}

func (f *fakeAllocator) Release(_ context.Context, _ string, ip netip.Addr, _ string) error {
	f.mu.Lock()
	f.releases = append(f.releases, ip)
	f.mu.Unlock()
	return f.releaseErr
}

// erroringPlumber is a recordingPlumber whose SetupTap returns setupErr.
type erroringPlumber struct {
	recordingPlumber

	setupErr error
}

var _ vm.NetworkPlumber = (*erroringPlumber)(nil)

func (p *erroringPlumber) SetupTap(vm.TapSpec) error { return p.setupErr }

// hookVolumeMounter is a vm.VolumeMounter whose Mount runs onMount, letting a
// test set the instance status so launch returns before starting QEMU.
type hookVolumeMounter struct {
	onMount  func(*vm.VM)
	mountErr error
}

var _ vm.VolumeMounter = (*hookVolumeMounter)(nil)

func (m *hookVolumeMounter) Mount(_ context.Context, v *vm.VM) error {
	if m.onMount != nil {
		m.onMount(v)
	}
	return m.mountErr
}

func (m *hookVolumeMounter) Unmount(context.Context, *vm.VM) error         { return nil }
func (m *hookVolumeMounter) Abandon(context.Context, *vm.VM, string) error { return nil }
func (m *hookVolumeMounter) MountOne(context.Context, string, *viperblocklegacyv1.EBSRequest) error {
	return nil
}
func (m *hookVolumeMounter) UnmountOne(context.Context, string, viperblocklegacyv1.EBSRequest) error {
	return nil
}

// noReplyMsg builds a message with no reply subject, so msg.Respond fails
// and the handler's "respond failed" branch runs.
func noReplyMsg(subject string, data []byte) *nats.Msg {
	return &nats.Msg{Subject: subject, Data: data}
}
