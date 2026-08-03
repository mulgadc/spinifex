package predastoretopology

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// The migration runs as root under `spx admin upgrade`, but predastore runs as
// its own service user. Anything root creates along the way is root-owned and
// unreachable to it, so the migration has to hand each path back.

// dirOwner reports the uid and gid owning path. The migration takes the
// ownership it applies from the data it is moving rather than from a service
// user name, so a dev install running as the invoking user needs no special
// case.
func dirOwner(path string) (int, int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, fmt.Errorf("stat %s: %w", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("cannot read ownership of %s", path)
	}
	return int(st.Uid), int(st.Gid), nil
}

// chown gives path to uid:gid.
//
// Only root can hand a file to another user, and only root ever creates one
// owned by the wrong user, so an unprivileged run has nothing to correct.
func chown(path string, uid, gid int) error {
	if os.Geteuid() != 0 {
		return nil
	}
	if err := os.Lchown(path, uid, gid); err != nil {
		return fmt.Errorf("chown %s to %d:%d: %w", path, uid, gid, err)
	}
	return nil
}

// chownTree gives path and everything beneath it to uid:gid.
//
// os.Root scopes the walk to the tree, so a symlink planted inside it cannot
// redirect a chown running as root.
func chownTree(path string, uid, gid int) error {
	if os.Geteuid() != 0 {
		return nil
	}

	root, err := os.OpenRoot(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer root.Close()

	return fs.WalkDir(root.FS(), ".", func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", filepath.Join(path, p), err)
		}
		return chown(filepath.Join(path, p), uid, gid)
	})
}
