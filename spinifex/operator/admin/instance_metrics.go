package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	telemetryv1 "github.com/mulgadc/spinifex/contracts/telemetry/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go"
)

// GetInstanceMetricsAction is open to any account, authorized per instance.
const GetInstanceMetricsAction = "GetInstanceMetrics"

// One stream ends well inside the console's write timeout, and the client
// reconnects with Since, which also re-checks the caller's policy each time.
// Variables only so tests can shorten them.
var (
	liveStreamDuration = 60 * time.Second
	liveWatchInterval  = 5 * time.Second
	liveKeepAlive      = 15 * time.Second
	liveWatchTimeout   = 2 * time.Second
)

// getInstanceMetrics streams an instance's live statistics as
// text/event-stream. The policy is checked against the instance's ARN, and the
// subject carries the caller's account, so only that account's sampler answers.
func (d Deps) getInstanceMetrics(w http.ResponseWriter, inv dispatch.Invocation, queryArgs map[string]string) error {
	r := inv.Request
	accountID := inv.AccountID
	if accountID == "" {
		slog.ErrorContext(r.Context(), "GetInstanceMetrics: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	instanceID := queryArgs["InstanceId"]
	if instanceID == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if !telemetryv1.ValidLiveTarget(accountID, instanceID) {
		return errors.New(awserrors.ErrorInvalidInstanceIDMalformed)
	}

	var since int64
	if raw := queryArgs["Since"]; raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			return errors.New(awserrors.ErrorInvalidParameterValue)
		}
		since = parsed
	}

	instanceARN := arn.FormatEC2(arn.EC2Instance, inv.Region, accountID, instanceID)
	if err := inv.Authorize(ServiceName, GetInstanceMetricsAction, []string{instanceARN}, nil); err != nil {
		return err
	}

	if d.NATS == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	if !flushSupported(w) {
		slog.ErrorContext(r.Context(), "GetInstanceMetrics: response writer cannot flush")
		return errors.New(awserrors.ErrorInternalError)
	}

	release, ok, accountFull := liveStreams.acquire(accountID)
	if !ok {
		outcome := otelsetup.LiveStreamRefusedTotal
		if accountFull {
			outcome = otelsetup.LiveStreamRefusedAccount
		}
		otelsetup.RecordLiveStream(r.Context(), outcome)
		slog.WarnContext(r.Context(), "GetInstanceMetrics: too many live streams",
			"account_id", accountID, "instance_id", instanceID, "cap", string(outcome))
		return errors.New(awserrors.ErrorRequestLimitExceeded)
	}
	defer release()
	otelsetup.RecordLiveStream(r.Context(), otelsetup.LiveStreamOpened)
	otelsetup.RecordLiveStreamOpen(r.Context(), 1)
	defer otelsetup.RecordLiveStreamOpen(r.Context(), -1)

	return streamInstanceMetrics(w, r, d.NATS, accountID, instanceID, since)
}

// flushSupported reports whether w, or anything it wraps, can flush. It is a
// probe with no side effects, so a refusal still reaches the error path.
func flushSupported(w http.ResponseWriter) bool {
	for {
		if _, ok := w.(http.Flusher); ok {
			return true
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = u.Unwrap()
	}
}

func streamInstanceMetrics(w http.ResponseWriter, r *http.Request, nc *nats.Conn, accountID, instanceID string, since int64) error {
	return streamLive(w, r, nc, liveSubjects{
		action: GetInstanceMetricsAction,
		sample: telemetryv1.LiveSampleSubject(accountID, instanceID),
		watch:  telemetryv1.LiveWatchSubject(accountID, instanceID),
		absent: awserrors.ErrorInvalidInstanceIDNotFound,
	}, since, func(data []byte) ([]telemetryv1.LiveSample, error) {
		var ring telemetryv1.LiveRing
		err := json.Unmarshal(data, &ring)
		return ring.Samples, err
	}, func(s telemetryv1.LiveSample) int64 { return s.TimeMillis })
}

// liveSubjects names one live source: where its samples are published, where
// to ask for its ring, and the error for a source nobody answers for.
type liveSubjects struct {
	action string
	sample string
	watch  string
	absent string
}

func streamLive[S any](w http.ResponseWriter, r *http.Request, nc *nats.Conn, src liveSubjects, since int64,
	decodeRing func([]byte) ([]S, error), stamp func(S) int64) error {
	samples := make(chan *nats.Msg, 64)
	sub, err := nc.ChanSubscribe(src.sample, samples)
	if err != nil {
		slog.ErrorContext(r.Context(), src.action+": subscribe", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	defer func() { _ = sub.Unsubscribe() }()

	watch, err := json.Marshal(telemetryv1.LiveWatch{SinceMillis: since})
	if err != nil {
		return errors.New(awserrors.ErrorInternalError)
	}

	// Subscribed first, then asked for the ring, so nothing falls between them.
	// No responder is a source that is not running in this account, which
	// reads the same as one that does not exist.
	reply, err := nc.Request(src.watch, watch, liveWatchTimeout)
	if err != nil {
		if errors.Is(err, nats.ErrNoResponders) || errors.Is(err, nats.ErrTimeout) {
			return errors.New(src.absent)
		}
		slog.ErrorContext(r.Context(), src.action+": watch request", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	ring, err := decodeRing(reply.Data)
	if err != nil {
		slog.ErrorContext(r.Context(), src.action+": malformed ring", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	ctrl := http.NewResponseController(w)

	// From here the status is committed, so failures end the stream quietly and
	// the client reconnects.
	last := since
	send := func(s S) bool {
		t := stamp(s)
		if t <= last {
			return true
		}
		data, err := json.Marshal(s)
		if err != nil {
			return true
		}
		last = t
		if _, err := fmt.Fprintf(w, "id: %d\nevent: sample\ndata: %s\n\n", t, data); err != nil {
			return false
		}
		return ctrl.Flush() == nil
	}

	for _, s := range ring {
		if !send(s) {
			return nil
		}
	}
	if err := ctrl.Flush(); err != nil {
		return nil
	}

	end := time.NewTimer(liveStreamDuration)
	defer end.Stop()
	renew := time.NewTicker(liveWatchInterval)
	defer renew.Stop()
	keepAlive := time.NewTicker(liveKeepAlive)
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-end.C:
			return nil
		case <-renew.C:
			if err := nc.Publish(src.watch, watch); err != nil {
				slog.DebugContext(r.Context(), src.action+": renew watch", "err", err)
			}
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil || ctrl.Flush() != nil {
				return nil
			}
		case msg := <-samples:
			var s S
			if err := json.Unmarshal(msg.Data, &s); err != nil {
				slog.DebugContext(r.Context(), src.action+": malformed sample", "err", err)
				continue
			}
			if !send(s) {
				return nil
			}
		}
	}
}
