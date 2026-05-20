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

// Helper functions for OS images

var ErrQCOWDetected = errors.New("qcow format detected")

const sumsFileMaxSize = 1 * 1024 * 1024 // 1 MB, matches formation.go JoinRequest cap.

var (
	ErrChecksumMismatch        = errors.New("image checksum mismatch")
	ErrChecksumNotFound        = errors.New("checksum entry for image filename not found")
	ErrUnsupportedChecksumType = errors.New("unsupported checksum type")
	ErrChecksumFetchFailed     = errors.New("checksum fetch failed")
)

// checksumExtraRootCAs is a test-only hook so unit tests can stand up an
// httptest.NewTLSServer (which uses a self-signed cert) without relaxing the
// HTTPS enforcement. Production builds leave this nil and the verification
// client uses the system trust store.
var checksumExtraRootCAs *x509.CertPool

// checksumFetchTimeout is a var (not const) so tests can shrink it to exercise
// the context-deadline path without waiting 30s.
var checksumFetchTimeout = 30 * time.Second

// VerifyImageChecksum fetches the sums file at checksumURL, locates the entry
// for imagePath's basename, hashes imagePath with the algorithm named by
// checksumType ("sha256" or "sha512"), and compares digests.
//
// Fails closed: every error path returns without accepting the image. A
// non-HTTPS scheme, non-2xx status, transport error, response over 1 MB, or
// cross-scheme redirect all wrap ErrChecksumFetchFailed. Unknown algorithm
// wraps ErrUnsupportedChecksumType. Missing filename entry wraps
// ErrChecksumNotFound. Digest mismatch wraps ErrChecksumMismatch and the
// wrapped error's %v includes expected and actual hex.
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

	// Distinct error for a catalog/sums-file algorithm mismatch — without this
	// it'd surface as "tampering" via ConstantTimeCompare's length-0 return.
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

// fetchExpectedDigest downloads the sums file and returns the hex digest for
// the given filename. Enforces HTTPS on initial URL and every redirect hop,
// caps redirects at 10, and truncates response at sumsFileMaxSize+1 so the
// size check is unambiguous.
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

// parseSumsFile scans a sums file and returns the hex digest matching filename.
// Filename match is case-sensitive: upstream Debian/Ubuntu/Alpine sums are
// consistently lowercase and a divergence is a real signal.
//
// Accepts four on-the-wire shapes: "<hex>  <name>" (coreutils text mode),
// "<hex> *<name>" (coreutils binary mode), "<algo> (<name>) = <hex>" (BSD
// style, used by Rocky/RHEL/Alma/Fedora/CentOS Stream CHECKSUM files), and a
// bare single-token "<hex>" line (single-file .sha512 from Alpine, which does
// not carry a filename).
//
// GPG cleartext-signed armor is tolerated implicitly: the BEGIN/END markers
// and "Hash:" header don't match any shape's structural guards, and signature
// body lines are individually single tokens but always appear in pairs
// (base64 + the =CRC trailer at minimum), so bareCount stays ≥ 2 and the
// bare-digest fallback never fires on a signed file.
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
			// BSD-style: "<algo> (<name>) = <hex>". The "=" guard also rejects
			// armor lines like "-----BEGIN PGP SIGNED MESSAGE-----" that happen
			// to tokenise into 4 fields. Filenames have no spaces in upstream
			// usage (Rocky/Alma/Fedora pin dated builds).
			if fields[2] != "=" || !strings.HasPrefix(fields[1], "(") || !strings.HasSuffix(fields[1], ")") {
				continue
			}
			name := strings.TrimSuffix(strings.TrimPrefix(fields[1], "("), ")")
			if name == filename {
				return fields[3], nil
			}
		default:
			// Unrecognised shape. Skip rather than reject outright — signed sums
			// files occasionally have trailing commentary we want to tolerate.
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("%w: scan sums file: %v", ErrChecksumFetchFailed, err)
	}

	// Alpine single-file .sha512 fallback: exactly one bare-digest line in the
	// whole file. Any more and we refuse, because we can't tell which line is
	// the right one without a filename.
	if bareCount == 1 {
		return bareDigest, nil
	}

	return "", fmt.Errorf("%w: %s", ErrChecksumNotFound, filename)
}

// hashImageFile streams the image through hasher and returns the digest as
// lowercase hex. Zero-byte files are rejected outright so a truncated
// download surfaces as an explicit error rather than an opaque "checksum
// mismatch".
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
	// Tags are copied onto the imported AMI's AMIMetadata.Tags so the UI
	// can filter/classify the image. Used to mark system-owned AMIs
	// (e.g. the LB/HAProxy image) via spinifex:managed-by.
	Tags map[string]string `json:"tags,omitempty"`
}

// distroFamilies maps a distro name to its cloud-init family. Family selects
// the per-distro branches in the cloud-init template (sudoers group,
// NetworkManager keyfile vs netplan). Keep keys lowercase; callers normalise.
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

// DistroFamily returns the cloud-init family for distro. Unknown or empty
// distro maps to "debian" (today's default rendering) and logs a warning so
// operators using --file imports for custom appliances aren't broken by a
// missing --distro flag; explicit RHEL-family rendering requires
// --distro rocky|rhel|alma|fedora|centos.
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
		CreatedAt:    time.Date(2025, 11, 16, 0, 0, 0, 0, time.UTC),
		URL:          "https://dl.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud-Base-10.1-20251116.0.x86_64.qcow2",
		Checksum:     "https://dl.rockylinux.org/pub/rocky/10/images/x86_64/Rocky-10-GenericCloud-Base-10.1-20251116.0.x86_64.qcow2.CHECKSUM",
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
		CreatedAt:    time.Date(2025, 11, 16, 0, 0, 0, 0, time.UTC),
		URL:          "https://dl.rockylinux.org/pub/rocky/10/images/aarch64/Rocky-10-GenericCloud-Base-10.1-20251116.0.aarch64.qcow2",
		Checksum:     "https://dl.rockylinux.org/pub/rocky/10/images/aarch64/Rocky-10-GenericCloud-Base-10.1-20251116.0.aarch64.qcow2.CHECKSUM",
		ChecksumType: "sha256",
		BootMode:     "uefi",
	},
}

// AMI / image extraction utils
func ExtractDiskImageFromFile(imagepath string, tmpdir string) (diskimage string, err error) {
	var args []string
	var execCmd string

	// Confirm file exists
	_, err = os.Stat(imagepath)

	if err != nil {
		return diskimage, err
	}

	// Extract the filepath
	imagefile := filepath.Base(imagepath)

	// Already in raw/image formt, confirm the file contains a valid disk image/MBR
	if strings.HasSuffix(imagefile, ".raw") || strings.HasSuffix(imagefile, ".img") || strings.HasSuffix(imagefile, ".qcow2") || strings.HasSuffix(imagefile, ".qcow") {
		path, err := filepath.Abs(imagepath)

		if err != nil {
			return path, err
		}

		// Validate the specified filename is indeed a disk image / MBR
		err = validateDiskImagePath(path)

		// Check error response

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
