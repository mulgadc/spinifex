package telemetryv1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The console parses these names, so a rename here breaks its live charts.
func TestLiveSampleJSONContract(t *testing.T) {
	data, err := json.Marshal(LiveSample{
		TimeMillis: 1793450123250, CPUPercent: 12.5,
		NetworkIn: 48213, NetworkOut: 1290, PacketsIn: 41, PacketsOut: 12,
	})
	require.NoError(t, err)
	require.JSONEq(t, `{"t":1793450123250,"cpu":12.5,"netIn":48213,"netOut":1290,"pktIn":41,"pktOut":12}`, string(data))
}

func TestValidLiveTarget(t *testing.T) {
	assert.True(t, ValidLiveTarget("123456789012", "i-0123456789abcdef0"))
	assert.True(t, ValidLiveTarget("123456789012", "i-0abc1234"))

	for _, tc := range []struct{ account, instance string }{
		{"", "i-0123456789abcdef0"},
		{"12345678901", "i-0123456789abcdef0"},
		{"123456789012", ""},
		{"123456789012", ">"},
		{"123456789012", "*"},
		{"123456789012", "i-0123.abcdef"},
		{"123456789012", "i-0123456789ABCDEF0"},
		{"123456789012", "vol-0123456789abcdef0"},
	} {
		assert.False(t, ValidLiveTarget(tc.account, tc.instance), "%q %q", tc.account, tc.instance)
	}
}

func TestLiveSubjectsAreOutsideMetrics(t *testing.T) {
	assert.Equal(t, "telemetry.watch.ec2.123456789012.i-0abc1234", LiveWatchSubject("123456789012", "i-0abc1234"))
	assert.Equal(t, "telemetry.live.ec2.123456789012.i-0abc1234", LiveSampleSubject("123456789012", "i-0abc1234"))
}
