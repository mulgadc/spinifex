package gateway_eks

import (
	"context"
	"testing"
	"time"

	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublishInternal_RelaysToSubjects(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	cases := []struct {
		name    string
		body    string
		subject string
		payload string
	}{
		{
			name:    "bootstrap token",
			body:    `{"accountId":"111122223333","channel":"bootstrap","kind":"k3s-bootstrap-token","payload":{"token":"k3s-secret"}}`,
			subject: handlers_eks.BootstrapSubject("111122223333", "alpha", handlers_eks.BootstrapSubjectToken),
			payload: `{"token":"k3s-secret"}`,
		},
		{
			name:    "state report",
			body:    `{"accountId":"111122223333","channel":"state","payload":{"healthz":"ok","node_count":1,"ts":42}}`,
			subject: handlers_eks.StateSubject("111122223333", "alpha"),
			payload: `{"healthz":"ok","node_count":1,"ts":42}`,
		},
		{
			name:    "addon status report",
			body:    `{"accountId":"111122223333","channel":"addon","payload":{"addon":"spinifex-noop","version":"0.1.0","phase":"ready","ts":42}}`,
			subject: handlers_eks.AddonStatusSubject("111122223333", "alpha"),
			payload: `{"addon":"spinifex-noop","version":"0.1.0","phase":"ready","ts":42}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub, err := nc.SubscribeSync(tc.subject)
			require.NoError(t, err)
			t.Cleanup(func() { _ = sub.Unsubscribe() })

			out, err := PublishInternal(context.Background(), nc, "alpha", []byte(tc.body))
			require.NoError(t, err)
			require.NotNil(t, out)

			msg, err := sub.NextMsg(2 * time.Second)
			require.NoError(t, err)
			assert.JSONEq(t, tc.payload, string(msg.Data))
		})
	}
}

func TestPublishInternal_Rejects(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	cases := []struct {
		name string
		body string
	}{
		{"empty account", `{"accountId":"","channel":"state","payload":{"healthz":"ok"}}`},
		{"empty payload", `{"accountId":"111122223333","channel":"state"}`},
		{"unknown channel", `{"accountId":"111122223333","channel":"wat","payload":{"x":1}}`},
		{"unknown bootstrap kind", `{"accountId":"111122223333","channel":"bootstrap","kind":"evil","payload":{"x":1}}`},
		{"malformed json", `{not-json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PublishInternal(context.Background(), nc, "alpha", []byte(tc.body))
			require.Error(t, err)
		})
	}
}

func TestPublishInternal_NilConnAndEmptyCluster(t *testing.T) {
	_, err := PublishInternal(context.Background(), nil, "alpha", []byte(`{}`))
	require.Error(t, err)

	_, nc := testutil.StartTestNATS(t)
	_, err = PublishInternal(context.Background(), nc, "", []byte(`{"accountId":"1","channel":"state","payload":{"x":1}}`))
	require.Error(t, err)
}
