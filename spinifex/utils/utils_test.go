package utils

import (
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateResourceID(t *testing.T) {
	tests := []struct {
		prefix string
	}{
		{"i"},
		{"r"},
		{"vol"},
		{"snap"},
		{"key"},
		{"eigw"},
		{"ami"},
	}

	for _, tt := range tests {
		t.Run(tt.prefix, func(t *testing.T) {
			id := GenerateResourceID(tt.prefix)
			assert.True(t, strings.HasPrefix(id, tt.prefix+"-"))
			// prefix + "-" + 17 hex chars
			assert.Len(t, id, len(tt.prefix)+1+17)

			// Verify uniqueness
			id2 := GenerateResourceID(tt.prefix)
			assert.NotEqual(t, id, id2)
		})
	}
}

func TestGenerateSocketFile(t *testing.T) {
	socketPath := fmt.Sprintf("%s/%s", t.TempDir(), "utilsunittest")

	name, err := GenerateSocketFile(socketPath)

	assert.NoError(t, err)

	assert.True(t, strings.HasSuffix(name, "utilsunittest.sock"))

	// Test empty socket path
	_, err = GenerateSocketFile("")

	assert.Error(t, err)
}

func TestWaitForNBDReady(t *testing.T) {
	t.Run("unix socket ready", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "nbd.sock")
		ln, err := net.Listen("unix", path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })

		require.NoError(t, WaitForNBDReady(FormatNBDSocketURI(path), time.Second))
	})

	t.Run("unix socket times out", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "missing.sock")
		err := WaitForNBDReady(FormatNBDSocketURI(path), 100*time.Millisecond)
		require.Error(t, err)
	})

	t.Run("tcp listener ready", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
		_, portStr, err := net.SplitHostPort(ln.Addr().String())
		require.NoError(t, err)
		port, err := strconv.Atoi(portStr)
		require.NoError(t, err)

		require.NoError(t, WaitForNBDReady(FormatNBDTCPURI("127.0.0.1", port), time.Second))
	})

	t.Run("tcp listener times out", func(t *testing.T) {
		// Port 1 is unprivileged-bind reserved; nothing will be listening.
		err := WaitForNBDReady(FormatNBDTCPURI("127.0.0.1", 1), 100*time.Millisecond)
		require.Error(t, err)
	})

	t.Run("rejects malformed uri", func(t *testing.T) {
		err := WaitForNBDReady("garbage://nope", time.Second)
		require.Error(t, err)
	})
}

func TestUnmarshalJsonPayload(t *testing.T) {
	type TestStruct struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}

	tests := []struct {
		name        string
		jsonData    string
		expectError bool
		validate    func(t *testing.T, result *TestStruct)
	}{
		{
			name:        "Valid JSON",
			jsonData:    `{"name":"test","value":123}`,
			expectError: false,
			validate: func(t *testing.T, result *TestStruct) {
				assert.Equal(t, "test", result.Name)
				assert.Equal(t, 123, result.Value)
			},
		},
		{
			name:        "Invalid JSON - malformed",
			jsonData:    `{"name":"test","value":}`,
			expectError: true,
			validate:    nil,
		},
		{
			name:        "Invalid JSON - unknown field",
			jsonData:    `{"name":"test","value":123,"unknown":"field"}`,
			expectError: true, // DisallowUnknownFields should cause error
			validate:    nil,
		},
		{
			name:        "Empty JSON",
			jsonData:    `{}`,
			expectError: false,
			validate: func(t *testing.T, result *TestStruct) {
				assert.Empty(t, result.Name)
				assert.Equal(t, 0, result.Value)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var result TestStruct
			errResp := UnmarshalJsonPayload(&result, []byte(tt.jsonData))

			if tt.expectError {
				assert.NotNil(t, errResp, "Expected error response")
			} else {
				assert.Nil(t, errResp, "Expected no error response")
				if tt.validate != nil {
					tt.validate(t, &result)
				}
			}
		})
	}
}

func TestGenerateErrorPayload(t *testing.T) {
	tests := []struct {
		name     string
		code     string
		validate func(t *testing.T, payload []byte)
	}{
		{
			name: "ValidationError",
			code: "ValidationError",
			validate: func(t *testing.T, payload []byte) {
				assert.Contains(t, string(payload), "ValidationError")
				assert.Contains(t, string(payload), "Code")
			},
		},
		{
			name: "InvalidInstanceType",
			code: "InvalidInstanceType",
			validate: func(t *testing.T, payload []byte) {
				assert.Contains(t, string(payload), "InvalidInstanceType")
			},
		},
		{
			name: "CustomError",
			code: "CustomError",
			validate: func(t *testing.T, payload []byte) {
				assert.Contains(t, string(payload), "CustomError")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := GenerateErrorPayload(tt.code)
			assert.NotNil(t, payload)
			assert.NotEmpty(t, payload)
			if tt.validate != nil {
				tt.validate(t, payload)
			}
		})
	}
}

