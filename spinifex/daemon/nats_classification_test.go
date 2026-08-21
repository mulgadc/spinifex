package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A daemon error rate is only useful if it counts daemon faults. An unknown
// instance id or a volume already in use is the caller's doing and must not
// land in the same series as a server fault.
func TestOutcomeForErrorSeparatesCallerFromDaemon(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"unknown instance", errors.New(awserrors.ErrorInvalidInstanceIDNotFound), outcomeClientError},
		{"volume in use", errors.New(awserrors.ErrorVolumeInUse), outcomeClientError},
		{"bad parameter", errors.New(awserrors.ErrorInvalidParameterValue), outcomeClientError},
		{"server fault", errors.New(awserrors.ErrorServerInternal), outcomeError},
		{"no recognised code", errors.New("the disk fell over"), outcomeError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, outcomeForError(tc.err))
		})
	}
}

// The NATS and HTTP paths must give the same word to the same failure, or a
// dashboard that spans both counts one service's client errors as faults.
func TestOutcomeForErrorAgreesWithTheHTTPMiddleware(t *testing.T) {
	for _, code := range []string{
		awserrors.ErrorInvalidInstanceIDNotFound,
		awserrors.ErrorVolumeInUse,
		awserrors.ErrorServerInternal,
	} {
		status := awserrors.HTTPStatusForError(errors.New(code))
		assert.Equal(t, otelsetup.OutcomeForStatus(status), outcomeForError(errors.New(code)),
			"%s (%d) must classify the same on both paths", code, status)
	}
}

// An error carrying no AWS code counts against the daemon. Defaulting the other
// way would let an unclassifiable failure disappear into the caller's column.
func TestHTTPStatusForErrorDefaultsToServerFault(t *testing.T) {
	assert.Equal(t, http.StatusInternalServerError,
		awserrors.HTTPStatusForError(errors.New("no code in here")))
}

// levelsLogged runs fn with a capturing logger and returns the levels it wrote.
func levelsLogged(t *testing.T, fn func()) []slog.Level {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	fn()

	var levels []slog.Level
	for line := range bytes.SplitSeq(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rec struct {
			Level string `json:"level"`
		}
		require.NoError(t, json.Unmarshal(line, &rec))
		var lvl slog.Level
		require.NoError(t, lvl.UnmarshalText([]byte(rec.Level)))
		levels = append(levels, lvl)
	}
	return levels
}

// DescribeInstanceAttribute fans out and only the node holding the instance can
// answer; every other node replies InvalidInstanceID.NotFound by design and the
// aggregator drops those. Logging them at ERROR wrote three meaningless lines
// per IMDS fetch on a four-node cluster.
func TestFanOutMissIsNotLoggedAsAnError(t *testing.T) {
	levels := levelsLogged(t, func() {
		logHandlerError(context.Background(), "service call failed",
			"ec2.DescribeInstanceAttribute", errors.New(awserrors.ErrorInvalidInstanceIDNotFound))
	})
	require.Len(t, levels, 1)
	assert.Equal(t, slog.LevelWarn, levels[0],
		"an expected fan-out miss must not be an ERROR line on the nodes that never held the instance")
}

// The demotion above must not swallow a real fault on the same subject.
func TestServerFaultIsStillLoggedAsAnError(t *testing.T) {
	levels := levelsLogged(t, func() {
		logHandlerError(context.Background(), "service call failed",
			"ec2.DescribeInstanceAttribute", errors.New(awserrors.ErrorServerInternal))
	})
	require.Len(t, levels, 1)
	assert.Equal(t, slog.LevelError, levels[0], "a daemon fault must stay at ERROR")
}

// commandFor builds a per-instance command with one attribute set.
func commandFor(id string, set func(*types.EC2CommandAttributes)) types.EC2InstanceCommand {
	command := types.EC2InstanceCommand{ID: id}
	set(&command.Attributes)
	return command
}

