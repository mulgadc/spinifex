package elbv2

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRoutableTargets(t *testing.T) {
	tgt := func(id, ip, state string) Target {
		return Target{Id: id, PrivateIP: ip, HealthState: state}
	}
	ids := func(ts []Target) []string {
		var out []string
		for _, x := range ts {
			out = append(out, x.Id)
		}
		return out
	}

	tests := []struct {
		name    string
		targets []Target
		want    []string
	}{
		{"all healthy", []Target{tgt("a", "10.0.0.1", TargetHealthHealthy), tgt("b", "10.0.0.2", TargetHealthHealthy)}, []string{"a", "b"}},
		{"one unhealthy is dropped", []Target{tgt("a", "10.0.0.1", TargetHealthHealthy), tgt("b", "10.0.0.2", TargetHealthUnhealthy)}, []string{"a"}},
		{"initial stays routable", []Target{tgt("a", "10.0.0.1", TargetHealthInitial), tgt("b", "10.0.0.2", TargetHealthUnhealthy)}, []string{"a"}},
		{"draining is dropped", []Target{tgt("a", "10.0.0.1", TargetHealthHealthy), tgt("b", "10.0.0.2", TargetHealthDraining)}, []string{"a"}},
		{"no ip is dropped", []Target{tgt("a", "10.0.0.1", TargetHealthHealthy), tgt("b", "", TargetHealthHealthy)}, []string{"a"}},
		{"mixed", []Target{
			tgt("a", "10.0.0.1", TargetHealthHealthy),
			tgt("b", "10.0.0.2", TargetHealthUnhealthy),
			tgt("c", "10.0.0.3", TargetHealthInitial),
			tgt("d", "10.0.0.4", TargetHealthDraining),
			tgt("e", "", TargetHealthHealthy),
		}, []string{"a", "c"}},
		{"all unhealthy fails open but keeps draining and no-ip out", []Target{
			tgt("a", "10.0.0.1", TargetHealthUnhealthy),
			tgt("b", "10.0.0.2", TargetHealthUnhealthy),
			tgt("c", "10.0.0.3", TargetHealthDraining),
			tgt("d", "", TargetHealthUnhealthy),
		}, []string{"a", "b"}},
		{"all draining is empty", []Target{tgt("a", "10.0.0.1", TargetHealthDraining)}, nil},
		{"empty group", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ids(routableTargets(tc.targets)))
		})
	}
}