func TestValidateErrorPayload(t *testing.T) {
	tests := []struct {
		name         string
		payload      string
		expectError  bool
		expectedCode string
	}{
		{
			name:         "Valid error payload",
			payload:      `{"Code":"ValidationError","Message":null}`,
			expectError:  true,
			expectedCode: "ValidationError",
		},
		{
			name:        "Valid success payload (no Code field)",
			payload:     `{"ReservationId":"r-123","Instances":[]}`,
			expectError: false,
		},
		{
			name:        "Empty payload",
			payload:     `{}`,
			expectError: true, // Empty payload treated as error by ValidateErrorPayload
		},
		{
			name:        "Invalid JSON",
			payload:     `{invalid}`,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responseError, err := ValidateErrorPayload([]byte(tt.payload))

			if tt.expectError {
				if tt.expectedCode != "" {
					// Check for specific error code
					assert.Error(t, err)
					if responseError.Code != nil {
						assert.Equal(t, tt.expectedCode, *responseError.Code)
					}
				}
			} else {
				// No error expected
				assert.NoError(t, err)
			}
		})
	}
}

func TestParseNBDURI(t *testing.T) {
	tests := []struct {
		name     string
		uri      string
		wantType string
		wantPath string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{
			name:     "Unix socket URI",
			uri:      "nbd+unix:///?socket=/run/user/1000/nbd-vol-123.sock",
			wantType: "unix",
			wantPath: "/run/user/1000/nbd-vol-123.sock",
		},
		{
			name:     "Unix socket URI without the empty export path",
			uri:      "nbd+unix://?socket=/run/nbd-vol.sock",
			wantType: "unix",
			wantPath: "/run/nbd-vol.sock",
		},
		{
			name:     "legacy QEMU nbd:unix: filename form",
			uri:      "nbd:unix:/run/user/1000/nbd-vol-123.sock",
			wantType: "unix",
			wantPath: "/run/user/1000/nbd-vol-123.sock",
		},
		{
			name:     "TCP address",
			uri:      "nbd://127.0.0.1:34305",
			wantType: "inet",
			wantHost: "127.0.0.1",
			wantPort: 34305,
		},
		{
			name:     "TCP with hostname",
			uri:      "nbd://storage.local:9000",
			wantType: "inet",
			wantHost: "storage.local",
			wantPort: 9000,
		},
		{
			name:    "Empty socket path",
			uri:     "nbd:unix:",
			wantErr: true,
		},
		{
			name:    "Unix socket URI with no socket parameter",
			uri:     "nbd+unix:///",
			wantErr: true,
		},
		{
			name:    "Missing port in TCP",
			uri:     "nbd://127.0.0.1",
			wantErr: true,
		},
		{
			name:    "Invalid port",
			uri:     "nbd://127.0.0.1:notaport",
			wantErr: true,
		},
		{
			name:    "Unsupported format",
			uri:     "http://example.com",
			wantErr: true,
		},
		{
			name:    "Empty string",
			uri:     "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serverType, path, host, port, err := ParseNBDURI(tt.uri)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tt.wantType, serverType)
			assert.Equal(t, tt.wantPath, path)
			assert.Equal(t, tt.wantHost, host)
			assert.Equal(t, tt.wantPort, port)
		})
	}
}

// Test file extraction process

