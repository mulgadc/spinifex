package handlers_eks

import (
	"errors"
	"log/slog"
	"time"
)

// applyAddonStatusReport CASes the AddonRecord named by an on-VM addon-sync
// delivery report onto its next AWS-visible status. It is a no-op when the
// report names an unknown add-on (DeleteAddon already removed the record) or
// when the status would not change, so a steady stream of "applied"/"ready"
// reports is cheap and idempotent.
func (r *ClusterReconciler) applyAddonStatusReport(report AddonStatusReport) {
	if report.Addon == "" {
		return
	}
	now := time.Now().UTC()
	_, err := casUpdateAddon(r.acctKV, r.clusterName, report.Addon, func(rec *AddonRecord) bool {
		next, changed := nextAddonStatus(rec.Status, report.Phase)
		// Surface the agent's message as the add-on health on failure, and clear
		// it once the add-on recovers to ACTIVE.
		health := rec.Health
		switch report.Phase {
		case AddonPhaseFailed:
			health = report.Message
		case AddonPhaseReady:
			health = ""
		}
		if !changed && health == rec.Health {
			return false
		}
		rec.Status = next
		rec.Health = health
		rec.ModifiedAt = now
		return true
	})
	if err != nil {
		if errors.Is(err, ErrAddonNotFound) {
			// Record gone (DeleteAddon) — the agent will GC the rendered manifest
			// on its next sync; nothing to update.
			return
		}
		slog.Warn("ClusterReconciler: addon status CAS failed",
			"cluster", r.clusterName, "addon", report.Addon, "phase", report.Phase, "err", err)
	}
}

// nextAddonStatus maps a VM-observed delivery phase onto the next AWS-visible
// AddonStatus given the current status, returning the next status and whether it
// changed. The transitions mirror AWS EKS add-on semantics:
//
//   - ready  → ACTIVE (the add-on's workloads rolled out).
//   - applied → no status change: the manifest landed but pods are not yet
//     Ready, so the record stays CREATING/UPDATING until a ready report.
//   - failed → DEGRADED if the add-on was healthy (ACTIVE), else CREATE_FAILED
//     (it never reached ACTIVE). Already-terminal failure states are sticky.
func nextAddonStatus(cur AddonStatus, phase AddonDeliveryPhase) (AddonStatus, bool) {
	switch phase {
	case AddonPhaseReady:
		if cur == AddonStatusActive {
			return cur, false
		}
		return AddonStatusActive, true
	case AddonPhaseFailed:
		switch cur {
		case AddonStatusActive:
			return AddonStatusDegraded, true
		case AddonStatusDegraded, AddonStatusCreateFailed:
			return cur, false
		default:
			return AddonStatusCreateFailed, true
		}
	default:
		// AddonPhaseApplied and any unknown phase are informational only.
		return cur, false
	}
}
