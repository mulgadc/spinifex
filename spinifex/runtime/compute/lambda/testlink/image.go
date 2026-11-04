//go:build lambdatestlink

package testlink

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"golang.org/x/sys/unix"
)

// The runtime image's identity constants; they are part of its digest and are
// never read from disk.
const (
	ImageName    = "lambda-runtime"
	ImageProfile = "lambda-testlink"
	ImageArch    = "x86_64"
)

// The three boot files, in the name order the digest uses.
const (
	FileInitrd   = "initramfs.cpio.gz"
	FileFirmware = "qboot.rom"
	FileKernel   = "vmlinuz"
)

var imageFiles = []string{FileInitrd, FileFirmware, FileKernel}

// maxImageFile caps each boot file, bounding the memory a prepared image holds.
var maxImageFile = map[string]int64{FileFirmware: 4 << 20, FileKernel: 64 << 20, FileInitrd: 512 << 20}

const imageDigestPrefix = "spinifex-lambda-image-content-v1\x00"

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ErrImage means the runtime image could not be prepared or does not match its pin.
var ErrImage = errors.New("lambda test link: runtime image refused")

// FileSum is one boot file's SHA-256 and size.
type FileSum struct {
	SHA256 string
	Size   int64
}

// ImagePaths names the three boot files inside dir; the digest tool and the
// daemon both select files through it.
func ImagePaths(dir string) map[string]string {
	p := make(map[string]string, len(imageFiles))
	for _, name := range imageFiles {
		p[name] = filepath.Join(dir, name)
	}
	return p
}

// ImageDigest is the runtime image's identity: the constants, then each file's
// name, SHA-256 and size in name order, each length-prefixed. It identifies the
// bytes; it does not authenticate where they came from.
func ImageDigest(sums map[string]FileSum) (string, error) {
	h := sha256.New()
	h.Write([]byte(imageDigestPrefix))
	for _, s := range []string{ImageName, ImageProfile, ImageArch} {
		field(h, s)
	}
	for _, name := range imageFiles {
		s, ok := sums[name]
		if !ok || !digestPattern.MatchString(s.SHA256) || s.Size <= 0 || s.Size > maxImageFile[name] {
			return "", fmt.Errorf("%w: %s missing or out of range", ErrImage, name)
		}
		field(h, name)
		field(h, s.SHA256)
		field(h, strconv.FormatInt(s.Size, 10))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func field(h hash.Hash, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	h.Write(n[:])
	h.Write([]byte(s))
}

// ValidDigest reports whether s is 64 lowercase hex digits.
func ValidDigest(s string) bool { return digestPattern.MatchString(s) }

// openRegular opens path without following a final symlink or blocking on a
// FIFO, and refuses anything that is not a regular file.
func openRegular(path string) (*os.File, int64, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, 0, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, 0, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, 0, errors.New("not a regular file")
	}
	return os.NewFile(uintptr(fd), path), st.Size, nil
}

// SumFile hashes one boot file in place; the digest tool uses it, the daemon
// hashes its sealed copies instead.
func SumFile(name, path string) (FileSum, error) {
	f, size, err := openRegular(path)
	if err != nil {
		return FileSum{}, err
	}
	defer func() { _ = f.Close() }()
	if size <= 0 || size > maxImageFile[name] {
		return FileSum{}, fmt.Errorf("size %d outside 1..%d", size, maxImageFile[name])
	}
	h := sha256.New()
	if err := copyExact(h, f, size); err != nil {
		return FileSum{}, err
	}
	return FileSum{SHA256: hex.EncodeToString(h.Sum(nil)), Size: size}, nil
}

// DirDigest is the digest of the three boot files in dir, hashed in place;
// PrepareImage computes the same value from its sealed copies.
func DirDigest(dir string) (string, error) {
	sums := make(map[string]FileSum, len(imageFiles))
	for name, path := range ImagePaths(dir) {
		sum, err := SumFile(name, path)
		if err != nil {
			return "", fmt.Errorf("%w: %s: %w", ErrImage, name, err)
		}
		sums[name] = sum
	}
	return ImageDigest(sums)
}

// copyExact copies exactly n bytes and then requires a clean end of file, so a
// file that grows or shrinks while it is read is refused.
func copyExact(dst io.Writer, src io.Reader, n int64) error {
	if got, err := io.CopyN(dst, src, n); err != nil || got != n {
		return fmt.Errorf("copied %d of %d bytes: %w", got, n, err)
	}
	var b [1]byte
	m, err := src.Read(b[:])
	if m != 0 {
		return errors.New("file longer than its size")
	}
	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("end of file not confirmed: %w", err)
	}
	return nil
}

