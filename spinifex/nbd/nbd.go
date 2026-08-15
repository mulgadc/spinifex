package nbd

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
)

// accessKeyEnv and secretKeyEnv name the environment variables the nbdkit
// child process reads S3 credentials from. Passing credentials this way
// instead of via argv keeps them out of /proc/<pid>/cmdline, which is
// world-readable for the life of the process.
const (
	accessKeyEnv = "VB_ACCESS_KEY"
	secretKeyEnv = "VB_SECRET_KEY" //nolint:gosec // G101 false positive: env var name, not a credential value
)

type NBDKitConfig struct {
	Port       int    `json:"port"`   // TCP port (when using TCP transport)
	Socket     string `json:"socket"` // Unix socket path (when using socket transport)
	PidFile    string `json:"pid_file"`
	PluginPath string `json:"plugin_path"`
	Verbose    bool   `json:"verbose"`
	Foreground bool   `json:"foreground"`
	Size       int64  `json:"size"`
	Volume     string `json:"volume"`
	Bucket     string `json:"bucket"`
	Region     string `json:"region"`
	AccessKey  string `json:"access_key"`
	SecretKey  string `json:"secret_key"`
	BaseDir    string `json:"base_dir"`
	Host       string `json:"host"`
	CacheSize  int    `json:"cache_size"`
	ShardWAL   bool   `json:"shardwal"` // Enable sharded WAL (default false)
	UseTCP     bool   `json:"use_tcp"`  // If true, use TCP transport; otherwise use Unix socket

	// EncryptionKeyFile is the path to the shared AES-256 master key. When set,
	// it is forwarded to the viperblock plugin so encrypted volumes open with
	// matching encryption state. Empty → plugin opens in cleartext mode.
	EncryptionKeyFile string `json:"encryption_key_file"`

	// GCEnabled forwards spinifex's [viperblock] gc_enabled toggle to the
	// plugin's gc_enabled param, gating chunk garbage collection on the VB
	// this nbdkit process constructs. Default false, matching ShardWAL.
	GCEnabled bool `json:"gc_enabled"`

	// ReadOnly serves the export read-only via nbdkit -r, which both sets the
	// NBD read-only transmission flag and passes readonly=true to the plugin's
	// Open. The plugin refuses writes on that flag.
	ReadOnly bool `json:"read_only"`

	// Threads is nbdkit's -t, the worker threads served per NBD connection.
	// It only takes effect because the Go plugin binding declares
	// thread_model=parallel. QEMU opens one connection per volume, so this
	// bounds in-flight requests for the whole volume. 0 omits the flag and
	// leaves nbdkit on its own default.
	Threads int `json:"threads"`
}

// Block size constraints advertised to clients through nbdkit's
// blocksize-policy filter. The plugin itself has no way to advertise them:
// the nbdkit Go binding has no block-size callback.
//
// The minimum is 1 because viperblock's WriteAt handles unaligned requests
// with a read-modify-write; the preferred size is its 4KB block, which is the
// size that avoids that cycle. The maximum matches what qemu-nbd advertises.
const (
	blockSizeMinimum   = 1
	blockSizePreferred = 4096
	blockSizeMaximum   = 32 << 20
)

// buildArgs constructs the nbdkit command-line arguments from the config.
func (cfg *NBDKitConfig) buildArgs() ([]string, error) {
	args := []string{
		"-f", // foreground required for Golang plugin via nbdkit
		"--pidfile", cfg.PidFile,
	}

	if cfg.ReadOnly {
		args = append(args, "-r")
	}

	// Server option, so it has to precede the plugin path below.
	if cfg.Threads > 0 {
		args = append(args, "-t", strconv.Itoa(cfg.Threads))
	}

	args = append(args, "--filter=blocksize-policy")

	// Add transport-specific arguments
	if cfg.UseTCP {
		// TCP transport - for remote/DPU scenarios
		args = append(args, "-p", strconv.Itoa(cfg.Port))
	} else {
		// Unix socket transport (default) - faster for local connections
		if cfg.Socket == "" {
			return nil, fmt.Errorf("socket path is required when not using TCP transport")
		}
		args = append(args, "--unix", cfg.Socket)
	}

	args = append(args, cfg.PluginPath)

	if cfg.Verbose {
		args = append(args, "-v")
	}

	// Add plugin-specific arguments. Credentials are deliberately not here —
	// they are passed via cmd.Env in Execute so they never appear in argv,
	// which is world-readable via /proc/<pid>/cmdline.
	pluginArgs := []string{
		fmt.Sprintf("blocksize-minimum=%d", blockSizeMinimum),
		fmt.Sprintf("blocksize-preferred=%d", blockSizePreferred),
		fmt.Sprintf("blocksize-maximum=%d", blockSizeMaximum),
		fmt.Sprintf("size=%d", cfg.Size),
		fmt.Sprintf("volume=%s", cfg.Volume),
		fmt.Sprintf("bucket=%s", cfg.Bucket),
		fmt.Sprintf("region=%s", cfg.Region),
		fmt.Sprintf("base_dir=%s", cfg.BaseDir),
		fmt.Sprintf("host=%s", cfg.Host),
		fmt.Sprintf("cache_size=%d", cfg.CacheSize),
		fmt.Sprintf("shardwal=%t", cfg.ShardWAL),
		fmt.Sprintf("gc_enabled=%t", cfg.GCEnabled),
	}
	// Only forward the key when configured; an empty value would explicitly
	// set the plugin to cleartext and override its ENCRYPTION_KEY_FILE fallback.
	if cfg.EncryptionKeyFile != "" {
		pluginArgs = append(pluginArgs, fmt.Sprintf("encryption_key_file=%s", cfg.EncryptionKeyFile))
	}

	args = append(args, pluginArgs...)
	return args, nil
}

// buildCmd constructs the nbdkit exec.Cmd with argv and environment set, but
// does not start it. Split out from Execute so tests can inspect cmd.Env and
// cmd.Args without spawning the real nbdkit binary.
func (cfg *NBDKitConfig) buildCmd() (*exec.Cmd, error) {
	args, err := cfg.buildArgs()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command("nbdkit", args...)
	// Credentials travel via the environment, not argv, so nbdkit's plugin
	// can read them without exposing them in /proc/<pid>/cmdline.
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("%s=%s", accessKeyEnv, cfg.AccessKey),
		fmt.Sprintf("%s=%s", secretKeyEnv, cfg.SecretKey),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd, nil
}

func (cfg *NBDKitConfig) Execute() (*exec.Cmd, error) {
	cmd, err := cfg.buildCmd()
	if err != nil {
		return nil, err
	}

	return cmd, cmd.Start()
}