func TestExtractDiskImageFromFile(t *testing.T) {
	tmpDir := t.TempDir()

	t.Log("Temp dir:", tmpDir)

	// Sample .xz (fail)
	imagePath, err := ExtractDiskImageFromFile("/tmp/file.xz", tmpDir)

	assert.Empty(t, imagePath, "Should be blank")
	assert.Error(t, err, "Should error")

	// Sample incorrect image (fail)
	imagePath, err = ExtractDiskImageFromFile("../../tests/ebs.json", tmpDir)

	assert.Empty(t, imagePath, "Should be blank")
	assert.Error(t, err, "Should error")
	assert.ErrorContains(t, err, "unsupported filetype")

	// Sample incorrect image (fail)
	imagePath, err = ExtractDiskImageFromFile("../../tests/unit-test-disk-image-bad.raw", tmpDir)

	assert.NotEmpty(t, imagePath, "Should be blank")
	assert.Error(t, err, "Should error")
	assert.ErrorContains(t, err, "no valid disk image found")

	// Sample raw
	imagePath, err = ExtractDiskImageFromFile("../../tests/unit-test-disk-image.raw", tmpDir)

	assert.NotEmpty(t, imagePath, "Should not be blank")
	assert.Contains(t, imagePath, ".raw")

	assert.NoError(t, err, "Should not error")

	_, err = exec.LookPath("tar")

	if err == nil {
		// Sample .tgz
		imagePath, err = ExtractDiskImageFromFile("../../tests/unit-test-disk-image2.tgz", tmpDir)

		assert.NotEmpty(t, imagePath, "Should not be blank")
		assert.Contains(t, imagePath, ".img")

		assert.NoError(t, err, "Should not error")

		// Sample .tar
		imagePath, err = ExtractDiskImageFromFile("../../tests/unit-test-disk-image.tar", tmpDir)

		assert.NotEmpty(t, imagePath, "Should not be blank")
		assert.Contains(t, imagePath, ".raw")

		assert.NoError(t, err, "Should not error")

		// Sample .tar.gz
		imagePath, err = ExtractDiskImageFromFile("../../tests/unit-test-disk-image.tar.gz", tmpDir)

		assert.NotEmpty(t, imagePath, "Should not be blank")
		assert.Contains(t, imagePath, ".raw")

		assert.NoError(t, err, "Should not error")

		// Sample xz
		imagePath, err = ExtractDiskImageFromFile("../../tests/unit-test-disk-image.tar.xz", tmpDir)

		assert.NotEmpty(t, imagePath, "Should not be blank")
		assert.Contains(t, imagePath, ".raw")

		assert.NoError(t, err, "Should not error")

		// Sample tgz
		imagePath, err = ExtractDiskImageFromFile("../../tests/unit-test-disk-image.tgz", tmpDir)

		assert.NotEmpty(t, imagePath, "Should not be blank")
		assert.Contains(t, imagePath, ".raw")

		assert.NoError(t, err, "Should not error")
	} else {
		t.Skip("tar command not found, skipping archive extraction tests")
	}

	//err = os.RemoveAll(tmpDir)
	//assert.NoError(t, err, "Could not remove temp dir")
}

func TestIsSocketURI(t *testing.T) {
	tests := []struct {
		name string
		uri  string
		want bool
	}{
		{"Socket suffix", "/run/nbd-vol.sock", true},
		{"Unix prefix", "unix:/run/nbd-vol", true},
		{"Both", "unix:/run/nbd-vol.sock", true},
		{"TCP URI", "nbd://127.0.0.1:9000", false},
		{"Empty", "", false},
		{"Random path", "/tmp/somefile.txt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsSocketURI(tt.uri))
		})
	}
}

func TestFormatNBDSocketURI(t *testing.T) {
	assert.Equal(t, "nbd+unix:///?socket=/run/nbd-vol.sock", FormatNBDSocketURI("/run/nbd-vol.sock"))
	assert.Equal(t, "nbd+unix:///?socket=/tmp/test.sock", FormatNBDSocketURI("/tmp/test.sock"))
}

// TestFormatNBDSocketURIRoundTrips guards the pairing that a third-party NBD
// client depends on: what we publish must be what we can parse back.
func TestFormatNBDSocketURIRoundTrips(t *testing.T) {
	const path = "/run/spinifex/nbd/nbd-vol-abc123.sock"
	serverType, parsed, _, _, err := ParseNBDURI(FormatNBDSocketURI(path))
	require.NoError(t, err)
	assert.Equal(t, "unix", serverType)
	assert.Equal(t, path, parsed)
}

func TestFormatNBDTCPURI(t *testing.T) {
	assert.Equal(t, "nbd://127.0.0.1:9000", FormatNBDTCPURI("127.0.0.1", 9000))
	assert.Equal(t, "nbd://storage.local:34305", FormatNBDTCPURI("storage.local", 34305))
}

func TestGenerateUniqueSocketFile(t *testing.T) {
	path1, err := GenerateUniqueSocketFile("vol-123")
	require.NoError(t, err)
	assert.Contains(t, path1, "nbd-vol-123-")
	assert.True(t, strings.HasSuffix(path1, ".sock"))

	// Two calls should produce different paths (different timestamps)
	time.Sleep(time.Nanosecond)
	path2, err := GenerateUniqueSocketFile("vol-123")
	require.NoError(t, err)
	assert.NotEqual(t, path1, path2)

	// Empty volume name
	_, err = GenerateUniqueSocketFile("")
	assert.Error(t, err)
}

func TestDirExists(t *testing.T) {
	// Existing directory
	assert.True(t, dirExists(os.TempDir()))

	// Non-existent path
	assert.False(t, dirExists("/nonexistent/path/should/not/exist"))

	// File (not a directory)
	tmpFile, err := os.CreateTemp(t.TempDir(), "direxists-test-*")
	require.NoError(t, err)
	tmpFile.Close()
	assert.False(t, dirExists(tmpFile.Name()))
}

func TestGenerateSocketFile_EmptyName(t *testing.T) {
	_, err := GenerateSocketFile("")
	assert.Error(t, err)
}

func TestExtractDiskImagePath_NoMatch(t *testing.T) {
	output := []byte("somefile.txt\nanotherfile.conf\n")
	diskimage, err := extractDiskImagePath(t.TempDir(), output)
	assert.Empty(t, diskimage)
	assert.NoError(t, err)
}

