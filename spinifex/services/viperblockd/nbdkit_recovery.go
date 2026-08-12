package viperblockd

// Rebuilds cfg.MountedVolumes from nbdkit processes that survived a
// viperblockd restart, so findMountedVolume can find them again (see
// orphan_scan.go for the equivalent problem/fix on the QEMU side).

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/utils"
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

// rebuildMountedVolume re-derives disc's NBD URI, builds its daemon-side VB
// (cfg.buildVB, mountVolume's construction) and registers the same
// config/owner subscriptions a fresh mount would. Never touches the process.
func rebuildMountedVolume(ctx context.Context, cfg *Config, nc *nats.Conn, disc discoveredNbdkit) (MountedVolume, error) {
	vb, _, lease, err := cfg.buildVB(ctx, disc.Volume)
	if err != nil {
		return MountedVolume{}, fmt.Errorf("construct VB: %w", err)
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
		// adopt the first and leave the second visible in the log.
		if _, ok := findMountedVolume(cfg, disc.Volume); ok {
			slog.Warn("recovery: volume already recovered from another nbdkit process, skipping",
				"pid", disc.PID, "volume", disc.Volume)
			continue
		}

		mv, err := rebuildMountedVolume(ctx, cfg, nc, disc)
		if err != nil {
			slog.Error("recovery: failed to rebuild mounted volume registry entry, skipping",
				"pid", disc.PID, "volume", disc.Volume, "err", err)
			continue
		}

		cfg.mu.Lock()
		cfg.MountedVolumes = append(cfg.MountedVolumes, mv)
		cfg.mu.Unlock()

		slog.Info("recovery: rebuilt mounted volume registry entry from surviving nbdkit process",
			"pid", mv.PID, "volume", mv.Name, "socket", mv.Socket, "port", mv.Port)
	}
}
