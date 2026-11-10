package imagewrite_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/imagewrite"
)

// recorder stands in for both writers, so a test sees which one ran and with
// what, and never runs qemu-img.
type recorder struct {
	nbdURIs []string
	argv    [][]string
}

func (r *recorder) writer() imagewrite.Writer {
	return imagewrite.Writer{
		NBD: func(_ context.Context, _, nbdURI string, _ io.Writer) error {
			r.nbdURIs = append(r.nbdURIs, nbdURI)
			return nil
		},
		QemuImg: func(_ context.Context, _ io.Writer, args ...string) error {
			r.argv = append(r.argv, append([]string{"qemu-img"}, args...))
			return nil
		},
	}
}

func rawImage(t *testing.T, size int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "image.raw")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	require.NoError(t, f.Close())
	return path
}

func blockExport(size uint64) ebsprovider.VolumeExport {
	return ebsprovider.VolumeExport{Kind: ebsprovider.ExportKindBlock, Block: &ebsprovider.BlockExport{DevicePath: "/dev/disk/by-path/ip-169.254.2.2:3260-iscsi-iqn.2015-12.com.oracleiaas:vol-lun-1", SizeBytes: size}}
}

func TestWriteToABlockExportConvertsOntoTheDevice(t *testing.T) {
	r := &recorder{}
	image := rawImage(t, 1<<20)
	export := blockExport(1 << 20)

	require.NoError(t, r.writer().Write(context.Background(), image, export, nil))

	require.Len(t, r.argv, 1)
	assert.Equal(t, []string{"qemu-img", "convert", "-f", "raw", "-O", "raw", "-n", "-p", image, export.Block.DevicePath}, r.argv[0],
		"without -n qemu-img would try to create the device, and the target must be the published device")
	assert.Empty(t, r.nbdURIs)
}

func TestWriteRefusesAnImageLargerThanTheBlockExport(t *testing.T) {
	r := &recorder{}
	image := rawImage(t, 1<<20+1)

	err := r.writer().Write(context.Background(), image, blockExport(1<<20), nil)
	require.ErrorContains(t, err, "larger than")
	assert.Empty(t, r.argv, "an oversized image must be refused before anything is written")
}

func TestWriteDispatchesAnNBDExportToTheNBDWriter(t *testing.T) {
	r := &recorder{}
	require.NoError(t, r.writer().Write(context.Background(), rawImage(t, 1<<20), ebsprovider.NewNBDExport("nbd://127.0.0.1:10809"), nil))
	assert.Equal(t, []string{"nbd://127.0.0.1:10809"}, r.nbdURIs)
	assert.Empty(t, r.argv)
}

func TestWriteRefusesAnExportItCannotWrite(t *testing.T) {
	for name, export := range map[string]ebsprovider.VolumeExport{
		"unknown kind":           {Kind: "iscsi"},
		"block without details":  {Kind: ebsprovider.ExportKindBlock},
		"relative device path":   {Kind: ebsprovider.ExportKindBlock, Block: &ebsprovider.BlockExport{DevicePath: "sdb", SizeBytes: 1 << 20}},
		"nbd without its detail": {Kind: ebsprovider.ExportKindNBD},
	} {
		t.Run(name, func(t *testing.T) {
			r := &recorder{}
			require.Error(t, r.writer().Write(context.Background(), rawImage(t, 1<<20), export, nil))
			assert.Empty(t, r.argv)
			assert.Empty(t, r.nbdURIs)
		})
	}
}
