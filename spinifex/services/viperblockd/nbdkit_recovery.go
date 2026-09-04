package viperblockd

// Rebuilds cfg.MountedVolumes from nbdkit processes that survived a
// viperblockd restart, so findMountedVolume can find them again (see
// orphan_scan.go for the equivalent problem/fix on the QEMU side), and reaps
// the two classes of surviving nbdkit that recovery itself cannot adopt.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/nats-io/nats.go"
)

// nbdkitComm is the exact /proc/<pid>/comm value for an nbdkit child process
// (short enough that Linux's 15-byte comm truncation never applies).
const nbdkitComm = "nbdkit"

// vbPluginSuffix identifies our own plugin in a discovered argv, so an nbdkit
// serving something else is never adopted.
const vbPluginSuffix = "nbdkit-viperblock-plugin.so"

// discoveredNbdkit is one nbdkit process found in /proc, before corroboration.
// Exactly one of Socket/Port is set, matching whichever transport
// nbd.buildArgs put on this process's argv.
type discoveredNbdkit struct {
	PID     int
	Volume  string
	Socket  string
	Port    int
	Plugin  string
	BaseDir string
}

// scanNbdkitProcs walks procRoot for nbdkit-comm processes, parsing each
// survivor's cmdline for volume=<id> and its NBD endpoint (--unix <path> or
// -p <port>; see nbd.buildArgs). procRoot lets tests use a fabricated dir.
func scanNbdkitProcs(procRoot string) []discoveredNbdkit {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		slog.Warn("recovery: failed to read proc root", "path", procRoot, "err", err)
		return nil
	}

	var found []discoveredNbdkit
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}

		comm, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != nbdkitComm {
			continue
		}

		cmdline, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "cmdline"))
		if err != nil {
			continue
		}

		disc, ok := parseNbdkitCmdline(cmdline)
		if !ok {
			continue
		}
		disc.PID = pid
		found = append(found, disc)
	}
	return found
}

// parseNbdkitCmdline extracts the volume ID and NBD endpoint from a nbdkit
// process's NUL-separated /proc/<pid>/cmdline (matching nbd.buildArgs). ok is
// false when no volume=<id> argument was found: not one of ours.
func parseNbdkitCmdline(cmdline []byte) (disc discoveredNbdkit, ok bool) {
	args := bytes.Split(bytes.TrimRight(cmdline, "\x00"), []byte{0})
	for i := 0; i < len(args); i++ {
		arg := string(args[i])
		switch {
		case arg == "--unix" && i+1 < len(args):
			disc.Socket = string(args[i+1])
			i++
		case arg == "-p" && i+1 < len(args):
			if port, err := strconv.Atoi(string(args[i+1])); err == nil {
				disc.Port = port
			}
			i++
		case strings.HasPrefix(arg, "volume="):
			disc.Volume = strings.TrimPrefix(arg, "volume=")
		case strings.HasPrefix(arg, "base_dir="):
			disc.BaseDir = strings.TrimPrefix(arg, "base_dir=")
		case strings.HasSuffix(arg, ".so"):
			disc.Plugin = arg
		}
	}
	return disc, disc.Volume != ""
}

// corroborateNbdkit reports whether disc is this daemon's own nbdkit: our
// plugin, our base dir, and a live endpoint. nbdkit runs with -f and never
// writes the --pidfile it is given, so argv is the only evidence there is.
func corroborateNbdkit(cfg *Config, disc discoveredNbdkit) bool {
	if !strings.HasSuffix(disc.Plugin, vbPluginSuffix) {
		return false
	}
	if filepath.Clean(disc.BaseDir) != filepath.Clean(cfg.BaseDir) {
		return false
	}

	// A socket mount must still have its socket; a TCP mount leaves no
	// filesystem trace, so the parsed port is all there is to check.
	if disc.Socket == "" {
		return disc.Port > 0
	}
	info, err := os.Stat(disc.Socket)
	return err == nil && info.Mode()&os.ModeSocket != 0
}

