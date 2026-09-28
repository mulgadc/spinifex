// The generate directive lives here, not in generate.go: that file is
// //go:build ignore, so go generate never scans it and the regeneration
// every failure message asks for would silently do nothing.
//
//go:generate go run generate.go

package systemd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Action describes what Reconcile did, or would do, for one unit.
type Action string

const (
	ActionNoop     Action = "noop"     // installed content matches the embedded copy
	ActionInstall  Action = "install"  // unit missing on disk
	ActionReplace  Action = "replace"  // installed marker version is older than embedded
	ActionConflict Action = "conflict" // marker version matches but content differs — operator-modified
)

// UnitStatus reports the reconcile decision for a single unit.
type UnitStatus struct {
	Name             string
	Action           Action
	InstalledVersion int
	EmbeddedVersion  int
	Backup           string // set once a replace has written a backup
	Applied          bool   // true once Install/Replace has actually been written
}

// Options controls how Reconcile behaves.
type Options struct {
	// DryRun computes and returns the full status without writing anything.
	DryRun bool
}

// Result is the outcome of a Reconcile call.
type Result struct {
	Statuses  []UnitStatus
	ReloadRan bool
}

// HasChanges reports whether any unit is missing or stale — i.e. whether
// applying (DryRun: false) would write anything to disk.
func (r Result) HasChanges() bool {
	for _, s := range r.Statuses {
		if s.Action == ActionInstall || s.Action == ActionReplace {
			return true
		}
	}
	return false
}

// HasConflicts reports whether any unit is at the current version but was
// modified on disk — drift Reconcile deliberately refuses to overwrite.
func (r Result) HasConflicts() bool {
	for _, s := range r.Statuses {
		if s.Action == ActionConflict {
			return true
		}
	}
	return false
}

// ErrRootRequired is returned when Reconcile has pending changes but root
// cannot write to the target directory. The Result is still populated and
// safe to print — no file was written.
var ErrRootRequired = errors.New("root privileges required to write systemd units")

var unitVersionRe = regexp.MustCompile(`^#\s*spinifex-unit-version:\s*(\d+)\s*$`)

// unitVersion reads the marker on a unit's first line. An absent or
// unparsable marker is version 0, matching every node installed before
// units were versioned.
func unitVersion(content string) int {
	line, _, _ := strings.Cut(content, "\n")
	m := unitVersionRe.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return 0
	}
	v, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return v
}

// systemctlDaemonReload is overridable in tests so Reconcile never shells
// out to a real systemd instance.
var systemctlDaemonReload = func() error {
	return exec.Command("systemctl", "daemon-reload").Run()
}

func sortedUnitNames() []string {
	names := make([]string, 0, len(Units))
	for n := range Units {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Reconcile compares each embedded unit (Units, generated from
// build/systemd) against the copy installed under root. Missing or
// version-stale units are replaced after a timestamped backup; units whose
// marker version matches but whose content differs are left untouched and
// reported as operator-modified. Never restarts anything — only writes
// files and, when it wrote at least one, runs systemctl daemon-reload.
func Reconcile(root string, opts Options) (Result, error) {
	var result Result

	for _, name := range sortedUnitNames() {
		embedded := Units[name]
		embeddedVersion := unitVersion(embedded)
		path := filepath.Join(root, name)

		installed, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			result.Statuses = append(result.Statuses, UnitStatus{
				Name: name, Action: ActionInstall, EmbeddedVersion: embeddedVersion,
			})
			continue
		}
		if err != nil {
			return result, fmt.Errorf("read %s: %w", path, err)
		}

		installedVersion := unitVersion(string(installed))
		status := UnitStatus{Name: name, InstalledVersion: installedVersion, EmbeddedVersion: embeddedVersion}
		switch {
		case installedVersion < embeddedVersion:
			status.Action = ActionReplace
		case string(installed) == embedded:
			status.Action = ActionNoop
		default:
			status.Action = ActionConflict
		}
		result.Statuses = append(result.Statuses, status)
	}

	if opts.DryRun || !result.HasChanges() {
		return result, nil
	}

	// Check writability up front so a permission problem is reported before
	// any unit is touched, rather than after replacing some and failing on
	// the next — the failure mode this exists to avoid.
	if err := checkWritable(root); err != nil {
		return result, fmt.Errorf("%w (writing to %s): %w", ErrRootRequired, root, err)
	}

	for i := range result.Statuses {
		s := &result.Statuses[i]
		if s.Action != ActionInstall && s.Action != ActionReplace {
			continue
		}
		path := filepath.Join(root, s.Name)
		if s.Action == ActionReplace {
			backup, err := backupUnit(path, s.InstalledVersion, s.EmbeddedVersion)
			if err != nil {
				return result, fmt.Errorf("backup %s: %w", path, err)
			}
			s.Backup = backup
		}
		if err := os.WriteFile(path, []byte(Units[s.Name]), 0o644); err != nil { //nolint:gosec // systemd unit files are intentionally world-readable, matching setup.sh's install -m 0644
			return result, fmt.Errorf("write %s: %w", path, err)
		}
		s.Applied = true
	}

	if err := systemctlDaemonReload(); err != nil {
		return result, fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	result.ReloadRan = true

	return result, nil
}

// checkWritable reports whether root is writable by the current process,
// via a throwaway probe file rather than an EUID check — Reconcile is
// pointed at a temp directory in tests, not necessarily run as root there.
func checkWritable(root string) error {
	f, err := os.CreateTemp(root, ".spx-systemd-write-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if cerr := f.Close(); cerr != nil {
		return cerr
	}
	return os.Remove(name)
}

// backupUnit mirrors migrate.BackupConfig's timestamped-backup convention,
// scoped to unit reconciliation instead of config migration.
func backupUnit(path string, fromVersion, toVersion int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}

	backupPath := fmt.Sprintf("%s.pre-reconcile-%dto%d.%d", path, fromVersion, toVersion, time.Now().Unix())
	if err := os.WriteFile(backupPath, data, info.Mode()); err != nil {
		return "", err
	}
	return backupPath, nil
}
