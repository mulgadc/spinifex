package cmd

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
)

// productionConfigDir is the config directory of a production install, and the
// path the systemd units read their configuration from. Overridable in tests.
var productionConfigDir = "/etc/spinifex"

// DefaultConfigDir returns the default configuration directory.
// Production: /etc/spinifex
// Development: ~/spinifex/config.
func DefaultConfigDir() string {
	if isProductionLayout() {
		return productionConfigDir
	}
	return filepath.Join(realUserHomeDir(), "spinifex", "config")
}

// DefaultDataDir returns the default data directory.
// Production: /var/lib/spinifex
// Development: ~/spinifex.
func DefaultDataDir() string {
	if isProductionLayout() {
		return "/var/lib/spinifex"
	}
	return filepath.Join(realUserHomeDir(), "spinifex")
}

// LogDirFor returns the log directory for a given data directory.
// Production: /var/log/spinifex (matches systemd ReadWritePaths)
// Development: <dataDir>/logs (supports custom per-node data dirs).
func LogDirFor(dataDir string) string {
	if isProductionLayout() {
		return "/var/log/spinifex"
	}
	return filepath.Join(dataDir, "logs")
}

// realUserHomeDir returns the home directory of the real (non-sudo) user.
// When running under sudo, SUDO_USER is set to the invoking user — resolve
// their home directory so config/data land in the right place.
func realUserHomeDir() string {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		if u, err := user.Lookup(sudoUser); err == nil {
			return u.HomeDir
		}
	}
	homeDir, _ := os.UserHomeDir()
	return homeDir
}

// DefaultConfigFile returns the default path to spinifex.toml.
func DefaultConfigFile() string {
	return filepath.Join(DefaultConfigDir(), "spinifex.toml")
}

// productionMarkerPaths identify a production install by presence. Overridable
// in tests. The systemd target is the durable marker: setup.sh installs it and
// only an uninstall removes it, whereas /etc/spinifex is state that a node reset
// legitimately clears.
var productionMarkerPaths = []string{
	"/etc/systemd/system/spinifex.target",
	productionConfigDir,
}

// isProductionLayout returns true when running in a production install.
// No root check — allows non-root users (e.g. tf-user) to run CLI commands
// like `spx get nodes` without sudo or --config flags.
func isProductionLayout() bool {
	for _, marker := range productionMarkerPaths {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	return false
}

// EnsureConfigDir prepares the config directory, and refuses to proceed when a
// production node has lost it.
//
// setup.sh owns that tree: the per-service subdirectories and their ownership
// (spinifex-nats, spinifex-gw, ...) come from there, not from init. Recreating
// just the top level would produce a node that inits cleanly and then fails at
// service start on a missing nats.conf, so say so now instead.
func EnsureConfigDir(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	if dir == productionConfigDir && isProductionLayout() {
		return fmt.Errorf("%s is missing on a production install: re-run setup.sh to rebuild the config tree, then init", dir)
	}
	return os.MkdirAll(dir, 0750)
}
