package reconciler

import "sync"

// workQueue holds the keys a per-key loop has yet to reconcile, in arrival
// order and without duplicates.
//
// It is not the correctness boundary. Whatever the queue drops or never learns
// about is still covered by the resync, which reconciles the whole set: a key
// deleted while the watch was down is never enqueued by anything, because
// nobody observes an absence.
type workQueue struct {
	mu sync.Mutex
	// order preserves arrival order; pending is the same set, for the
	// membership test that keeps a key from being queued twice.
	order   []string
	pending map[string]struct{}
	// whole records that something asked for a whole-set pass — an update that
	// could not be attributed to a key, or a watch that dropped and lost the
	// gap. It outranks the keys rather than joining them.
	whole bool
}

// add queues keys that are not already waiting.
func (q *workQueue) add(keys ...string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pending == nil {
		q.pending = map[string]struct{}{}
	}
	for _, key := range keys {
		if _, ok := q.pending[key]; ok {
			continue
		}
		q.pending[key] = struct{}{}
		q.order = append(q.order, key)
	}
}

// addWhole asks for a whole-set pass instead of per-key work.
func (q *workQueue) addWhole() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.whole = true
}

// take removes and returns everything queued so far. A key taken here is no
// longer pending, so a change arriving while it is being reconciled queues it
// again rather than being folded into the pass that is already running.
func (q *workQueue) take() (whole bool, keys []string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	whole, keys = q.whole, q.order
	q.whole, q.order, q.pending = false, nil, nil
	return whole, keys
}
