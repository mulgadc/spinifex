//test:in-package — it swaps the manager's volume mounter, an unexported dependency.
package instance

import (
	"context"
	"errors"
	"sync"
	"testing"

	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mountRecorder notes which volumes a launch asked to mount and then fails the
// launch, which is the earliest point that shows what the start would attach.
type mountRecorder struct {
	refusingMounter
	mu      sync.Mutex
	mounted []string
}

func (r *mountRecorder) Mount(_ context.Context, v *vm.VM) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, req := range v.EBSRequests.Requests {
		r.mounted = append(r.mounted, req.Name)
	}
	return errors.New("stop after recording")
}

func (r *mountRecorder) MountOne(context.Context, string, *viperblocklegacyv1.EBSRequest) error {
	return nil
}

// A volume detached while the instance was stopped must not come back when the
// instance is started: the start path reads the same record the detach edited.
func TestStartStoppedInstance_DoesNotReattachAVolumeDetachedWhileStopped(t *testing.T) {
	id := "i-detach-then-start"
	stopped := stoppedWithVolumes("acc", id)
	stopped.InstanceType = "t3.micro"
	f := newStartFixture(t, stopped)
	rec := &mountRecorder{}
	f.svc.vmMgr.SetDeps(vm.Deps{VolumeMounter: rec})

	_, err := f.svc.DetachVolumeFromStopped(context.Background(), stoppedDetachInput(id, "vol-data"), "acc")
	require.NoError(t, err)
	require.Error(t, f.start(t, id), "the recorder fails the launch after the mount")

	assert.Equal(t, []string{"vol-root", "vol-other"}, rec.mounted,
		"the detached volume is not part of the start")
}
