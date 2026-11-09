package featurelevel

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	// DefaultAckTimeout bounds how long a rollback waits for every member to
	// acknowledge it.
	DefaultAckTimeout = 60 * time.Second
	ackPollInterval   = 2 * time.Second
)

// InFlightAbove counts the callers the replying node has admitted for levels
// above target.
func (r *StatusReply) InFlightAbove(target int) int {
	total := 0
	for n, c := range r.InFlight {
		if n > target {
			total += c
		}
	}
	return total
}

// RollbackDeps are the collaborators a rollback runs against.
type RollbackDeps struct {
	Store *Store
	// Probe asks every configured member for its status.
	Probe func(ctx context.Context) []NodeStatus
	// Drains returns the registered drain checks above a target.
	Drains func(target int) map[int]DrainCheck
	// Build is the running CLI's own build.
	Build int
	// AckTimeout and PollInterval default to DefaultAckTimeout and 2s.
	AckTimeout   time.Duration
	PollInterval time.Duration
}

// UnackedError names the members that did not acknowledge the rollback.
type UnackedError struct {
	Target   int
	Revision uint64
	Nodes    []string
	Reasons  map[string]string
}

func (e *UnackedError) Error() string {
	parts := make([]string, 0, len(e.Nodes))
	for _, n := range e.Nodes {
		parts = append(parts, n+" ("+e.Reasons[n]+")")
	}
	return fmt.Sprintf("rollback to %d is recorded but not acknowledged by: %s; it stays in place, rerun the same command to resume or use --cancel",
		e.Target, strings.Join(parts, ", "))
}

// DrainOutstandingError names a level whose drain check found work left.
type DrainOutstandingError struct {
	Level       int
	Outstanding int
	Err         error
}

func (e *DrainOutstandingError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("rollback blocked: the level %d drain check failed: %v; the rollback stays in place", e.Level, e.Err)
	}
	return fmt.Sprintf("rollback blocked: level %d has %d outstanding item(s) to drain; the rollback stays in place, rerun the same command once they are gone", e.Level, e.Outstanding)
}

// ackedBy returns the nodes that have not acknowledged: each must have answered
// with a record at or after rev and nothing in flight above target.
func ackedBy(statuses []NodeStatus, rev uint64, target int) (nodes []string, reasons map[string]string) {
	reasons = map[string]string{}
	for _, s := range statuses {
		switch {
		case s.Reply == nil:
			reasons[s.Node] = "no usable answer: " + s.Reason
		case s.Reply.Revision < rev:
			reasons[s.Node] = fmt.Sprintf("has read revision %d, need %d", s.Reply.Revision, rev)
		case s.Reply.InFlightAbove(target) > 0:
			reasons[s.Node] = fmt.Sprintf("%d in flight above level %d", s.Reply.InFlightAbove(target), target)
		default:
			continue
		}
		nodes = append(nodes, s.Node)
	}
	slices.Sort(nodes)
	return nodes, reasons
}

// RunRollback lowers the cluster level to target. It records the rollback, waits
// until every member has read it and has nothing admitted above target, runs
// every drain check for the levels being given up, and only then lowers the
// level. Any failure leaves the rollback recorded so the same command resumes.
func RunRollback(ctx context.Context, target int, d RollbackDeps) (Record, error) {
	rec, rev, err := d.Store.StartRollback(ctx, target, d.Build)
	if err != nil {
		return rec, err
	}
	recorded := rec.Level

	timeout, interval := d.AckTimeout, d.PollInterval
	if timeout <= 0 {
		timeout = DefaultAckTimeout
	}
	if interval <= 0 {
		interval = ackPollInterval
	}
	deadline := time.Now().Add(timeout)
	for {
		pctx, cancel := context.WithDeadline(ctx, deadline)
		statuses := d.Probe(pctx)
		cancel()
		nodes, reasons := ackedBy(statuses, rev, target)
		if len(statuses) > 0 && len(nodes) == 0 {
			break
		}
		if ctx.Err() != nil || !time.Now().Add(interval).Before(deadline) {
			if len(statuses) == 0 {
				return rec, errors.New("rollback has no configured members to acknowledge it")
			}
			return rec, &UnackedError{Target: target, Revision: rev, Nodes: nodes, Reasons: reasons}
		}
		select {
		case <-ctx.Done():
		case <-time.After(interval):
		}
	}

	checks := d.Drains(target)
	for n := target + 1; n <= recorded; n++ {
		check := checks[n]
		if check == nil {
			return rec, &DrainOutstandingError{Level: n, Err: errors.New("no drain check is registered")}
		}
		outstanding, cerr := check(ctx)
		if cerr != nil {
			return rec, &DrainOutstandingError{Level: n, Err: cerr}
		}
		if outstanding > 0 {
			return rec, &DrainOutstandingError{Level: n, Outstanding: outstanding}
		}
	}
	return d.Store.FinishRollback(ctx)
}

func (e *DrainOutstandingError) Unwrap() error { return e.Err }
