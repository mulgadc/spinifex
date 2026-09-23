package utils

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/klauspost/compress/zstd"
)

var ErrQCOWDetected = errors.New("qcow format detected")

const sumsFileMaxSize = 1 * 1024 * 1024 // 1 MB, matches formation.go JoinRequest cap.

var (
	ErrChecksumMismatch        = errors.New("image checksum mismatch")
	ErrChecksumNotFound        = errors.New("checksum entry for image filename not found")
	ErrUnsupportedChecksumType = errors.New("unsupported checksum type")
	ErrChecksumFetchFailed     = errors.New("checksum fetch failed")
	ErrMalformedChecksum       = errors.New("malformed checksum")
)

// checksumExtraRootCAs is a test-only hook for httptest TLS servers; nil in production.
var checksumExtraRootCAs *x509.CertPool

// checksumFetchTimeout is a var so tests can shrink it to exercise the deadline path.
var checksumFetchTimeout = 30 * time.Second

// VerifyImageChecksum fetches the sums file at checksumURL, finds the entry for imagePath's basename,
// hashes the file with checksumType ("sha256" or "sha512"), compares digests and returns the actual one.
// Fails closed: non-HTTPS, non-2xx, oversized response, or digest mismatch all return a wrapped error.
func VerifyImageChecksum(imagePath, checksumURL, checksumType string) (string, error) {
	hasher, err := newHasher(checksumType)
	if err != nil {
		return "", err
	}

	expected, err := fetchExpectedDigest(checksumURL, filepath.Base(imagePath))
	if err != nil {
		slog.Error("image checksum fetch failed", "source", checksumURL, "err", err)
		return "", err
	}

	// Catch algorithm mismatch before ConstantTimeCompare; a length mismatch would look like tampering.
	if len(expected) != hasher.Size()*2 {
		return "", fmt.Errorf("%w: digest length %d from sums file does not match %s output length %d",
			ErrChecksumFetchFailed, len(expected), checksumType, hasher.Size()*2)
	}

	actual, err := VerifyImageDigest(imagePath, checksumType, expected)
	if errors.Is(err, ErrChecksumMismatch) {
		slog.Error("image checksum mismatch",
			"image", imagePath,
			"algorithm", checksumType,
			"expected", expected,
			"actual", actual,
			"source", checksumURL,
		)
	}
	return actual, err
}

// VerifyImageDigest hashes imagePath with algo and compares it with the expected hex digest.
// The actual digest is returned on mismatch too, so callers can report it.
func VerifyImageDigest(imagePath, algo, expected string) (string, error) {
	actual, err := HashImageFile(imagePath, algo)
	if err != nil {
		return "", err
	}
	if subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) != 1 {
		return actual, fmt.Errorf("%w: expected %s got %s", ErrChecksumMismatch, expected, actual)
	}
	return actual, nil
}

// HashImageFile returns the lowercase hex digest of imagePath under algo ("sha256" or "sha512").
func HashImageFile(imagePath, algo string) (string, error) {
	hasher, err := newHasher(algo)
	if err != nil {
		return "", err
	}
	actual, err := hashImageFile(imagePath, hasher)
	if err != nil {
		return "", fmt.Errorf("hash image file: %w", err)
	}
	return actual, nil
}

// ReadExpectedDigest reads a local sums file and returns the algorithm and lowercase hex digest for imageName.
// The algorithm is inferred from the digest length; the file's name and BSD prefix only cross-check it.
func ReadExpectedDigest(sumsPath, imageName string) (algo, digest string, err error) {
	// Stat before open: opening a FIFO would block until a writer appears.
	info, err := os.Stat(sumsPath)
	if err != nil {
		return "", "", err
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("%w: %s is not a regular file", ErrMalformedChecksum, sumsPath)
	}

	f, err := os.Open(sumsPath)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	body, err := io.ReadAll(io.LimitReader(f, sumsFileMaxSize+1))
	if err != nil {
		return "", "", fmt.Errorf("read sums file: %w", err)
	}
	if len(body) > sumsFileMaxSize {
		return "", "", fmt.Errorf("%w: sums file exceeds %d byte limit", ErrMalformedChecksum, sumsFileMaxSize)
	}

	digest, bsdAlgo, err := parseSumsFile(body, imageName)
	if err != nil {
		return "", "", err
	}
	digest = strings.ToLower(digest)
	algo, err = algoForDigest(digest)
	if err != nil {
		return "", "", err
	}
	if err := crossCheckDigestAlgo(algo, bsdAlgo, algoFromSumsName(filepath.Base(sumsPath))); err != nil {
		return "", "", err
	}
	return algo, digest, nil
}

