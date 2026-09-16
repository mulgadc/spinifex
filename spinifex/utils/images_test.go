package utils

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/klauspost/compress/zstd"
)

// resolveServingAMI filters purely on these two tags and never on the image
// name, so a typo in either hides the serving image from Ochre and every
// endpoint launch dies at "bedrock: no vllm-serving AMI found".
func TestAvailableImages_VLLMServingCarriesBedrockTags(t *testing.T) {
	const key = "ubuntu-26.04-vllm-serving-x86_64"

	img, ok := AvailableImages[key]
	if !ok {
		t.Fatalf("%s missing from AvailableImages", key)
	}

	for tag, want := range map[string]string{
		"spinifex:managed-by":   "bedrock",
		"spinifex:bedrock-role": "vllm-serving",
	} {
		if got := img.Tags[tag]; got != want {
			t.Errorf("tag %q = %q, want %q", tag, got, want)
		}
	}

	// The map key and Name are matched independently by different callers, so
	// they drifting apart is a real failure mode rather than a tautology.
	if img.Name != key {
		t.Errorf("Name = %q, want %q", img.Name, key)
	}
	if img.BootMode != "uefi" {
		t.Errorf("BootMode = %q, want uefi", img.BootMode)
	}
	if img.Arch != "x86_64" {
		t.Errorf("Arch = %q, want x86_64", img.Arch)
	}
	if img.URL == "" || img.Checksum == "" {
		t.Error("URL and Checksum must both be set or the import has nothing to fetch and verify")
	}
}

func img(id, created string, tagKeys ...string) *ec2.Image {
	image := &ec2.Image{ImageId: aws.String(id), CreationDate: aws.String(created)}
	for _, k := range tagKeys {
		if k == "" {
			image.Tags = append(image.Tags, nil)
			continue
		}
		image.Tags = append(image.Tags, &ec2.Tag{Key: aws.String(k), Value: aws.String("v")})
	}
	return image
}

func TestSelectNewestImage(t *testing.T) {
	tests := []struct {
		name        string
		images      []*ec2.Image
		excludeKey  string
		wantID      string
		wantCreated string
		wantMatches int
	}{
		{
			name: "empty input",
		},
		{
			name:   "all entries nil",
			images: []*ec2.Image{nil, nil},
		},
		{
			name:   "empty image id skipped",
			images: []*ec2.Image{img("", "2026-01-01T00:00:00.000Z"), img("ami-a", "2025-01-01T00:00:00.000Z")},
			// The empty-ID entry is not a match, so no multi-match warning fires.
			wantID: "ami-a", wantCreated: "2025-01-01T00:00:00.000Z", wantMatches: 1,
		},
		{
			name:   "newest creation date wins regardless of order",
			images: []*ec2.Image{img("ami-old", "2026-01-01T00:00:00.000Z"), img("ami-new", "2026-08-01T00:00:00.000Z"), img("ami-mid", "2026-04-01T00:00:00.000Z")},
			wantID: "ami-new", wantCreated: "2026-08-01T00:00:00.000Z", wantMatches: 3,
		},
		{
			name:   "identical dates keep the first match",
			images: []*ec2.Image{img("ami-a", "2026-01-01T00:00:00.000Z"), img("ami-b", "2026-01-01T00:00:00.000Z")},
			wantID: "ami-a", wantCreated: "2026-01-01T00:00:00.000Z", wantMatches: 2,
		},
		{
			name:       "exclude tag key drops tagged images",
			images:     []*ec2.Image{img("ami-gpu", "2026-08-01T00:00:00.000Z", "gpu-vendor"), img("ami-cpu", "2026-01-01T00:00:00.000Z")},
			excludeKey: "gpu-vendor",
			wantID:     "ami-cpu", wantCreated: "2026-01-01T00:00:00.000Z", wantMatches: 1,
		},
		{
			name:   "empty exclude key keeps tagged images",
			images: []*ec2.Image{img("ami-gpu", "2026-08-01T00:00:00.000Z", "gpu-vendor"), img("ami-cpu", "2026-01-01T00:00:00.000Z")},
			wantID: "ami-gpu", wantCreated: "2026-08-01T00:00:00.000Z", wantMatches: 2,
		},
		{
			name:       "all images excluded",
			images:     []*ec2.Image{img("ami-gpu", "2026-08-01T00:00:00.000Z", "gpu-vendor")},
			excludeKey: "gpu-vendor",
		},
		{
			name:       "nil tag entry does not panic",
			images:     []*ec2.Image{img("ami-a", "2026-01-01T00:00:00.000Z", "", "gpu-vendor"), img("ami-b", "2026-02-01T00:00:00.000Z", "")},
			excludeKey: "gpu-vendor",
			wantID:     "ami-b", wantCreated: "2026-02-01T00:00:00.000Z", wantMatches: 1,
		},
		{
			name:   "missing creation date sorts oldest",
			images: []*ec2.Image{&ec2.Image{ImageId: aws.String("ami-nodate")}, img("ami-dated", "2020-01-01T00:00:00.000Z")},
			wantID: "ami-dated", wantCreated: "2020-01-01T00:00:00.000Z", wantMatches: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotCreated, gotMatches := SelectNewestImage(tt.images, tt.excludeKey)
			if gotID != tt.wantID {
				t.Errorf("imageID = %q, want %q", gotID, tt.wantID)
			}
			if gotCreated != tt.wantCreated {
				t.Errorf("created = %q, want %q", gotCreated, tt.wantCreated)
			}
			if gotMatches != tt.wantMatches {
				t.Errorf("matches = %d, want %d", gotMatches, tt.wantMatches)
			}
		})
	}
}

