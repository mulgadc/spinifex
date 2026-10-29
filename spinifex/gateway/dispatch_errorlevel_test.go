// Tests that a dispatch failure is logged at the level the status the caller
// receives implies, so a client's own 4xx does not read as a fault of ours.

package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loggedLevelOf returns the level of the first record in logs whose msg is
// msg, failing the test when no record carries it.
func loggedLevelOf(t *testing.T, logs *bytes.Buffer, msg string) string {
	t.Helper()
	for line := range strings.SplitSeq(logs.String(), "\n") {
		var rec struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec.Msg == msg {
			return rec.Level
		}
	}
	t.Fatalf("no %q record in:\n%s", msg, logs.String())
	return ""
}

func TestDispatchSeam_ErrorLevelFollowsTheStatusTheCallerGets(t *testing.T) {
	cases := []struct {
		name   string
		code   string
		status int
		level  string
	}{
		{"a dry run is the answer AWS documents for one", awserrors.ErrorDryRunOperation, http.StatusPreconditionFailed, "INFO"},
		{"a volume the caller named does not exist", awserrors.ErrorInvalidVolumeNotFound, http.StatusNotFound, "INFO"},
		{"a malformed parameter is the caller's request", awserrors.ErrorInvalidParameterValue, http.StatusBadRequest, "INFO"},
		{"an internal failure is ours", awserrors.ErrorInternalError, http.StatusInternalServerError, "ERROR"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &bytes.Buffer{}
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			capt := &dispatchCapture{dispatchErr: errors.New(tc.code)}
			_, app := dispatchSeamApp(t, allowAllECRDocs(), fakeJSONRegistration(capt))
			resp := doRequest(app, signDispatchRequest(t, fakeJSONService, []byte("{}")))

			require.Equal(t, tc.status, resp.StatusCode)
			assert.Equal(t, tc.level, loggedLevelOf(t, logs, "Service request error"))
		})
	}
}
