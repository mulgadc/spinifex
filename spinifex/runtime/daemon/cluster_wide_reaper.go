package daemon

import (
	"context"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// sweeper is a domain's reaper body, which names its class and sweeps but
// leaves its scheduling scope to the runtime that registers it.
type sweeper interface {
	Class() string
	Sweep(ctx context.Context) (int, error)
}

// clusterWideReaper runs a domain sweeper once per cluster, on the leader.
type clusterWideReaper struct {
	sweeper
}

var _ vm.Reaper = clusterWideReaper{}

func (clusterWideReaper) Scope() vm.ReaperScope { return vm.ScopeClusterWide }
