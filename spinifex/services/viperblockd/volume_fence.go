package viperblockd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/mulgadc/spinifex/spinifex/otelsetup"
	"github.com/mulgadc/spinifex/spinifex/utils"
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

// VolumeFencedSubject is where a fenced volume is announced so the daemon on
// this node can stop the guest that was using it. Node-addressed: the guest is
// local, and every other node's daemon has nothing to do about it.
func VolumeFencedSubject(node string) string {
	if node == "" {
		return "ebs.fenced"
	}
	return "ebs." + node + ".fenced"
}

// VolumeFencedEvent announces that this node tore down a volume's export
// because the lease moved. Winner is who holds it now, for the operator reading
// the guest's stop reason.
type VolumeFencedEvent struct {
	Volume string `json:"volume"`
	Node   string `json:"node"`
	Winner string `json:"winner"`
	Reason string `json:"reason"`
}

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
func (cfg *Config) fenceVolume(ctx context.Context, volumeName, winner, outcome string) {
	cfg.mu.Lock()
	var matched MountedVolume
	found := false
	for _, volume := range cfg.MountedVolumes {
		if volume.Name == volumeName {
			matched, found = volume, true
			break
		}
	}
	cfg.mu.Unlock()

	if !found {
		// The export went away on its own between losing the lease and getting
		// here, which is the ordinary unmount racing this path. Nothing to do.
		slog.InfoContext(ctx, "volume lease lost after the export had already gone", "volume", volumeName)
		return
	}

	slog.ErrorContext(ctx, "fencing volume: the lease moved while this node had it open, so the guest is losing its disk",
		"volume", volumeName, "previous_owner", cfg.leases.owner, "winner", winner,
		"pid", matched.PID)

	if matched.ConfigSub != nil {
		if err := matched.ConfigSub.Unsubscribe(); err != nil {
			slog.ErrorContext(ctx, "fence: unsubscribe config topic", "volume", volumeName, "err", err)
		}
	}
	unsubscribeOwnerSubjects(matched.Name, matched.OwnerSubs)

	// Detach before the kill so the state-tracking VB stops its background
	// flushes. Killing nbdkit stops the data path; this stops the metadata one.
	if matched.VB != nil {
		matched.VB.Detach()
	}

	// Everything below assumes the writer is gone, so nothing below may run
	// until it is observed gone. A fence that has not killed nbdkit has fenced
	// nothing, and reporting it as done is the failure that lets a second
	// writer keep going while the cluster believes it stopped.
	if err := utils.ForceKillProcess(matched.PID, fenceKillTimeout); err != nil {
		slog.ErrorContext(ctx, "fence FAILED: nbdkit did not exit, so this node is still a writer on a volume it does not own",
			"volume", volumeName, "pid", matched.PID, "winner", winner, "err", err)
		otelsetup.RecordVolumeFence(ctx, otelsetup.FenceOutcomeKillFailed)
		return
	}

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
			slog.ErrorContext(ctx, "fence: could not remove nbd socket", "socket", matched.Socket, "err", err)
		}
	}

	// Stops the renewal goroutine and drops the local entry. The lease was
	// already flagged lost, so this cannot delete the winner's key.
	cfg.releaseVolumeLease(ctx, matched.Lease)

	otelsetup.RecordVolumeFence(ctx, outcome)
	cfg.publishVolumeFenced(ctx, volumeName, winner)
}

// publishVolumeFenced tells the local daemon a guest is now running against a
// disk that is gone. Best effort: the fence has already happened, and a guest
// left running with dead I/O is a worse outcome to report than to cause.
func (cfg *Config) publishVolumeFenced(ctx context.Context, volumeName, winner string) {
	if cfg.nc == nil {
		return
	}
	event := VolumeFencedEvent{
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
	subject := VolumeFencedSubject(cfg.NodeName)
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