// algoForDigest infers the hash algorithm from a lowercase hex digest's length.
func algoForDigest(digest string) (string, error) {
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("%w: digest contains non-hex character %q", ErrMalformedChecksum, c)
		}
	}
	switch len(digest) {
	case sha256.Size * 2:
		return "sha256", nil
	case sha512.Size * 2:
		return "sha512", nil
	default:
		return "", fmt.Errorf("%w: digest length %d", ErrUnsupportedChecksumType, len(digest))
	}
}

// crossCheckDigestAlgo refuses a sums file whose name or BSD prefix names a different algorithm than its digest length.
// It adds no security, since the digest is compared by its own bytes; it may be removed.
func crossCheckDigestAlgo(algo, bsdAlgo, nameAlgo string) error {
	for _, named := range []string{bsdAlgo, nameAlgo} {
		if named != "" && named != algo {
			return fmt.Errorf("%w: sums file names %s but digest length is %s", ErrMalformedChecksum, named, algo)
		}
	}
	return nil
}

// algoFromSumsName returns the algorithm a sums file's basename names, or "" when it names none.
func algoFromSumsName(base string) string {
	base = strings.ToLower(base)
	for _, algo := range []string{"sha256", "sha512"} {
		if base == algo+"sums" || strings.HasSuffix(base, "."+algo) {
			return algo
		}
	}
	return ""
}

func newHasher(checksumType string) (hash.Hash, error) {
	switch strings.ToLower(checksumType) {
	case "sha256":
		return sha256.New(), nil
	case "sha512":
		return sha512.New(), nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedChecksumType, checksumType)
	}
}

// fetchExpectedDigest downloads the sums file and returns the hex digest for filename.
// Enforces HTTPS on every redirect hop, caps at 10 redirects, and limits response to sumsFileMaxSize.
func fetchExpectedDigest(checksumURL, filename string) (string, error) {
	parsed, err := url.Parse(checksumURL)
	if err != nil {
		return "", fmt.Errorf("%w: parse url: %w", ErrChecksumFetchFailed, err)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("%w: non-https checksum url scheme %q", ErrChecksumFetchFailed, parsed.Scheme)
	}

	ctx, cancel := context.WithTimeout(context.Background(), checksumFetchTimeout)
	defer cancel()
	intCh := make(chan os.Signal, 1)
	signal.Notify(intCh, os.Interrupt)
	defer signal.Stop(intCh)
	go func() {
		select {
		case <-intCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	transport := &http.Transport{}
	if checksumExtraRootCAs != nil {
		transport.TLSClientConfig = &tls.Config{RootCAs: checksumExtraRootCAs}
	}
	client := &http.Client{
		Timeout:   checksumFetchTimeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing non-https redirect to %s", req.URL.Redacted())
			}
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: build request: %w", ErrChecksumFetchFailed, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrChecksumFetchFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("%w: unexpected status %s", ErrChecksumFetchFailed, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, sumsFileMaxSize+1))
	if err != nil {
		return "", fmt.Errorf("%w: read body: %w", ErrChecksumFetchFailed, err)
	}
	if len(body) > sumsFileMaxSize {
		return "", fmt.Errorf("%w: sums file exceeds %d byte limit", ErrChecksumFetchFailed, sumsFileMaxSize)
	}

	digest, _, err := parseSumsFile(body, filename)
	if err != nil {
		return "", err
	}
	return digest, nil
}

// parseSumsFile returns the hex digest matching filename (case-sensitive), plus the lowercase algorithm a BSD line names.
// Accepts GNU coreutils text/binary, BSD-style, and bare single-token (Alpine .sha512) formats.
// GPG cleartext-signed armor is tolerated: its body lines always appear in pairs, keeping bareCount ≥ 2.
func parseSumsFile(body []byte, filename string) (digest, bsdAlgo string, err error) {
	var bareDigest string
	bareCount := 0
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), sumsFileMaxSize+1)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		switch len(fields) {
		case 1:
			bareDigest = fields[0]
			bareCount++
		case 2:
			name := strings.TrimPrefix(fields[1], "*")
			if name == filename {
				return fields[0], "", nil
			}
		case 4:
			// BSD-style: "<algo> (<name>) = <hex>". The "=" guard rejects PGP armor lines that happen to tokenise to 4 fields.
			inner, open := strings.CutPrefix(fields[1], "(")
			name, closed := strings.CutSuffix(inner, ")")
			if fields[2] != "=" || !open || !closed {
				continue
			}
			if name == filename {
				return fields[3], strings.ToLower(fields[0]), nil
			}
		default:
			// Skip unrecognised shapes; signed sums files may have trailing commentary.
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", fmt.Errorf("%w: scan sums file: %w", ErrChecksumFetchFailed, err)
	}

	// Alpine single-file fallback: accept only if exactly one bare-digest line in the file.
	if bareCount == 1 {
		return bareDigest, "", nil
	}

	return "", "", fmt.Errorf("%w: %s", ErrChecksumNotFound, filename)
}

