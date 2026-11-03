package viperblock

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/mulgadc/viperblock/viperblock"
)

const (
	// exportDrainDialTimeout bounds the connect to the plugin's drain socket.
	// The socket is local, so a slow connect is a wedged plugin.
	exportDrainDialTimeout = time.Second

	// exportDrainAckTimeout bounds the plugin's flush of its WAL and live
	// checkpoint to the backend, which scales with the dirty set.
	exportDrainAckTimeout = 30 * time.Second

	// maxUnixSocketPath is sun_path's size. The plugin cannot listen on a
	// longer path, so a drain socket there never exists.
	maxUnixSocketPath = 108
)

// errExportDrainFailed is a drain the plugin was asked for and did not
// confirm, so the live checkpoint cannot be shown to be current.
var errExportDrainFailed = errors.New("export drain failed")

// errExportSettling is an export whose client is connecting or disconnecting:
// it holds the volume lock without serving the drain socket, so its writes are
// neither drainable nor yet sealed. It clears once the client settles.
var errExportSettling = errors.New("export is between connections")

// drainMountedExport asks the nbdkit plugin serving volumeID on this node to
// flush its WAL and live checkpoint to the backend before a snapshot reads
// them. The plugin serves the socket only while a client is connected.
func drainMountedExport(ctx context.Context, cfg *Config, volumeID string) error {
	sockPath := filepath.Join(cfg.BaseDir, volumeID, "snapshot.sock")
	if len(sockPath) >= maxUnixSocketPath {
		return requireNoConnectedClient(ctx, cfg, volumeID, "")
	}
	start := time.Now()
	conn, err := net.DialTimeout("unix", sockPath, exportDrainDialTimeout)
	if errors.Is(err, fs.ErrNotExist) {
		return requireNoConnectedClient(ctx, cfg, volumeID, sockPath)
	}
	if err != nil {
		return fmt.Errorf("%w: %s: %w", errExportDrainFailed, volumeID, err)
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(exportDrainAckTimeout))
	buf := make([]byte, 8)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("%w: %s: read ack: %w", errExportDrainFailed, volumeID, err)
	}
	if !strings.HasPrefix(string(buf[:n]), "OK") {
		return fmt.Errorf("%w: %s: ack %q", errExportDrainFailed, volumeID, string(buf[:n]))
	}
	slog.InfoContext(ctx, "ebs.provider.snapshot.create: drained export", "volume", volumeID, "drain_ms", otelsetup.Millis(time.Since(start)))
	return nil
}

// requireNoConnectedClient proves an export serving no drain socket has nothing
// buffered. sockPath is empty when the socket path is too long for the plugin
// to listen on, so a held lock there never clears.
func requireNoConnectedClient(ctx context.Context, cfg *Config, volumeID, sockPath string) error {
	lock, err := viperblock.AcquireVolumeLock(cfg.BaseDir, volumeID)
	if errors.Is(err, viperblock.ErrVolumeLocked) {
		if sockPath == "" {
			return fmt.Errorf("%w: %s: client connected with no drain socket", errExportDrainFailed, volumeID)
		}
		slog.WarnContext(ctx, "ebs.provider.snapshot.create: volume locked with no drain socket, refusing as retry-later",
			"volume", volumeID, "socket", sockPath)
		return fmt.Errorf("%w: %s", errExportSettling, volumeID)
	}
	if err != nil {
		return fmt.Errorf("%w: %s: %w", errExportDrainFailed, volumeID, err)
	}
	if err := viperblock.ReleaseVolumeLock(lock); err != nil {
		return fmt.Errorf("%w: %s: %w", errExportDrainFailed, volumeID, err)
	}
	return requireLastClientSealed(cfg, volumeID)
}

// requireLastClientSealed refuses an export whose last client left WAL behind
// without a seal receipt newer than it. The plugin releases the lock after a
// failed seal too, so a free lock alone does not show the writes reached the backend.
func requireLastClientSealed(cfg *Config, volumeID string) error {
	if isAuxVolume(volumeID) || !volumeHoldsLocalWAL(cfg, volumeID) || sealReceiptCoversWAL(cfg, volumeID) {
		return nil
	}
	return fmt.Errorf("%w: %s: last client left unsealed WAL", errExportDrainFailed, volumeID)
}

// excludeLocalWriter proves no export on this node holds writes the backend
// lacks before a snapshot opens its own engine. The returned lock keeps a new
// writer out until the snapshot is read; it is nil when a live export was drained.
func excludeLocalWriter(ctx context.Context, cfg *Config, volumeID string) (*os.File, error) {
	lock, err := viperblock.AcquireVolumeLock(cfg.BaseDir, volumeID)
	if errors.Is(err, viperblock.ErrVolumeLocked) {
		return nil, drainMountedExport(ctx, cfg, volumeID)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errExportDrainFailed, volumeID, err)
	}
	if err := requireLastClientSealed(cfg, volumeID); err != nil {
		releaseWriterLock(ctx, lock, volumeID)
		return nil, err
	}
	return lock, nil
}

// releaseWriterLock gives up a lock excludeLocalWriter took. A failed release
// leaves only the kernel to drop it at exit, so it is logged, not returned.
func releaseWriterLock(ctx context.Context, lock *os.File, volumeID string) {
	if err := viperblock.ReleaseVolumeLock(lock); err != nil {
		slog.WarnContext(ctx, "ebs.provider.snapshot.create: failed to release volume lock", "volume", volumeID, "err", err)
	}
}