func TestExtractDiskImagePath_EmptyOutput(t *testing.T) {
	diskimage, err := extractDiskImagePath(t.TempDir(), []byte{})
	assert.Empty(t, diskimage)
	assert.NoError(t, err)
}

// writeTempImage writes content to a file named name under t.TempDir() and
// returns the absolute path.
func writeTempImage(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return path
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sha512Hex(b []byte) string {
	sum := sha512.Sum512(b)
	return hex.EncodeToString(sum[:])
}

// newSumsServer stands up a TLS test server returning body for any request,
// configures the package-level trust hook, and registers cleanup. Returns the
// server URL.
func newSumsServer(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	trustTestServer(t, srv)
	return srv.URL + "/SUMS"
}

// trustTestServer adds srv's certificate to the package-level trust pool so
// VerifyImageChecksum's HTTPS-only client accepts it. Restores the previous
// pool on test cleanup.
func trustTestServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := checksumExtraRootCAs
	pool := x509.NewCertPool()
	if prev != nil {
		pool = prev.Clone()
	}
	pool.AddCert(srv.Certificate())
	checksumExtraRootCAs = pool
	t.Cleanup(func() { checksumExtraRootCAs = prev })
}

func TestVerifyImageChecksum(t *testing.T) {
	const debianName = "debian-13-genericcloud-amd64-20260518-2482.tar.xz"
	const ubuntuName = "resolute-server-cloudimg-amd64.img"
	const alpineName = "alb-alpine-3.21.6-x86_64.raw"

	debianBytes := []byte("debian-image-bytes-fixture")
	ubuntuBytes := []byte("ubuntu-image-bytes-fixture")
	alpineBytes := []byte("alpine-image-bytes-fixture")

	t.Run("debian sha512 multi-line match", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		body := fmt.Sprintf("%s  %s\n%s  other-file\n",
			sha512Hex(debianBytes), debianName, sha512Hex([]byte("unrelated")))
		url := newSumsServer(t, body)
		got, err := VerifyImageChecksum(img, url, "sha512")
		require.NoError(t, err)
		assert.Equal(t, sha512Hex(debianBytes), got)
	})

	t.Run("ubuntu sha256 binary-mode asterisk match", func(t *testing.T) {
		img := writeTempImage(t, ubuntuName, ubuntuBytes)
		body := fmt.Sprintf("%s *%s\n", sha256Hex(ubuntuBytes), ubuntuName)
		url := newSumsServer(t, body)
		_, err := VerifyImageChecksum(img, url, "sha256")
		assert.NoError(t, err)
	})

	t.Run("alpine single-line named match", func(t *testing.T) {
		img := writeTempImage(t, alpineName, alpineBytes)
		body := fmt.Sprintf("%s  %s\n", sha512Hex(alpineBytes), alpineName)
		url := newSumsServer(t, body)
		_, err := VerifyImageChecksum(img, url, "sha512")
		assert.NoError(t, err)
	})

	t.Run("alpine bare-digest fallback", func(t *testing.T) {
		img := writeTempImage(t, alpineName, alpineBytes)
		body := sha512Hex(alpineBytes) + "\n"
		url := newSumsServer(t, body)
		_, err := VerifyImageChecksum(img, url, "sha512")
		assert.NoError(t, err)
	})

	t.Run("case-sensitive filename mismatch", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		// Capitalised "Debian-..." in sums file: legitimate divergence from upstream
		// convention, treated as not-found.
		body := fmt.Sprintf("%s  Debian-12-generic-amd64.tar.xz\n", sha512Hex(debianBytes))
		url := newSumsServer(t, body)
		_, err := VerifyImageChecksum(img, url, "sha512")
		assert.ErrorIs(t, err, ErrChecksumNotFound)
	})

	t.Run("comment lines skipped", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		body := fmt.Sprintf("# Hash file generated by upstream\n# PGP-signed below\n%s  %s\n",
			sha512Hex(debianBytes), debianName)
		url := newSumsServer(t, body)
		_, err := VerifyImageChecksum(img, url, "sha512")
		assert.NoError(t, err)
	})

	t.Run("digest mismatch", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		wrong := sha512Hex([]byte("a different blob entirely"))
		body := fmt.Sprintf("%s  %s\n", wrong, debianName)
		url := newSumsServer(t, body)
		_, err := VerifyImageChecksum(img, url, "sha512")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrChecksumMismatch)
		// Both expected and actual digests should be in the rendered error so
		// operators can compare without scraping slog.
		assert.Contains(t, err.Error(), wrong)
		assert.Contains(t, err.Error(), sha512Hex(debianBytes))
	})

	t.Run("filename absent from sums file", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		body := fmt.Sprintf("%s  some-other-file.tar.xz\n%s  yet-another.iso\n",
			sha512Hex([]byte("a")), sha512Hex([]byte("b")))
		url := newSumsServer(t, body)
		_, err := VerifyImageChecksum(img, url, "sha512")
		assert.ErrorIs(t, err, ErrChecksumNotFound)
	})

	t.Run("unsupported checksum type rejected before any fetch", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		// Server URL is gibberish — function must fail before reaching it.
		_, err := VerifyImageChecksum(img, "https://example.invalid/SUMS", "md5")
		assert.ErrorIs(t, err, ErrUnsupportedChecksumType)
	})

	t.Run("http 404 wraps fetch failure", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)
		trustTestServer(t, srv)
		_, err := VerifyImageChecksum(img, srv.URL+"/SUMS", "sha512")
		assert.ErrorIs(t, err, ErrChecksumFetchFailed)
	})

	t.Run("non-https initial url rejected before any request", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		// Hit a port nothing listens on — if HTTPS check ever regresses we'd
		// still get an error, but we want ErrChecksumFetchFailed wrapping the
		// scheme refusal specifically.
		_, err := VerifyImageChecksum(img, "http://127.0.0.1:1/SUMS", "sha512")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrChecksumFetchFailed)
		assert.Contains(t, err.Error(), "non-https")
	})

	t.Run("non-https redirect refused", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		// Plain-HTTP target the TLS server will try to redirect us to.
		plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "%s  %s\n", sha512Hex(debianBytes), debianName)
		}))
		t.Cleanup(plain.Close)
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, plain.URL+"/SUMS", http.StatusFound)
		}))
		t.Cleanup(srv.Close)
		trustTestServer(t, srv)
		_, err := VerifyImageChecksum(img, srv.URL+"/SUMS", "sha512")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrChecksumFetchFailed)
	})

	t.Run("redirect cap exceeded", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		var hits atomic.Int32
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			http.Redirect(w, r, r.URL.Path, http.StatusFound)
		}))
		t.Cleanup(srv.Close)
		trustTestServer(t, srv)
		_, err := VerifyImageChecksum(img, srv.URL+"/SUMS", "sha512")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrChecksumFetchFailed)
		assert.GreaterOrEqual(t, int(hits.Load()), 10)
	})

	t.Run("sums file size cap exceeded", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		// Body of 2 MB of unrelated entries — over the 1 MB cap.
		var b strings.Builder
		junk := strings.Repeat("a", 128)
		for b.Len() <= sumsFileMaxSize+1 {
			fmt.Fprintf(&b, "%s  %s.bin\n", sha512Hex([]byte(junk)), junk)
		}
		url := newSumsServer(t, b.String())
		_, err := VerifyImageChecksum(img, url, "sha512")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrChecksumFetchFailed)
		assert.Contains(t, err.Error(), "exceeds")
	})

	t.Run("context deadline exceeded wraps fetch failure", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		t.Cleanup(srv.Close)
		trustTestServer(t, srv)

		prev := checksumFetchTimeout
		checksumFetchTimeout = 100 * time.Millisecond
		t.Cleanup(func() { checksumFetchTimeout = prev })

		_, err := VerifyImageChecksum(img, srv.URL+"/SUMS", "sha512")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrChecksumFetchFailed)
	})

	t.Run("digest length mismatch rejected before hashing", func(t *testing.T) {
		// sha256 digest served, caller declares sha512: algorithm disagreement,
		// not tampering. Must be distinct from ErrChecksumMismatch.
		img := writeTempImage(t, debianName, debianBytes)
		body := fmt.Sprintf("%s  %s\n", sha256Hex(debianBytes), debianName)
		url := newSumsServer(t, body)
		_, err := VerifyImageChecksum(img, url, "sha512")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrChecksumFetchFailed)
		assert.NotErrorIs(t, err, ErrChecksumMismatch)
		assert.Contains(t, err.Error(), "digest length")
	})

	t.Run("empty image file rejected with clear error", func(t *testing.T) {
		img := writeTempImage(t, debianName, nil)
		body := fmt.Sprintf("%s  %s\n", sha512Hex(nil), debianName)
		url := newSumsServer(t, body)
		_, err := VerifyImageChecksum(img, url, "sha512")
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrChecksumMismatch)
		assert.Contains(t, err.Error(), "empty")
	})

	t.Run("transport error wrapped", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		// Stand up a TLS server, trust it, then close immediately so any
		// subsequent request fails at the transport layer.
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		trustTestServer(t, srv)
		url := srv.URL + "/SUMS"
		srv.Close()
		_, err := VerifyImageChecksum(img, url, "sha512")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrChecksumFetchFailed)
	})

	t.Run("rocky bsd-style gpg-armored sums match", func(t *testing.T) {
		const rockyName = "Rocky-10.0-20250612.0.x86_64.qcow2"
		rockyBytes := []byte("rocky-image-bytes-fixture")
		img := writeTempImage(t, rockyName, rockyBytes)
		// Captured shape of a real Rocky CHECKSUM file: GPG cleartext-signed
		// armor wrapping BSD-style "SHA256 (name) = hex" lines, followed by an
		// ASCII-armored signature block we must skip without misparsing.
		body := fmt.Sprintf(`-----BEGIN PGP SIGNED MESSAGE-----
Hash: SHA256

SHA256 (Rocky-10.0-20250612.0-x86_64-boot.iso) = %s
SHA256 (%s) = %s
SHA256 (Rocky-10.0-20250612.0-x86_64-dvd.iso) = %s
-----BEGIN PGP SIGNATURE-----

iQIzBAEBCAAdFiEEnK6Ehq8eQ0z6yqv7tQAaIQAaIQAAaIQFAmJabcdEFGhijklm
nopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ+/abcdefghijklmn
=ABCD
-----END PGP SIGNATURE-----
`, sha256Hex([]byte("boot-iso")), rockyName, sha256Hex(rockyBytes), sha256Hex([]byte("dvd-iso")))
		url := newSumsServer(t, body)
		_, err := VerifyImageChecksum(img, url, "sha256")
		assert.NoError(t, err)
	})

	t.Run("alma bsd-style gpg-armored sums match", func(t *testing.T) {
		const almaName = "AlmaLinux-10-GenericCloud-10.0-x86_64.qcow2"
		almaBytes := []byte("alma-image-bytes-fixture")
		img := writeTempImage(t, almaName, almaBytes)
		body := fmt.Sprintf(`-----BEGIN PGP SIGNED MESSAGE-----
Hash: SHA256

SHA256 (%s) = %s
-----BEGIN PGP SIGNATURE-----

iHUEABYKAB0WIQTm5n8/yqv7tQAaIQAaIQAFAmJabcdEFGhijklmnopqrstuvwxyz
=EFGH
-----END PGP SIGNATURE-----
`, almaName, sha256Hex(almaBytes))
		url := newSumsServer(t, body)
		_, err := VerifyImageChecksum(img, url, "sha256")
		assert.NoError(t, err)
	})

	t.Run("bsd-style filename absent", func(t *testing.T) {
		const rockyName = "Rocky-10.0-20250612.0.x86_64.qcow2"
		rockyBytes := []byte("rocky-image-bytes-fixture")
		img := writeTempImage(t, rockyName, rockyBytes)
		body := fmt.Sprintf(`-----BEGIN PGP SIGNED MESSAGE-----
Hash: SHA256

SHA256 (Rocky-10.0-20250612.0-x86_64-boot.iso) = %s
-----BEGIN PGP SIGNATURE-----

iQIzBAEBCAAdFiEEnK6Ehq8eQ0z6yqv7tQAaIQAaIQAAaIQFAmJabcdEFGhijklm
=ABCD
-----END PGP SIGNATURE-----
`, sha256Hex([]byte("boot-iso")))
		url := newSumsServer(t, body)
		_, err := VerifyImageChecksum(img, url, "sha256")
		assert.ErrorIs(t, err, ErrChecksumNotFound)
	})
}

