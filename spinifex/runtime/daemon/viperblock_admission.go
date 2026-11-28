package daemon

import (
	"context"
	"maps"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
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
	// exportPending is how long an admitted export not yet bound to an instance is charged
	// on top of memory.current. The owner binds it within its request, so this only ends
	// the charge of an allocation released before its instance existed.
	exportPending = 5 * time.Minute
	// exportOpenBound ends a bound charge the sweep cannot settle. It outlasts the longest
	// greeting wait (900 s) plus a cold mount.
	exportOpenBound = 20 * time.Minute
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

// pendingExports counts the admitted exports whose drive is not yet open: unbound
// admissions younger than exportPending and bound ones younger than exportOpenBound.
// Caller holds rm.mu.
func (rm *ResourceManager) pendingExports(now time.Time) int {
	n := 0
	for _, at := range rm.pending {
		if now.Sub(at) < exportPending {
			n++
		}
	}
	for _, at := range rm.bound {
		if now.Sub(at) < exportOpenBound {
			n++
		}
	}
	return n
}

// exportAdmitted records one admitted export, dropping expired pending entries.
// Caller holds rm.mu for write.
func (rm *ResourceManager) exportAdmitted(now time.Time) {
	rm.exports++
	rm.dropExpiredPending(now)
	rm.pending = append(rm.pending, now)
}

// dropExpiredPending removes the unbound and bound entries past their bound.
// Caller holds rm.mu for write.
func (rm *ResourceManager) dropExpiredPending(now time.Time) {
	kept := rm.pending[:0]
	for _, at := range rm.pending {
		if now.Sub(at) < exportPending {
			kept = append(kept, at)
		}
	}
	rm.pending = kept
	for id, at := range rm.bound {
		if now.Sub(at) >= exportOpenBound {
			delete(rm.bound, id)
		}
	}
}

// exportReleased records one closed export. A pending charge is ended by its instance
// (ExportOpened, the sweep) or by its expiry, never by a release, which cannot tell
// which launch it belongs to. Caller holds rm.mu for write.
func (rm *ResourceManager) exportReleased() {
	rm.exports = max(rm.exports-1, 0)
}

// BindExport gives the newest allocation's pending charge to instance id, keeping its
// admission time. It replaces a charge id held from an earlier, released attempt; with
// no unbound charge left (it expired) it binds nothing.
func (rm *ResourceManager) BindExport(id string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.dropExpiredPending(time.Now())
	if id == "" || len(rm.pending) == 0 {
		return
	}
	last := len(rm.pending) - 1
	if rm.bound == nil {
		rm.bound = make(map[string]time.Time)
	}
	rm.bound[id] = rm.pending[last]
	rm.pending = rm.pending[:last]
}

// ExportOpened ends instance id's pending charge: its drive is open, so its nbdkit now
// shows in memory.current. A charge already ended has no effect.
func (rm *ResourceManager) ExportOpened(id string) {
	rm.mu.Lock()
	_, held := rm.bound[id]
	delete(rm.bound, id)
	rm.mu.Unlock()
	if held {
		rm.updateInstanceSubscriptions()
	}
}

// sweepBoundExports ends the charges of instances that can no longer open a drive here.
// The VM manager is read outside rm.mu. An instance it does not know yet keeps its
// charge: a launch binds before the handler inserts it.
func (rm *ResourceManager) sweepBoundExports(now time.Time) {
	if rm.exportLive == nil {
		return
	}
	rm.mu.RLock()
	held := make(map[string]time.Time, len(rm.bound))
	maps.Copy(held, rm.bound)
	rm.mu.RUnlock()
	var dead []string
	for id := range held {
		if live, known := rm.exportLive(id); known && !live {
			dead = append(dead, id)
		}
	}
	if len(dead) == 0 {
		return
	}
	rm.mu.Lock()
	for _, id := range dead {
		if at, ok := rm.bound[id]; ok && at.Equal(held[id]) {
			delete(rm.bound, id)
		}
	}
	rm.mu.Unlock()
}

// resyncExports ends the charges of instances that can no longer open a drive, then
// re-evaluates every type's subscription, so a freed slot reopens the queue.
func (rm *ResourceManager) resyncExports(now time.Time) {
	rm.sweepBoundExports(now)
	rm.updateInstanceSubscriptions()
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
			rm.resyncExports(time.Now())
		}
	}
}

// instanceCanOpenExport is the sweep's view of a bound instance. Running follows the
// greeting; the others end a launch. Error and stopped can be retried, so they keep it.
func (d *Daemon) instanceCanOpenExport(id string) (live, known bool) {
	v, ok := d.vmMgr.Get(id)
	if !ok {
		return false, false
	}
	switch d.vmMgr.Status(v) {
	case vm.StateRunning, vm.StateStopping, vm.StateShuttingDown, vm.StateTerminated:
		return false, true
	default:
		return true, true
	}
}
