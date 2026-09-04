//test:in-package — synthesize is unexported, and these tests reuse the NATS
// fan-out harness the package's existing in-package tests already define.

package gateway_ec2_instance

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/instancecache"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

const synthAccount = "123456789012"

// fakeRecords is a cache stand-in: a fixed record set plus a readiness flag,
// so a test can assert the cold-cache case as well as the warm one.
type fakeRecords struct {
	vms   []*vm.VM
	ready bool
}

func (f fakeRecords) List(context.Context, string) ([]*vm.VM, bool) { return f.vms, f.ready }

// fakeLiveness answers per node, defaulting to Unknown for anything unlisted.
type fakeLiveness struct {
	states map[string]instancecache.NodeState
}

func (f fakeLiveness) State(_ context.Context, node string) instancecache.NodeState {
	return f.states[node]
}

// cachedVM builds a record as the cache would hand it over.
func cachedVM(id, node, az string, state vm.InstanceState) *vm.VM {
	launched := time.Now().Add(-time.Hour)
	return &vm.VM{
		ID:        id,
		AccountID: synthAccount,
		Status:    state,
		LastNode:  node,
		AZ:        az,
		Instance:  &ec2.Instance{InstanceId: aws.String(id), LaunchTime: &launched},
	}
}

// statusByID indexes an output for assertions.
func statusByID(out *ec2.DescribeInstanceStatusOutput) map[string]*ec2.InstanceStatus {
	byID := make(map[string]*ec2.InstanceStatus, len(out.InstanceStatuses))
	for _, s := range out.InstanceStatuses {
		byID[aws.StringValue(s.InstanceId)] = s
	}
	return byID
}

// answerWith subscribes a single node that returns the given statuses.
func answerWith(t *testing.T, nc *nats.Conn, statuses ...*ec2.InstanceStatus) {
	t.Helper()
	_, err := nc.Subscribe("ec2.DescribeInstanceStatus", func(msg *nats.Msg) {
		respondJSON(t, msg, &ec2.DescribeInstanceStatusOutput{InstanceStatuses: statuses})
	})
	require.NoError(t, err)
}

// An instance whose node stopped answering stays in the output and reports
// impaired, rather than vanishing as it does with fan-out alone.
func TestSynthesis_StaleNodeInstanceReportsImpaired(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	answerWith(t, nc, runningStatus("i-live", "az-a"))

	synth := StatusSynthesis{
		Records: fakeRecords{ready: true, vms: []*vm.VM{
			cachedVM("i-live", "node-a", "az-a", vm.StateRunning),
			cachedVM("i-orphan", "node-dead", "az-b", vm.StateRunning),
		}},
		Liveness: fakeLiveness{states: map[string]instancecache.NodeState{
			"node-a":    instancecache.NodeLive,
			"node-dead": instancecache.NodeStale,
		}},
	}

	out, err := DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, nc, 1, synthAccount, "az-a", synth)
	require.NoError(t, err)

	byID := statusByID(out)
	require.Len(t, byID, 2, "the dead node's instance must not disappear")
	require.Equal(t, "impaired", aws.StringValue(byID["i-orphan"].SystemStatus.Status))
	require.Equal(t, "az-b", aws.StringValue(byID["i-orphan"].AvailabilityZone),
		"a synthesised frame carries the record's AZ, not this gateway's")
}

