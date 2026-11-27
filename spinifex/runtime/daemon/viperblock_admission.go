package daemon

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"
)

// Every guest's nbdkit runs inside viperblock's service cgroup, so its memory limit, not
// host RAM, bounds how many root volumes a node can serve. At memory.high the kernel
// throttles the whole service and its lease renewals lapse, fencing every guest.
const (
	viperblockCgroup = "/sys/fs/cgroup/spinifex.slice/spinifex-system.slice/spinifex-viperblock.service"
	// exportMarginPct of the limit is kept free: viperblock's own working set grows with load.
	exportMarginPct = 5
	// exportResync re-checks the limit, so a node that stopped answering a type answers
	// again when reclaim or a detach lowers memory.current with no terminate to trigger it.
	exportResync = 10 * time.Second
	// exportPending is how long an admitted export is charged on top of memory.current,
	// before it is assumed to show there. It outlasts the 180 s a launch waits for a
	// network-backed guest to open its drives, so an export that opens slowly stays counted.
	exportPending = 5 * time.Minute
)

// readViperblockMemMiB returns viperblock's cgroup memory limit and use in MiB. It is
// false when the cgroup is absent (a node without viperblock) or has no limit.
func readViperblockMemMiB() (limitMiB, currentMiB int64, ok bool) {
	limit, ok := readCgroupMiB(viperblockCgroup + "/memory.high")
	if !ok {
		if limit, ok = readCgroupMiB(viperblockCgroup + "/memory.max"); !ok {
			return 0, 0, false
		}
	}
	current, ok := readCgroupMiB(viperblockCgroup + "/memory.current")
	if !ok {
		return 0, 0, false
	}
	return limit, current, true
}

func readCgroupMiB(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	return parseCgroupMiB(string(data))
}

// parseCgroupMiB reads a cgroup memory value in bytes; "max" is no limit. Pure.
func parseCgroupMiB(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "max" {
		return 0, false
	}
	b, err := strconv.ParseInt(s, 10, 64)
	if err != nil || b < 0 {
		return 0, false
	}
	return b >> 20, true
}

// exportCount clamps n launches to the root volumes that fit under the limit. The use is
// the cgroup's reading plus the exports admitted too recently to show in it, or the
// charge of every admitted export if that is larger. Attached data volumes show in the
// reading. Pure.
func exportCount(n int, limitMiB, currentMiB int64, exports, pending int, perExportMiB int64) int {
	if n <= 0 || perExportMiB <= 0 {
		return n
	}
	budget := limitMiB - limitMiB*exportMarginPct/100
	used := max(int64(exports)*perExportMiB, currentMiB+int64(pending)*perExportMiB)
	fit := (budget - used) / perExportMiB
	if fit <= 0 {
		return 0
	}
	return int(min(int64(n), fit))
}

// exportGate is exportCount with the node's own reading. A failed read fails open,
// as the live-memory gate does. Caller holds rm.mu.
func (rm *ResourceManager) exportGate(n int) int {
	if rm.readViperblockMem == nil || n <= 0 {
		return n
	}
	limit, current, ok := rm.readViperblockMem()
	if !ok {
		return n
	}
	return exportCount(n, limit, current, rm.exports, rm.pendingExports(time.Now()), int64(rm.nbdkitMainMiB))
}

// pendingExports counts admissions younger than exportPending. Caller holds rm.mu.
func (rm *ResourceManager) pendingExports(now time.Time) int {
	n := 0
	for _, at := range rm.pending {
		if now.Sub(at) < exportPending {
			n++
		}
	}
	return n
}

// exportAdmitted records one admitted export, dropping expired pending entries.
// Caller holds rm.mu for write.
func (rm *ResourceManager) exportAdmitted(now time.Time) {
	rm.exports++
	kept := rm.pending[:0]
	for _, at := range rm.pending {
		if now.Sub(at) < exportPending {
			kept = append(kept, at)
		}
	}
	rm.pending = append(kept, now)
}

// exportOpened clears the oldest pending charge: an export that has opened is in
// memory.current, and charging it as pending too counts it twice, so during a launch
// burst a node looked full at half its room. The window stays as the bound for a
// charge whose open is never reported.
func (rm *ResourceManager) exportOpened() {
	rm.mu.Lock()
	if len(rm.pending) > 0 {
		rm.pending = rm.pending[1:]
	}
	rm.mu.Unlock()
}

// exportReleased records one closed export. Pending charges are left to expire: a release
// cannot tell which launch it belongs to, so a rolled-back launch holds its slot until then,
// which only refuses sooner. Caller holds rm.mu for write.
func (rm *ResourceManager) exportReleased() {
	rm.exports = max(rm.exports-1, 0)
}

// resyncSubscriptions re-evaluates every type's subscription on a timer until ctx ends.
func (rm *ResourceManager) resyncSubscriptions(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rm.updateInstanceSubscriptions()
		}
	}
}
