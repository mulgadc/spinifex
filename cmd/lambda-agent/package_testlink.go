//go:build lambdatestlink

package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/sys/unix"
)

// Dev-only code delivery for the test path: the host attaches the ZIP as a
// read-only disk and passes its exact size and SHA-256 through fw_cfg. This
// is not the production delivery, and these bounds are not AWS limits.
const (
	entryPackageSize   = "test-package-size"
	entryPackageSHA256 = "test-package-sha256"
	entryHandler       = "test-handler"

	devMaxZipBytes    = 16 << 20
	devMaxUnzipBytes  = 64 << 20
	devMaxEntries     = 1024
	packageDevice     = "/dev/vda"
	taskTmpfsSizeOpts = "mode=0755,size=72m"
)

type packageSpec struct {
	size    int64
	sha256  [sha256.Size]byte
	handler string
}

type unpackLimits struct {
	entries int
	bytes   int64
}

var devLimits = unpackLimits{entries: devMaxEntries, bytes: devMaxUnzipBytes}

// readPackageSpec reads the expected size and SHA-256 from fw_cfg; it must
// run before fw_cfg is sealed.
func readPackageSpec(fwCfgDir string) (packageSpec, error) {
	var spec packageSpec
	read := func(name string) (string, error) {
		b, err := os.ReadFile(filepath.Join(fwCfgDir, name, "raw"))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	s, err := read(entryPackageSize)
	if err != nil {
		return spec, err
	}
	spec.size, err = strconv.ParseInt(s, 10, 64)
	if err != nil || spec.size <= 0 || spec.size > devMaxZipBytes {
		return spec, fmt.Errorf("package size %q outside 1..%d bytes", s, devMaxZipBytes)
	}
	h, err := read(entryPackageSHA256)
	if err != nil {
		return spec, err
	}
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != sha256.Size {
		return spec, errors.New("package sha256 is not 64 hex digits")
	}
	copy(spec.sha256[:], b)
	if spec.handler, err = read(entryHandler); err != nil {
		return spec, err
	}
	if spec.handler == "" || len(spec.handler) > 128 || strings.IndexFunc(spec.handler, unicode.IsSpace) >= 0 ||
		strings.ContainsRune(spec.handler, 0) {
		return spec, errors.New("handler must be 1..128 characters with no whitespace")
	}
	return spec, nil
}

// taskPackage puts the package under dir: a fresh nosuid,nodev tmpfs, made
// read-only once the files are in place.
type taskPackage struct {
	device     string
	dir        string
	loadDriver func() error
	mountDir   func(dir string) error
	sealDir    func(dir string) error
}

func devTaskPackage() taskPackage {
	return taskPackage{device: packageDevice, dir: taskRoot, loadDriver: loadBlockDriver, mountDir: mountTaskDir, sealDir: sealTaskDir}
}

func (p taskPackage) install(spec packageSpec) error {
	if err := p.loadDriver(); err != nil {
		return fmt.Errorf("load block driver: %w", err)
	}
	dev, err := os.Open(p.device)
	if err != nil {
		return fmt.Errorf("open package disk: %w", err)
	}
	defer dev.Close()
	if err := p.mountDir(p.dir); err != nil {
		return fmt.Errorf("mount %s: %w", p.dir, err)
	}
	if err := unpack(dev, spec, p.dir, devLimits); err != nil {
		return err
	}
	if err := p.sealDir(p.dir); err != nil {
		return fmt.Errorf("make %s read-only: %w", p.dir, err)
	}
	return nil
}

// unpack reads exactly spec.size bytes once, checks their SHA-256, then
// validates every entry before writing any of them under dir.
func unpack(r io.ReaderAt, spec packageSpec, dir string, lim unpackLimits) error {
	buf := make([]byte, spec.size)
	if _, err := io.ReadFull(io.NewSectionReader(r, 0, spec.size), buf); err != nil {
		return fmt.Errorf("read package: %w", err)
	}
	if sha256.Sum256(buf) != spec.sha256 {
		return errors.New("package sha256 mismatch")
	}
	zr, err := zip.NewReader(bytes.NewReader(buf), spec.size)
	if err != nil {
		return fmt.Errorf("open package: %w", err)
	}
	if len(zr.File) > lim.entries {
		return fmt.Errorf("package has %d entries, limit %d", len(zr.File), lim.entries)
	}
	names := make([]string, len(zr.File))
	var declared int64
	for i, f := range zr.File {
		name, err := entryName(f.Name)
		if err != nil {
			return err
		}
		names[i] = name
		switch mode := f.Mode(); {
		case mode.IsDir():
		case mode.IsRegular():
			// Compared against what remains, so a huge size cannot wrap the sum.
			if f.UncompressedSize64 > uint64(lim.bytes-declared) {
				return fmt.Errorf("%q: unpacked size over the %d-byte limit", f.Name, lim.bytes)
			}
			declared += int64(f.UncompressedSize64)
		default:
			return fmt.Errorf("%q: %s entries are refused", f.Name, mode.Type())
		}
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	var written int64
	for i, f := range zr.File {
		n, err := extract(root, names[i], f, lim.bytes-written)
		written += n
		if err != nil {
			return fmt.Errorf("%q: %w", f.Name, err)
		}
	}
	return nil
}

// extract writes one entry, refusing it if its actual bytes exceed budget
// whatever its header claims.
func extract(root *os.Root, name string, f *zip.File, budget int64) (int64, error) {
	if f.Mode().IsDir() {
		return 0, mkdirs(root, name)
	}
	if d := path.Dir(name); d != "." {
		if err := mkdirs(root, d); err != nil {
			return 0, err
		}
	}
	perm := os.FileMode(0o644)
	if f.Mode()&0o111 != 0 {
		perm = 0o755
	}
	in, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return 0, err
	}
	// The umask applies at create; fchmod does not, so the mode is exact.
	if err := out.Chmod(perm); err != nil {
		out.Close()
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(in, budget+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > budget {
		err = errors.New("unpacked bytes over the limit")
	}
	return n, err
}

// mkdirs creates each component of dir as mode 0755 whatever the umask. An
// existing component must be a real directory, never a symlink.
func mkdirs(root *os.Root, dir string) error {
	cur := ""
	for _, part := range strings.Split(dir, "/") {
		cur = path.Join(cur, part)
		if err := root.Mkdir(cur, 0o755); err != nil {
			if !errors.Is(err, fs.ErrExist) {
				return err
			}
			fi, err := root.Lstat(cur)
			if err != nil {
				return err
			}
			if !fi.IsDir() {
				return fmt.Errorf("%q exists and is not a directory", cur)
			}
		}
		if err := root.Chmod(cur, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// entryName returns a clean relative path, refusing anything that could name
// a location outside the task root.
func entryName(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "\\\x00") || path.IsAbs(name) {
		return "", fmt.Errorf("%q: entry name refused", name)
	}
	c := path.Clean(name)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%q: entry name refused", name)
	}
	return c, nil
}

// loadBlockDriver loads virtio_blk, which the image ships but init does not
// load, then waits briefly for the package disk to appear.
func loadBlockDriver() error {
	if err := exec.Command("/sbin/modprobe", "virtio_blk").Run(); err != nil {
		var uts unix.Utsname
		if uerr := unix.Uname(&uts); uerr != nil {
			return uerr
		}
		ko := "/lib/modules/" + unix.ByteSliceToString(uts.Release[:]) + "/kernel/drivers/block/virtio_blk.ko"
		if err := exec.Command("/sbin/insmod", ko).Run(); err != nil {
			return fmt.Errorf("modprobe and insmod virtio_blk failed: %w", err)
		}
	}
	for range 50 {
		if _, err := os.Stat(packageDevice); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("%s did not appear", packageDevice)
}

func mountTaskDir(dir string) error {
	return unix.Mount("tmpfs", dir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, taskTmpfsSizeOpts)
}

func sealTaskDir(dir string) error {
	return unix.Mount("", dir, "", unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, "")
}
