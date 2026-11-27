package daemon

import (
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
	// again when reclaim lowers memory.current with no launch or terminate to trigger it.
	exportResync = 10 * time.Second
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
// the larger of what admitted exports are charged and what the cgroup holds now, so
// launches admitted but not yet open still count, and so do attached data volumes. Pure.
func exportCount(n int, limitMiB, currentMiB int64, exports int, perExportMiB int64) int {
	if n <= 0 || perExportMiB <= 0 {
		return n
	}
	budget := limitMiB - limitMiB*exportMarginPct/100
	used := max(int64(exports)*perExportMiB, currentMiB)
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
	return exportCount(n, limit, current, rm.exports, int64(rm.nbdkitMainMiB))
}

// resyncSubscriptions re-evaluates every type's subscription on a timer.
func (rm *ResourceManager) resyncSubscriptions(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for range t.C {
		rm.updateInstanceSubscriptions()
	}
}