func TestParseSumsFile(t *testing.T) {
	const target = "Rocky-10.0-20250612.0.x86_64.qcow2"
	const targetHex = "a3f1c2d4e5f60718293a4b5c6d7e8f9012345678901234567890abcdefabcdef"

	t.Run("bsd 4-field line", func(t *testing.T) {
		body := []byte("SHA256 (" + target + ") = " + targetHex + "\n")
		got, err := parseSumsFile(body, target)
		require.NoError(t, err)
		assert.Equal(t, targetHex, got)
	})

	t.Run("bsd missing equals separator skipped", func(t *testing.T) {
		// Malformed line — third field is not "=". Must not match.
		body := []byte("SHA256 (" + target + ") - " + targetHex + "\n")
		_, err := parseSumsFile(body, target)
		assert.ErrorIs(t, err, ErrChecksumNotFound)
	})

	t.Run("bsd missing parens skipped", func(t *testing.T) {
		// Without parens the filename field is ambiguous; reject by not matching.
		body := []byte("SHA256 " + target + " = " + targetHex + "\n")
		_, err := parseSumsFile(body, target)
		assert.ErrorIs(t, err, ErrChecksumNotFound)
	})

	t.Run("gpg-armored body with no sums returns not found", func(t *testing.T) {
		// PGP signatures always have ≥ 2 bare-token lines (base64 + =CRC), so bareCount stays ≥ 2 and NotFound is returned.
		body := []byte(`-----BEGIN PGP SIGNED MESSAGE-----
Hash: SHA256

-----BEGIN PGP SIGNATURE-----

iQIzBAEBCAAdFiEEnK6Ehq8eQ0z6yqv7tQAaIQAaIQAAaIQFAmJabcdEFGhijklm
=ABCD
-----END PGP SIGNATURE-----
`)
		_, err := parseSumsFile(body, target)
		assert.ErrorIs(t, err, ErrChecksumNotFound)
	})

	t.Run("mixed bsd and coreutils shapes", func(t *testing.T) {
		// Defensive: a sums file with both shapes should still resolve the
		// requested filename regardless of which line carries it.
		body := []byte("# header\n" +
			targetHex + "  some-other.iso\n" +
			"SHA256 (" + target + ") = " + targetHex + "\n")
		got, err := parseSumsFile(body, target)
		require.NoError(t, err)
		assert.Equal(t, targetHex, got)
	})
}