// hashImageFile streams imagePath through hasher and returns lowercase hex.
// Rejects zero-byte files so a truncated download surfaces as an explicit error.
func hashImageFile(imagePath string, hasher hash.Hash) (string, error) {
	f, err := os.Open(imagePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return "", err
	}
	if stat.Size() == 0 {
		return "", fmt.Errorf("image file %s is empty (likely a truncated or failed download)", imagePath)
	}

	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// SelectNewestImage returns the newest image ID among images, its CreationDate,
// and how many images were considered. Nil images, images with an empty ImageId
// and nil tag entries are skipped; a non-empty excludeTagKey drops images
// carrying that tag key. Returns an empty ID when nothing matches.
func SelectNewestImage(images []*ec2.Image, excludeTagKey string) (imageID, created string, matches int) {
	for _, img := range images {
		if img == nil || aws.StringValue(img.ImageId) == "" {
			continue
		}
		// DescribeImages filters have no negation, so callers that must reject a
		// tag (e.g. gpu-vendor) can only do it client-side, here.
		if excludeTagKey != "" && imageHasTagKey(img, excludeTagKey) {
			continue
		}
		matches++
		// CreationDate is a fixed-width RFC3339 timestamp, so lexicographic
		// comparison orders it correctly without parsing.
		if c := aws.StringValue(img.CreationDate); imageID == "" || c > created {
			imageID, created = aws.StringValue(img.ImageId), c
		}
	}
	return imageID, created, matches
}

// imageHasTagKey reports whether img carries a tag with the given key.
func imageHasTagKey(img *ec2.Image, key string) bool {
	for _, t := range img.Tags {
		if t != nil && aws.StringValue(t.Key) == key {
			return true
		}
	}
	return false
}

type Images struct {
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	Distro       string    `json:"distro"`
	Version      string    `json:"version"`
	Arch         string    `json:"arch"`
	Platform     string    `json:"platform"`
	CreatedAt    time.Time `json:"created_at"`
	URL          string    `json:"url"`
	Checksum     string    `json:"checksum"`
	ChecksumType string    `json:"checksum_type"`
	BootMode     string    `json:"boot_mode"`
	// Tags are copied onto the imported AMI's metadata for UI filtering (e.g. spinifex:managed-by).
	Tags map[string]string `json:"tags,omitempty"`
}

// distroFamilies maps a distro name to its cloud-init family. Keys are lowercase.
var distroFamilies = map[string]string{
	"debian": "debian",
	"ubuntu": "debian",
	"rocky":  "rhel",
	"rhel":   "rhel",
	"alma":   "rhel",
	"fedora": "rhel",
	"centos": "rhel",
	"alpine": "alpine",
}

// DistroFamily returns the cloud-init family for distro.
// Unknown or empty distro defaults to "debian" with a warning; RHEL family requires an explicit distro flag.
func DistroFamily(distro string) string {
	d := strings.ToLower(strings.TrimSpace(distro))
	if family, ok := distroFamilies[d]; ok {
		return family
	}
	slog.Warn("unknown distro, defaulting to debian-family cloud-init", "distro", distro)
	return "debian"
}

var AvailableImages = map[string]Images{

	"debian-13-x86_64": {
		Name:         "debian-13-x86_64",
		Description:  "Debian 13 (Trixie) x86_64 cloud image",
		Distro:       "debian",
		Version:      "13",
		Arch:         "x86_64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC),
		URL:          "https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.tar.xz",
		Checksum:     "https://cloud.debian.org/images/cloud/trixie/latest/SHA512SUMS",
		ChecksumType: "sha512",
		BootMode:     "uefi",
	},

	"debian-13-arm64": {
		Name:         "debian-13-arm64",
		Description:  "Debian 13 (Trixie) arm64 cloud image",
		Distro:       "debian",
		Version:      "13",
		Arch:         "arm64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC),
		URL:          "https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-arm64.tar.xz",
		Checksum:     "https://cloud.debian.org/images/cloud/trixie/latest/SHA512SUMS",
		ChecksumType: "sha512",
		BootMode:     "uefi",
	},

	"ubuntu-26.04-x86_64": {
		Name:         "ubuntu-26.04-x86_64",
		Description:  "Ubuntu 26.04 LTS (Resolute Reindeer) x86_64 cloud image",
		Distro:       "ubuntu",
		Version:      "26.04",
		Arch:         "x86_64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC),
		URL:          "https://cloud-images.ubuntu.com/releases/resolute/release/ubuntu-26.04-server-cloudimg-amd64.img",
		Checksum:     "https://cloud-images.ubuntu.com/releases/resolute/release/SHA256SUMS",
		ChecksumType: "sha256",
		BootMode:     "uefi",
	},

	"ubuntu-26.04-arm64": {
		Name:         "ubuntu-26.04-arm64",
		Description:  "Ubuntu 26.04 LTS (Resolute Reindeer) arm64 cloud image",
		Distro:       "ubuntu",
		Version:      "26.04",
		Arch:         "arm64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC),
		URL:          "https://cloud-images.ubuntu.com/releases/resolute/release/ubuntu-26.04-server-cloudimg-arm64.img",
		Checksum:     "https://cloud-images.ubuntu.com/releases/resolute/release/SHA256SUMS",
		ChecksumType: "sha256",
		BootMode:     "uefi",
	},

	"alpine-3.24.1-x86_64": {
		Name:         "alpine-3.24.1-x86_64",
		Description:  "Alpine Linux 3.24.1 x86_64 cloud image",
		Distro:       "alpine",
		Version:      "3.24.1",
		Arch:         "x86_64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC),
		URL:          "https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/cloud/generic_alpine-3.24.1-x86_64-uefi-cloudinit-r0.qcow2",
		Checksum:     "https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/cloud/generic_alpine-3.24.1-x86_64-uefi-cloudinit-r0.qcow2.sha512",
		ChecksumType: "sha512",
		BootMode:     "uefi",
	},

	"alpine-3.24.1-arm64": {
		Name:         "alpine-3.24.1-arm64",
		Description:  "Alpine Linux 3.24.1 arm64 cloud image",
		Distro:       "alpine",
		Version:      "3.24.1",
		Arch:         "arm64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC),
		URL:          "https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/cloud/generic_alpine-3.24.1-aarch64-uefi-cloudinit-r0.qcow2",
		Checksum:     "https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/cloud/generic_alpine-3.24.1-aarch64-uefi-cloudinit-r0.qcow2.sha512",
		ChecksumType: "sha512",
		BootMode:     "uefi",
	},

	"rocky-10-x86_64": {
		Name:         "rocky-10-x86_64",
		Description:  "Rocky Linux 10 x86_64 cloud image",
		Distro:       "rocky",
		Version:      "10",
		Arch:         "x86_64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
		URL:          "https://dl.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud-Base.latest.x86_64.qcow2",
		Checksum:     "https://dl.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud-Base.latest.x86_64.qcow2.CHECKSUM",
		ChecksumType: "sha256",
		BootMode:     "uefi",
	},

	"rocky-10-arm64": {
		Name:         "rocky-10-arm64",
		Description:  "Rocky Linux 10 arm64 cloud image",
		Distro:       "rocky",
		Version:      "10",
		Arch:         "arm64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
		URL:          "https://dl.rockylinux.org/pub/rocky/10/images/aarch64/Rocky-10-GenericCloud-Base.latest.aarch64.qcow2",
		Checksum:     "https://dl.rockylinux.org/pub/rocky/10/images/aarch64/Rocky-10-GenericCloud-Base.latest.aarch64.qcow2.CHECKSUM",
		ChecksumType: "sha256",
		BootMode:     "uefi",
	},

	"ubuntu-26.04-nvidia-gpu-x86_64": {
		Name:         "ubuntu-26.04-nvidia-gpu-x86_64",
		Description:  "Ubuntu 26.04 NVIDIA GPU base image — NVIDIA server driver, Python toolchain, Docker, nvidia-container-toolkit",
		Distro:       "ubuntu",
		Version:      "26.04",
		Arch:         "x86_64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		URL:          "https://iso.mulgadc.com/system-ami/ubuntu-26.04-nvidia-gpu-x86_64.qcow2",
		Checksum:     "https://iso.mulgadc.com/system-ami/ubuntu-26.04-nvidia-gpu-x86_64.qcow2.sha256",
		ChecksumType: "sha256",
		BootMode:     "uefi",
		Tags:         map[string]string{"gpu-vendor": "nvidia"},
	},

	// Bedrock self-host serving AMI. Resolved by spinifex:managed-by=bedrock +
	// spinifex:bedrock-role=vllm-serving tags — LaunchServingVM's
	// resolveServingAMI filters on both, never by name.
	"ubuntu-26.04-vllm-serving-x86_64": {
		Name:         "ubuntu-26.04-vllm-serving-x86_64",
		Description:  "Ubuntu 26.04 Bedrock co-served bundle image — NVIDIA GPU base + vLLM and TEI baked side by side (uv-managed venv, TEI staged from HuggingFace's CUDA image), one bedrock-serve@ instance per bundle member against its own read-only weights mount",
		Distro:       "ubuntu",
		Version:      "26.04",
		Arch:         "x86_64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC),
		URL:          "https://iso.mulgadc.com/system-ami/ubuntu-26.04-vllm-serving-x86_64.qcow2",
		Checksum:     "https://iso.mulgadc.com/system-ami/ubuntu-26.04-vllm-serving-x86_64.qcow2.sha256",
		ChecksumType: "sha256",
		BootMode:     "uefi",
		Tags: map[string]string{
			"spinifex:managed-by":   "bedrock",
			"spinifex:bedrock-role": "vllm-serving",
			"gpu-vendor":            "nvidia",
		},
	},

	"ubuntu-26.04-amd-gpu-x86_64": {
		Name:         "ubuntu-26.04-amd-gpu-x86_64",
		Description:  "Ubuntu 26.04 AMD GPU base image — linux-firmware, ROCm CLI, Python toolchain, Docker",
		Distro:       "ubuntu",
		Version:      "26.04",
		Arch:         "x86_64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		URL:          "https://iso.mulgadc.com/system-ami/ubuntu-26.04-amd-gpu-x86_64.qcow2",
		Checksum:     "https://iso.mulgadc.com/system-ami/ubuntu-26.04-amd-gpu-x86_64.qcow2.sha256",
		ChecksumType: "sha256",
		BootMode:     "uefi",
		Tags:         map[string]string{"gpu-vendor": "amd"},
	},

	// EKS node system AMI. Resolved by spinifex:managed-by=eks tag — importing this entry is sufficient for CreateCluster/CreateNodegroup.
	"spinifex-eks-node": {
		Name:         "spinifex-eks-node",
		Description:  "Mulga EKS node image — Alpine 3.21.7 + K3s v1.32.5 + eks-token-webhook (server|agent role selected at first boot)",
		Distro:       "alpine",
		Version:      "3.21.7",
		Arch:         "x86_64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
		URL:          "https://iso.mulgadc.com/system-ami/spinifex-eks-node-x86_64.qcow2",
		Checksum:     "https://iso.mulgadc.com/system-ami/spinifex-eks-node-x86_64.qcow2.sha256",
		ChecksumType: "sha256",
		BootMode:     "bios",
		Tags:         map[string]string{"spinifex:managed-by": "eks"},
	},

	// GPU EKS node system AMI. Resolved by spinifex:managed-by=eks + gpu-vendor=nvidia
	// tags — CreateNodegroup selects it for GPU instance types instead of the Alpine AMI.
	"spinifex-eks-node-gpu": {
		Name:         "spinifex-eks-node-gpu",
		Description:  "Mulga EKS GPU node image — Ubuntu 26.04 + NVIDIA driver + K3s v1.32.5, agent-only (GPU nodes never run the EKS control plane, so no role selector or eks-token-webhook)",
		Distro:       "ubuntu",
		Version:      "26.04",
		Arch:         "x86_64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
		URL:          "https://iso.mulgadc.com/system-ami/spinifex-eks-node-gpu-x86_64.qcow2",
		Checksum:     "https://iso.mulgadc.com/system-ami/spinifex-eks-node-gpu-x86_64.qcow2.sha256",
		ChecksumType: "sha256",
		// Ubuntu 26.04 is built GPT/EFI-only; legacy BIOS finds no bootable MBR
		// bootloader and hangs at firmware with zero serial output (no OVMF/pflash
		// is attached unless BootMode is uefi/uefi-preferred, see vm/lifecycle.go).
		BootMode: "uefi",
		Tags:     map[string]string{"spinifex:managed-by": "eks", "gpu-vendor": "nvidia"},
	},

	// ECS container-instance system AMI. Resolved by spinifex:managed-by=ecs tag —
	// importing this entry lets ECS launch container instances that register and run tasks.
	"spinifex-ecs-node": {
		Name:         "spinifex-ecs-node",
		Description:  "Mulga ECS node image — Alpine 3.21.7 + containerd + ecs-agent (registers as a container instance at first boot)",
		Distro:       "alpine",
		Version:      "3.21.7",
		Arch:         "x86_64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 6, 26, 0, 0, 0, 0, time.UTC),
		URL:          "https://iso.mulgadc.com/system-ami/spinifex-ecs-node-x86_64.qcow2",
		Checksum:     "https://iso.mulgadc.com/system-ami/spinifex-ecs-node-x86_64.qcow2.sha256",
		ChecksumType: "sha256",
		BootMode:     "bios",
		Tags:         map[string]string{"spinifex:managed-by": "ecs"},
	},

	// GPU ECS node system AMI. Resolved by spinifex:managed-by=ecs + gpu-vendor=nvidia
	// tags — ProvisionCapacity selects it for GPU instance types instead of the Alpine AMI.
	"spinifex-ecs-node-gpu": {
		Name:         "spinifex-ecs-node-gpu",
		Description:  "Mulga ECS GPU node image — Ubuntu 26.04 + NVIDIA driver + containerd + ecs-agent (registers as a container instance at first boot)",
		Distro:       "ubuntu",
		Version:      "26.04",
		Arch:         "x86_64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 6, 26, 0, 0, 0, 0, time.UTC),
		URL:          "https://iso.mulgadc.com/system-ami/spinifex-ecs-node-gpu-x86_64.qcow2",
		Checksum:     "https://iso.mulgadc.com/system-ami/spinifex-ecs-node-gpu-x86_64.qcow2.sha256",
		ChecksumType: "sha256",
		// Ubuntu 26.04 is built GPT/EFI-only; legacy BIOS finds no bootable MBR
		// bootloader and hangs at firmware with zero serial output (no OVMF/pflash
		// is attached unless BootMode is uefi/uefi-preferred, see vm/lifecycle.go).
		BootMode: "uefi",
		Tags:     map[string]string{"spinifex:managed-by": "ecs", "gpu-vendor": "nvidia"},
	},

	// Resolved by spinifex:managed-by=rds plus the engine tags an
	// Engine/EngineVersion request maps onto, not by parsing the image name.
	"spinifex-rds-postgres": {
		Name:         "spinifex-rds-postgres",
		Description:  "Mulga RDS PostgreSQL image — Alpine 3.24.1 + PostgreSQL 18 + rds-init (initdb, master password, parameters and TLS applied at first boot)",
		Distro:       "alpine",
		Version:      "3.24.1",
		Arch:         "x86_64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
		URL:          "https://iso.mulgadc.com/system-ami/spinifex-rds-postgres-x86_64.qcow2",
		Checksum:     "https://iso.mulgadc.com/system-ami/spinifex-rds-postgres-x86_64.qcow2.sha256",
		ChecksumType: "sha256",
		BootMode:     "bios",
		Tags: map[string]string{
			"spinifex:managed-by": "rds", "engine": "postgres", "engine-version": "18",
			"rds-data-volume-contract": "format-auth-v1",
		},
	},

	// Same resolution path as rds-postgres — only the engine tags differ, which
	// is what keeps a MariaDB request off the PostgreSQL image and vice versa.
	"spinifex-rds-mariadb": {
		Name:         "spinifex-rds-mariadb",
		Description:  "Mulga RDS MariaDB image — Alpine 3.24.1 + MariaDB 11.8 + rds-init (install-db, master password, parameters and TLS applied at first boot)",
		Distro:       "alpine",
		Version:      "3.24.1",
		Arch:         "x86_64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
		URL:          "https://iso.mulgadc.com/system-ami/spinifex-rds-mariadb-x86_64.qcow2",
		Checksum:     "https://iso.mulgadc.com/system-ami/spinifex-rds-mariadb-x86_64.qcow2.sha256",
		ChecksumType: "sha256",
		BootMode:     "bios",
		Tags: map[string]string{
			"spinifex:managed-by": "rds", "engine": "mariadb", "engine-version": "11.8",
			"rds-data-volume-contract": "format-auth-v1",
		},
	},
}

// ExtractDiskImageFromFile turns a downloaded artifact at imagepath into a bootable disk
// image and returns its path. tar-based archives are expanded into tmpdir and their listing
// is scanned for the member that carries the payload; single-file compressed artifacts
// (.zst, .bz2, .xz, .gz) are streamed straight into tmpdir under their own decompressed name.
// The source artifact at imagepath is never modified or overwritten.
func ExtractDiskImageFromFile(imagepath string, tmpdir string) (diskimage string, err error) {
	var args []string
	var execCmd string

	_, err = os.Stat(imagepath)
	if err != nil {
		return diskimage, err
	}

	imagefile := filepath.Base(imagepath)

	switch {
	case strings.HasSuffix(imagefile, ".raw") || strings.HasSuffix(imagefile, ".img") || strings.HasSuffix(imagefile, ".qcow2") || strings.HasSuffix(imagefile, ".qcow"):
		path, err := filepath.Abs(imagepath)
		if err != nil {
			return path, err
		}
		return finalizeDiskImage(path, tmpdir)

	case strings.HasSuffix(imagefile, ".tar.xz"):
		args = []string{"xfvJ", imagepath, "-C", tmpdir}
		execCmd = "tar"

	case strings.HasSuffix(imagefile, ".tar.gz") || strings.HasSuffix(imagefile, ".tgz"):
		args = []string{"xfvz", imagepath, "-C", tmpdir}
		execCmd = "tar"

	case strings.HasSuffix(imagefile, ".tar"):
		args = []string{"xfv", imagepath, "-C", tmpdir}
		execCmd = "tar"

	case strings.HasSuffix(imagefile, ".zst"):
		extracted, err := decompressZstd(imagepath, tmpdir)
		if err != nil {
			return diskimage, err
		}
		return finalizeDiskImage(extracted, tmpdir)

	case strings.HasSuffix(imagefile, ".bz2"):
		extracted, err := decompressBzip2(imagepath, tmpdir)
		if err != nil {
			return diskimage, err
		}
		return finalizeDiskImage(extracted, tmpdir)

	case strings.HasSuffix(imagefile, ".xz"):
		extracted, err := decompressXz(imagepath, tmpdir)
		if err != nil {
			return diskimage, err
		}
		return finalizeDiskImage(extracted, tmpdir)

	case strings.HasSuffix(imagefile, ".gz"):
		extracted, err := decompressGzip(imagepath, tmpdir)
		if err != nil {
			return diskimage, err
		}
		return finalizeDiskImage(extracted, tmpdir)

	default:
		return diskimage, errors.New("unsupported filetype")
	}

	cmd := exec.Command(execCmd, args...)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return diskimage, err
	}

	diskimage, err = extractDiskImagePath(tmpdir, output)

	return diskimage, err
}

