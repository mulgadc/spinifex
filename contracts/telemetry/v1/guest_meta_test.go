package telemetryv1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGuestTelemetryMetaJSONContract(t *testing.T) {
	data, err := json.Marshal(GuestTelemetryMeta{
		InstanceID:    "i-0123456789abcdef0",
		AccountID:     "123456789012",
		VCPUs:         2,
		PeriodSeconds: 60,
		Taps:          []string{"tap0abc", "tap0def"},
		Socket:        "/run/spinifex/qmp-telemetry-i-0123456789abcdef0.sock",
	})
	require.NoError(t, err)
	require.JSONEq(t, `{
		"instance_id":"i-0123456789abcdef0",
		"account_id":"123456789012",
		"vcpus":2,
		"period_seconds":60,
		"taps":["tap0abc","tap0def"],
		"socket":"/run/spinifex/qmp-telemetry-i-0123456789abcdef0.sock"
	}`, string(data))
}
