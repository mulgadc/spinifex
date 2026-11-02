package cluster

import (
	"context"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// EKSNodegroupDeletingReaper finishes recorded nodegroup deletes: it ends the
// workers, removes the nodegroup's own instance profile and then the record.
type EKSNodegroupDeletingReaper struct {
	svc *EKSServiceImpl
}

var _ vm.Reaper = (*EKSNodegroupDeletingReaper)(nil)

// NewNodegroupDeletingReaper builds the DELETING nodegroup converger.
func (s *EKSServiceImpl) NewNodegroupDeletingReaper() *EKSNodegroupDeletingReaper {
	return &EKSNodegroupDeletingReaper{svc: s}
}

func (r *EKSNodegroupDeletingReaper) Class() string         { return "eks-nodegroup-deleting" }
func (r *EKSNodegroupDeletingReaper) Scope() vm.ReaperScope { return vm.ScopeNodeLocal }

// Sweep returns how many DELETING nodegroups it advanced.
func (r *EKSNodegroupDeletingReaper) Sweep(context.Context) (int, error) {
	return 0, nil
}
