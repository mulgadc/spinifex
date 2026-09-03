package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The global meter binds its instruments to the first real provider installed
// in the process and ignores later ones, so the reader is process-wide and each
// test filters by its own action name rather than installing a fresh provider.
var (
	outcomeReaderOnce sync.Once
	outcomeReader     *sdkmetric.ManualReader
)

// installOutcomeReader installs the process-wide reader. It must run before
// the recording under test: an instrument used while no real provider is
// installed drops the point rather than buffering it.
func installOutcomeReader(t *testing.T) {
	t.Helper()
	outcomeReaderOnce.Do(func() {
		outcomeReader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(outcomeReader)))
	})
}

// requestOutcomesFor returns the outcome attribute of every mulga.requests
// point recorded under action. Actions must be unique per test: points are
// cumulative for the life of the test binary.
func requestOutcomesFor(t *testing.T, action string) []string {
	t.Helper()
	installOutcomeReader(t)

	var rm metricdata.ResourceMetrics
	require.NoError(t, outcomeReader.Collect(context.Background(), &rm))

	var outcomes []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "mulga.requests" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if v, found := dp.Attributes.Value(attribute.Key("rpc.method")); !found || v.AsString() != action {
					continue
				}
				v, found := dp.Attributes.Value(attribute.Key("outcome"))
				if !found {
					outcomes = append(outcomes, "<missing>")
					continue
				}
				outcomes = append(outcomes, v.AsString())
			}
		}
	}
	return outcomes
}

// The wrapper must record whatever the handler reports. Before the handler
// type carried an outcome there was nothing to record and every daemon NATS
// point arrived with the attribute unset.
func TestNATSMetricsHandlerRecordsHandlerOutcome(t *testing.T) {
	for _, want := range []string{outcomeSuccess, outcomeError, outcomeSkipped} {
		t.Run(want, func(t *testing.T) {
			installOutcomeReader(t)
			action := "test.outcome.wrapper." + want

			natsMetricsHandler(action, func(*nats.Msg) string {
				return want
			})(&nats.Msg{Subject: action})

			assert.Equal(t, []string{want}, requestOutcomesFor(t, action))
		})
	}
}

// A handler that answers from a goroutine records its own point once the work
// finishes. Timing the wrapper would report the dispatch as an instant success.
func TestNATSMetricsHandlerSkipsDeferredOutcome(t *testing.T) {
	installOutcomeReader(t)
	const action = "test.outcome.deferred"

	natsMetricsHandler(action, func(*nats.Msg) string {
		return outcomeDeferred
	})(&nats.Msg{Subject: action})

	assert.Empty(t, requestOutcomesFor(t, action), "a deferred handler owns its own point")
}

// End to end over the real transport: the generic request constructor must
// report the failure's class for a failing service call and success for a
// passing one, so a failed NATS request is distinguishable in Kibana without
// reading logs, and a caller mistake does not read as a daemon fault.
func TestHandleNATSRequestReportsOutcome(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		svcErr  error
		want    string
	}{
		{"caller error", "test.outcome.err", errors.New(awserrors.ErrorVolumeInUse), outcomeClientError},
		{"server fault", "test.outcome.fault", errors.New(awserrors.ErrorServerInternal), outcomeError},
		{"unrecognised failure", "test.outcome.opaque", errors.New("disk fell over"), outcomeError},
		{"service success", "test.outcome.ok", nil, outcomeSuccess},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			installOutcomeReader(t)

			nc, err := nats.Connect(sharedNATSURL)
			require.NoError(t, err)
			defer nc.Close()

			handler := natsMetricsHandler(tc.subject,
				handleNATSRequest("test-node", func(context.Context, *testInput, string) (*testOutput, error) {
					if tc.svcErr != nil {
						return nil, tc.svcErr
					}
					return &testOutput{Greeting: "hello"}, nil
				}))
			sub, err := nc.Subscribe(tc.subject, handler)
			require.NoError(t, err)
			defer func() { _ = sub.Unsubscribe() }()

			reqData, err := json.Marshal(testInput{Name: "world"})
			require.NoError(t, err)
			_, err = nc.Request(tc.subject, reqData, 5*time.Second)
			require.NoError(t, err)

			// The handler records after it replies, so poll rather than assume the
			// point has landed by the time the reply arrives.
			require.Eventually(t, func() bool {
				return len(requestOutcomesFor(t, tc.subject)) > 0
			}, 5*time.Second, 20*time.Millisecond, "the handler must record its point")

			assert.Equal(t, []string{tc.want}, requestOutcomesFor(t, tc.subject))
		})
	}
}

// A malformed payload never reaches the service. It is still not a success,
// but it is the caller's mistake, so it records as a client error rather than
// counting against the daemon.
func TestHandleNATSRequestReportsOutcomeForBadPayload(t *testing.T) {
	installOutcomeReader(t)
	const subject = "test.outcome.malformed"

	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	defer nc.Close()

	handler := natsMetricsHandler(subject,
		handleNATSRequest("test-node", func(context.Context, *testInput, string) (*testOutput, error) {
			t.Fatal("service must not be reached for a malformed payload")
			return nil, nil
		}))
	sub, err := nc.Subscribe(subject, handler)
	require.NoError(t, err)
	defer func() { _ = sub.Unsubscribe() }()

	_, err = nc.Request(subject, []byte("{not json"), 5*time.Second)
	require.NoError(t, err)

	// The handler records after it replies, so poll rather than assume the point
	// has landed by the time the reply arrives.
	require.Eventually(t, func() bool {
		return len(requestOutcomesFor(t, subject)) > 0
	}, 5*time.Second, 20*time.Millisecond, "the handler must record its point")

	assert.Equal(t, []string{outcomeClientError}, requestOutcomesFor(t, subject))
}

// asMsgHandler drops the outcome a handler reports, for tests that subscribe a
// handler directly instead of going through the metrics wrapper.
func asMsgHandler(h natsHandler) nats.MsgHandler {
	return func(msg *nats.Msg) { _ = h(msg) }
}

// The system launch handler answers from a goroutine, so it defers its point
// and records it once the launch finishes. Without the deferral the wrapper
// would time the dispatch and call a five-minute boot an instant success.
func TestHandleSystemLaunchInstanceDefersItsOwnOutcome(t *testing.T) {
	installOutcomeReader(t)

	nc, err := nats.Connect(sharedNATSURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	d := &Daemon{natsConn: nc, natsSubscriptions: make(map[string]*nats.Subscription)}

	replyCh := make(chan []byte, 1)
	sub, err := nc.Subscribe("test.outcome.deferred.reply", func(msg *nats.Msg) {
		replyCh <- msg.Data
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	msg := msgWithConn(nc, &nats.Msg{
		Subject: "system.LaunchInstance.sys.micro",
		Reply:   "test.outcome.deferred.reply",
		Sub:     sub,
		Data:    []byte("{not valid"),
	})

	assert.Equal(t, outcomeDeferred, d.handleSystemLaunchInstance(msg),
		"the wrapper must not record for a handler that answers off-thread")

	select {
	case <-replyCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no reply from the deferred launch handler")
	}
	d.systemDispatchWg.Wait()

	assert.Equal(t, []string{outcomeError}, requestOutcomesFor(t, systemLaunchAction),
		"a rejected launch must land as an error, recorded by the handler itself")
}
