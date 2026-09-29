//test:in-package: asserts on buildArgs, the unexported nbdkit argv builder.

package nbd

import (
	"slices"
	"testing"
)

// baseNBDKitConfig is the minimum a plugin invocation needs, so the tests below
// vary only WALBaseDir.
func baseNBDKitConfig() *NBDKitConfig {
	return &NBDKitConfig{
		UseTCP:     true,
		Port:       10809,
		PidFile:    "/tmp/nbd.pid",
		PluginPath: "/usr/lib/nbdkit/plugins/vb.so",
		Size:       1 << 30,
		Volume:     "vol-abc123",
		Bucket:     "my-bucket",
		Region:     "us-east-1",
		BaseDir:    "/data",
		Host:       "localhost:9000",
		CacheSize:  256,
	}
}

// TestBuildArgsForwardsWALBaseDir pins that a node configured for a separate
// WAL device passes it to the plugin. The plugin backs every mounted volume,
// so without this the setting reaches only the short-lived control-plane VBs
// and misses exactly the volumes whose fsync latency it exists to protect.
func TestBuildArgsForwardsWALBaseDir(t *testing.T) {
	cfg := baseNBDKitConfig()
	cfg.WALBaseDir = "/wal"

	args, err := cfg.buildArgs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Contains(args, "wal_base_dir=/wal") {
		t.Errorf("wal_base_dir not forwarded to the plugin: %v", args)
	}
}

// TestBuildArgsOmitsAnUnsetWALBaseDir pins that an unset value is absent
// rather than empty. The plugin reads an empty wal_base_dir as an explicit
// choice, which would override its own "same as base_dir" default.
func TestBuildArgsOmitsAnUnsetWALBaseDir(t *testing.T) {
	args, err := baseNBDKitConfig().buildArgs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, a := range args {
		if a == "wal_base_dir=" {
			t.Errorf("an unset WALBaseDir must be omitted, not passed empty: %v", args)
		}
	}
}
