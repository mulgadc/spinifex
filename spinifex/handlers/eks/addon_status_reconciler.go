package handlers_eks

import (
	"context"
	eksv1 "github.com/mulgadc/spinifex/contracts/eks/v1"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/domains/eks/addon"
	"github.com/nats-io/nats.go/jetstream"
)

// addonReports applies one guest add-on delivery report to the add-on's record.
type addonReports interface {
	ApplyReport(ctx context.Context, kv jetstream.KeyValue, cluster string, report addon.Report) error
}

var _ addonReports = (*addon.Owner)(nil)

// applyAddonStatusReport hands a guest delivery report to the add-on owner,
// which decides the status transition; failures are logged and the next
// level-triggered report retries.
func (r *ClusterReconciler) applyAddonStatusReport(ctx context.Context, report eksv1.AddonStatusReport) {
	err := r.addonReports.ApplyReport(ctx, r.acctKV, r.clusterName, addon.Report{
		Addon:   report.Addon,
		Phase:   addon.Phase(report.Phase),
		Message: report.Message,
	})
	if err != nil {
		slog.Warn("ClusterReconciler: addon status CAS failed",
			"cluster", r.clusterName, "addon", report.Addon, "phase", report.Phase, "err", err)
	}
}