// Image is the prepared runtime image: one sealed memfd per boot file, which
// QEMU boots by fd. The daemon owns it for its whole life.
type Image struct {
	Digest                   string
	Firmware, Kernel, Initrd *os.File
}

// Close releases the sealed copies; safe on a partial or nil image.
func (im *Image) Close() {
	if im == nil {
		return
	}
	for _, f := range []*os.File{im.Firmware, im.Kernel, im.Initrd} {
		if f != nil {
			_ = f.Close()
		}
	}
}

// PrepareImage seals the three boot files in dir, hashes the sealed copies and
// refuses unless their digest equals pin. On any failure nothing stays open.
func PrepareImage(dir, pin string) (*Image, error) {
	if !ValidDigest(pin) {
		return nil, fmt.Errorf("%w: pin is not 64 lowercase hex digits", ErrImage)
	}
	paths := ImagePaths(dir)
	im := &Image{}
	sums := make(map[string]FileSum, len(imageFiles))
	for _, f := range []struct {
		name string
		dst  **os.File
	}{{FileFirmware, &im.Firmware}, {FileKernel, &im.Kernel}, {FileInitrd, &im.Initrd}} {
		sealed, sum, err := sealImageFile(f.name, paths[f.name])
		if err != nil {
			im.Close()
			return nil, fmt.Errorf("%w: %s: %w", ErrImage, f.name, err)
		}
		*f.dst = sealed
		sums[f.name] = sum
	}
	got, err := ImageDigest(sums)
	if err != nil {
		im.Close()
		return nil, err
	}
	if got != pin {
		im.Close()
		return nil, fmt.Errorf("%w: digest %s does not match the pin", ErrImage, got)
	}
	im.Digest = got
	return im, nil
}

// sealImageFile copies one boot file into a sealed memfd created with
// MFD_NOEXEC_SEAL and hashes the sealed copy, which is what QEMU will read.
func sealImageFile(name, path string) (*os.File, FileSum, error) {
	src, size, err := openRegular(path)
	if err != nil {
		return nil, FileSum{}, err
	}
	defer func() { _ = src.Close() }()
	if size <= 0 || size > maxImageFile[name] {
		return nil, FileSum{}, fmt.Errorf("size %d outside 1..%d", size, maxImageFile[name])
	}
	fd, err := memfdCreate("spx-image", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING|unix.MFD_NOEXEC_SEAL)
	if err != nil {
		// Kernels before 6.3 lack MFD_NOEXEC_SEAL; no retry without it.
		return nil, FileSum{}, fmt.Errorf("sealed memfd unavailable: %w", err)
	}
	m := os.NewFile(uintptr(fd), "spx-image")
	fail := func(err error) (*os.File, FileSum, error) { _ = m.Close(); return nil, FileSum{}, err }
	if err := copyExact(m, src, size); err != nil {
		return fail(err)
	}
	if _, err := unix.FcntlInt(m.Fd(), unix.F_ADD_SEALS, requiredSeals); err != nil {
		return fail(fmt.Errorf("seal memfd: %w", err))
	}
	seals, err := unix.FcntlInt(m.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || seals&requiredSeals != requiredSeals {
		return fail(errors.New("memfd seals not confirmed"))
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(m.Fd()), &st); err != nil || st.Size != size {
		return fail(errors.New("sealed size differs from the source"))
	}
	h := sha256.New()
	if n, err := io.Copy(h, io.NewSectionReader(m, 0, size)); err != nil || n != size {
		return fail(errors.New("sealed copy unreadable"))
	}
	return m, FileSum{SHA256: hex.EncodeToString(h.Sum(nil)), Size: size}, nil
}
