package viperblock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	viperblocklegacyv1 "github.com/mulgadc/spinifex/contracts/viperblockd/legacy/v1"
	"github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	hostprocess "github.com/mulgadc/spinifex/spinifex/runtime/host/process"
	"github.com/nats-io/nats.go/jetstream"
)

// The fence is what makes the volume lease more than a request. Acquiring it is
// checked once, at mount; nothing rechecks it afterwards, so a node that loses
// its lease while an engine is open goes on writing into a volume another node
// now owns. Both copies then advance, and whichever reaches the backend last
// wins by arrival order rather than by ownership.
//
// Stopping the loser locally is the cheap half of that problem. It does not
// make a stale write impossible — only an epoch the backend enforces does that
// — but it closes the window from the lease TTL down to one renewal interval,
// and it costs nothing on the write path.

// fenceKillTimeout bounds the wait for nbdkit to exit after SIGKILL. Generous
// for an uninterruptible sleep in the storage path; short enough that a fence
// that will never complete is reported rather than waited on.
const fenceKillTimeout = 30 * time.Second

// onVolumeLeaseLost fences a volume this node can no longer show it owns.
// Called from the renewal goroutine's failure path, on its own goroutine:
// fencing releases the lease, and releasing waits for that goroutine to exit.
//
// It never tries to reclaim. Reclaiming looks attractive — an entry that merely
// aged out under JetStream pressure has no new holder, and stopping a healthy
// guest for that is availability spent on nothing — but the two errors are not
// symmetric. A false fence costs one guest a restart. A false reclaim leaves two
// nodes writing one volume. Reclaiming correctly needs a state transition that
// atomically replaces the lost in-memory lease and starts a new renewal loop,
// and that is an optimisation, not a safety property.
//
// The holder is read only to say who won, which is what the operator needs and
// not something the decision turns on.
func (cfg *Config) onVolumeLeaseLost(ctx context.Context, volumeName string, kind leaseLossKind) {
	if cfg.leases == nil {
		return
	}

	// Read only to name the winner for the operator. No holder, or one this node
	// cannot read, still fences: it cannot show it is the only writer, and
	// continuing to write is the one option that can corrupt.
	winner := "unknown"
	if owner, held := cfg.leases.currentOwner(ctx, volumeName); held && owner != cfg.leases.owner {
		winner = owner
	}

	outcome := otelsetup.FenceOutcomeExpired
	switch {
	case kind == leaseLostStalled:
		outcome = otelsetup.FenceOutcomeStalled
	case winner != "unknown":
		outcome = otelsetup.FenceOutcomeTaken
	}
	cfg.fenceVolume(ctx, volumeName, winner, outcome)
}

// fenceVolume tears this node's export down because the volume belongs to
// somebody else now. The guest loses its disk immediately, which is the point:
// an export left up is a second writer.
//
// Deliberately does not seal. This node's copy is the stale one, and sealing it
// would publish an older state over the winner's. For the same reason the dirty
// marker is left alone — the winner has taken it over and it now names them.
func (cfg *Config) fenceVolume(ctx context.Context, volumeName, winner string, outcome otelsetup.FenceOutcome) {
	matched, found := cfg.lookupMountedVolume(volumeName)
	if !found {
		// The export went away on its own between losing the lease and getting
		// here, which is the ordinary unmount racing this path. Nothing to do.
		slog.InfoContext(ctx, "volume lease lost after the export had already gone", "volume", volumeName)
		return
	}

	slog.ErrorContext(ctx, "fencing volume: the lease moved while this node had it open, so the guest is losing its disk",
		"volume", volumeName, "previous_owner", cfg.leases.owner, "winner", winner,
		"pid", matched.PID)

	if err := cfg.tearDownExport(ctx, matched, "fence"); err != nil {
		slog.ErrorContext(ctx, "fence FAILED: nbdkit did not exit, so this node is still a writer on a volume it does not own",
			"volume", volumeName, "pid", matched.PID, "winner", winner, "err", err)
		otelsetup.RecordVolumeFence(ctx, otelsetup.FenceOutcomeKillFailed)
		if !cfg.writerAlive(matched.PID) {
			// The kill was refused rather than ignored, so nothing here says what
			// happened to the writer. The export stays registered and the lease
			// stays held, which is the only reading of "unknown" that is safe.
			slog.ErrorContext(ctx, "fence FAILED with no writer to watch: the export stays registered and the lease held",
				"volume", volumeName, "pid", matched.PID, "winner", winner)
			return
		}
		cfg.awaitFencedWriterExit(ctx, volumeName, matched, winner, outcome)
		return
	}

	otelsetup.RecordVolumeFence(ctx, outcome)
	cfg.publishVolumeFenced(ctx, volumeName, winner)
}

// defaultFenceWatchInterval is how often a fence that did not complete re-checks
// and re-reports. One log line and one metric sample per interval, for a
// condition that must not be reported once and then look resolved.
const defaultFenceWatchInterval = 30 * time.Second

