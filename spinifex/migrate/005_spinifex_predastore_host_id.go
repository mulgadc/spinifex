package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The spinifex config target, relative to the Spinifex config directory.
const (
	spinifexTarget  = "spinifex.toml"
	spinifexRelPath = "spinifex.toml"
	// spinifexHostIDVersion is the schema where the predastore section names a
	// cluster host rather than a node.
	spinifexHostIDVersion = 4
)

// spinifexPredastoreTableRe matches the per-node predastore section header,
// [nodes.<name>.predastore], which is the only place node_id ever appeared.
var spinifexPredastoreTableRe = regexp.MustCompile(`^\[nodes\.[^\[\]]+\.predastore\]$`)

// spinifexNodeIDKeyRe matches the key to rename, preserving its indentation
// and spacing so the rest of the line is untouched.
var spinifexNodeIDKeyRe = regexp.MustCompile(`^(\s*)node_id(\s*=)`)

// spinifexTableHeaderRe captures the name of a table or table-array header.
var spinifexTableHeaderRe = regexp.MustCompile(`^\[\[?([^\[\]]+)\]?\]$`)

func init() {
	DefaultRegistry.RegisterConfigTarget(spinifexTarget, spinifexRelPath, &TOMLVersionReader{})
	DefaultRegistry.RegisterConfig(spinifexTarget, ConfigMigration{
		FromVersion: 3,
		ToVersion:   spinifexHostIDVersion,
		Description: "rename [nodes.*.predastore] node_id → host_id",
		Run:         migrateSpinifexPredastoreHostID,
	})
}

// migrateSpinifexPredastoreHostID renames the predastore node selector.
//
// A machine is one predastore host now, and derivePredastoreBind reads this
// key to pick the process's host. Left as node_id, a multi-node member falls
// back to host 0 and tries to run every node in the cluster in its own process.
//
// Single-node installs never had the key — the template only emits it for
// multi-node — so for them this is a version bump and nothing else.
func migrateSpinifexPredastoreHostID(ctx ConfigContext) error {
	path := filepath.Join(ctx.ConfigDir, spinifexRelPath)

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	lines := strings.Split(string(data), "\n")
	inPredastore := false
	renamed := 0

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		if spinifexTableHeaderRe.MatchString(trimmed) {
			inPredastore = spinifexPredastoreTableRe.MatchString(trimmed)
			continue
		}
		if !inPredastore {
			continue
		}
		if spinifexNodeIDKeyRe.MatchString(line) {
			lines[i] = spinifexNodeIDKeyRe.ReplaceAllString(line, "${1}host_id${2}")
			renamed++
		}
	}

	if renamed == 0 {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), info.Mode()); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	ctx.Logger.Info("Renamed predastore node_id to host_id", "path", path, "keys", renamed)
	return nil
}