func TestReadExpectedDigest(t *testing.T) {
	const image = "debian-13-generic-amd64.tar.xz"
	imgBytes := []byte("local-image-bytes-fixture")
	h256, h512 := sha256Hex(imgBytes), sha512Hex(imgBytes)
	other := sha512Hex([]byte("other"))

	cases := []struct {
		name       string
		sumsName   string
		body       string
		imageName  string
		dir        bool
		missing    bool
		wantAlgo   string
		wantDigest string
		wantErr    error
		wantErrMsg string
	}{
		{name: "coreutils text sha512", sumsName: "SHA512SUMS",
			body: h512 + "  " + image + "\n" + other + "  other.iso\n", wantAlgo: "sha512", wantDigest: h512},
		{name: "coreutils binary sha256", sumsName: "SHA256SUMS",
			body: h256 + " *" + image + "\n", wantAlgo: "sha256", wantDigest: h256},
		{name: "bsd sha256", sumsName: "CHECKSUM",
			body: "SHA256 (" + image + ") = " + h256 + "\n", wantAlgo: "sha256", wantDigest: h256},
		{name: "bare sha512", sumsName: image + ".sha512",
			body: h512 + "\n", wantAlgo: "sha512", wantDigest: h512},
		{name: "neutral sums name uses length", sumsName: "checksums.txt",
			body: h256 + "  " + image + "\n", wantAlgo: "sha256", wantDigest: h256},
		{name: "uppercase hex normalised", sumsName: "SHA512SUMS",
			body: strings.ToUpper(h512) + "  " + image + "\n", wantAlgo: "sha512", wantDigest: h512},
		{name: "sums file name does not decide the algorithm", sumsName: "SHA256SUMS",
			body: h512 + "  " + image + "\n", wantAlgo: "sha512", wantDigest: h512},
		{name: "renamed image in multi-entry file", sumsName: "SHA512SUMS", imageName: "renamed.tar.xz",
			body: h512 + "  " + image + "\n" + other + "  other.iso\n", wantErr: ErrChecksumNotFound, wantErrMsg: "renamed.tar.xz"},
		{name: "path-prefixed entry names the prefix", sumsName: "image.sha512",
			body: h512 + "  ./" + image + "\n", wantErr: ErrChecksumNotFound, wantErrMsg: "directory prefix"},
		{name: "renamed image with bare digest", sumsName: "image.sha512", imageName: "renamed.tar.xz",
			body: h512 + "\n", wantAlgo: "sha512", wantDigest: h512},
		{name: "sha1 length unsupported", sumsName: "checksums.txt",
			body: strings.Repeat("a", 40) + "  " + image + "\n", wantErr: ErrUnsupportedChecksumType},
		{name: "non-hex digest", sumsName: "checksums.txt",
			body: strings.Repeat("g", 64) + "  " + image + "\n", wantErr: ErrMalformedChecksum},
		{name: "oversized file", sumsName: "SHA512SUMS",
			body: h512 + "  " + image + "\n" + strings.Repeat("#", sumsFileMaxSize), wantErr: ErrMalformedChecksum},
		{name: "directory refused", sumsName: "SHA512SUMS", dir: true, wantErr: ErrMalformedChecksum},
		{name: "missing file refused", sumsName: "SHA512SUMS", missing: true, wantErr: os.ErrNotExist},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.sumsName)
			switch {
			case tc.dir:
				require.NoError(t, os.Mkdir(path, 0o755))
			case !tc.missing:
				require.NoError(t, os.WriteFile(path, []byte(tc.body), 0o600))
			}
			imageName := tc.imageName
			if imageName == "" {
				imageName = image
			}

			algo, digest, err := ReadExpectedDigest(path, imageName)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Contains(t, err.Error(), tc.wantErrMsg)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantAlgo, algo)
			assert.Equal(t, tc.wantDigest, digest)
		})
	}
}