// finalizeDiskImage runs the file(1) sniff gate on path and, if it detects a QCOW2 payload,
// converts it to raw inside tmpdir. path may be the original source (already .raw/.img/.qcow2)
// or a file this package just decompressed into tmpdir; either way the source is left intact
// and only the raw output lands in tmpdir.
func finalizeDiskImage(path string, tmpdir string) (diskimage string, err error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return absPath, err
	}

	err = validateDiskImagePath(absPath)
	if errors.Is(err, ErrQCOWDetected) {
		base := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(absPath), ".qcow2"), ".qcow")
		extractpath := filepath.Join(tmpdir, base+".raw")

		cmd := exec.Command("qemu-img", "convert", "-f", "qcow2", "-O", "raw", absPath, "-C", extractpath)
		if _, err := cmd.Output(); err != nil {
			return absPath, err
		}

		return extractpath, nil
	}

	return absPath, err
}

// sparseBlockSize is the unit decompressToTemp checks for an all-zero run before punching a
// hole instead of writing it. 1 MiB comfortably amortises the read/seek syscall overhead over
// a multi-GiB image and still catches the large contiguous zero runs that dominate a sparse
// raw disk (unused extents span tens of MiB or more); a zero run shorter than one block simply
// gets written literally, which is the safe direction for a false negative to fail in.
const sparseBlockSize = 1 << 20

