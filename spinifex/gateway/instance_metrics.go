package gateway

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
	"github.com/nats-io/nats.go"
)

const getInstanceMetricsAction = "GetInstanceMetrics"

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
func (gw *GatewayConfig) getInstanceMetrics(w http.ResponseWriter, r *http.Request, queryArgs map[string]string) error {
	accountID, _ := r.Context().Value(ctxAccountID).(string)
	if accountID == "" {
		slog.Error("GetInstanceMetrics: no account ID in auth context")
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

	instanceARN := arn.FormatEC2(arn.EC2Instance, gw.Region, accountID, instanceID)
	if err := gw.checkPolicyResources(r, "spinifex", getInstanceMetricsAction, []string{instanceARN}); err != nil {
		return err
	}

	if gw.NATSConn == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	if !flushSupported(w) {
		slog.Error("GetInstanceMetrics: response writer cannot flush")
		return errors.New(awserrors.ErrorInternalError)
	}

	return gw.streamInstanceMetrics(w, r, accountID, instanceID, since)
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

func (gw *GatewayConfig) streamInstanceMetrics(w http.ResponseWriter, r *http.Request, accountID, instanceID string, since int64) error {
	nc := gw.NATSConn
	samples := make(chan *nats.Msg, 64)
	sub, err := nc.ChanSubscribe(telemetryv1.LiveSampleSubject(accountID, instanceID), samples)
	if err != nil {
		slog.Error("GetInstanceMetrics: subscribe", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	defer func() { _ = sub.Unsubscribe() }()

	watch, err := json.Marshal(telemetryv1.LiveWatch{SinceMillis: since})
	if err != nil {
		return errors.New(awserrors.ErrorInternalError)
	}
	watchSubject := telemetryv1.LiveWatchSubject(accountID, instanceID)

	// Subscribed first, then asked for the ring, so nothing falls between them.
	// No responder is an instance that is not running in this account, which
	// reads the same as one that does not exist.
	reply, err := nc.Request(watchSubject, watch, liveWatchTimeout)
	if err != nil {
		if errors.Is(err, nats.ErrNoResponders) || errors.Is(err, nats.ErrTimeout) {
			return errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
		}
		slog.Error("GetInstanceMetrics: watch request", "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	var ring telemetryv1.LiveRing
	if err := json.Unmarshal(reply.Data, &ring); err != nil {
		slog.Error("GetInstanceMetrics: malformed ring", "err", err)
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
	send := func(s telemetryv1.LiveSample) bool {
		if s.TimeMillis <= last {
			return true
		}
		data, err := json.Marshal(s)
		if err != nil {
			return true
		}
		last = s.TimeMillis
		if _, err := fmt.Fprintf(w, "id: %d\nevent: sample\ndata: %s\n\n", s.TimeMillis, data); err != nil {
			return false
		}
		return ctrl.Flush() == nil
	}

	for _, s := range ring.Samples {
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
			if err := nc.Publish(watchSubject, watch); err != nil {
				slog.Debug("GetInstanceMetrics: renew watch", "err", err)
			}
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil || ctrl.Flush() != nil {
				return nil
			}
		case msg := <-samples:
			var s telemetryv1.LiveSample
			if err := json.Unmarshal(msg.Data, &s); err != nil {
				slog.Debug("GetInstanceMetrics: malformed sample", "err", err)
				continue
			}
			if !send(s) {
				return nil
			}
		}
	}
}
