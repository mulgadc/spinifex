package host

import "testing"

func TestNeighResolved(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"no entry", "", false},
		{"reachable", "192.168.0.211 dev br-wan lladdr 52:54:00:aa:bb:cc REACHABLE", true},
		{"stale keeps a usable mac", "192.168.0.211 dev br-wan lladdr 52:54:00:aa:bb:cc STALE", true},
		{"delay", "192.168.0.211 dev br-wan lladdr 52:54:00:aa:bb:cc DELAY", true},
		{"incomplete has no mac", "192.168.0.211 dev br-wan  INCOMPLETE", false},
		{"failed", "192.168.0.211 dev br-wan FAILED", false},
		{"failed even with a stale mac", "192.168.0.211 dev br-wan lladdr 52:54:00:aa:bb:cc FAILED", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := neighResolved(tc.out); got != tc.want {
				t.Errorf("neighResolved(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}
