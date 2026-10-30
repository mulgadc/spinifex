package gateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/audit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type loggedRecord struct {
	Level  string  `json:"level"`
	Msg    string  `json:"msg"`
	Status float64 `json:"status"`
}

// logRecords parses the JSON log lines captured while a request ran.
func logRecords(t *testing.T, logs *bytes.Buffer) []loggedRecord {
	t.Helper()
	var out []loggedRecord
	for line := range strings.SplitSeq(logs.String(), "\n") {
		var rec loggedRecord
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg != "" {
			out = append(out, rec)
		}
	}
	return out
}

func findRecord(recs []loggedRecord, msg string) (loggedRecord, bool) {
	for _, rec := range recs {
		if rec.Msg == msg {
			return rec, true
		}
	}
	return loggedRecord{}, false
}

// The outcome a /admin request is logged with must be the one the caller got:
// a denial or a fault is never recorded as a completed request.
func TestAdminOutcome_LogFollowsTheRealResult(t *testing.T) {
	cases := []struct {
		name       string
		accountID  string
		mutate     func(*GatewayConfig)
		wantStatus int
		wantLevel  string
	}{
		{"success", awsidentifiers.DefaultAccountID(), nil, http.StatusOK, ""},
		{"denied caller", authzAccountID, nil, http.StatusForbidden, "INFO"},
		{"cluster fault", awsidentifiers.DefaultAccountID(), func(gw *GatewayConfig) { gw.NATSConn = nil }, http.StatusServiceUnavailable, "ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, app := operatorPipelineApp(t, tc.accountID, allowAllOperatorDocs(), tc.mutate)
			logs := &bytes.Buffer{}
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			resp := adminCall(t, audit.RequestLogger(app), http.MethodPost, "ListAccounts", "spinifex", `{}`)
			require.Equal(t, tc.wantStatus, resp.StatusCode)

			recs := logRecords(t, logs)
			access, ok := findRecord(recs, "request")
			require.True(t, ok, "no access record in:\n%s", logs.String())
			assert.EqualValues(t, tc.wantStatus, access.Status)

			_, completed := findRecord(recs, "Service request completed")
			failure, failed := findRecord(recs, "Service request error")
			if tc.wantLevel == "" {
				assert.True(t, completed, "a success must be logged as completed")
				assert.False(t, failed, "a success must not be logged as an error")
				return
			}
			assert.False(t, completed, "a failed request must never be logged as completed")
			require.True(t, failed, "a failed request must be logged as an error")
			assert.Equal(t, tc.wantLevel, failure.Level)
		})
	}
}
