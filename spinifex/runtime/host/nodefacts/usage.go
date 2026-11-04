package nodefacts

import (
	"context"
	"errors"
	"sync"
	"time"

	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"golang.org/x/sys/unix"
)

// Usage probe timing: how often every mount is read, and how long one read
// may take before its row says the mount is not answering.
const (
	UsageInterval = 30 * time.Second
	UsageDeadline = 2 * time.Second
)

var errNoBlockSize = errors.New("statfs reported no block size")

type usageReading struct {
	size, used, avail uint64
	err               error
	at                time.Time
	stalled           bool
}

// UsageProber keeps the last statfs reading of each mount. A context cannot
// cancel statfs on a hung mount, so a probe runs in its own goroutine and a
// mount is not probed again until its previous call returns.
type UsageProber struct {
	// Statfs reads one mount; nil means unix.Statfs.
	Statfs   func(path string, st *unix.Statfs_t) error
	Deadline time.Duration

	mu       sync.Mutex
	readings map[string]usageReading
	inflight map[string]bool
}

// Run probes the mounts Mounts returns now and every interval until ctx ends.
func (p *UsageProber) Run(ctx context.Context, mounts func() []string, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		p.Probe(mounts())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Probe reads every mount at once and returns when each has answered or the
// deadline has passed. A mount that misses it keeps its last good reading.
func (p *UsageProber) Probe(mounts []string) {
	statfs := p.Statfs
	if statfs == nil {
		statfs = unix.Statfs
	}
	deadline := p.Deadline
	if deadline <= 0 {
		deadline = UsageDeadline
	}

	p.mu.Lock()
	if p.readings == nil {
		p.readings, p.inflight = map[string]usageReading{}, map[string]bool{}
	}
	done := map[string]chan struct{}{}
	for _, m := range mounts {
		if p.inflight[m] {
			r := p.readings[m]
			r.stalled = true
			p.readings[m] = r
			continue
		}
		p.inflight[m] = true
		ch := make(chan struct{})
		done[m] = ch
		go p.read(m, statfs, ch)
	}
	p.mu.Unlock()

	expired := time.After(deadline)
	for _, ch := range done {
		select {
		case <-ch:
		case <-expired:
			p.markStalled(done)
			return
		}
	}
}

// markStalled flags each mount whose read has not returned. read closes its
// channel under p.mu, so a closed channel here means the reading is stored.
func (p *UsageProber) markStalled(done map[string]chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for m, ch := range done {
		select {
		case <-ch:
		default:
			r := p.readings[m]
			r.stalled = true
			p.readings[m] = r
		}
	}
}

func (p *UsageProber) read(mount string, statfs func(string, *unix.Statfs_t) error, done chan struct{}) {
	var st unix.Statfs_t
	err := statfs(mount, &st)

	p.mu.Lock()
	defer p.mu.Unlock()
	defer close(done)
	p.inflight[mount] = false
	r := p.readings[mount]
	r.stalled = false
	if err != nil {
		r.err = err
		p.readings[mount] = r
		return
	}
	block := st.Frsize
	if block <= 0 {
		block = st.Bsize
	}
	if block <= 0 {
		r.err = errNoBlockSize
		p.readings[mount] = r
		return
	}
	r = usageReading{
		size:  st.Blocks * uint64(block),
		used:  (st.Blocks - st.Bfree) * uint64(block),
		avail: st.Bavail * uint64(block),
		at:    time.Now(),
	}
	p.readings[mount] = r
}

// Mounts lists the mounts of fs, the paths Probe takes.
func Mounts(fs []clusterv1.FilesystemFacts) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Mount)
	}
	return out
}

// Apply fills each filesystem's sizes from its last reading, and says on the
// row when the mount has not been read, could not be, or is not answering.
func (p *UsageProber) Apply(fs []clusterv1.FilesystemFacts) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range fs {
		r, ok := p.readings[fs[i].Mount]
		switch {
		case !ok:
			fs[i].Error = "not read yet"
			continue
		case r.stalled && r.at.IsZero():
			fs[i].Error = "not answering"
		case r.stalled:
			fs[i].Error = "not answering; sizes as of " + r.at.UTC().Format(time.RFC3339)
		case r.err != nil:
			fs[i].Error = r.err.Error()
			continue
		}
		fs[i].SizeBytes, fs[i].UsedBytes, fs[i].AvailBytes = r.size, r.used, r.avail
	}
}