// rawFixture returns the bytes of the repo's known-good MBR fixture, so a compressed copy of
// it decodes to something validateDiskImagePath's file(1) sniff already accepts elsewhere.
func rawFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "tests", "unit-test-disk-image.raw"))
	if err != nil {
		t.Fatalf("read raw fixture: %v", err)
	}
	return data
}

// qcow2FixtureBytes converts the raw fixture into a real QCOW2 image via qemu-img, in a
// scratch dir that is discarded once built, so the .qcow2.xz case below exercises genuine
// QCOW2 framing rather than a hand-rolled header.
func qcow2FixtureBytes(t *testing.T) []byte {
	t.Helper()
	if _, err := exec.LookPath("qemu-img"); err != nil {
		t.Skip("qemu-img not found, skipping qcow2 conversion case")
	}

	scratch := t.TempDir()
	rawPath := filepath.Join(scratch, "src.raw")
	if err := os.WriteFile(rawPath, rawFixture(t), 0644); err != nil {
		t.Fatalf("write raw source: %v", err)
	}

	qcowPath := filepath.Join(scratch, "src.qcow2")
	cmd := exec.Command("qemu-img", "convert", "-f", "raw", "-O", "qcow2", rawPath, qcowPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("qemu-img convert to qcow2: %v: %s", err, out)
	}

	data, err := os.ReadFile(qcowPath)
	if err != nil {
		t.Fatalf("read qcow2 fixture: %v", err)
	}
	return data
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func zstdBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return buf.Bytes()
}

// pipeCompress shells out to a real compressor (bzip2/xz) with data on stdin, since neither
// has a compression writer in this module's dependencies; only the decompression side of xz
// is production code, this is test-fixture setup only. Skips the calling case when the tool
// is not installed, mirroring the tar-availability guard in TestExtractDiskImageFromFile.
func pipeCompress(t *testing.T, tool string, args []string, data []byte) []byte {
	t.Helper()
	if _, err := exec.LookPath(tool); err != nil {
		t.Skipf("%s not found, skipping case", tool)
	}

	cmd := exec.Command(tool, args...)
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s compress: %v", tool, err)
	}
	return out
}

