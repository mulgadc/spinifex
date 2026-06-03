package nbd

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
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
	NatsURL    string `json:"nats_url"`
	NatsToken  string `json:"nats_token"`
	NatsCACert string `json:"nats_ca_cert"`
}

// buildArgs constructs the nbdkit command-line arguments from the config.
func (cfg *NBDKitConfig) buildArgs() ([]string, error) {
	args := []string{
		"-f", // foreground required for Golang plugin via nbdkit
		"--pidfile", cfg.PidFile,
	}

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

	// Add plugin-specific arguments
	pluginArgs := []string{
		fmt.Sprintf("size=%d", cfg.Size),
		fmt.Sprintf("volume=%s", cfg.Volume),
		fmt.Sprintf("bucket=%s", cfg.Bucket),
		fmt.Sprintf("region=%s", cfg.Region),
		fmt.Sprintf("access_key=%s", cfg.AccessKey),
		fmt.Sprintf("secret_key=%s", cfg.SecretKey),
		fmt.Sprintf("base_dir=%s", cfg.BaseDir),
		fmt.Sprintf("host=%s", cfg.Host),
		fmt.Sprintf("cache_size=%d", cfg.CacheSize),
		fmt.Sprintf("shardwal=%t", cfg.ShardWAL),
	}
	if cfg.NatsURL != "" {
		pluginArgs = append(pluginArgs, fmt.Sprintf("nats_url=%s", cfg.NatsURL))
	}
	if cfg.NatsToken != "" {
		pluginArgs = append(pluginArgs, fmt.Sprintf("nats_token=%s", cfg.NatsToken))
	}
	if cfg.NatsCACert != "" {
		pluginArgs = append(pluginArgs, fmt.Sprintf("nats_ca_cert=%s", cfg.NatsCACert))
	}

	args = append(args, pluginArgs...)
	return args, nil
}

func (cfg *NBDKitConfig) Execute() (*exec.Cmd, error) {
	// TODO: Consider setting threads to X% of the host availability (preference for VMs)
	/*
			--threads=THREADS
		           Set the number of threads to be used per connection, which in turn controls the number of outstanding requests  that  can
		           be  processed  at  once.   Only  matters  for  plugins  with  thread_model=parallel  (where it defaults to 16).  To force
		           serialized behavior (useful if the client is not prepared for out-of-order responses), set this to 1.
	*/
	args, err := cfg.buildArgs()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command("nbdkit", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd, cmd.Start()
}