// A frame an answering node returned is authoritative and must survive
// unchanged, even when that node is also reported stale. Two guards deliver
// this and either alone suffices, so see TestSynthesis_SkipsCoveredInstances
// for the one that pins the real mechanism.
func TestSynthesis_LiveFrameWinsUnchanged(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	// The node reports its own memory pressure, which no cached record can
	// reconstruct. Synthesis must not overwrite it.
	nodeFrame := runningStatus("i-001", "az-a")
	nodeFrame.SystemStatus.Status = aws.String("impaired")
	answerWith(t, nc, nodeFrame)

	synth := StatusSynthesis{
		Records:  fakeRecords{ready: true, vms: []*vm.VM{cachedVM("i-001", "node-a", "az-a", vm.StateRunning)}},
		Liveness: fakeLiveness{states: map[string]instancecache.NodeState{"node-a": instancecache.NodeStale}},
	}

	out, err := DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, nc, 1, synthAccount, "az-a", synth)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)
	require.Equal(t, "impaired", aws.StringValue(out.InstanceStatuses[0].SystemStatus.Status))
	require.Equal(t, "ok", aws.StringValue(out.InstanceStatuses[0].InstanceStatus.Status),
		"the node's own frame must be used unchanged")
}

// A live node that did not report an instance excluded it deliberately.
// Synthesising over that would override the only party that can see it.
func TestSynthesis_LiveNodeSilenceIsNotBackfilled(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	answerWith(t, nc, runningStatus("i-001", "az-a"))

	synth := StatusSynthesis{
		Records: fakeRecords{ready: true, vms: []*vm.VM{
			cachedVM("i-001", "node-a", "az-a", vm.StateRunning),
			cachedVM("i-gone", "node-a", "az-a", vm.StateRunning),
		}},
		Liveness: fakeLiveness{states: map[string]instancecache.NodeState{"node-a": instancecache.NodeLive}},
	}

	out, err := DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, nc, 1, synthAccount, "az-a", synth)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1, "a live node's omission is its own answer")
}

// An unreadable heartbeat store degrades only the synthesised frames to
// insufficient-data. Blanking every frame would hide the genuine health of
// every answering node behind a shrug.
func TestSynthesis_UnknownLivenessDegradesOnlySynthesised(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	nodeFrame := runningStatus("i-live", "az-a")
	nodeFrame.SystemStatus.Status = aws.String("impaired")
	answerWith(t, nc, nodeFrame)

	synth := StatusSynthesis{
		Records: fakeRecords{ready: true, vms: []*vm.VM{
			cachedVM("i-live", "node-a", "az-a", vm.StateRunning),
			cachedVM("i-orphan", "node-b", "az-a", vm.StateRunning),
		}},
		// Empty map: every lookup answers Unknown, as an unreadable store does.
		Liveness: fakeLiveness{states: map[string]instancecache.NodeState{}},
	}

	out, err := DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, nc, 1, synthAccount, "az-a", synth)
	require.NoError(t, err)

	byID := statusByID(out)
	require.Equal(t, "impaired", aws.StringValue(byID["i-live"].SystemStatus.Status),
		"a live node's frame must survive an unreadable heartbeat store")
	require.Equal(t, "insufficient-data", aws.StringValue(byID["i-orphan"].SystemStatus.Status),
		"not knowing a host is healthy is not knowing it is not")
}

// A stopped instance on a stale node stays not-applicable. Node liveness says
// nothing about an instance that is not meant to be running, and it must not
// appear in a default request at all.
func TestSynthesis_StoppedOnStaleNodeStaysOutAndNotApplicable(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	answerWith(t, nc)

	records := fakeRecords{ready: true, vms: []*vm.VM{
		cachedVM("i-stopped", "node-dead", "az-a", vm.StateStopped),
	}}
	liveness := fakeLiveness{states: map[string]instancecache.NodeState{"node-dead": instancecache.NodeStale}}
	synth := StatusSynthesis{Records: records, Liveness: liveness}

	out, err := DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, nc, 1, synthAccount, "az-a", synth)
	require.NoError(t, err)
	require.Empty(t, out.InstanceStatuses, "a default request is running-only")

	all := &ec2.DescribeInstanceStatusInput{IncludeAllInstances: aws.Bool(true)}
	out, err = DescribeInstanceStatus(context.Background(), all, nc, 1, synthAccount, "az-a", synth)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)
	require.Equal(t, "not-applicable", aws.StringValue(out.InstanceStatuses[0].SystemStatus.Status),
		"a stopped instance is not impaired because its host is unreachable")
}