// writerAlive reports whether an export's process is still running, honouring a
// test's injected probe if there is one.
func (cfg *Config) writerAlive(pid int) bool {
	if cfg.processAlive != nil {
		return cfg.processAlive(pid)
	}
	return hostprocess.ProcessAlive(pid)
}

// fenceWatchInterval is how often a failed fence re-checks its writer.
func (cfg *Config) fenceWatchInterval() time.Duration {
	if cfg.fenceWatchEvery > 0 {
		return cfg.fenceWatchEvery
	}
	return defaultFenceWatchInterval
}

// awaitFencedWriterExit keeps a failed fence open until its writer is gone. It
// is entered only with that writer observed alive, because a PID this node
// cannot see says nothing about whether it exited.
//
// A SIGKILL that has not taken effect is a process in uninterruptible sleep with
// the signal already pending. It exits when its I/O completes, so there is
// nothing to re-send and nothing else to try — and because it has not exited, its
// PID cannot have been reused, which is what makes waiting safe where re-killing
// would not be.
//
// What there is to do is not stop watching. The teardown deliberately left the
// registry entry and the lease in place: releasing a lease while still a writer
// would hand the volume on with this node still on it. Both are finished here,
// once the exit is confirmed. Until then the failure is re-reported every
// interval, because a second writer that is only in one old log line is a second
// writer nobody knows about.
func (cfg *Config) awaitFencedWriterExit(ctx context.Context, volumeName string,
	matched MountedVolume, winner string, outcome otelsetup.FenceOutcome) {
	began := time.Now()
	for {
		select {
		case <-ctx.Done():
			slog.ErrorContext(ctx, "fence never completed: stopping with a live writer on a volume this node does not own",
				"volume", volumeName, "pid", matched.PID, "winner", winner,
				"unfenced_ms", otelsetup.Millis(time.Since(began)))
			return
		case <-time.After(cfg.fenceWatchInterval()):
		}

		if cfg.writerAlive(matched.PID) {
			slog.ErrorContext(ctx, "fence still FAILED: this node is writing a volume it does not own",
				"volume", volumeName, "pid", matched.PID, "winner", winner,
				"unfenced_ms", otelsetup.Millis(time.Since(began)))
			otelsetup.RecordVolumeFence(ctx, otelsetup.FenceOutcomeKillFailed)
			continue
		}

		cfg.finishTeardown(ctx, matched, "fence")
		slog.WarnContext(ctx, "fence completed late: the writer finally exited",
			"volume", volumeName, "pid", matched.PID, "winner", winner,
			"unfenced_ms", otelsetup.Millis(time.Since(began)))
		otelsetup.RecordVolumeFence(ctx, outcome)
		cfg.publishVolumeFenced(ctx, volumeName, winner)
		return
	}
}

// abandonVolume tears down an export because the instance that was using it
// belongs to another node now, and this node has just been told so.
//
// It is the returning node's half of the fence, and it is needed for a reason
// that is easy to miss: stopping the local guest is not enough. The export
// outlives it, and the export holds the volume lease, which this node goes on
// renewing because nothing is wrong with it. A lease held by a healthy node
// never expires, so the winner can never acquire, and the instance ends up
// stopped here and unable to start anywhere — recovery retries until its budget
// runs out and then gives up on a guest that was never unrecoverable.
//
// Like the fence it does not seal, for the same reason: this node's copy is the
// stale one, and sealing would publish an older state over the winner's.
//
// Unlike the fence, the lease is not lost here, so releasing it deletes the key
// rather than leaving it to expire — the winner gets the volume on the next
// attempt instead of waiting out the TTL.
func (cfg *Config) abandonVolume(ctx context.Context, volumeName, reason string) (bool, error) {
	matched, found := cfg.lookupMountedVolume(volumeName)
	if !found {
		// The ordinary case: the caller is reconciling, and most volumes it asks
		// about were never exported here or have already gone.
		slog.InfoContext(ctx, "volume abandon: not exported here, nothing to tear down",
			"volume", volumeName, "reason", reason)
		return false, nil
	}

	slog.WarnContext(ctx, "abandoning volume: the instance using it is owned by another node now",
		"volume", volumeName, "owner", cfg.leaseOwner(), "pid", matched.PID, "reason", reason)

	if err := cfg.tearDownExport(ctx, matched, "abandon"); err != nil {
		slog.ErrorContext(ctx, "volume abandon FAILED: nbdkit did not exit, so this node still holds the lease "+
			"and the volume cannot start anywhere else",
			"volume", volumeName, "pid", matched.PID, "err", err)
		return false, err
	}

	slog.InfoContext(ctx, "volume abandoned", "volume", volumeName, "reason", reason)
	return true, nil
}

