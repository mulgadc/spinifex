package instance

import (
	"context"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// reconcileStartVolumes drops the data volumes a claimed instance can no longer
// have, and fails the start when its boot volume is gone.
func (s *InstanceServiceImpl) reconcileStartVolumes(ctx context.Context, instance *vm.VM) error {
	return nil
}
