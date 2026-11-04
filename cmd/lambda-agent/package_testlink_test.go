//go:build lambdatestlink

package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"hash/crc32"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testZip builds a ZIP; an entry named bootstrap is marked executable.
func testZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		h := &zip.FileHeader{Name: n, Method: zip.Deflate}
		h.SetMode(0o644)
		if n == "bootstrap" {
			h.SetMode(0o755)
		}
		w, err := zw.CreateHeader(h)
		require.NoError(t, err)
		_, err = w.Write([]byte(files[n]))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// rawZip writes entries with headers set by the caller, so a test can claim
// sizes or modes the data does not have.
func rawZip(t *testing.T, entries ...*zip.FileHeader) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, h := range entries {
		w, err := zw.CreateRaw(h)
		require.NoError(t, err)
		_, err = w.Write([]byte("x"))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func specFor(b []byte) packageSpec {
	return packageSpec{size: int64(len(b)), sha256: sha256.Sum256(b)}
}

// Not parallel: the umask is process-wide, so the test sets a restrictive one
// and restores it.
func TestUnpackInstallsFilesWithFixedModes(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	b := testZip(t, map[string]string{"bootstrap": "#!/bin/sh\n", "lib/data.txt": "data"})
	dir := t.TempDir()
	require.NoError(t, unpack(bytes.NewReader(b), specFor(b), dir, devLimits))

	pfi, err := os.Stat(filepath.Join(dir, "lib"))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o755), pfi.Mode().Perm(), "implicit parent dir")

	fi, err := os.Stat(filepath.Join(dir, "bootstrap"))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o755), fi.Mode().Perm(), "executable")
	fi, err = os.Stat(filepath.Join(dir, "lib", "data.txt"))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o644), fi.Mode().Perm())
	got, err := os.ReadFile(filepath.Join(dir, "lib", "data.txt"))
	require.NoError(t, err)
	assert.Equal(t, "data", string(got))
}

func TestUnpackRefusesBeforeWritingAnything(t *testing.T) {
	symlink := &zip.FileHeader{Name: "link", Method: zip.Store, CRC32: crc32.ChecksumIEEE([]byte("x")), CompressedSize64: 1, UncompressedSize64: 1}
	symlink.SetMode(fs.ModeSymlink | 0o777)
	fifo := &zip.FileHeader{Name: "pipe", Method: zip.Store, CRC32: crc32.ChecksumIEEE([]byte("x")), CompressedSize64: 1, UncompressedSize64: 1}
	fifo.SetMode(fs.ModeNamedPipe | 0o644)
	// The second size would wrap a plain running sum back under the limit.
	big := func(name string, n uint64) *zip.FileHeader {
		return &zip.FileHeader{Name: name, Method: zip.Store, CRC32: crc32.ChecksumIEEE([]byte("x")), CompressedSize64: 1, UncompressedSize64: n}
	}
	cases := map[string]struct {
		zip  []byte
		want string
	}{
		"parent traversal": {testZip(t, map[string]string{"a": "ok", "../escape": "x"}), "entry name refused"},
		"nested traversal": {testZip(t, map[string]string{"a": "ok", "lib/../../escape": "x"}), "entry name refused"},
		"absolute path":    {testZip(t, map[string]string{"a": "ok", "/etc/escape": "x"}), "entry name refused"},
		"backslash":        {testZip(t, map[string]string{"a": "ok", `..\escape`: "x"}), "entry name refused"},
		"symlink":          {rawZip(t, big("a", 1), symlink), "entries are refused"},
		"named pipe":       {rawZip(t, big("a", 1), fifo), "entries are refused"},
		"declared size":    {rawZip(t, big("a", devMaxUnzipBytes+1)), "over the"},
		"wrapping sizes":   {rawZip(t, big("a", devMaxUnzipBytes), big("b", math.MaxUint64)), "over the"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			err := unpack(bytes.NewReader(tc.zip), specFor(tc.zip), dir, devLimits)
			assert.ErrorContains(t, err, tc.want)
			left, rerr := os.ReadDir(dir)
			require.NoError(t, rerr)
			assert.Empty(t, left, "files written before the refusal")
		})
	}
}

func TestUnpackRefusesTooManyEntries(t *testing.T) {
	b := testZip(t, map[string]string{"a": "1", "b": "2", "c": "3"})
	err := unpack(bytes.NewReader(b), specFor(b), t.TempDir(), unpackLimits{entries: 2, bytes: devMaxUnzipBytes})
	assert.ErrorContains(t, err, "limit 2")
}

// The header claims a size within the limit, but the data is longer: the
// actual bytes are refused, whatever the header says.
func TestUnpackRefusesBytesBeyondTheLimit(t *testing.T) {
	data := bytes.Repeat([]byte("a"), 64)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: "a", Method: zip.Store, CRC32: crc32.ChecksumIEEE(data), CompressedSize64: uint64(len(data)), UncompressedSize64: 8}
	w, err := zw.CreateRaw(h)
	require.NoError(t, err)
	_, err = w.Write(data)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	b := buf.Bytes()

	dir := t.TempDir()
	err = unpack(bytes.NewReader(b), specFor(b), dir, unpackLimits{entries: 4, bytes: 16})
	require.Error(t, err)
	fi, serr := os.Stat(filepath.Join(dir, "a"))
	if serr == nil {
		assert.LessOrEqual(t, fi.Size(), int64(17))
	}
}

func TestUnpackChecksSizeAndHash(t *testing.T) {
	b := testZip(t, map[string]string{"bootstrap": "x"})
	wrongHash := specFor(b)
	wrongHash.sha256[0] ^= 1
	assert.ErrorContains(t, unpack(bytes.NewReader(b), wrongHash, t.TempDir(), devLimits), "sha256 mismatch")

	longer := specFor(b)
	longer.size++
	assert.ErrorContains(t, unpack(bytes.NewReader(b), longer, t.TempDir(), devLimits), "read package")
}

func TestReadPackageSpecBounds(t *testing.T) {
	sum := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	cases := map[string]struct {
		size, hash, handler string
		want                string
	}{
		"valid":            {"10", sum, "bootstrap.h", ""},
		"zero":             {"0", sum, "bootstrap.h", "outside"},
		"too large":        {"16777217", sum, "bootstrap.h", "outside"},
		"not hex":          {"10", "zz", "bootstrap.h", "64 hex digits"},
		"short":            {"10", sum[:62], "bootstrap.h", "64 hex digits"},
		"empty handler":    {"10", sum, "", "handler"},
		"handler space":    {"10", sum, "a b", "handler"},
		"128-char handler": {"10", sum, strings.Repeat("h", 128), ""},
		"129-char handler": {"10", sum, strings.Repeat("h", 129), "handler"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for n, v := range map[string]string{entryPackageSize: tc.size, entryPackageSHA256: tc.hash, entryHandler: tc.handler} {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, n), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, n, "raw"), []byte(v), 0o600))
			}
			spec, err := readPackageSpec(dir)
			if tc.want == "" {
				require.NoError(t, err)
				assert.Equal(t, int64(10), spec.size)
				assert.Equal(t, tc.handler, spec.handler)
				return
			}
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

// A pre-existing symlink where a directory belongs is refused, not followed
// or chmodded.
func TestMkdirsRefusesSymlinkComponent(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.Chmod(outside, 0o700))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "lib")))
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()
	assert.ErrorContains(t, mkdirs(root, "lib/sub"), "not a directory")
	fi, err := os.Stat(outside)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o700), fi.Mode().Perm(), "symlink target was chmodded")
}
