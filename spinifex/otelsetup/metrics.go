package otelsetup

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName is the instrumentation scope every spinifex request metric shares,
// so the HTTP and NATS paths land on one set of instruments.
const meterName = "github.com/mulgadc/spinifex/spinifex/otelsetup"

// actionAttrKey names the logical operation on request metrics. Values must
// stay low-cardinality: resolved action names only, never resource IDs.
const actionAttrKey = "rpc.method"

// leakKindAttrKey names the class of resource that could not be reclaimed.
// Values must stay low-cardinality: resource classes only, never addresses or
// IDs — those belong in the accompanying log line.
const leakKindAttrKey = "resource.kind"

// fenceOutcomeAttrKey names what a node did after losing a volume lease.
// The four FenceOutcome constants below are the whole domain, and none carries
// a volume or node identity: those belong in the log line beside the metric.
const fenceOutcomeAttrKey = "outcome"

// FenceOutcomeTaken means another node held the lease, so this one gave up its
// export. The clearest signal in the set: somewhere a volume changed hands.
const FenceOutcomeTaken = "taken"

// FenceOutcomeExpired means the entry was gone or unreadable with no successor.
// The node could not show it was the only writer, which is fenced on the same
// terms — a node that cannot prove ownership must not keep writing.
const FenceOutcomeExpired = "expired"

// FenceOutcomeStalled means renewal could not be confirmed for long enough that
// the server TTL was about to admit another owner. This is the self-fence, and
// a rising rate is a NATS problem showing up as guest restarts.
const FenceOutcomeStalled = "stalled"

// FenceOutcomeKillFailed means the export could not be torn down: nbdkit did
// not exit. Nothing downstream is safe after this — the volume is neither
// fenced nor released — so it is the one value here that is a page.
const FenceOutcomeKillFailed = "kill_failed"

var (
	instrumentsOnce sync.Once
	requestCounter  metric.Int64Counter
	requestDuration metric.Float64Histogram

	leakOnce    sync.Once
	leakCounter metric.Int64Counter

	fenceOnce       sync.Once
	fenceCounter    metric.Int64Counter
	takeoverOnce    sync.Once
	takeoverCounter metric.Int64Counter
)

// requestInstruments lazily creates the shared request instruments. The
// global meter delegates to the real provider once Init installs it.
func requestInstruments() (metric.Int64Counter, metric.Float64Histogram) {
	instrumentsOnce.Do(func() {
		m := otel.Meter(meterName)
		var err error
		requestCounter, err = m.Int64Counter("mulga.requests",
			metric.WithDescription("Count of service requests handled."),
			metric.WithUnit("{request}"))
		if err != nil {
			otel.Handle(err)
		}
		requestDuration, err = m.Float64Histogram("mulga.request.duration",
			metric.WithDescription("Duration of handled service requests."),
			metric.WithUnit("s"))
		if err != nil {
			otel.Handle(err)
		}
	})
	return requestCounter, requestDuration
}

// RecordRequest records one handled request on the shared counter and
// duration histogram. outcome is "success"/"error", or empty when the
// result is not observable at the instrumentation point.
func RecordRequest(ctx context.Context, action, outcome string, elapsed time.Duration) {
	counter, duration := requestInstruments()
	attrs := []attribute.KeyValue{attribute.String(actionAttrKey, action)}
	if outcome != "" {
		attrs = append(attrs, attribute.String("outcome", outcome))
	}
	opt := metric.WithAttributeSet(attribute.NewSet(attrs...))
	if counter != nil {
		counter.Add(ctx, 1, opt)
	}
	if duration != nil {
		duration.Record(ctx, elapsed.Seconds(), opt)
	}
}

// RecordResourceLeak counts one resource that teardown could not reclaim and
// will not retry. kind is a resource class such as "public_ip"; the identity of
// the specific resource goes in the caller's log line, not here.
func RecordResourceLeak(ctx context.Context, kind string) {
	leakOnce.Do(func() {
		var err error
		leakCounter, err = otel.Meter(meterName).Int64Counter("mulga.resource.leaked",
			metric.WithDescription("Count of resources teardown abandoned without reclaiming."),
			metric.WithUnit("{resource}"))
		if err != nil {
			otel.Handle(err)
		}
	})
	if leakCounter != nil {
		leakCounter.Add(ctx, 1, metric.WithAttributeSet(
			attribute.NewSet(attribute.String(leakKindAttrKey, kind))))
	}
}

// RecordVolumeFence counts one volume whose lease this node stopped holding
// while an engine was open on it. outcome is one of the FenceOutcome constants.
//
// Any non-zero rate means guests are being stopped to protect a volume somebody
// else now owns, which is a correctness action with an availability cost and is
// worth paging on. FenceOutcomeKillFailed is the urgent one: the guest was not
// stopped, so the protection did not happen.
func RecordVolumeFence(ctx context.Context, outcome string) {
	fenceOnce.Do(func() {
		var err error
		fenceCounter, err = otel.Meter(meterName).Int64Counter("mulga.volume.fence",
			metric.WithDescription("Count of volume leases lost while an engine was open, by what the node did next."),
			metric.WithUnit("{volume}"))
		if err != nil {
			otel.Handle(err)
		}
	})
	if fenceCounter != nil {
		fenceCounter.Add(ctx, 1, metric.WithAttributeSet(
			attribute.NewSet(attribute.String(fenceOutcomeAttrKey, outcome))))
	}
}

// RecordVolumeTakeover counts one volume opened from the backend checkpoint
// while another node held writes the backend never received.
//
// This is the deliberate trade the placement design makes — an instance that
// runs with an older copy beats one that runs nowhere — so it is not an error.
// It is the only quantity that says how often that trade is being paid.
func RecordVolumeTakeover(ctx context.Context) {
	takeoverOnce.Do(func() {
		var err error
		takeoverCounter, err = otel.Meter(meterName).Int64Counter("mulga.volume.takeover",
			metric.WithDescription("Count of volumes opened from the backend while another node held unsealed writes."),
			metric.WithUnit("{volume}"))
		if err != nil {
			otel.Handle(err)
		}
	})
	if takeoverCounter != nil {
		takeoverCounter.Add(ctx, 1)
	}
}
