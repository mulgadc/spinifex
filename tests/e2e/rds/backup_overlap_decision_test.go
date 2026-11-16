//go:build e2e

package rds

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRotationOverlapped(t *testing.T) {
	sent := time.Date(2026, 11, 13, 3, 47, 2, 0, time.UTC)
	at := func(offset time.Duration, status string) statusSample {
		return statusSample{sent: sent.Add(offset), status: status}
	}

	cases := map[string]struct {
		samples []statusSample
		want    bool
	}{
		"a describe sent after the rotation reads backing-up": {
			samples: []statusSample{at(-50*time.Millisecond, dbInstanceBackingUp), at(20*time.Millisecond, dbInstanceBackingUp)},
			want:    true,
		},
		"a describe sent before the rotation is not evidence however late it returned": {
			samples: []statusSample{at(-time.Millisecond, dbInstanceBackingUp)},
			want:    false,
		},
		"the first describe after the rotation reads available: a late send": {
			samples: []statusSample{
				at(-50*time.Millisecond, dbInstanceBackingUp),
				at(10*time.Millisecond, "available"),
				at(60*time.Millisecond, dbInstanceBackingUp),
			},
			want: false,
		},
		"no describe was sent after the rotation": {
			samples: []statusSample{at(-50*time.Millisecond, dbInstanceBackingUp)},
			want:    false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, rotationOverlapped(tc.samples, sent))
		})
	}
}

func TestClassifyOverlapAttempt(t *testing.T) {
	sent := time.Date(2026, 11, 13, 3, 47, 2, 0, time.UTC)
	held := []statusSample{{sent: sent.Add(-50 * time.Millisecond), status: dbInstanceBackingUp},
		{sent: sent.Add(20 * time.Millisecond), status: dbInstanceBackingUp}}
	released := []statusSample{{sent: sent.Add(-50 * time.Millisecond), status: dbInstanceBackingUp},
		{sent: sent.Add(20 * time.Millisecond), status: "available"}}
	neverSeen := []statusSample{{sent: sent, status: "available"}, {sent: sent.Add(time.Second), status: "available"}}

	assert.Equal(t, overlapped, classifyOverlapAttempt(held, &sent))
	assert.Equal(t, lateSend, classifyOverlapAttempt(released, &sent))
	assert.Equal(t, missedStatus, classifyOverlapAttempt(neverSeen, nil))
}

func TestRetryUntilOverlap(t *testing.T) {
	scripted := func(results ...overlapAttempt) (func(int) overlapAttempt, *[]int) {
		var called []int
		return func(n int) overlapAttempt {
			called = append(called, n)
			return results[n-1]
		}, &called
	}

	t.Run("a missed status and a late send each open another window", func(t *testing.T) {
		attempt, called := scripted(missedStatus, lateSend, overlapped)
		n, ok := retryUntilOverlap(3, attempt)
		assert.True(t, ok)
		assert.Equal(t, 3, n)
		assert.Equal(t, []int{1, 2, 3}, *called)
	})
	t.Run("three misses end with none", func(t *testing.T) {
		attempt, called := scripted(missedStatus, lateSend, missedStatus)
		_, ok := retryUntilOverlap(3, attempt)
		assert.False(t, ok)
		assert.Equal(t, []int{1, 2, 3}, *called)
	})
	t.Run("an overlap on the first window opens no other", func(t *testing.T) {
		attempt, called := scripted(overlapped)
		n, ok := retryUntilOverlap(3, attempt)
		assert.True(t, ok)
		assert.Equal(t, 1, n)
		assert.Equal(t, []int{1}, *called)
	})
}