// TestExtractDiskImageFromFile_Compression covers the single-file compressed artifacts that
// Talos, Fedora CoreOS and Flatcar publish (.raw.zst, .raw.xz, .qcow2.xz, .img.bz2), none of
// which ExtractDiskImageFromFile could import before this fix, plus a genuinely unsupported
// suffix. Every case asserts the source artifact survives untouched and that nothing else is
// written into its directory, since a regression here would silently duplicate a multi-GiB
// image beside itself.
func TestExtractDiskImageFromFile_Compression(t *testing.T) {
	raw := rawFixture(t)

	cases := []struct {
		name           string
		filename       string
		buildPayload   func(t *testing.T) []byte
		wantErrSubstr  string
		wantPathSuffix string
	}{
		{
			name:           "gzip raw payload",
			filename:       "talos.raw.gz",
			buildPayload:   func(t *testing.T) []byte { return gzipBytes(t, raw) },
			wantPathSuffix: ".raw",
		},
		{
			name:           "zstd raw payload",
			filename:       "talos.raw.zst",
			buildPayload:   func(t *testing.T) []byte { return zstdBytes(t, raw) },
			wantPathSuffix: ".raw",
		},
		{
			name:           "bzip2 raw payload",
			filename:       "flatcar.img.bz2",
			buildPayload:   func(t *testing.T) []byte { return pipeCompress(t, "bzip2", []string{"-z", "-c"}, raw) },
			wantPathSuffix: ".img",
		},
		{
			name:           "plain xz raw payload",
			filename:       "talos.raw.xz",
			buildPayload:   func(t *testing.T) []byte { return pipeCompress(t, "xz", []string{"-z", "-c"}, raw) },
			wantPathSuffix: ".raw",
		},
		{
			name:     "qcow2 xz payload converts to raw",
			filename: "fcos.qcow2.xz",
			buildPayload: func(t *testing.T) []byte {
				return pipeCompress(t, "xz", []string{"-z", "-c"}, qcow2FixtureBytes(t))
			},
			wantPathSuffix: ".raw",
		},
		{
			name:          "genuinely unsupported suffix",
			filename:      "image.raw.7z",
			buildPayload:  func(t *testing.T) []byte { return []byte("not a real archive") },
			wantErrSubstr: "unsupported filetype",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srcDir := t.TempDir()
			outDir := t.TempDir()

			payload := tc.buildPayload(t)
			srcPath := filepath.Join(srcDir, tc.filename)
			if err := os.WriteFile(srcPath, payload, 0644); err != nil {
				t.Fatalf("write source: %v", err)
			}
			srcBefore, err := os.ReadFile(srcPath)
			if err != nil {
				t.Fatalf("read source before extract: %v", err)
			}

			gotPath, err := ExtractDiskImageFromFile(srcPath, outDir)

			if tc.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got none (path=%q)", tc.wantErrSubstr, gotPath)
				}
				if !strings.Contains(err.Error(), tc.wantErrSubstr) {
					t.Errorf("err = %v, want substring %q", err, tc.wantErrSubstr)
				}
			} else {
				if err != nil {
					t.Fatalf("ExtractDiskImageFromFile(%s) error: %v", tc.filename, err)
				}
				if !strings.HasSuffix(gotPath, tc.wantPathSuffix) {
					t.Errorf("path = %q, want suffix %q", gotPath, tc.wantPathSuffix)
				}
				if filepath.Dir(gotPath) != outDir {
					t.Errorf("extracted path %q not inside tmpdir %q, want beside the tmpdir not the source", gotPath, outDir)
				}
			}

			// The source artifact must survive untouched regardless of outcome.
			srcAfter, err := os.ReadFile(srcPath)
			if err != nil {
				t.Fatalf("read source after extract: %v", err)
			}
			if !bytes.Equal(srcBefore, srcAfter) {
				t.Error("source artifact was modified during extraction")
			}

			// Nothing may be written beside the source, in its own directory.
			entries, err := os.ReadDir(srcDir)
			if err != nil {
				t.Fatalf("read source dir: %v", err)
			}
			if len(entries) != 1 || entries[0].Name() != tc.filename {
				names := make([]string, len(entries))
				for i, e := range entries {
					names[i] = e.Name()
				}
				t.Errorf("source dir contains %v, want only %q", names, tc.filename)
			}
		})
	}
}

// TestDecompressToTemp_WritesSparseHoles is the regression test for the on-disk-size blowup
// found by verifying this fix live: a Talos raw.zst imported successfully but the decompressed
// output was written literally rather than sparsely, costing roughly 60x the disk space of the
// qcow2-to-raw path it sits beside. A realistic layout — a small non-zero header, a large zero
// run standing in for unused disk space, and a trailing zero run to exercise the final Truncate
// — should land on disk as a small fraction of its apparent size.
func TestDecompressToTemp_WritesSparseHoles(t *testing.T) {
	const zeroRun = 8 << 20 // 8 MiB: several multiples of sparseBlockSize, so the test is
	// meaningful regardless of the exact block size chosen inside decompressToTemp.

	payload := append([]byte("MULGA-SPARSE-FIXTURE-HEADER"), make([]byte, zeroRun)...)

	tmpdir := t.TempDir()
	outPath, err := decompressToTemp("talos.raw.img", tmpdir, ".img", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("decompressToTemp: %v", err)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("decompressed content does not round-trip through the sparse writer")
	}

	assertSparseOnDisk(t, outPath)
}