// defaultRecoveryAttempts is how many times recovery tries to build a
// survivor's VB before giving up on it, and defaultRecoveryBackoff is the
// pause between those attempts.
//
// Recovery runs at daemon start, when JetStream is routinely still catching up
// and a KV claim can time out on its own default. One shot there orphans a
// running guest's data path for the life of the process, which is a far worse
// outcome than waiting a few seconds.
const (
	defaultRecoveryAttempts = 4
	defaultRecoveryBackoff  = 3 * time.Second
)

// rebuildMountedVolume re-derives disc's NBD URI, builds its daemon-side VB
// (cfg.buildVB, mountVolume's construction) and registers the same
// config/owner subscriptions a fresh mount would. Never touches the process.
func rebuildMountedVolume(ctx context.Context, cfg *Config, nc *nats.Conn, disc discoveredNbdkit) (MountedVolume, error) {
	attempts, backoff := cfg.recoveryBuildPolicy()
	var vb *viperblock.VB
	var lease *volumeLease
	var err error
	for attempt := 1; ; attempt++ {
		vb, _, lease, err = cfg.buildVB(ctx, disc.Volume)
		if err == nil {
			break
		}
		if attempt >= attempts {
			return MountedVolume{}, fmt.Errorf("construct VB after %d attempts: %w", attempt, err)
		}
		slog.WarnContext(ctx, "recovery: could not build VB for a surviving nbdkit, retrying",
			"pid", disc.PID, "volume", disc.Volume, "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return MountedVolume{}, fmt.Errorf("construct VB: %w", ctx.Err())
		case <-time.After(backoff):
		}
	}

	var nbdURI string
	if disc.Socket != "" {
		nbdURI = utils.FormatNBDSocketURI(disc.Socket)
	} else {
		nbdURI = utils.FormatNBDTCPURI("127.0.0.1", disc.Port)
	}

	configSub, err := nc.Subscribe(fmt.Sprintf("ebs.config.%s", disc.Volume), makeConfigUpdateHandler(vb, disc.Volume))
	if err != nil {
		slog.ErrorContext(ctx, "recovery: failed to subscribe to volume config topic", "volume", disc.Volume, "err", err)
	}

	ownerSubs := subscribeOwnerSubjects(ctx, cfg, nc, disc.Volume)

	return MountedVolume{
		Name:      disc.Volume,
		Port:      disc.Port,
		Socket:    disc.Socket,
		NBDURI:    nbdURI,
		PID:       disc.PID,
		VB:        vb,
		ConfigSub: configSub,
		OwnerSubs: ownerSubs,
		Lease:     lease,
	}, nil
}

// recoverMountedVolumes discovers, corroborates and rebuilds
// cfg.MountedVolumes from procRoot's surviving nbdkit processes. Never
// signals/kills/starts a process; failures are logged and skipped.
func recoverMountedVolumes(ctx context.Context, cfg *Config, nc *nats.Conn, procRoot string) {
	for _, disc := range scanNbdkitProcs(procRoot) {
		if !corroborateNbdkit(cfg, disc) {
			slog.Warn("recovery: nbdkit process is not this daemon's, skipping",
				"pid", disc.PID, "volume", disc.Volume, "base_dir", disc.BaseDir, "plugin", disc.Plugin)
			continue
		}

		// Two live nbdkits for one volume is the double-mount hazard itself:
		// adopt the first and reap the rest if nothing is using them.
		if _, ok := findMountedVolume(cfg, disc.Volume); ok {
			slog.Warn("recovery: volume already recovered from another nbdkit process, skipping",
				"pid", disc.PID, "volume", disc.Volume)
			reapOrphanedNbdkit(procRoot, disc, reapReasonDuplicate)
			continue
		}

		mv, err := rebuildMountedVolume(ctx, cfg, nc, disc)
		if err != nil {
			slog.Error("recovery: failed to rebuild mounted volume registry entry, skipping",
				"pid", disc.PID, "volume", disc.Volume, "err", err)
			reapOrphanedNbdkit(procRoot, disc, reapReasonUnadoptable)
			continue
		}

		cfg.mu.Lock()
		cfg.MountedVolumes = append(cfg.MountedVolumes, mv)
		cfg.mu.Unlock()

		slog.Info("recovery: rebuilt mounted volume registry entry from surviving nbdkit process",
			"pid", mv.PID, "volume", mv.Name, "socket", mv.Socket, "port", mv.Port)
	}
}

