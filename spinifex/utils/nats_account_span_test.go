package utils

import (
	"context"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// handlerDone signals that a subscriber callback has returned. ServeNATSRequestCtx
// ends its consumer span in a defer, after the reply is published, so a test that
// asserts on recorded spans must wait for the callback rather than for the reply.
type handlerDone chan struct{}

func newHandlerDone() handlerDone { return make(handlerDone, 1) }

func (d handlerDone) done() { d <- struct{}{} }

func (d handlerDone) wait(t *testing.T) {
	t.Helper()
	select {
	case <-d:
	case <-time.After(5 * time.Second):
		t.Fatal("NATS subscriber callback did not return")
	}
}

// spanAccounts returns the account attribute of each recorded span by kind,
// with an empty string where the span carries none.
func spanAccounts(spans []sdktrace.ReadOnlySpan) map[trace.SpanKind]string {
	accounts := make(map[trace.SpanKind]string, len(spans))
	for _, span := range spans {
		accounts[span.SpanKind()] = ""
		for _, attr := range span.Attributes() {
			if string(attr.Key) == AttrAccountID {
				accounts[span.SpanKind()] = attr.Value.AsString()
			}
		}
	}
	return accounts
}

// Both ends of a NATS hop must name the account. Without it a tenant's request
// is attributable at the gateway and anonymous everywhere it does its work,
// which is the whole difficulty with one cluster serving many accounts.
func TestNATSSpansCarryTheCallerAccount(t *testing.T) {
	recorder := testutil.RecordSpans(t)

	ns := startTestNATSServer(t)
	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	served := newHandlerDone()
	_, err = nc.Subscribe("test.account.echo", func(msg *nats.Msg) {
		defer served.done()
		ServeNATSRequestCtx(msg, func(context.Context, *traceEchoRequest) (*traceEchoResponse, error) {
			return &traceEchoResponse{Name: "ok"}, nil
		})
	})
	require.NoError(t, err)

	_, err = NATSRequest[traceEchoResponse](context.Background(), nc, "test.account.echo",
		traceEchoRequest{Name: "x"}, 5*time.Second, "000000000042")
	require.NoError(t, err)
	served.wait(t)

	accounts := spanAccounts(recorder.Ended())
	assert.Equal(t, "000000000042", accounts[trace.SpanKindClient], "producer span must name the account it sent for")
	assert.Equal(t, "000000000042", accounts[trace.SpanKindConsumer], "consumer span must name the account it served")
}

// Work with no caller is left unattributed. Defaulting it to the admin account
// would make every sweep and probe look like tenant activity.
func TestNATSConsumerSpanOmitsAnAbsentAccount(t *testing.T) {
	recorder := testutil.RecordSpans(t)

	msg := nats.NewMsg("test.account.none")
	_, span := StartConsumerSpan(msg)
	span.End()

	require.Len(t, recorder.Ended(), 1)
	assert.Empty(t, spanAccounts(recorder.Ended())[trace.SpanKindConsumer])
}

// The header is the source of truth on the consumer side: a handler reached
// directly, without going through NATSRequest, is still attributed.
func TestNATSConsumerSpanReadsTheAccountHeader(t *testing.T) {
	recorder := testutil.RecordSpans(t)

	msg := nats.NewMsg("test.account.header")
	msg.Header.Set(AccountIDHeader, "000000000007")
	_, span := StartConsumerSpan(msg)
	span.End()

	require.Len(t, recorder.Ended(), 1)
	assert.Equal(t, "000000000007", spanAccounts(recorder.Ended())[trace.SpanKindConsumer])
}
