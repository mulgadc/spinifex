package handlers_elbv2

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefaultHealthCheck(t *testing.T) {
	t.Parallel()
	hc := DefaultHealthCheck()

	assert.Equal(t, ProtocolHTTP, hc.Protocol)
	assert.Equal(t, DefaultHealthCheckPort, hc.Port)
	assert.Equal(t, DefaultHealthCheckPath, hc.Path)
	assert.Equal(t, int64(DefaultHealthCheckInterval), hc.IntervalSeconds)
	assert.Equal(t, int64(DefaultHealthCheckTimeout), hc.TimeoutSeconds)
	assert.Equal(t, int64(DefaultHealthyThreshold), hc.HealthyThreshold)
	assert.Equal(t, int64(DefaultUnhealthyThreshold), hc.UnhealthyThreshold)
	assert.Equal(t, DefaultHealthCheckMatcher, hc.Matcher)
}