// The ec2.cmd subject carries the instance id, so the metric action has to come
// from the command. Every command must name itself, or the lifecycle work most
// likely to fail lands in one anonymous bucket.
func TestEC2CommandNameCoversEveryCommand(t *testing.T) {
	tests := []struct {
		want string
		set  func(*types.EC2CommandAttributes)
	}{
		{"AttachVolume", func(a *types.EC2CommandAttributes) { a.AttachVolume = true }},
		{"DetachVolume", func(a *types.EC2CommandAttributes) { a.DetachVolume = true }},
		{"DrainVolume", func(a *types.EC2CommandAttributes) { a.DrainVolume = true }},
		{"AttachNetworkInterface", func(a *types.EC2CommandAttributes) { a.AttachENI = true }},
		{"DetachNetworkInterface", func(a *types.EC2CommandAttributes) { a.DetachENI = true }},
		{"AssociateIamInstanceProfile", func(a *types.EC2CommandAttributes) { a.AssociateIamInstanceProfile = true }},
		{"SetSpotLineage", func(a *types.EC2CommandAttributes) { a.SetSpotLineage = true }},
		{"SetInstanceTags", func(a *types.EC2CommandAttributes) { a.SetInstanceTags = true }},
		{"RemoveInstanceTags", func(a *types.EC2CommandAttributes) { a.RemoveInstanceTags = true }},
		{"SetInstanceMonitoring", func(a *types.EC2CommandAttributes) { a.SetInstanceMonitoring = true }},
		{"StartInstance", func(a *types.EC2CommandAttributes) { a.StartInstance = true }},
		{"RebootInstance", func(a *types.EC2CommandAttributes) { a.RebootInstance = true }},
		{"StopInstance", func(a *types.EC2CommandAttributes) { a.StopInstance = true }},
		{"TerminateInstance", func(a *types.EC2CommandAttributes) { a.TerminateInstance = true }},
	}
	// The table is hand-written, so without this a new attribute would pass by
	// simply not being listed — and its commands would report as "unknown".
	assert.Len(t, tests, reflect.TypeFor[types.EC2CommandAttributes]().NumField(),
		"every EC2CommandAttributes field needs a case here and in ec2CommandName")

	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, ec2CommandName(commandFor("i-123", tc.set)))
		})
	}

	assert.Equal(t, "unknown", ec2CommandName(types.EC2InstanceCommand{ID: "i-123"}),
		"a command nothing handles must still be named, not dropped")
}

// The per-instance command path emitted no request metrics at all, so a failed
// stop was invisible in Kibana. It must now record under an action naming the
// command — and never under one naming the instance, which would be one series
// per instance.
func TestEC2CommandRecordsUnderTheCommandNotTheInstance(t *testing.T) {
	installOutcomeReader(t)
	daemon := createTestDaemon(t, sharedNATSURL)

	const instanceID = "i-not-on-this-node"
	subject := "ec2.cmd." + instanceID
	sub, err := daemon.natsConn.Subscribe(subject, daemon.handleEC2Events)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	data, err := json.Marshal(commandFor(instanceID,
		func(a *types.EC2CommandAttributes) { a.StopInstance = true }))
	require.NoError(t, err)
	_, err = daemon.natsConn.Request(subject, data, 5*time.Second)
	require.NoError(t, err)

	assert.Contains(t, requestOutcomesFor(t, ec2CmdAction("StopInstance")), outcomeClientError,
		"a stop naming an instance this node does not hold is the caller's error, and must be counted")
	assert.Empty(t, requestOutcomesFor(t, subject),
		"the instance id must not reach the metric dimension")
}

// A drain runs off-thread so the command subscription is not held for the
// length of the flush. Its point therefore has to be recorded by the goroutine
// once the work finishes: recording at dispatch would call every drain an
// instant success.
func TestDrainVolumeRecordsItsPointAfterTheWork(t *testing.T) {
	installOutcomeReader(t)
	const instanceID, volumeID = "i-drain-metric", "vol-drain-metric"
	daemon := drainTestDaemon(t, instanceID)
	testutil.StartDrainSocket(t, daemon.config.DataDir, volumeID, "OK\n")
	subscribeEC2Commands(t, daemon, instanceID)

	before := len(requestOutcomesFor(t, ec2CmdAction("DrainVolume")))
	reply := drainRequest(t, daemon, instanceID, volumeID)

	var ack types.DrainVolumeResponse
	require.NoError(t, json.Unmarshal(reply.Data, &ack))
	require.Equal(t, types.DrainVolumeStatusDrained, ack.Status)

	// The goroutine records after it replies, so poll rather than assume the
	// point has landed by the time the reply arrives.
	require.Eventually(t, func() bool {
		return len(requestOutcomesFor(t, ec2CmdAction("DrainVolume"))) > before
	}, 5*time.Second, 20*time.Millisecond, "the drain goroutine must record its own point")

	assert.Contains(t, requestOutcomesFor(t, ec2CmdAction("DrainVolume")), outcomeSuccess)
}