// decompressToTemp streams r into a new sparse file inside tmpdir, named after src with
// suffix stripped, and returns that path. All-zero blocks are skipped via Seek rather than
// written, so a mostly-unused multi-GiB raw image (e.g. a Talos artifact, ~11 GiB apparent but
// only a few hundred MiB of real content) costs disk space proportional to its real content —
// matching the existing qcow2-to-raw path, which already writes sparse via qemu-img convert.
// It never writes into src's own directory, so a multi-GiB source is never duplicated beside
// itself while the caller-provided tmpdir receives the copy.
func decompressToTemp(src, tmpdir, suffix string, r io.Reader) (string, error) {
	outPath := filepath.Join(tmpdir, strings.TrimSuffix(filepath.Base(src), suffix))

	out, err := os.Create(outPath)
	if err != nil {
		return "", err
	}
	defer out.Close()

	var size int64
	buf := make([]byte, sparseBlockSize)
	for {
		n, readErr := io.ReadFull(r, buf)
		if n > 0 {
			block := buf[:n]
			if isZeroBlock(block) {
				if _, err := out.Seek(int64(n), io.SeekCurrent); err != nil {
					return "", fmt.Errorf("decompress %s: seek hole: %w", src, err)
				}
			} else if _, err := out.Write(block); err != nil {
				return "", fmt.Errorf("decompress %s: %w", src, err)
			}
			size += int64(n)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				break
			}
			return "", fmt.Errorf("decompress %s: %w", src, readErr)
		}
	}

	// A trailing all-zero block only advances the file position via Seek, which does not
	// extend the file on its own; Truncate records that final hole at the right size.
	if err := out.Truncate(size); err != nil {
		return "", fmt.Errorf("decompress %s: truncate: %w", src, err)
	}

	return outPath, nil
}

