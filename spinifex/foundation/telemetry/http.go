// Package otelsetup carries spinifex's request instrumentation: the request
// counter/duration histogram, and a thin binding of the shared HTTP middleware
// to those instruments. The middleware body and the OTel bootstrap itself live
// in bluebottle/pkg/otelsetup, which service entrypoints call directly.
package otelsetup

import (
	"context"
	"net/http"

	bbotel "github.com/mulgadc/bluebottle/pkg/otelsetup"
)

// OutcomeForStatus classifies a response for the outcome dimension. Re-exported
// so the NATS path, which classifies an awserrors HTTP status, provably shares
// one implementation with the HTTP path.
func OutcomeForStatus(status int) string { return bbotel.OutcomeForStatus(status) }

// SetRequestAction sets the logical action recorded on request metrics for the
// in-flight request. No-op when the request did not pass through
// HTTPMiddleware.
func SetRequestAction(ctx context.Context, action string) { bbotel.SetRequestAction(ctx, action) }

// HTTPMiddleware binds the shared server middleware to spinifex's conventions:
// health probes record metrics without rooting a trace, and requests land on
// the same instruments the NATS paths write to rather than bluebottle's.
func HTTPMiddleware(serverName string) func(http.Handler) http.Handler {
	return bbotel.HTTPMiddleware(serverName,
		bbotel.WithUntracedPaths("/health"),
		bbotel.WithRecorder(recordSharedRequest))
}

// recordSharedRequest funnels the shared middleware's metric onto spinifex's
// counter. Status code and byte counts are dropped: they would add series the
// spinifex dashboards do not read.
func recordSharedRequest(ctx context.Context, m bbotel.RequestMetric) {
	RecordRequest(ctx, m.Action, m.Outcome, m.Elapsed)
}
