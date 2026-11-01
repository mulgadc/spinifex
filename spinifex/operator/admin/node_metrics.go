package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"

	telemetryv1 "github.com/mulgadc/spinifex/contracts/telemetry/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// GetNodeMetricsAction streams one node's host load. The dispatcher has
// already passed the policy and super-admin gates before it runs.
const GetNodeMetricsAction = "GetNodeMetrics"

func (d Deps) getNodeMetrics(w http.ResponseWriter, inv dispatch.Invocation, queryArgs map[string]string) error {
	r := inv.Request
	node := queryArgs["NodeId"]
	if node == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if !telemetryv1.ValidLiveNode(node) || (len(d.NodeIDs) > 0 && !slices.Contains(d.NodeIDs, node)) {
		return awserrors.Errorf(awserrors.ErrorInvalidParameterValue, "unknown NodeId")
	}
	var since int64
	if raw := queryArgs["Since"]; raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			return errors.New(awserrors.ErrorInvalidParameterValue)
		}
		since = parsed
	}
	if d.NATS == nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	if !flushSupported(w) {
		slog.ErrorContext(r.Context(), "GetNodeMetrics: response writer cannot flush")
		return errors.New(awserrors.ErrorInternalError)
	}

	release, ok, _ := liveStreams.acquire(inv.AccountID)
	if !ok {
		otelsetup.RecordLiveStream(r.Context(), otelsetup.LiveStreamRefusedTotal)
		return errors.New(awserrors.ErrorRequestLimitExceeded)
	}
	defer release()

	return streamLive(w, r, d.NATS, liveSubjects{
		action: GetNodeMetricsAction,
		sample: telemetryv1.NodeLiveSampleSubject(node),
		watch:  telemetryv1.NodeLiveWatchSubject(node),
		absent: awserrors.ErrorInvalidParameterValue,
	}, since, func(data []byte) ([]telemetryv1.NodeLiveSample, error) {
		var ring telemetryv1.NodeLiveRing
		err := json.Unmarshal(data, &ring)
		return ring.Samples, err
	}, func(s telemetryv1.NodeLiveSample) int64 { return s.TimeMillis })
}
