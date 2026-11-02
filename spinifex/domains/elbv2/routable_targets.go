package elbv2

// routableTargets returns the targets a load balancer may send traffic to.
// Draining, unhealthy and IP-less targets are excluded; when that leaves none,
// every non-draining target with an IP is returned (fail open, as AWS does).
func routableTargets(targets []Target) []Target {
	var healthy, eligible []Target
	for _, t := range targets {
		if t.PrivateIP == "" || t.HealthState == TargetHealthDraining {
			continue
		}
		eligible = append(eligible, t)
		if t.HealthState != TargetHealthUnhealthy {
			healthy = append(healthy, t)
		}
	}
	if len(healthy) > 0 {
		return healthy
	}
	return eligible
}
