package gateway

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func captureInfoLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

func TestRequestLogIncludesRDSAgentAction(t *testing.T) {
	actions := []string{
		"RegisterDBInstance",
		"SubmitDBStateChange",
		"GetDBBootstrapConfig",
	}

	for _, action := range actions {
		t.Run(action, func(t *testing.T) {
			logs := captureInfoLogs(t)
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			ctx := context.WithValue(req.Context(), ctxService, "rds")
			ctx = context.WithValue(ctx, ctxAction, action)

			(&GatewayConfig{}).Request(httptest.NewRecorder(), req.WithContext(ctx))

			out := logs.String()
			require.Contains(t, out, "msg=Request")
			require.Contains(t, out, "service=rds")
			require.Contains(t, out, "action="+action)
		})
	}
}
