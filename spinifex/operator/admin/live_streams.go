package admin

import "sync"

// The caps on concurrent live metrics streams one gateway holds. Each stream
// keeps a NATS subscription open, so these bound the gateway, not the sampler.
const (
	liveStreamsPerAccount = 10
	liveStreamsTotal      = 200
)

// streamLimiter counts the live streams open on this gateway, per account and
// in total, and refuses one past either cap.
type streamLimiter struct {
	mu         sync.Mutex
	perAccount int
	total      int
	open       map[string]int
	all        int
}

func newStreamLimiter(perAccount, total int) *streamLimiter {
	return &streamLimiter{perAccount: perAccount, total: total, open: make(map[string]int)}
}

// acquire takes a slot for accountID. ok is false when a cap is reached, and
// accountFull says which one.
func (l *streamLimiter) acquire(accountID string) (release func(), ok, accountFull bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.open[accountID] >= l.perAccount {
		return nil, false, true
	}
	if l.all >= l.total {
		return nil, false, false
	}
	l.open[accountID]++
	l.all++
	var once sync.Once
	return func() { once.Do(func() { l.release(accountID) }) }, true, false
}

func (l *streamLimiter) release(accountID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all--
	if l.open[accountID]--; l.open[accountID] <= 0 {
		delete(l.open, accountID)
	}
}

// liveStreams is process-wide because a gateway process holds one NATS
// connection that every stream's subscription shares.
var liveStreams = newStreamLimiter(liveStreamsPerAccount, liveStreamsTotal)