// reapReason names which of recovery's two failure-to-adopt paths produced
// an unclaimed nbdkit process, carried only for logging.
type reapReason string

const (
	reapReasonUnadoptable reapReason = "unadoptable"
	reapReasonDuplicate   reapReason = "duplicate"
	// reapReasonUnregistered is an export still serving a volume the registry
	// has no entry for, found when that volume is unmounted.
	reapReasonUnregistered reapReason = "unregistered"
)

// reapOrphanedNbdkit decides whether disc -- a corroborated nbdkit process
// recovery could not fold into MountedVolumes -- may be safely signalled.
// Reaping the wrong process destroys a running guest's data path, so disc is
// only ever signalled once a scan of every other process on the host finds no
// reference to its NBD endpoint. A referencing process, or a scan that could
// not complete, both leave it running: the caller gets a loud log either way.
// Reports whether disc was signalled.
func reapOrphanedNbdkit(procRoot string, disc discoveredNbdkit, reason reapReason) bool {
	if hidden, herr := procHidepidInvisible(procRoot); hidden {
		slog.Warn("recovery: proc mount hides other users' process entries; a live referencer could be invisible to this scan, leaving unclaimed nbdkit running",
			"pid", disc.PID, "volume", disc.Volume, "reason", reason, "err", herr)
		return false
	}

	referencingPID, referenced, err := endpointReferencer(procRoot, disc)
	if err != nil {
		slog.Warn("recovery: could not scan for processes using an unclaimed nbdkit's endpoint, leaving it running",
			"pid", disc.PID, "volume", disc.Volume, "reason", reason, "err", err)
		return false
	}
	if referenced {
		slog.Error("recovery: unclaimed nbdkit process is still referenced by a live process; left running deliberately, needs operator attention",
			"pid", disc.PID, "volume", disc.Volume, "reason", reason, "referencing_pid", referencingPID)
		return false
	}

	proc, err := os.FindProcess(disc.PID)
	if err != nil {
		slog.Error("recovery: failed to locate unclaimed nbdkit process to reap",
			"pid", disc.PID, "volume", disc.Volume, "reason", reason, "err", err)
		return false
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		slog.Error("recovery: failed to signal unclaimed nbdkit process",
			"pid", disc.PID, "volume", disc.Volume, "reason", reason, "err", err)
		return false
	}

	if disc.Socket != "" {
		if err := os.Remove(disc.Socket); err != nil && !os.IsNotExist(err) {
			slog.Warn("recovery: failed to remove socket file for reaped nbdkit process",
				"pid", disc.PID, "volume", disc.Volume, "socket", disc.Socket, "err", err)
		}
	}

	slog.Info("recovery: reaped unclaimed nbdkit process with no live referencer",
		"pid", disc.PID, "volume", disc.Volume, "reason", reason)
	return true
}

// procHidepidInvisible reports whether procRoot's mount hides another uid's
// /proc/<pid> entry outright (hidepid=invisible/2), so a referencer never
// reaches os.ReadDir. Missing mountinfo reports false; any other read error cannot be ruled out.
func procHidepidInvisible(procRoot string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, "self", "mountinfo"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return true, err
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		sep := slices.Index(fields, "-")
		if sep < 0 || sep+3 >= len(fields) || len(fields) < 6 {
			continue
		}
		if fields[4] != "/proc" || fields[sep+1] != "proc" {
			continue
		}
		opts := fields[5] + "," + fields[sep+3]
		if strings.Contains(opts, "hidepid=invisible") || strings.Contains(opts, "hidepid=2") {
			return true, nil
		}
	}
	return false, nil
}

