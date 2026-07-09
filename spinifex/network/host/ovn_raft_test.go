package host

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const clusterStatusLeader = `0f21
Name: OVN_Northbound
Cluster ID: c8f5 (c8f5b3e0-...)
Server ID: 0f21 (0f21a1b2-...)
Address: tcp:10.0.0.1:6643
Status: cluster member
Role: leader
Term: 3
Leader: self
Vote: self

Servers:
    0f21 (0f21 at tcp:10.0.0.1:6643) (self)
    8a8f (8a8f at tcp:10.0.0.2:6643)
    abcd (abcd at tcp:10.0.0.3:6643)
`

const clusterStatusFollower = `8a8f
Name: OVN_Southbound
Cluster ID: c8f5 (c8f5b3e0-...)
Server ID: 8a8f (8a8fc3d4-...)
Address: tcp:10.0.0.2:6644
Status: cluster member
Role: follower
Term: 3
Leader: 0f21
Vote: 0f21

Servers:
    0f21 (0f21 at tcp:10.0.0.1:6644)
    8a8f (8a8f at tcp:10.0.0.2:6644) (self)
    abcd (abcd at tcp:10.0.0.3:6644)
`

// A standalone (non-clustered) DB reports no Role: header and no Raft servers.
const clusterStatusStandalone = `Name: OVN_Northbound
Status: standalone
`

func TestParseClusterStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		out         string
		wantServers int
		wantRole    string
	}{
		{"leader", clusterStatusLeader, 3, "leader"},
		{"follower", clusterStatusFollower, 3, "follower"},
		{"standalone yields empty role", clusterStatusStandalone, 0, ""},
		{"empty output", "", 0, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			servers, role := ParseClusterStatus(tt.out)
			assert.Equal(t, tt.wantServers, servers)
			assert.Equal(t, tt.wantRole, role)
		})
	}
}
