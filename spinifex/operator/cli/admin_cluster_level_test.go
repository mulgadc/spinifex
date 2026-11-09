package cli

import (
	"bytes"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/stretchr/testify/assert"
)

func answered(node string, build int, rev uint64, inFlight int) featurelevel.NodeStatus {
	return featurelevel.NodeStatus{Node: node, Reply: &featurelevel.StatusReply{Node: node, Build: build, Revision: rev, InFlightTotal: inFlight}}
}

func TestRenderLevelStatus_HeldAndRollbackStatesAndASilentMember(t *testing.T) {
	t.Parallel()
	members := []memberLevelReport{
		{Node: "n1", HasHeartbeat: true, BeatLevel: 1, BeatAge: 4 * time.Second, Status: answered("n1", 1, 9, 2)},
		{Node: "n2", HeartbeatNote: "no heartbeat", Status: featurelevel.NodeStatus{Node: "n2", Reason: "no-reply"}},
	}

	var plain bytes.Buffer
	renderLevelStatus(&plain, featurelevel.Record{Level: 1}, 9, 1, members)
	out := plain.String()
	assert.Contains(t, out, "Feature level: 1 (this build supports 1, record revision 9)")
	assert.Contains(t, out, "Hold:          none")
	assert.Contains(t, out, "Rollback:      none")
	assert.Contains(t, out, "n1: heartbeat level 1, age 4s; status: build 1, revision 9, in flight 2")
	assert.Contains(t, out, "n2: no heartbeat; status: none (no-reply)")

	var held bytes.Buffer
	renderLevelStatus(&held, featurelevel.Record{Level: 0, Hold: featurelevel.Hold{Held: true, Level: 0}}, 10, 1, members)
	assert.Contains(t, held.String(), "Hold:          held at 0", "a hold at level 0 is shown as a hold")
	assert.Contains(t, held.String(), "spx admin cluster release-hold")

	var rolling bytes.Buffer
	renderLevelStatus(&rolling, featurelevel.Record{
		Level: 2, Hold: featurelevel.Hold{Held: true, Level: 1}, Rollback: &featurelevel.Rollback{Target: 1},
	}, 11, 2, members)
	assert.Contains(t, rolling.String(), "Rollback:      to 1 in progress")
	assert.Contains(t, rolling.String(), "Hold:          held at 1")
}