// endpointReferencer reports whether any process other than disc's own PID
// has disc's NBD endpoint on its own /proc/<pid>/cmdline: the socket path for
// a unix mount, or the 127.0.0.1:<port> pair for a TCP one. That is how a
// guest's QEMU process is handed its drive backend at launch, and it is the
// only positive evidence available that an unclaimed nbdkit still serves a
// live guest. A non-nil error means the scan itself could not be completed,
// not that nothing was found -- callers must treat that as unknown, not safe.
func endpointReferencer(procRoot string, disc discoveredNbdkit) (pid int, found bool, err error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, false, err
	}

	for _, entry := range entries {
		candidate, cerr := strconv.Atoi(entry.Name())
		if cerr != nil || candidate <= 0 || candidate == disc.PID {
			continue
		}

		cmdline, rerr := os.ReadFile(filepath.Join(procRoot, entry.Name(), "cmdline"))
		if rerr != nil {
			// The process exited between ReadDir and this read: genuinely
			// not a referencer. Any other error (EACCES under a hidepid
			// /proc, most importantly) means the scan cannot rule this PID
			// out, so it must not be silently read as absence of evidence.
			if os.IsNotExist(rerr) {
				continue
			}
			return 0, false, rerr
		}

		if cmdlineReferencesEndpoint(cmdline, disc) {
			return candidate, true, nil
		}
	}
	return 0, false, nil
}

// cmdlineReferencesEndpoint checks a NUL-separated /proc/<pid>/cmdline for
// disc's endpoint. A socket mount's path appears verbatim in whichever
// argument named it (QEMU's nbd+unix:///?socket=<path> file= or its
// server.path=<path> -blockdev option). A TCP mount's 127.0.0.1:<port> pair
// may appear joined in one argument (a boot/EFI drive's nbd://host:port) or
// split across server.host=/server.port= within the same comma-joined
// -blockdev argument, so both shapes are checked.
func cmdlineReferencesEndpoint(cmdline []byte, disc discoveredNbdkit) bool {
	args := bytes.Split(bytes.TrimRight(cmdline, "\x00"), []byte{0})

	if disc.Socket != "" {
		needle := []byte(disc.Socket)
		for _, arg := range args {
			if bytes.Contains(arg, needle) {
				return true
			}
		}
		return false
	}

	if disc.Port <= 0 {
		return false
	}
	joined := fmt.Appendf(nil, "127.0.0.1:%d", disc.Port)
	serverPort := fmt.Appendf(nil, "server.port=%d", disc.Port)
	serverHost := []byte("server.host=127.0.0.1")
	for _, arg := range args {
		if bytes.Contains(arg, joined) {
			return true
		}
		if bytes.Contains(arg, serverPort) && bytes.Contains(arg, serverHost) {
			return true
		}
	}
	return false
}

// reapUnregisteredNbdkit reaps any nbdkit this daemon owns for volumeName that
// is not in cfg.MountedVolumes, and reports whether it reaped one.
//
// The registry can be missing an entry for a live export: recovery gives up on
// a survivor whose VB it could not build, and nothing reaps it afterwards, so
// it outlives the guest and holds its socket and volume open. Unmount is the
// point the volume is going away, so anything still serving it here is a leak.
//
// Only corroborated processes are considered -- our plugin, our base dir, that
// exact volume -- and the reap itself still declines anything a live process
// references, because the guest's disk may not be torn down yet.
func reapUnregisteredNbdkit(ctx context.Context, cfg *Config, volumeName string) bool {
	procRoot := cfg.procScanRoot()
	var reaped bool
	for _, disc := range scanNbdkitProcs(procRoot) {
		if disc.Volume != volumeName || !corroborateNbdkit(cfg, disc) {
			continue
		}
		slog.WarnContext(ctx, "ebs.unmount: found an nbdkit serving this volume with no registry entry",
			"volume", volumeName, "pid", disc.PID, "socket", disc.Socket)
		if reapOrphanedNbdkit(procRoot, disc, reapReasonUnregistered) {
			reaped = true
		}
	}
	return reaped
}
