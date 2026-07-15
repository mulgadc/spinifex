package cmd

import (
	"os"
	"os/user"
	"path/filepath"
)

// DefaultConfigDir returns the default configuration directory.
// Production: /etc/spinifex (when /etc/spinifex exists)
// Development: ~/spinifex/config.
func DefaultConfigDir() string {
	if isProductionLayout() {
		return "/etc/spinifex"
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

// productionMarkerPath identifies a production install by directory presence.
// Created by setup.sh during binary install. Overridable in tests.
var productionMarkerPath = "/etc/spinifex"

// isProductionLayout returns true when running in a production install.
// No root check — allows non-root users (e.g. tf-user) to run CLI commands
// like `spx get nodes` without sudo or --config flags.
func isProductionLayout() bool {
	if info, err := os.Stat(productionMarkerPath); err == nil && info.IsDir() {
		return true
	}
	return false
}