// isZeroBlock reports whether every byte in b is zero.
func isZeroBlock(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// decompressZstd streams a single-file .zst artifact (e.g. Talos raw.zst) into tmpdir using
// klauspost/compress, already a transitive dependency via containerd, so no new module is
// added. It is a pure-Go streaming decoder, unlike shelling out to a zstd binary that may not
// exist on a customer node.
func decompressZstd(src, tmpdir string) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()

	zr, err := zstd.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("zstd reader for %s: %w", src, err)
	}
	defer zr.Close()

	return decompressToTemp(src, tmpdir, ".zst", zr)
}

// decompressBzip2 streams a single-file .bz2 artifact (e.g. Flatcar img.bz2) into tmpdir
// using the standard library's decompress-only reader; no bzip2 encoder exists in this
// codebase, so there is nothing to conflate this with a compression path.
func decompressBzip2(src, tmpdir string) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()

	return decompressToTemp(src, tmpdir, ".bz2", bzip2.NewReader(f))
}

// decompressGzip streams a single-file .gz artifact into tmpdir using the standard library.
func decompressGzip(src, tmpdir string) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("gzip reader for %s: %w", src, err)
	}
	defer gr.Close()

	return decompressToTemp(src, tmpdir, ".gz", gr)
}

// decompressXz shells out to the xz binary, piping its decompressed stdout through
// decompressToTemp via an io.Pipe so a plain .xz image gets the same sparse-hole handling as
// the other codecs rather than a dense copy. No pure-Go xz decoder is already a dependency of
// this module, and the existing .tar.xz branch already requires the xz binary on the host
// (tar's -J flag invokes it internally), so this adds no new host requirement — only makes the
// existing one explicit for the plain-.xz case.
func decompressXz(src, tmpdir string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.Command("xz", "-dc", src)
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start xz for %s: %w", src, err)
	}

	// cmd.Stdout is not an *os.File, so exec.Cmd already runs its own copier goroutine from
	// the child's real pipe into pw; Wait joins it. This goroutine closes pw with the
	// process's exit error once Wait returns, so a Read past the end of the stream on pr
	// surfaces that error instead of a bare EOF.
	waitErrCh := make(chan error, 1)
	go func() {
		waitErr := cmd.Wait()
		waitErrCh <- waitErr
		pw.CloseWithError(waitErr)
	}()

	outPath, err := decompressToTemp(src, tmpdir, ".xz", pr)
	if err != nil {
		// decompressToTemp gave up before draining the pipe (e.g. a local write failure), so
		// close the read end now — otherwise the copier goroutine above would stay blocked
		// writing into it forever, wedging cmd.Wait() and leaking both goroutines.
		pr.CloseWithError(err)
	}
	waitErr := <-waitErrCh

	if waitErr != nil {
		return "", fmt.Errorf("xz decompress %s: %w: %s", src, waitErr, strings.TrimSpace(stderr.String()))
	}
	if err != nil {
		return "", fmt.Errorf("decompress %s: %w", src, err)
	}

	return outPath, nil
}