func TestVerifyImageDigest(t *testing.T) {
	imgBytes := []byte("verify-digest-fixture")
	img := writeTempImage(t, "image.raw", imgBytes)

	t.Run("sha256 match returns digest", func(t *testing.T) {
		got, err := VerifyImageDigest(img, "sha256", sha256Hex(imgBytes))
		require.NoError(t, err)
		assert.Equal(t, sha256Hex(imgBytes), got)
	})

	t.Run("sha512 match returns digest", func(t *testing.T) {
		got, err := VerifyImageDigest(img, "sha512", sha512Hex(imgBytes))
		require.NoError(t, err)
		assert.Equal(t, sha512Hex(imgBytes), got)
	})

	t.Run("mismatch names both digests", func(t *testing.T) {
		wrong := sha512Hex([]byte("tampered"))
		got, err := VerifyImageDigest(img, "sha512", wrong)
		require.ErrorIs(t, err, ErrChecksumMismatch)
		assert.Equal(t, sha512Hex(imgBytes), got)
		assert.Contains(t, err.Error(), wrong)
		assert.Contains(t, err.Error(), sha512Hex(imgBytes))
	})

	t.Run("unsupported algorithm", func(t *testing.T) {
		_, err := VerifyImageDigest(img, "md5", "abc")
		assert.ErrorIs(t, err, ErrUnsupportedChecksumType)
	})
}