// Synthesis honours the request rather than dumping the cache: an instance-ID
// filter excludes everything it did not ask for.
func TestSynthesis_HonoursInstanceIDFilter(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	answerWith(t, nc)

	synth := StatusSynthesis{
		Records: fakeRecords{ready: true, vms: []*vm.VM{
			cachedVM("i-asked", "node-dead", "az-a", vm.StateRunning),
			cachedVM("i-notasked", "node-dead", "az-a", vm.StateRunning),
		}},
		Liveness: fakeLiveness{states: map[string]instancecache.NodeState{"node-dead": instancecache.NodeStale}},
	}

	input := &ec2.DescribeInstanceStatusInput{InstanceIds: []*string{aws.String("i-asked")}}
	out, err := DescribeInstanceStatus(context.Background(), input, nc, 1, synthAccount, "az-a", synth)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1)
	require.Equal(t, "i-asked", aws.StringValue(out.InstanceStatuses[0].InstanceId))
}

// A cache that has not finished its first whole-set sync cannot support a
// claim about what is missing, so it contributes nothing.
func TestSynthesis_ColdCacheContributesNothing(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	answerWith(t, nc)

	synth := StatusSynthesis{
		Records:  fakeRecords{ready: false, vms: []*vm.VM{cachedVM("i-orphan", "node-dead", "az-a", vm.StateRunning)}},
		Liveness: fakeLiveness{states: map[string]instancecache.NodeState{"node-dead": instancecache.NodeStale}},
	}

	out, err := DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, nc, 1, synthAccount, "az-a", synth)
	require.NoError(t, err)
	require.Empty(t, out.InstanceStatuses)
}

// The covered set is what actually keeps a live node's frame authoritative,
// so it is asserted directly. Through the full handler it is redundant with
// dedupStatuses being first-writer-wins, and a test there passes with either
// guard alone — which would let both rot until they broke together.
func TestSynthesis_SkipsCoveredInstances(t *testing.T) {
	t.Parallel()

	synth := StatusSynthesis{
		Records: fakeRecords{ready: true, vms: []*vm.VM{
			cachedVM("i-covered", "node-dead", "az-a", vm.StateRunning),
			cachedVM("i-uncovered", "node-dead", "az-a", vm.StateRunning),
		}},
		Liveness: fakeLiveness{states: map[string]instancecache.NodeState{"node-dead": instancecache.NodeStale}},
	}

	got := synth.synthesize(context.Background(), &ec2.DescribeInstanceStatusInput{}, synthAccount,
		map[string]bool{"i-covered": true}, fanoutResponders{})

	require.Len(t, got, 1, "an instance a node answered for must not be synthesised at all")
	require.Equal(t, "i-uncovered", aws.StringValue(got[0].InstanceId))
}

// A node that never answers this fan-out at all, but whose heartbeat has not
// yet gone stale, must not make its instances vanish. This is the regression
// the responder-set fix exists for: heartbeat age alone cannot tell "excluded
// on purpose" from "didn't answer this request".
func TestSynthesis_SilentButLiveNodeShowsInsufficientData(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	live, err := json.Marshal(&ec2.DescribeInstanceStatusOutput{
		InstanceStatuses: []*ec2.InstanceStatus{runningStatus("i-live", "az-a")},
	})
	require.NoError(t, err)
	subscribeAsNode(t, nc, "ec2.DescribeInstanceStatus", "node-live", live)
	// node-silent never subscribes: it is expected but does not answer.

	synth := StatusSynthesis{
		Records: fakeRecords{ready: true, vms: []*vm.VM{
			cachedVM("i-live", "node-live", "az-a", vm.StateRunning),
			cachedVM("i-orphan", "node-silent", "az-a", vm.StateRunning),
		}},
		Liveness: fakeLiveness{states: map[string]instancecache.NodeState{
			"node-live":   instancecache.NodeLive,
			"node-silent": instancecache.NodeLive,
		}},
	}

	out, err := DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, nc, 2, synthAccount, "az-a", synth)
	require.NoError(t, err)

	byID := statusByID(out)
	require.Len(t, byID, 2, "a silent-but-live node's instance must not disappear")
	require.Equal(t, "insufficient-data", aws.StringValue(byID["i-orphan"].SystemStatus.Status))
}

