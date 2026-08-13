package ebsctl

import (
	"errors"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"
)

// msFloat converts a duration to fractional milliseconds, the unit every
// SeriesStats field is reported in.
func msFloat(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// errCodeMessage extracts an AWS error code/message pair from err. Non-AWS
// errors (e.g. a transport failure) get a synthetic "non-aws-error" code so
// they still show up in the tally instead of vanishing into "unknown".
func errCodeMessage(err error) (code, message string) {
	if err == nil {
		return "", ""
	}
	var aerr awserr.Error
	if errors.As(err, &aerr) {
		return aerr.Code(), aerr.Message()
	}
	return "non-aws-error", err.Error()
}

// collector accumulates one operation's samples/errors across concurrent
// workers. Safe for concurrent use.
type collector struct {
	mu             sync.Mutex
	api            []float64
	settle         []float64
	settleTimeouts int
	tally          *ErrorTally
	attempts       int
}

func newCollector() *collector {
	return &collector{tally: newErrorTally()}
}

// record logs one attempt. Exactly one of (err != nil) or
// (api recorded, optionally settle/settleTimedOut) applies:
//   - err != nil: the API call itself failed; settle/settleTimedOut are
//     ignored since there is nothing to settle.
//   - err == nil, settleTimedOut: the call succeeded but the resource never
//     reached the target state within budget.
//   - err == nil, settle != nil: the call succeeded and settle records how
//     long the resource took to reach the target state.
//   - err == nil, settle == nil, !settleTimedOut: the call succeeded and
//     settle time isn't tracked for this operation (e.g. DescribeVolumes).
func (c *collector) record(api time.Duration, settle *time.Duration, settleTimedOut bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts++
	if err != nil {
		code, msg := errCodeMessage(err)
		c.tally.add(code, msg)
		return
	}
	c.api = append(c.api, msFloat(api))
	switch {
	case settleTimedOut:
		c.settleTimeouts++
	case settle != nil:
		c.settle = append(c.settle, msFloat(*settle))
	}
}

// result finalises the collected samples into an OpResult named name.
func (c *collector) result(name string) *OpResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	res := &OpResult{
		Operation:      name,
		Attempts:       c.attempts,
		APILatency:     computeSeries(c.api),
		SettleTimeouts: c.settleTimeouts,
		Errors:         *c.tally,
		ErrorRate:      c.tally.rate(c.attempts),
	}
	if len(c.settle) > 0 {
		s := computeSeries(c.settle)
		res.SettleTime = &s
	}
	return res
}

// parallelFor runs fn across `concurrency` workers (>=1). Each worker first
// runs `warmup` discarded iterations, then `iterations` measured ones. Warm-up
// is per-worker, not globally barriered — for the default concurrency=1 this
// is exact; at higher concurrency it's an approximation (workers' warm-up
// windows can overlap in wall-clock time), which is an acceptable trade for
// not needing a synchronization barrier no one asked for.
func parallelFor(concurrency, warmup, iterations int, fn func(workerID, iter int, warm bool)) {
	if concurrency < 1 {
		concurrency = 1
	}
	var wg sync.WaitGroup
	for w := range concurrency {
		wg.Go(func() {
			for i := range warmup {
				fn(w, i, true)
			}
			for i := range iterations {
				fn(w, i, false)
			}
		})
	}
	wg.Wait()
}

// drainDelete concurrently drains ids across `concurrency` workers, calling
// del for each and recording the result into the returned collector. The
// first warmupCount completions (in dequeue order, which is not necessarily
// creation order once concurrency>1) are discarded — a work-queue analogue
// of parallelFor's per-worker warm-up, needed because a delete pass doesn't
// have a natural "iterations per worker" shape: it must delete exactly the
// set of ids a paired create pass produced, however many that turned out to
// be if some creates failed.
func drainDelete(ids []string, concurrency, warmupCount int, del func(id string) (api time.Duration, settle *time.Duration, settleTimedOut bool, err error)) *collector {
	c := newCollector()
	if concurrency < 1 {
		concurrency = 1
	}
	idCh := make(chan string, len(ids))
	for _, id := range ids {
		idCh <- id
	}
	close(idCh)

	var mu sync.Mutex
	warmLeft := warmupCount
	var wg sync.WaitGroup
	for range concurrency {
		wg.Go(func() {
			for id := range idCh {
				mu.Lock()
				warm := warmLeft > 0
				if warm {
					warmLeft--
				}
				mu.Unlock()

				api, settle, timedOut, err := del(id)
				if !warm {
					c.record(api, settle, timedOut, err)
				}
			}
		})
	}
	wg.Wait()
	return c
}
