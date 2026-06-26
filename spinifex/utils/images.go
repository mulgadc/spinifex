package utils

import (
	"bufio"
	"bytes"
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
)

var ErrQCOWDetected = errors.New("qcow format detected")

const sumsFileMaxSize = 1 * 1024 * 1024 // 1 MB, matches formation.go JoinRequest cap.

var (
	ErrChecksumMismatch        = errors.New("image checksum mismatch")
	ErrChecksumNotFound        = errors.New("checksum entry for image filename not found")
	ErrUnsupportedChecksumType = errors.New("unsupported checksum type")
	ErrChecksumFetchFailed     = errors.New("checksum fetch failed")
)

// checksumExtraRootCAs is a test-only hook for httptest TLS servers; nil in production.
var checksumExtraRootCAs *x509.CertPool

// checksumFetchTimeout is a var so tests can shrink it to exercise the deadline path.
var checksumFetchTimeout = 30 * time.Second

// VerifyImageChecksum fetches the sums file at checksumURL, finds the entry for imagePath's basename,
// hashes the file with checksumType ("sha256" or "sha512"), and compares digests.
// Fails closed: non-HTTPS, non-2xx, oversized response, or digest mismatch all return a wrapped error.
func VerifyImageChecksum(imagePath, checksumURL, checksumType string) error {
	hasher, err := newHasher(checksumType)
	if err != nil {
		return err
	}

	expected, err := fetchExpectedDigest(checksumURL, filepath.Base(imagePath))
	if err != nil {
		slog.Error("image checksum fetch failed", "source", checksumURL, "err", err)
		return err
	}

	// Catch algorithm mismatch before ConstantTimeCompare; a length mismatch would look like tampering.
	if len(expected) != hasher.Size()*2 {
		return fmt.Errorf("%w: digest length %d from sums file does not match %s output length %d",
			ErrChecksumFetchFailed, len(expected), checksumType, hasher.Size()*2)
	}

	actual, err := hashImageFile(imagePath, hasher)
	if err != nil {
		return fmt.Errorf("hash image file: %w", err)
	}

	if subtle.ConstantTimeCompare([]byte(expected), []byte(actual)) != 1 {
		slog.Error("image checksum mismatch",
			"image", imagePath,
			"algorithm", checksumType,
			"expected", expected,
			"actual", actual,
			"source", checksumURL,
		)
		return fmt.Errorf("%w: expected %s got %s", ErrChecksumMismatch, expected, actual)
	}

	return nil
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
		return "", fmt.Errorf("%w: parse url: %v", ErrChecksumFetchFailed, err)
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
		return "", fmt.Errorf("%w: build request: %v", ErrChecksumFetchFailed, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrChecksumFetchFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("%w: unexpected status %s", ErrChecksumFetchFailed, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, sumsFileMaxSize+1))
	if err != nil {
		return "", fmt.Errorf("%w: read body: %v", ErrChecksumFetchFailed, err)
	}
	if len(body) > sumsFileMaxSize {
		return "", fmt.Errorf("%w: sums file exceeds %d byte limit", ErrChecksumFetchFailed, sumsFileMaxSize)
	}

	digest, err := parseSumsFile(body, filename)
	if err != nil {
		return "", err
	}
	return digest, nil
}