func extractDiskImagePath(imagedir string, output []byte) (diskimage string, err error) {
	reader := bytes.NewReader(output)

	r := bufio.NewReader(reader)

	for {
		line, readErr := r.ReadString('\n')
		line = strings.TrimRight(line, "\n")

		// MacOS tar, filenames begin with `x FILE` (to STDERR)
		if runtime.GOOS == "darwin" && strings.HasPrefix(line, "x ") {
			line = strings.Replace(line, "x ", "", 1)
		}

		if strings.HasSuffix(line, ".raw") || strings.HasSuffix(line, ".img") {
			diskimage := fmt.Sprintf("%s/%s", imagedir, line)
			err = validateDiskImagePath(diskimage)
			return diskimage, err
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return "", fmt.Errorf("read tar output: %w", readErr)
		}
	}

	return diskimage, err
}

func validateDiskImagePath(diskimage string) (err error) {
	args := []string{
		diskimage,
	}

	cmd := exec.Command("file", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("run file command on %s: %w", diskimage, err)
	}

	filetype := strings.Split(string(output), ":")

	if len(filetype) > 1 {
		if strings.Contains(filetype[1], "DOS/MBR boot sector") || strings.Contains(filetype[1], "Linux ") {
			return nil
		} else if strings.Contains(filetype[1], "QEMU QCOW") {
			return ErrQCOWDetected
		}
	}

	return errors.New("no valid disk image found")
}
