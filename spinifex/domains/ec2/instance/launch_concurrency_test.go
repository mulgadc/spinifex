package instance

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
)

// rendezvousProvider holds each root volume create until a second create
// arrives, so a create only succeeds when two launches overlap.
type rendezvousProvider struct {
	*ebsprovider.MemoryProvider

	arrivals   atomic.Int32
	both       chan struct{}
	overlapped atomic.Int32
}

func (p *rendezvousProvider) CreateVolume(ctx context.Context, req ebsprovider.CreateVolumeRequest) (*ebsprovider.Volume, error) {
	if p.arrivals.Add(1) == 2 {
		close(p.both)
	}
	select {
	case <-p.both:
		p.overlapped.Add(1)
		return p.MemoryProvider.CreateVolume(ctx, req)
	case <-time.After(5 * time.Second):
		return nil, errors.New("no second launch reached its volume create")
	}
}

// The instances of one batch launch at the same time: the first instance's
// volume preparation is still running when the second one starts its own.
func TestLaunchRunInstances_BatchLaunchesConcurrently(t *testing.T) {
	provider := &rendezvousProvider{
		MemoryProvider: ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{CrashConsistentSnapshot: true}),
		both:           make(chan struct{}),
	}
	seedProviderSnapshot(t, provider.MemoryProvider, "vol-origin", "snap-source")

	mgr := vm.NewManager()
	mgr.SetDeps(vm.Deps{VolumeMounter: refusingMounter{}})
	store := objectstore.NewMemoryObjectStore()
	cfg := &config.Config{WalDir: t.TempDir(), AZ: testRootAZ, Predastore: config.PredastoreConfig{Bucket: testRootBucket}}
	svc := NewInstanceServiceImpl(cfg, nil, nil, store, mgr, nil, nil)
	svc.SetEBSProvider(provider)
	svc.SetRunInstancesDeps(rootVolumeAMILoader(), nil, nil, nil)

	instances := []*vm.VM{
		{ID: "i-0000000000000001a", AccountID: testRootAccount, Status: vm.StatePending, Instance: &ec2.Instance{}},
		{ID: "i-0000000000000001b", AccountID: testRootAccount, Status: vm.StatePending, Instance: &ec2.Instance{}},
	}
	for _, v := range instances {
		mgr.Insert(v)
	}

	input := &ec2.RunInstancesInput{ImageId: aws.String("ami-1"), InstanceType: aws.String("t3.nano")}
	svc.LaunchRunInstances(t.Context(), instances, input, &ec2.InstanceTypeInfo{InstanceType: aws.String("t3.nano")})

	assert.Equal(t, int32(2), provider.overlapped.Load(),
		"both root volume creates must run at the same time; a serial launch loop lets only the second one through")
}
