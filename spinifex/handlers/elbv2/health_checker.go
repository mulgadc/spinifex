package handlers_elbv2

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/mulgadc/spinifex/spinifex/lbagent"
)

// healthChecker processes target health reports from LB agents pushed via
// LBAgentHeartbeat, mapping backend statuses back to targets in the store.
type healthChecker struct {
	store *Store

	mu       sync.Mutex
	counters map[string]*targetCounter // key: "tgID:targetId:port"
}

// targetCounter tracks consecutive pass/fail counts for threshold logic.
type targetCounter struct {
	consecutiveHealthy   int64
	consecutiveUnhealthy int64
}

func newHealthChecker(store *Store) *healthChecker {
	return &healthChecker{
		store:    store,
		counters: make(map[string]*targetCounter),
	}
}

// handleHealthReport unmarshals a JSON-encoded health report and processes it.
func (hc *healthChecker) handleHealthReport(data []byte) {
	var report lbagent.HealthReport
	if err := json.Unmarshal(data, &report); err != nil {
		slog.Warn("healthChecker: invalid health report", "err", err)
		return
	}

	hc.handleHealthReportDirect(report)
}

// handleHealthReportDirect processes a health report without a JSON round-trip.
func (hc *healthChecker) handleHealthReportDirect(report lbagent.HealthReport) {
	if len(report.Servers) == 0 {
		return
	}

	// Build a map of HAProxy server name → UP/DOWN status
	serverUp := make(map[string]bool, len(report.Servers))
	for _, srv := range report.Servers {
		serverUp[srv.Server] = srv.Status == "UP"
	}

	// Look up target groups for this LB; fall back to all TGs if LBID is missing.
	var tgs []*TargetGroupRecord
	var err error
	if report.LBID != "" {
		tgs, err = hc.store.TargetGroupsForLB(report.LBID)
	} else {
		tgs, err = hc.store.ListTargetGroups()
	}
	if err != nil {
		slog.Warn("healthChecker: failed to list target groups", "lbId", report.LBID, "err", err)
		return
	}

	// Collect changed TGs under the lock; persist outside to avoid holding it across KV I/O.
	hc.mu.Lock()
	var changedTGs []*TargetGroupRecord
	for _, tg := range tgs {
		changed := false
		for i := range tg.Targets {
			target := &tg.Targets[i]

			if target.PrivateIP == "" || target.HealthState == TargetHealthDraining {
				continue
			}

			// HAProxy server name matches the sanitized target ID
			srvName := sanitizeName("srv", target.Id)
			healthy, exists := serverUp[srvName]
			if !exists {
				continue
			}

			port := target.Port
			if port == 0 {
				port = tg.Port
			}

			key := fmt.Sprintf("%s:%s:%d", tg.TargetGroupID, target.Id, port)
			ctr, ok := hc.counters[key]
			if !ok {
				ctr = &targetCounter{}
				hc.counters[key] = ctr
			}

			if healthy {
				ctr.consecutiveHealthy++
				ctr.consecutiveUnhealthy = 0
			} else {
				ctr.consecutiveUnhealthy++
				ctr.consecutiveHealthy = 0
			}

			newState, newDesc := evaluateHealth(target.HealthState, ctr, tg.HealthCheck)
			if newState != target.HealthState {
				slog.Info("Target health changed",
					"targetId", target.Id,
					"from", target.HealthState,
					"to", newState,
				)
				target.HealthState = newState
				target.HealthDesc = newDesc
				changed = true
			}
		}

		if changed {
			changedTGs = append(changedTGs, tg)
		}
	}
	hc.mu.Unlock()

	for _, tg := range changedTGs {
		if err := hc.store.PutTargetGroup(tg); err != nil {
			slog.Error("healthChecker: failed to persist target group", "tgId", tg.TargetGroupID, "err", err)
		}
	}
}

// evaluateHealth applies threshold logic to determine a target's new state.
// From "initial", one healthy probe transitions to healthy (AWS fast-start);
// from "healthy"/"unhealthy", full threshold counts are required.
func evaluateHealth(current string, ctr *targetCounter, cfg HealthCheckConfig) (string, string) {
	healthyThreshold := cfg.HealthyThreshold
	if healthyThreshold == 0 {
		healthyThreshold = DefaultHealthyThreshold
	}
	unhealthyThreshold := cfg.UnhealthyThreshold
	if unhealthyThreshold == 0 {
		unhealthyThreshold = DefaultUnhealthyThreshold
	}

	switch current {
	case TargetHealthInitial:
		if ctr.consecutiveHealthy >= 1 {
			return TargetHealthHealthy, "Target is healthy"
		}
		if ctr.consecutiveUnhealthy >= unhealthyThreshold {
			return TargetHealthUnhealthy, "Health check failed"
		}
		return current, "Target registration is in progress"

	case TargetHealthHealthy:
		if ctr.consecutiveUnhealthy >= unhealthyThreshold {
			return TargetHealthUnhealthy, "Health check failed"
		}
		return current, "Target is healthy"

	case TargetHealthUnhealthy:
		if ctr.consecutiveHealthy >= healthyThreshold {
			return TargetHealthHealthy, "Target is healthy"
		}
		return current, "Health check failed"

	default:
		return current, ""
	}
}

// removeTarget cleans up counters for a deregistered target.
func (hc *healthChecker) removeTarget(tgID, targetId string, port int64) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	delete(hc.counters, fmt.Sprintf("%s:%s:%d", tgID, targetId, port))
}
