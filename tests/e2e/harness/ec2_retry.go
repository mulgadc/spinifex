//go:build e2e

package harness

import (
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
)

// capacityRetryEC2 wraps the EC2 client so concurrent e2e subtests serialise
// against real cluster capacity instead of overscheduling. When the daemon
// reports InsufficientInstanceCapacity (the cluster is momentarily full) it
// waits and retries RunInstances rather than failing the test; the launch
// proceeds as soon as a running guest is torn down and frees vCPU/memory. All
// other calls pass straight through to the embedded SDK client.
type capacityRetryEC2 struct {
	ec2iface.EC2API
	// interval / maxWait override the defaults; zero means use the constants.
	// Exposed only so tests can drive the retry loop without real-time sleeps.
	interval time.Duration
	maxWait  time.Duration
}

var _ ec2iface.EC2API = (*capacityRetryEC2)(nil)

// capacityRetryMax bounds the wait so a genuinely wedged cluster (never frees
// capacity) still fails rather than hanging until the test timeout.
const (
	capacityRetryMax      = 5 * time.Minute
	capacityRetryInterval = 5 * time.Second
)

func (c *capacityRetryEC2) RunInstances(in *ec2.RunInstancesInput) (*ec2.Reservation, error) {
	interval, maxWait := c.interval, c.maxWait
	if interval <= 0 {
		interval = capacityRetryInterval
	}
	if maxWait <= 0 {
		maxWait = capacityRetryMax
	}
	deadline := time.Now().Add(maxWait)
	for attempt := 1; ; attempt++ {
		out, err := c.EC2API.RunInstances(in)
		if err == nil || !isInsufficientCapacity(err) || time.Now().After(deadline) {
			return out, err
		}
		slog.Debug("RunInstances: cluster at capacity, waiting to retry",
			"attempt", attempt, "interval", interval, "err", err)
		time.Sleep(interval)
	}
}

// isInsufficientCapacity reports whether err is the daemon's
// InsufficientInstanceCapacity admission rejection — the signal to wait for a
// guest to free resources rather than fail.
func isInsufficientCapacity(err error) bool {
	if err == nil {
		return false
	}
	var ae awserr.Error
	if errors.As(err, &ae) {
		return ae.Code() == "InsufficientInstanceCapacity"
	}
	return strings.Contains(err.Error(), "InsufficientInstanceCapacity")
}