// A node that answered the fan-out successfully and chose not to report an
// instance is respected even when its heartbeat reads stale: the omission is
// this request's own answer, not a symptom for heartbeat age to reinterpret.
func TestSynthesis_SuccessResponderOmissionNotBackfilledEvenWhenStale(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	data, err := json.Marshal(&ec2.DescribeInstanceStatusOutput{
		InstanceStatuses: []*ec2.InstanceStatus{runningStatus("i-present", "az-a")},
	})
	require.NoError(t, err)
	subscribeAsNode(t, nc, "ec2.DescribeInstanceStatus", "node-a", data)

	synth := StatusSynthesis{
		Records: fakeRecords{ready: true, vms: []*vm.VM{
			cachedVM("i-present", "node-a", "az-a", vm.StateRunning),
			cachedVM("i-gone", "node-a", "az-a", vm.StateRunning),
		}},
		Liveness: fakeLiveness{states: map[string]instancecache.NodeState{"node-a": instancecache.NodeStale}},
	}

	out, err := DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, nc, 1, synthAccount, "az-a", synth)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1, "the answering node's own omission must survive even a stale heartbeat")
	require.Equal(t, "i-present", aws.StringValue(out.InstanceStatuses[0].InstanceId))
}

// A node that answered with an error envelope did not run its own selection
// logic, so its omission is not deliberate: its instances are synthesised.
func TestSynthesis_ErrorResponderInstancesAreSynthesised(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)

	subscribeAsNode(t, nc, "ec2.DescribeInstanceStatus", "node-a", utils.GenerateErrorPayload("InvalidParameterValue"))

	synth := StatusSynthesis{
		Records: fakeRecords{ready: true, vms: []*vm.VM{
			cachedVM("i-orphan", "node-a", "az-a", vm.StateRunning),
		}},
		Liveness: fakeLiveness{states: map[string]instancecache.NodeState{"node-a": instancecache.NodeLive}},
	}

	out, err := DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, nc, 1, synthAccount, "az-a", synth)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1, "an error envelope is not a deliberate omission")
	require.Equal(t, "i-orphan", aws.StringValue(out.InstanceStatuses[0].InstanceId))
	require.Equal(t, "insufficient-data", aws.StringValue(out.InstanceStatuses[0].SystemStatus.Status))
}

// When any frame cannot be attributed to a node, the responder set for the
// whole fan-out is untrustworthy, so synthesis falls back to the pre-fix
// heartbeat-only rule: a live node's silence is treated as deliberate again.
func TestSynthesis_UnidentifiedFrameFallsBackToHeartbeatOnly(t *testing.T) {
	t.Parallel()
	_, nc := startTestNATSServer(t)
	answerWith(t, nc, runningStatus("i-live", "az-a"))

	synth := StatusSynthesis{
		Records: fakeRecords{ready: true, vms: []*vm.VM{
			cachedVM("i-live", "node-live", "az-a", vm.StateRunning),
			cachedVM("i-orphan", "node-silent", "az-a", vm.StateRunning),
		}},
		Liveness: fakeLiveness{states: map[string]instancecache.NodeState{
			"node-live":   instancecache.NodeLive,
			"node-silent": instancecache.NodeLive,
		}},
	}

	out, err := DescribeInstanceStatus(context.Background(), &ec2.DescribeInstanceStatusInput{}, nc, 1, synthAccount, "az-a", synth)
	require.NoError(t, err)
	require.Len(t, out.InstanceStatuses, 1,
		"without responder identity, a live node's instance is skipped by the fallback rule")
}