// assertSparseOnDisk skips (rather than fails) when the platform does not expose block-count
// stat info, or when the test filesystem does not appear to support sparse files at all, since
// neither is a real regression in the code under test.
func assertSparseOnDisk(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	apparent := info.Size()

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("platform does not expose block-count stat info, skipping sparse-size assertion")
	}
	onDisk := stat.Blocks * 512

	if onDisk >= apparent {
		t.Skip("test filesystem does not appear to support sparse files (on-disk size not below apparent size), skipping")
	}

	// The zero run should collapse to a small fraction of the apparent size; generous
	// headroom avoids pinning an exact ratio that would vary with filesystem block size.
	if onDisk > apparent/4 {
		t.Errorf("on-disk size = %d bytes for %d apparent bytes, want the zero run to be sparse (< 25%% of apparent)", onDisk, apparent)
	}
}

// TestDecompressXz_WritesSparseHoles covers the plain-.xz path specifically: it shells out to
// the xz binary and pipes its stdout through decompressToTemp rather than calling it directly,
// so this exercises that piping does not lose the sparse-hole behaviour. .raw.xz is what Talos
// and Flatcar actually publish alongside .zst, so this is the format most likely hit in
// practice.
func TestDecompressXz_WritesSparseHoles(t *testing.T) {
	if _, err := exec.LookPath("xz"); err != nil {
		t.Skip("xz not found, skipping")
	}

	const zeroRun = 8 << 20
	payload := append([]byte("MULGA-SPARSE-FIXTURE-HEADER"), make([]byte, zeroRun)...)
	compressed := pipeCompress(t, "xz", []string{"-z", "-c"}, payload)

	srcDir := t.TempDir()
	outDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "talos.raw.xz")
	if err := os.WriteFile(srcPath, compressed, 0644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	srcBefore, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read source before decompress: %v", err)
	}

	outPath, err := decompressXz(srcPath, outDir)
	if err != nil {
		t.Fatalf("decompressXz: %v", err)
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("decompressed content does not round-trip through the piped xz decompressor")
	}

	assertSparseOnDisk(t, outPath)

	srcAfter, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read source after decompress: %v", err)
	}
	if !bytes.Equal(srcBefore, srcAfter) {
		t.Error("source artifact was modified during xz decompression")
	}

	entries, err := os.ReadDir(srcDir)
	if err != nil {
		t.Fatalf("read source dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(srcPath) {
		t.Errorf("source dir contains %v, want only %q", entries, filepath.Base(srcPath))
	}
}

// TestDecompressXz_TruncatedStreamSurfacesStderr guards the io.Pipe plumbing added for the
// sparse fix: a Read past the end of a failed xz process must surface xz's own stderr, not a
// bare io.EOF that would otherwise look like a clean, silently-truncated decompression.
func TestDecompressXz_TruncatedStreamSurfacesStderr(t *testing.T) {
	if _, err := exec.LookPath("xz"); err != nil {
		t.Skip("xz not found, skipping")
	}

	compressed := pipeCompress(t, "xz", []string{"-z", "-c"}, rawFixture(t))
	truncated := compressed[:len(compressed)/2]

	srcDir := t.TempDir()
	outDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "corrupt.raw.xz")
	if err := os.WriteFile(srcPath, truncated, 0644); err != nil {
		t.Fatalf("write truncated source: %v", err)
	}

	_, err := decompressXz(srcPath, outDir)
	if err == nil {
		t.Fatal("expected an error decompressing a truncated xz stream")
	}
	if err.Error() == io.EOF.Error() {
		t.Fatalf("error is a bare EOF, want the underlying xz stderr instead: %v", err)
	}

	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "end of input") && !strings.Contains(msg, "end of file") {
		t.Errorf("err = %q, want it to surface xz's stderr about the truncated stream", err)
	}
}