// parseSumsFile scans a sums file and returns the hex digest matching filename (case-sensitive).
// Accepts GNU coreutils text/binary, BSD-style, and bare single-token (Alpine .sha512) formats.
// GPG cleartext-signed armor is tolerated: its body lines always appear in pairs, keeping bareCount ≥ 2.
func parseSumsFile(body []byte, filename string) (string, error) {
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
				return fields[0], nil
			}
		case 4:
			// BSD-style: "<algo> (<name>) = <hex>". The "=" guard rejects PGP armor lines that happen to tokenise to 4 fields.
			if fields[2] != "=" || !strings.HasPrefix(fields[1], "(") || !strings.HasSuffix(fields[1], ")") {
				continue
			}
			name := strings.TrimSuffix(strings.TrimPrefix(fields[1], "("), ")")
			if name == filename {
				return fields[3], nil
			}
		default:
			// Skip unrecognised shapes; signed sums files may have trailing commentary.
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("%w: scan sums file: %v", ErrChecksumFetchFailed, err)
	}

	// Alpine single-file fallback: accept only if exactly one bare-digest line in the file.
	if bareCount == 1 {
		return bareDigest, nil
	}

	return "", fmt.Errorf("%w: %s", ErrChecksumNotFound, filename)
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
		CreatedAt:    time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC),
		URL:          "https://cloud.debian.org/images/cloud/trixie/20260518-2482/debian-13-genericcloud-amd64-20260518-2482.tar.xz",
		Checksum:     "https://cloud.debian.org/images/cloud/trixie/20260518-2482/SHA512SUMS",
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
		CreatedAt:    time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC),
		URL:          "https://cloud.debian.org/images/cloud/trixie/20260518-2482/debian-13-genericcloud-arm64-20260518-2482.tar.xz",
		Checksum:     "https://cloud.debian.org/images/cloud/trixie/20260518-2482/SHA512SUMS",
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
		CreatedAt:    time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC),
		URL:          "https://cloud-images.ubuntu.com/resolute/20260421/resolute-server-cloudimg-amd64.img",
		Checksum:     "https://cloud-images.ubuntu.com/resolute/20260421/SHA256SUMS",
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
		CreatedAt:    time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC),
		URL:          "https://cloud-images.ubuntu.com/resolute/20260421/resolute-server-cloudimg-arm64.img",
		Checksum:     "https://cloud-images.ubuntu.com/resolute/20260421/SHA256SUMS",
		ChecksumType: "sha256",
		BootMode:     "uefi",
	},

	"alpine-3.22.4-x86_64": {
		Name:         "alpine-3.22.4-x86_64",
		Description:  "Alpine Linux 3.22.4 x86_64 cloud image",
		Distro:       "alpine",
		Version:      "3.22.4",
		Arch:         "x86_64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC),
		URL:          "https://dl-cdn.alpinelinux.org/alpine/v3.22/releases/cloud/generic_alpine-3.22.4-x86_64-uefi-cloudinit-r0.qcow2",
		Checksum:     "https://dl-cdn.alpinelinux.org/alpine/v3.22/releases/cloud/generic_alpine-3.22.4-x86_64-uefi-cloudinit-r0.qcow2.sha512",
		ChecksumType: "sha512",
		BootMode:     "uefi",
	},

	"alpine-3.22.4-arm64": {
		Name:         "alpine-3.22.4-arm64",
		Description:  "Alpine Linux 3.22.4 arm64 cloud image",
		Distro:       "alpine",
		Version:      "3.22.4",
		Arch:         "arm64",
		Platform:     "Linux/UNIX",
		CreatedAt:    time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC),
		URL:          "https://dl-cdn.alpinelinux.org/alpine/v3.22/releases/cloud/generic_alpine-3.22.4-aarch64-uefi-cloudinit-r0.qcow2",
		Checksum:     "https://dl-cdn.alpinelinux.org/alpine/v3.22/releases/cloud/generic_alpine-3.22.4-aarch64-uefi-cloudinit-r0.qcow2.sha512",
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
		URL:          "https://dl.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud-Base-10.2-20260525.0.x86_64.qcow2",
		Checksum:     "https://dl.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud-Base-10.2-20260525.0.x86_64.qcow2.CHECKSUM",
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
		URL:          "https://dl.rockylinux.org/pub/rocky/10/images/aarch64/Rocky-10-GenericCloud-Base-10.2-20260525.0.aarch64.qcow2",
		Checksum:     "https://dl.rockylinux.org/pub/rocky/10/images/aarch64/Rocky-10-GenericCloud-Base-10.2-20260525.0.aarch64.qcow2.CHECKSUM",
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
}

func ExtractDiskImageFromFile(imagepath string, tmpdir string) (diskimage string, err error) {
	var args []string
	var execCmd string

	_, err = os.Stat(imagepath)
	if err != nil {
		return diskimage, err
	}

	imagefile := filepath.Base(imagepath)

	if strings.HasSuffix(imagefile, ".raw") || strings.HasSuffix(imagefile, ".img") || strings.HasSuffix(imagefile, ".qcow2") || strings.HasSuffix(imagefile, ".qcow") {
		path, err := filepath.Abs(imagepath)
		if err != nil {
			return path, err
		}
		err = validateDiskImagePath(path)
		if errors.Is(err, ErrQCOWDetected) {
			extractpath := fmt.Sprintf("%s/%s", tmpdir, imagefile)
			extractpath = strings.TrimSuffix(extractpath, ".qcow2") + ".raw"

			args = []string{
				"convert",
				"-f",
				"qcow2",
				"-O",
				"raw",
				imagepath,
				"-C",
				extractpath,
			}

			execCmd = "qemu-img"

			cmd := exec.Command(execCmd, args...)
			_, err = cmd.Output()

			if err != nil {
				return path, err
			}

			return extractpath, nil
		}

		return path, err
	} else if strings.HasSuffix(imagefile, ".tar.xz") {
		args = []string{
			"xfvJ",
			imagepath,
			"-C",
			tmpdir,
		}

		execCmd = "tar"
	} else if strings.HasSuffix(imagefile, ".tar.gz") || strings.HasSuffix(imagefile, ".tgz") {
		args = []string{
			"xfvz",
			imagepath,
			"-C",
			tmpdir,
		}

		execCmd = "tar"
	} else if strings.HasSuffix(imagefile, ".tar") {
		args = []string{
			"xfv",
			imagepath,
			"-C",
			tmpdir,
		}

		execCmd = "tar"
	} else if strings.HasSuffix(imagefile, ".xz") {
		args = []string{
			"-dk",
			imagepath,
		}

		execCmd = "xz"
	} else {
		err = errors.New("unsupported filetype")
		return diskimage, err
	}

	cmd := exec.Command(execCmd, args...)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return diskimage, err
	}

	diskimage, err = extractDiskImagePath(tmpdir, output)

	return diskimage, err
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