// lookupMountedVolume reports this node's export for volumeName, if it has one.
func (cfg *Config) lookupMountedVolume(volumeName string) (MountedVolume, bool) {
	cfg.mu.Lock()
	defer cfg.mu.Unlock()
	for _, volume := range cfg.MountedVolumes {
		if volume.Name == volumeName {
			return volume, true
		}
	}
	return MountedVolume{}, false
}

// tearDownExport removes an export without sealing it: subscriptions, the
// metadata flushes, nbdkit, the registry entry, the socket and the lease.
//
// Shared by the fence and the abandon because the teardown is the same act
// either way — what differs is who decided and what it means. The kill is the
// step nothing may run ahead of: everything after it assumes the writer is gone,
// and reporting a teardown that has not killed nbdkit is the failure that lets a
// second writer keep going while the cluster believes it stopped.
func (cfg *Config) tearDownExport(ctx context.Context, matched MountedVolume, why string) error {
	if matched.ConfigSub != nil {
		if err := matched.ConfigSub.Unsubscribe(); err != nil {
			slog.ErrorContext(ctx, why+": unsubscribe config topic", "volume", matched.Name, "err", err)
		}
	}
	unsubscribeOwnerSubjects(matched.Name, matched.OwnerSubs)

	// Detach before the kill so the state-tracking VB stops its background
	// flushes. Killing nbdkit stops the data path; this stops the metadata one.
	if matched.VB != nil {
		matched.VB.Detach()
	}

	if err := hostprocess.ForceKillProcess(matched.PID, fenceKillTimeout); err != nil {
		return err
	}

	cfg.finishTeardown(ctx, matched, why)
	return nil
}

// finishTeardown is everything after the writer is confirmed gone. Split out
// because a fence whose kill did not take effect has to run it later rather than
// not at all, and because every line of it assumes the process has exited.
func (cfg *Config) finishTeardown(ctx context.Context, matched MountedVolume, why string) {
	cfg.mu.Lock()
	for i, volume := range cfg.MountedVolumes {
		if volume.Name == matched.Name {
			cfg.MountedVolumes = append(cfg.MountedVolumes[:i], cfg.MountedVolumes[i+1:]...)
			break
		}
	}
	cfg.mu.Unlock()

	if matched.Socket != "" {
		if err := os.Remove(matched.Socket); err != nil && !os.IsNotExist(err) {
			slog.ErrorContext(ctx, why+": could not remove nbd socket", "socket", matched.Socket, "err", err)
		}
	}

	// Stops the renewal goroutine and drops the local entry. It deletes the key
	// only when this node still holds it, so a lost lease cannot evict the
	// winner and a held one does not make the volume wait out its TTL.
	cfg.releaseVolumeLease(ctx, matched.Lease)
}

// publishVolumeFenced tells the local daemon a guest is now running against a
// disk that is gone. Best effort: the fence has already happened, and a guest
// left running with dead I/O is a worse outcome to report than to cause.
func (cfg *Config) publishVolumeFenced(ctx context.Context, volumeName, winner string) {
	if cfg.nc == nil {
		return
	}
	event := viperblocklegacyv1.VolumeFencedEvent{
		Volume: volumeName,
		Node:   cfg.leaseOwner(),
		Winner: winner,
		Reason: fmt.Sprintf("volume lease moved to %s while this node had the volume open", winner),
	}
	payload, err := json.Marshal(event)
	if err != nil {
		slog.ErrorContext(ctx, "fence: marshal fenced event", "volume", volumeName, "err", err)
		return
	}
	subject := viperblocklegacyv1.VolumeFencedSubject(cfg.NodeName)
	if err := cfg.nc.Publish(subject, payload); err != nil {
		slog.ErrorContext(ctx, "fence: could not announce the fenced volume, the guest will be left with dead I/O",
			"volume", volumeName, "subject", subject, "err", err)
	}
}

// currentOwner reports who holds volumeName's lease now, and whether anyone
// does. An unreadable entry is reported as unheld: the caller fences on that,
// which is the safe reading of "cannot tell".
func (l *volumeLeases) currentOwner(ctx context.Context, volumeName string) (string, bool) {
	entry, err := l.kv.Get(ctx, volumeName)
	if err != nil {
		if !errors.Is(err, jetstream.ErrKeyNotFound) {
			slog.WarnContext(ctx, "volume lease: could not read the holder after losing it",
				"volume", volumeName, "err", err)
		}
		return "", false
	}
	var record volumeLeaseRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		slog.WarnContext(ctx, "volume lease: unreadable holder record", "volume", volumeName, "err", err)
		return "", false
	}
	return record.Owner, record.Owner != ""
}

// bindLeaseFence points a lease store's loss callback at this Config, so a
// renewal that discovers the lease has moved reaches the fence.
func (cfg *Config) bindLeaseFence(leases *volumeLeases) *volumeLeases {
	if leases != nil {
		leases.onLost = cfg.onVolumeLeaseLost
	}
	return leases
}
