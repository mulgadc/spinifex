//go:build integration

package integration

import (
	"sync"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// subjectStubMu guards subjectStubRegistry. subjectStubRegistry is keyed by
// *nats.Conn (each Gateway owns its own embedded connection, so unrelated
// tests — even running concurrently — never collide) then by NATS subject,
// holding the payload the stub responder for that subject currently returns.
var (
	subjectStubMu       sync.RWMutex
	subjectStubRegistry = map[*nats.Conn]map[string][]byte{}
)

// StubSubject registers a responder for subject on the gateway's NATS
// connection that replies to every request with payload, standing in for the
// daemon-side subscriber a live spinifex node would run. It satisfies both
// wire patterns the control plane uses: utils.Gather's scatter-gather (a
// fresh inbox per call, subject.Reply set) and a plain nc.RequestMsg — both
// publish with Reply set to an inbox, so a direct subscription on subject
// that calls msg.Respond answers either.
//
// To make a future test exercise a different control-plane path, call
// StubSubject once per NATS subject the handler under test publishes to (grep
// spinifex/gateway/**/*.go for the literal subject string passed to
// utils.Gather or natsConn.RequestMsg), then use SetSubjectReply to vary the
// response per test case — e.g. an error envelope from utils.GenerateErrorPayload
// for a validation-error test, or a populated output struct for a happy path.
func (gw *Gateway) StubSubject(t *testing.T, subject string, payload []byte) {
	t.Helper()
	gw.SetSubjectReply(subject, payload)

	sub, err := gw.NATSConn.Subscribe(subject, func(m *nats.Msg) {
		if m.Reply == "" {
			return
		}
		subjectStubMu.RLock()
		resp := subjectStubRegistry[gw.NATSConn][subject]
		subjectStubMu.RUnlock()
		_ = m.Respond(resp)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	// Drop only this subject's entry: StubSubject is per-subject, so removing
	// the whole connection's map here would wipe sibling stubs registered on
	// the same Gateway.
	t.Cleanup(func() {
		subjectStubMu.Lock()
		delete(subjectStubRegistry[gw.NATSConn], subject)
		if len(subjectStubRegistry[gw.NATSConn]) == 0 {
			delete(subjectStubRegistry, gw.NATSConn)
		}
		subjectStubMu.Unlock()
	})
}

// SetSubjectReply overrides the reply payload for a subject already stubbed
// via StubSubject on gw, updating the existing subscriber's response in
// place. Safe to call mid-test — it never adds a second subscriber, which
// would otherwise race the first for who answers the next request.
func (gw *Gateway) SetSubjectReply(subject string, payload []byte) {
	subjectStubMu.Lock()
	defer subjectStubMu.Unlock()
	if subjectStubRegistry[gw.NATSConn] == nil {
		subjectStubRegistry[gw.NATSConn] = make(map[string][]byte)
	}
	subjectStubRegistry[gw.NATSConn][subject] = payload
}
