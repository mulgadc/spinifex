package handlers_imds

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// roleMiss says why IMDS resolved no instance role. Every value ends in an
// empty role list or a 404, which an SDK reports as "no EC2 IMDS role found"
// and then keeps signing with whatever it already holds.
type roleMiss string

const (
	// roleMissNone means a role was resolved.
	roleMissNone roleMiss = ""

	// roleMissInstanceUnresolved: DescribeInstances returned nothing for the
	// ENI's instance. Normal while an instance launches or terminates, a fault
	// once the instance is running and its agents are calling the API.
	roleMissInstanceUnresolved roleMiss = "instance_unresolved"

	// roleMissNoProfile: the instance record carries no instance profile ARN.
	roleMissNoProfile roleMiss = "no_instance_profile"

	// roleMissProfileDeleted: the ARN is set but IAM no longer has the profile.
	roleMissProfileDeleted roleMiss = "instance_profile_deleted"
)

// roleMissLogInterval throttles the warning per instance and reason. An SDK
// that cannot refresh retries indefinitely — one stuck guest produced 42,517
// role-list requests over five days — so logging every miss would bury the
// signal it is meant to raise.
const roleMissLogInterval = 5 * time.Minute

// roleMissLogger warns the first time an instance cannot be given a role and
// then at most once per interval, so a stuck fleet stays legible.
type roleMissLogger struct {
	interval time.Duration
	now      func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time
}

func newRoleMissLogger(now func() time.Time) *roleMissLogger {
	return &roleMissLogger{interval: roleMissLogInterval, now: now, seen: make(map[string]time.Time)}
}

// warn logs the miss unless the same instance and reason were logged inside the
// interval. Returns whether it logged, which the tests assert on.
func (l *roleMissLogger) warn(ctx context.Context, eni *eniFacts, reason roleMiss) bool {
	if l == nil || reason == roleMissNone {
		return false
	}

	key := eni.instanceID + "\x00" + string(reason)
	now := l.now()

	l.mu.Lock()
	last, ok := l.seen[key]
	if ok && now.Sub(last) < l.interval {
		l.mu.Unlock()
		return false
	}
	l.seen[key] = now
	l.mu.Unlock()

	// Warn, not Error: on a launching or terminating instance this is expected.
	// It is the persistence that matters, which is what the interval exposes.
	slog.WarnContext(ctx, "IMDS: no instance role to serve; a guest holding expired credentials cannot refresh",
		"reason", string(reason),
		"instance_id", eni.instanceID,
		"eni_id", eni.eniID,
		"account_id", eni.iamAccountID())
	return true
}

// sweep drops entries older than twice the interval so the map is bounded by
// the instances currently missing a role, not by every instance ever seen.
func (l *roleMissLogger) sweep(now time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, last := range l.seen {
		if now.Sub(last) > 2*l.interval {
			delete(l.seen, key)
		}
	}
}
