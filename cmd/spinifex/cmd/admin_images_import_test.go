package cmd

import (
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readRawDiskFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "unit-test-disk-image.raw"))
	require.NoError(t, err)
	return data
}

func gzipData(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, err := gw.Write(data)
	require.NoError(t, err)
	require.NoError(t, gw.Close())
	return buf.Bytes()
}

func qcow2Data(t *testing.T, raw []byte) []byte {
	t.Helper()
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not found")
	}
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "src.raw")
	qcowPath := filepath.Join(dir, "src.qcow2")
	require.NoError(t, os.WriteFile(rawPath, raw, 0o600))
	out, err := exec.Command("qemu-img", "convert", "-f", "raw", "-O", "qcow2", rawPath, qcowPath).CombinedOutput()
	require.NoError(t, err, string(out))
	data, err := os.ReadFile(qcowPath)
	require.NoError(t, err)
	return data
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// The catalog dir doubles as the download cache, so the source lives there
// as it does for a --name import, with the images dir as the scratch root.
func TestExtractToScratch_LeavesNoIntermediateInCatalog(t *testing.T) {
	raw := readRawDiskFixture(t)

	cases := []struct {
		name    string
		source  string
		payload func(t *testing.T) []byte
		wantErr string
	}{
		{
			name:    "gzip raw is decompressed into scratch",
			source:  "disk.raw.gz",
			payload: func(t *testing.T) []byte { return gzipData(t, raw) },
		},
		{
			name:    "qcow2 is converted into scratch",
			source:  "disk.qcow2",
			payload: func(t *testing.T) []byte { return qcow2Data(t, raw) },
		},
		{
			name:    "intermediate that fails the disk sniff is removed",
			source:  "disk.raw.gz",
			payload: func(t *testing.T) []byte { return gzipData(t, []byte("not a disk image")) },
			wantErr: "extract image",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			imagesDir := t.TempDir()
			catalogDir := filepath.Join(imagesDir, "debian", "13", "x86_64")
			require.NoError(t, os.MkdirAll(catalogDir, 0o770))
			source := filepath.Join(catalogDir, tc.source)
			require.NoError(t, os.WriteFile(source, tc.payload(t), 0o600))

			extracted, cleanup, err := extractToScratch(source, imagesDir)

			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Nil(t, cleanup)
			} else {
				require.NoError(t, err)
				scratchDir := filepath.Dir(extracted)
				assert.Equal(t, imagesDir, filepath.Dir(scratchDir))
				assert.True(t, strings.HasPrefix(filepath.Base(scratchDir), ".import-"), "extracted into %s", extracted)
				assert.FileExists(t, extracted)
				cleanup()
			}

			assert.Equal(t, []string{"debian"}, dirNames(t, imagesDir), "scratch dir must be removed")
			assert.Equal(t, []string{tc.source}, dirNames(t, catalogDir), "catalog must hold only the source")
		})
	}
}
