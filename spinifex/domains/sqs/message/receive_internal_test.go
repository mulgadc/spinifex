package message

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The wait is the request's WaitTimeSeconds, else the queue's default.
func TestReceiveTimingWait(t *testing.T) {
	target := ReceiveTarget{
		Namespace: Namespace{Queue: "q1"},
		Account:   "000000000001", VisibilityTimeout: 30, MessageRetentionPeriod: 60, ReceiveWaitTime: 5,
	}
	_, _, wait, err := receiveTiming(target.Account, target, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, wait)

	override := 3
	_, _, wait, err = receiveTiming(target.Account, target, nil, &override)
	require.NoError(t, err)
	assert.Equal(t, 3*time.Second, wait)
}