func TestHashImageFile(t *testing.T) {
	imgBytes := []byte("hash-image-fixture")
	img := writeTempImage(t, "image.raw", imgBytes)

	got, err := HashImageFile(img, "sha256")
	require.NoError(t, err)
	assert.Equal(t, sha256Hex(imgBytes), got)

	_, err = HashImageFile(writeTempImage(t, "empty.raw", nil), "sha256")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestAvailableImages_Rocky(t *testing.T) {
	cases := []struct {
		name        string
		arch        string
		filenameSub string
	}{
		{"rocky-10-x86_64", "x86_64", "x86_64.qcow2"},
		{"rocky-10-arm64", "arm64", "aarch64.qcow2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			img, ok := AvailableImages[tc.name]
			require.True(t, ok, "catalog must contain %q", tc.name)
			assert.Equal(t, tc.name, img.Name)
			assert.Equal(t, "rocky", img.Distro)
			assert.Equal(t, "10", img.Version)
			assert.Equal(t, tc.arch, img.Arch)
			// Rocky 10 cloud images are UEFI-only on both architectures.
			assert.Equal(t, "uefi", img.BootMode)
			assert.Equal(t, "sha256", img.ChecksumType)
			// URL must track the moving .latest. alias — Rocky prunes dated
			// builds, so a pinned dated URL 404s once it ages out. The checksum
			// URL must be the BSD-style .CHECKSUM companion of that same alias.
			assert.Contains(t, img.URL, ".latest.")
			assert.Contains(t, img.URL, tc.filenameSub)
			assert.True(t, strings.HasSuffix(img.Checksum, ".CHECKSUM"),
				"Rocky publishes BSD-style .CHECKSUM files; got %q", img.Checksum)
			// Catalog Distro must resolve to the rhel family — typo here would
			// silently render Rocky guests with netplan/sudo group.
			assert.Equal(t, "rhel", DistroFamily(img.Distro))
		})
	}
}

func TestDistroFamily(t *testing.T) {
	cases := []struct {
		distro string
		want   string
	}{
		{"debian", "debian"},
		{"ubuntu", "debian"},
		{"rocky", "rhel"},
		{"rhel", "rhel"},
		{"alma", "rhel"},
		{"fedora", "rhel"},
		{"centos", "rhel"},
		{"alpine", "alpine"},
		// Case + whitespace normalisation
		{"  Rocky  ", "rhel"},
		{"UBUNTU", "debian"},
		// Unknown and empty fall through to debian with a warning logged.
		{"", "debian"},
		{"plan9", "debian"},
	}
	for _, tc := range cases {
		t.Run(tc.distro, func(t *testing.T) {
			assert.Equal(t, tc.want, DistroFamily(tc.distro))
		})
	}
}

func TestAvailableImages_ECSNodeEntry(t *testing.T) {
	img, ok := AvailableImages["spinifex-ecs-node"]
	require.True(t, ok, "spinifex-ecs-node must be in the system image catalog")
	assert.Equal(t, "ecs", img.Tags["spinifex:managed-by"],
		"ECS node image must carry the managed-by=ecs tag the UI guard resolves on")
}

func TestAvailableImages_RDSPostgresEntry(t *testing.T) {
	img, ok := AvailableImages["spinifex-rds-postgres"]
	require.True(t, ok, "spinifex-rds-postgres must be in the system image catalog")
	assert.Equal(t, "rds", img.Tags["spinifex:managed-by"],
		"RDS image must carry the managed-by=rds tag instance launches resolve on")
	assert.Equal(t, "postgres", img.Tags["engine"])
	assert.Equal(t, "18", img.Tags["engine-version"],
		"the pinned PostgreSQL major version is what EngineVersion resolves against")
	assert.Equal(t, "format-auth-v1", img.Tags["rds-data-volume-contract"],
		"RDS launches must exclude images that still discover generic data disks")
}
