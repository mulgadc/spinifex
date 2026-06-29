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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

func TestGeneratePidFile(t *testing.T) {
	// Simulate a sample process running (e.g cat)
	cmd := exec.Command("cat")
	cmd.Start()

	err := WritePidFile("utilsunittest", cmd.Process.Pid)

	assert.NoError(t, err)

	// Read the PID file and verify contents
	pid, err := ReadPidFile("utilsunittest")

	assert.NoError(t, err)
	assert.Equal(t, cmd.Process.Pid, pid)

	// Test attempt to read a PID file that doesn't exist
	_, err = ReadPidFile("nonexistentpidfile")
	assert.Error(t, err)

	// Cleanup
	err = RemovePidFile("utilsunittest")
	assert.NoError(t, err)

	// Give some time before killing the process
	//time.Sleep(2 * time.Second)

	// Simulate process ending
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

func TestExecProcessAndKill(t *testing.T) {
	// Simulate a sample process running (e.g sleep, 30 secs)
	cmd := exec.Command("sleep", "30")

	// Detach: new process group, no controlling terminal.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true, // put child in new process group
	}

	// Make it fully background-friendly:
	// - close stdio so parent doesn't block on pipes
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	// Start (non-blocking). If command is missing, error.
	if err := cmd.Start(); err != nil {
		assert.Fail(t, "Failed to start command", err)
	}

	// IMPORTANT: reap the child to avoid zombies.
	// Since we're "backgrounding" it, do Wait() in a goroutine.
	go func(c *exec.Cmd) {
		t.Log("Waiting for command to finish...")
		_ = c.Wait() // ignore error; ensures kernel reaps the process
		t.Log("Command finished.")
	}(cmd)

	err := WritePidFile("utilsunittest", cmd.Process.Pid)

	t.Log("Started process with PID:", cmd.Process.Pid)

	assert.NoError(t, err)

	// Test PID file removed
	err = WaitForPidFileRemoval("utilsunittest", 100*time.Millisecond)
	assert.Error(t, err) // Should timeout since file should still exist

	time.Sleep(500 * time.Millisecond)

	// Kill the process
	err = StopProcess("utilsunittest")
	assert.NoError(t, err)

	// Test PID file removed
	err = WaitForPidFileRemoval("utilsunittest", 1*time.Second)
	assert.NoError(t, err) // Should timeout since file should still exist

	// Verify process is killed
	err = cmd.Process.Signal(syscall.Signal(0))
	assert.Error(t, err) // Should return an error since process is killed
}

func TestWaitForPidFile(t *testing.T) {
	t.Run("returns pid when file appears within timeout", func(t *testing.T) {
		const name = "wait-pidfile-appears"
		_ = RemovePidFile(name)
		t.Cleanup(func() { _ = RemovePidFile(name) })

		go func() {
			time.Sleep(150 * time.Millisecond)
			_ = WritePidFile(name, 4242)
		}()

		pid, err := WaitForPidFile(name, time.Second)
		require.NoError(t, err)
		assert.Equal(t, 4242, pid)
	})

	t.Run("returns error when timeout expires", func(t *testing.T) {
		const name = "wait-pidfile-missing"
		_ = RemovePidFile(name)

		_, err := WaitForPidFile(name, 100*time.Millisecond)
		assert.Error(t, err)
	})

	t.Run("returns immediately when file already present", func(t *testing.T) {
		const name = "wait-pidfile-present"
		require.NoError(t, WritePidFile(name, 7777))
		t.Cleanup(func() { _ = RemovePidFile(name) })

		start := time.Now()
		pid, err := WaitForPidFile(name, time.Second)
		require.NoError(t, err)
		assert.Equal(t, 7777, pid)
		assert.Less(t, time.Since(start), 50*time.Millisecond)
	})
}

func TestWaitForUnixSocket(t *testing.T) {
	t.Run("returns nil when socket appears within timeout", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "appears.sock")

		ln, err := net.Listen("unix", path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })

		require.NoError(t, WaitForUnixSocket(path, time.Second))
	})

	t.Run("returns error when timeout expires", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "missing.sock")

		err := WaitForUnixSocket(path, 100*time.Millisecond)
		require.Error(t, err)
	})

	t.Run("waits for socket created after a delay", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "delayed.sock")

		go func() {
			time.Sleep(150 * time.Millisecond)
			ln, err := net.Listen("unix", path)
			if err == nil {
				t.Cleanup(func() { _ = ln.Close() })
			}
		}()

		require.NoError(t, WaitForUnixSocket(path, time.Second))
	})
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
				assert.Equal(t, "", result.Name)
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
			assert.Greater(t, len(payload), 0)
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
			name:     "Unix socket",
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

func TestMarshalToXML(t *testing.T) {
	type TestStruct struct {
		Name  string `xml:"Name"`
		Value int    `xml:"Value"`
	}

	tests := []struct {
		name        string
		input       any
		expectError bool
		validate    func(t *testing.T, xmlData []byte)
	}{
		{
			name: "Valid struct",
			input: TestStruct{
				Name:  "test",
				Value: 123,
			},
			expectError: false,
			validate: func(t *testing.T, xmlData []byte) {
				assert.Contains(t, string(xmlData), "<Name>test</Name>")
				assert.Contains(t, string(xmlData), "<Value>123</Value>")
			},
		},
		{
			name: "Pointer to struct",
			input: &TestStruct{
				Name:  "pointer",
				Value: 456,
			},
			expectError: false,
			validate: func(t *testing.T, xmlData []byte) {
				assert.Contains(t, string(xmlData), "<Name>pointer</Name>")
				assert.Contains(t, string(xmlData), "<Value>456</Value>")
			},
		},
		{
			name:        "Invalid type (channel)",
			input:       make(chan int),
			expectError: true,
			validate:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			xmlData, err := MarshalToXML(tt.input)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, xmlData)
				if tt.validate != nil {
					tt.validate(t, xmlData)
				}
			}
		})
	}
}

func TestGenerateIAMXMLPayload(t *testing.T) {
	type User struct {
		UserName string `locationName:"UserName" type:"string"`
		UserId   string `locationName:"UserId" type:"string"`
	}

	tests := []struct {
		name     string
		action   string
		payload  any
		validate func(t *testing.T, result any)
	}{
		{
			name:   "CreateUser wrapping",
			action: "CreateUser",
			payload: User{
				UserName: "testuser",
				UserId:   "AIDA12345",
			},
			validate: func(t *testing.T, result any) {
				xmlBytes, err := MarshalToXML(result)
				assert.NoError(t, err)
				xmlStr := string(xmlBytes)
				assert.Contains(t, xmlStr, "CreateUserResponse")
				assert.Contains(t, xmlStr, "CreateUserResult")
				assert.Contains(t, xmlStr, "testuser")
				assert.Contains(t, xmlStr, "AIDA12345")
			},
		},
		{
			name:   "ListUsers wrapping",
			action: "ListUsers",
			payload: User{
				UserName: "admin",
				UserId:   "AIDA99999",
			},
			validate: func(t *testing.T, result any) {
				xmlBytes, err := MarshalToXML(result)
				assert.NoError(t, err)
				xmlStr := string(xmlBytes)
				assert.Contains(t, xmlStr, "ListUsersResponse")
				assert.Contains(t, xmlStr, "ListUsersResult")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GenerateIAMXMLPayload(tt.action, tt.payload)
			assert.NotNil(t, result)
			if tt.validate != nil {
				tt.validate(t, result)
			}
		})
	}
}

func TestKillProcess(t *testing.T) {
	// Create a test process
	cmd := exec.Command("sleep", "60")
	err := cmd.Start()
	require.NoError(t, err)

	pid := cmd.Process.Pid

	// Reap in background so KillProcess can detect termination
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = cmd.Wait()
	})

	err = KillProcess(pid)
	assert.NoError(t, err)
	wg.Wait()

	// Test killing non-existent process
	err = KillProcess(999999)
	assert.Error(t, err, "Should error when killing non-existent process")
}

func TestProcessAlive(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid

	var wg sync.WaitGroup
	wg.Go(func() { _ = cmd.Wait() })

	assert.True(t, ProcessAlive(pid), "a running process must report alive")
	assert.False(t, ProcessAlive(0), "pid 0 must report not alive")
	assert.False(t, ProcessAlive(-1), "negative pid must report not alive")
	assert.False(t, ProcessAlive(999999), "a non-existent pid must report not alive")

	require.NoError(t, cmd.Process.Kill())
	wg.Wait()
	assert.False(t, ProcessAlive(pid), "a killed process must report not alive")
}

func TestForceKillProcess(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid

	var wg sync.WaitGroup
	wg.Go(func() { _ = cmd.Wait() })

	require.NoError(t, ForceKillProcess(pid, 5*time.Second))
	wg.Wait()
	assert.False(t, ProcessAlive(pid), "ForceKillProcess must SIGKILL and confirm exit")

	assert.Error(t, ForceKillProcess(0, time.Second), "invalid pid must error")
}

func TestStopProcess(t *testing.T) {
	// Create and start a test process
	cmd := exec.Command("sleep", "60")
	err := cmd.Start()
	require.NoError(t, err)

	// Write PID file
	testName := "stopprocess-test"
	err = WritePidFile(testName, cmd.Process.Pid)
	require.NoError(t, err)

	// Reap in background so StopProcess can detect termination
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = cmd.Wait()
	})

	err = StopProcess(testName)
	assert.NoError(t, err)
	wg.Wait()

	// Verify PID file was removed
	_, err = ReadPidFile(testName)
	assert.Error(t, err, "PID file should be removed")

	// Test stopping non-existent process
	err = StopProcess("nonexistent-process")
	assert.Error(t, err, "Should error when stopping non-existent process")
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
	assert.Equal(t, "nbd:unix:/run/nbd-vol.sock", FormatNBDSocketURI("/run/nbd-vol.sock"))
	assert.Equal(t, "nbd:unix:/tmp/test.sock", FormatNBDSocketURI("/tmp/test.sock"))
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

func TestGenerateXMLPayload(t *testing.T) {
	type Inner struct {
		Name string `locationName:"Name" type:"string"`
	}

	result := GenerateXMLPayload("DescribeInstancesResponse", Inner{Name: "test"})
	assert.NotNil(t, result)

	xmlBytes, err := MarshalToXML(result)
	require.NoError(t, err)
	xmlStr := string(xmlBytes)
	assert.Contains(t, xmlStr, "DescribeInstancesResponse")
	assert.Contains(t, xmlStr, "test")
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		name string
		b    uint64
		want string
	}{
		{"Zero", 0, "0 B"},
		{"Bytes", 512, "512 B"},
		{"KiB", 1024, "1.0 KiB"},
		{"KiB fractional", 1536, "1.5 KiB"},
		{"MiB", 1048576, "1.0 MiB"},
		{"GiB", 1073741824, "1.0 GiB"},
		{"Large GiB", 5368709120, "5.0 GiB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, HumanBytes(tt.b))
		})
	}
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

func TestProgressWriter(t *testing.T) {
	var total int
	pw := progressWriter(func(n int) {
		total += n
	})

	n, err := pw.Write([]byte("hello"))
	assert.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, 5, total)

	n, err = pw.Write([]byte("world!"))
	assert.NoError(t, err)
	assert.Equal(t, 6, n)
	assert.Equal(t, 11, total)
}

func TestGeneratePidFile_EmptyName(t *testing.T) {
	_, err := GeneratePidFile("")
	assert.Error(t, err)
}

func TestWritePidFileTo(t *testing.T) {
	dir := t.TempDir()

	cmd := exec.Command("cat")
	require.NoError(t, cmd.Start())
	defer cmd.Process.Kill()

	// Write PID to custom directory
	err := WritePidFileTo(dir, "testservice", cmd.Process.Pid)
	require.NoError(t, err)

	// Read it back from the same directory
	pid, err := ReadPidFileFrom(dir, "testservice")
	require.NoError(t, err)
	assert.Equal(t, cmd.Process.Pid, pid)

	// Clean up
	err = RemovePidFileAt(dir, "testservice")
	assert.NoError(t, err)

	// Verify it's gone
	_, err = ReadPidFileFrom(dir, "testservice")
	assert.Error(t, err)
}

func TestWritePidFileTo_EmptyDir(t *testing.T) {
	// With empty dir, should fall back to default pidPath()
	cmd := exec.Command("cat")
	require.NoError(t, cmd.Start())
	defer cmd.Process.Kill()

	err := WritePidFileTo("", "pidto-fallback", cmd.Process.Pid)
	require.NoError(t, err)

	// Should be readable via the default ReadPidFile
	pid, err := ReadPidFile("pidto-fallback")
	require.NoError(t, err)
	assert.Equal(t, cmd.Process.Pid, pid)

	// Clean up
	RemovePidFile("pidto-fallback")
}

func TestStopProcessAt(t *testing.T) {
	dir := t.TempDir()

	// Start a process we can kill
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())

	// Write PID file
	err := WritePidFileTo(dir, "stopat-test", cmd.Process.Pid)
	require.NoError(t, err)

	// Reap in background so StopProcessAt can detect termination
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = cmd.Wait()
	})

	err = StopProcessAt(dir, "stopat-test")
	assert.NoError(t, err)
	wg.Wait()

	// Verify PID file was removed
	_, err = ReadPidFileFrom(dir, "stopat-test")
	assert.Error(t, err, "PID file should be removed")
}

func TestStopProcessAt_StaleProcess(t *testing.T) {
	dir := t.TempDir()

	// Start a process and let it exit, leaving a stale PID file
	cmd := exec.Command("true")
	require.NoError(t, cmd.Start())
	stalePid := cmd.Process.Pid
	require.NoError(t, cmd.Wait())

	// Write stale PID file
	err := WritePidFileTo(dir, "stale-test", stalePid)
	require.NoError(t, err)

	// StopProcessAt should return an error (process is dead) but still
	// clean up the PID file
	err = StopProcessAt(dir, "stale-test")
	assert.Error(t, err, "should error because process is already dead")

	// PID file must be removed despite the kill error
	_, err = ReadPidFileFrom(dir, "stale-test")
	assert.Error(t, err, "PID file should be removed even when process is already dead")
}

func TestStopProcessAt_NoPidFile(t *testing.T) {
	dir := t.TempDir()
	err := StopProcessAt(dir, "nonexistent")
	assert.Error(t, err, "should error when PID file does not exist")
}

func TestGenerateSocketFile_EmptyName(t *testing.T) {
	_, err := GenerateSocketFile("")
	assert.Error(t, err)
}

func TestSetOOMScore(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("OOM score adjustment only supported on Linux")
	}

	// Read current value so we can set something higher (unprivileged processes
	// can only increase their OOM score, not decrease it)
	pid := os.Getpid()
	current, err := os.ReadFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid))
	if err != nil {
		t.Skipf("Cannot read OOM score: %v", err)
	}

	// Set a positive score (always allowed for unprivileged processes)
	err = SetOOMScore(pid, 100)
	if err != nil {
		t.Skipf("Insufficient permissions to set OOM score: %v", err)
	}

	// Read back and verify
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid))
	assert.NoError(t, err)
	assert.Equal(t, "100", strings.TrimSpace(string(data)))

	// Best-effort restore (may fail without privileges if original was lower)
	_ = os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid), current, 0644)
}

func TestSetOOMScore_InvalidPID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("OOM score adjustment only supported on Linux")
	}

	err := SetOOMScore(999999999, 100)
	assert.Error(t, err)
}

func TestRuntimeDir(t *testing.T) {
	dir := RuntimeDir()
	assert.NotEmpty(t, dir)
}

func TestPidPath_XDG(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/test-xdg-runtime")
	assert.Equal(t, "/tmp/test-xdg-runtime", pidPath())
}

func TestPidPath_HomeSpinifexFallback(t *testing.T) {
	tmpHome := t.TempDir()
	spinifexDir := fmt.Sprintf("%s/spinifex", tmpHome)
	require.NoError(t, os.Mkdir(spinifexDir, 0755))

	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("HOME", tmpHome)

	assert.Equal(t, spinifexDir, pidPath())
}

func TestPidPath_TempDirFallback(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("HOME", "/nonexistent-home-dir-utils-test")

	assert.Equal(t, os.TempDir(), pidPath())
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

func TestWritePidFile_CreateError(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/nonexistent-dir-utils-test")
	err := WritePidFile("testservice", 12345)
	assert.Error(t, err)
}

func TestRemovePidFileAt_EmptyDir(t *testing.T) {
	err := RemovePidFileAt("", fmt.Sprintf("nonexistent-service-%d", time.Now().UnixNano()))
	assert.Error(t, err)
}

func TestGeneratePidFile_InvalidPath(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/nonexistent-dir-utils-test")
	path, err := GeneratePidFile("test")
	// GeneratePidFile doesn't check if path exists, just builds it
	assert.NoError(t, err)
	assert.Contains(t, path, "test.pid")
}

func TestReadPidFileFrom_EmptyDir(t *testing.T) {
	_, err := ReadPidFileFrom("", fmt.Sprintf("nonexistent-service-%d", time.Now().UnixNano()))
	assert.Error(t, err)
}

func TestServiceStatus_Stopped(t *testing.T) {
	dir := t.TempDir()
	status, err := ServiceStatus(dir, fmt.Sprintf("no-such-svc-%d", time.Now().UnixNano()))
	require.NoError(t, err)
	assert.Equal(t, "stopped", status)
}

func TestServiceStatus_Running(t *testing.T) {
	dir := t.TempDir()

	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	require.NoError(t, WritePidFileTo(dir, "running-svc", cmd.Process.Pid))

	status, err := ServiceStatus(dir, "running-svc")
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("running (pid: %d)", cmd.Process.Pid), status)
}

func TestServiceStatus_CorruptPidFile(t *testing.T) {
	dir := t.TempDir()
	// Write a non-numeric pid file — ReadPidFileFrom should fail to parse it.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad-svc.pid"), []byte("not-a-pid"), 0o644))

	_, err := ServiceStatus(dir, "bad-svc")
	assert.Error(t, err)
}

func TestWaitForProcessExit_ProcessAlreadyDead(t *testing.T) {
	cmd := exec.Command("true")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	require.NoError(t, cmd.Wait())

	// `true` has exited and been reaped — WaitForProcessExit should return
	// nil immediately on the first tick.
	err := WaitForProcessExit(pid, 2*time.Second)
	assert.NoError(t, err)
}

func TestWaitForProcessExit_ProcessExitsBeforeTimeout(t *testing.T) {
	cmd := exec.Command("sleep", "0.1")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid

	// Reap in the background so the kernel releases the PID once sleep exits.
	go func() { _ = cmd.Wait() }()

	err := WaitForProcessExit(pid, 5*time.Second)
	assert.NoError(t, err)
}

func TestWaitForProcessExit_Timeout(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	err := WaitForProcessExit(cmd.Process.Pid, 200*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeout")
}

func TestHashMAC_FirstOctetPinnedTo02(t *testing.T) {
	// First octet must be 0x02 (not just any LAA value) to stay out of vendor OUI space.
	for i := range 1000 {
		hw, err := net.ParseMAC(HashMAC(fmt.Sprintf("id-%d", i)))
		require.NoError(t, err)
		assert.Equal(t, byte(0x02), hw[0], "first octet must be 0x02, got %#x", hw[0])
	}
}

func TestHashMAC_Determinism(t *testing.T) {
	// Same id must produce the same MAC across calls and across goroutines.
	// Reconcilers across nodes rely on this — non-determinism would
	// split-brain DHCP server_mac vs LRP MAC.
	const id = "i-abc123"
	want := HashMAC(id)

	for range 100 {
		assert.Equal(t, want, HashMAC(id))
	}

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			for range 100 {
				assert.Equal(t, want, HashMAC(id))
			}
		})
	}
	wg.Wait()
}

func TestHashMAC_IdSeparates(t *testing.T) {
	// Different ids must yield different MACs.
	a := HashMAC("subnet-aaaaaaaaaaaaaaaaa")
	b := HashMAC("subnet-bbbbbbbbbbbbbbbbb")
	assert.NotEqual(t, a, b)
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
		err := VerifyImageChecksum(img, url, "sha512")
		assert.NoError(t, err)
	})

	t.Run("ubuntu sha256 binary-mode asterisk match", func(t *testing.T) {
		img := writeTempImage(t, ubuntuName, ubuntuBytes)
		body := fmt.Sprintf("%s *%s\n", sha256Hex(ubuntuBytes), ubuntuName)
		url := newSumsServer(t, body)
		err := VerifyImageChecksum(img, url, "sha256")
		assert.NoError(t, err)
	})

	t.Run("alpine single-line named match", func(t *testing.T) {
		img := writeTempImage(t, alpineName, alpineBytes)
		body := fmt.Sprintf("%s  %s\n", sha512Hex(alpineBytes), alpineName)
		url := newSumsServer(t, body)
		err := VerifyImageChecksum(img, url, "sha512")
		assert.NoError(t, err)
	})

	t.Run("alpine bare-digest fallback", func(t *testing.T) {
		img := writeTempImage(t, alpineName, alpineBytes)
		body := sha512Hex(alpineBytes) + "\n"
		url := newSumsServer(t, body)
		err := VerifyImageChecksum(img, url, "sha512")
		assert.NoError(t, err)
	})

	t.Run("case-sensitive filename mismatch", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		// Capitalised "Debian-..." in sums file: legitimate divergence from upstream
		// convention, treated as not-found.
		body := fmt.Sprintf("%s  Debian-12-generic-amd64.tar.xz\n", sha512Hex(debianBytes))
		url := newSumsServer(t, body)
		err := VerifyImageChecksum(img, url, "sha512")
		assert.ErrorIs(t, err, ErrChecksumNotFound)
	})

	t.Run("comment lines skipped", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		body := fmt.Sprintf("# Hash file generated by upstream\n# PGP-signed below\n%s  %s\n",
			sha512Hex(debianBytes), debianName)
		url := newSumsServer(t, body)
		err := VerifyImageChecksum(img, url, "sha512")
		assert.NoError(t, err)
	})

	t.Run("digest mismatch", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		wrong := sha512Hex([]byte("a different blob entirely"))
		body := fmt.Sprintf("%s  %s\n", wrong, debianName)
		url := newSumsServer(t, body)
		err := VerifyImageChecksum(img, url, "sha512")
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
		err := VerifyImageChecksum(img, url, "sha512")
		assert.ErrorIs(t, err, ErrChecksumNotFound)
	})

	t.Run("unsupported checksum type rejected before any fetch", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		// Server URL is gibberish — function must fail before reaching it.
		err := VerifyImageChecksum(img, "https://example.invalid/SUMS", "md5")
		assert.ErrorIs(t, err, ErrUnsupportedChecksumType)
	})

	t.Run("http 404 wraps fetch failure", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)
		trustTestServer(t, srv)
		err := VerifyImageChecksum(img, srv.URL+"/SUMS", "sha512")
		assert.ErrorIs(t, err, ErrChecksumFetchFailed)
	})

	t.Run("non-https initial url rejected before any request", func(t *testing.T) {
		img := writeTempImage(t, debianName, debianBytes)
		// Hit a port nothing listens on — if HTTPS check ever regresses we'd
		// still get an error, but we want ErrChecksumFetchFailed wrapping the
		// scheme refusal specifically.
		err := VerifyImageChecksum(img, "http://127.0.0.1:1/SUMS", "sha512")
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
		err := VerifyImageChecksum(img, srv.URL+"/SUMS", "sha512")
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
		err := VerifyImageChecksum(img, srv.URL+"/SUMS", "sha512")
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
		err := VerifyImageChecksum(img, url, "sha512")
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

		err := VerifyImageChecksum(img, srv.URL+"/SUMS", "sha512")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrChecksumFetchFailed)
	})

	t.Run("digest length mismatch rejected before hashing", func(t *testing.T) {
		// sha256 digest served, caller declares sha512: algorithm disagreement,
		// not tampering. Must be distinct from ErrChecksumMismatch.
		img := writeTempImage(t, debianName, debianBytes)
		body := fmt.Sprintf("%s  %s\n", sha256Hex(debianBytes), debianName)
		url := newSumsServer(t, body)
		err := VerifyImageChecksum(img, url, "sha512")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrChecksumFetchFailed)
		assert.NotErrorIs(t, err, ErrChecksumMismatch)
		assert.Contains(t, err.Error(), "digest length")
	})

	t.Run("empty image file rejected with clear error", func(t *testing.T) {
		img := writeTempImage(t, debianName, nil)
		body := fmt.Sprintf("%s  %s\n", sha512Hex(nil), debianName)
		url := newSumsServer(t, body)
		err := VerifyImageChecksum(img, url, "sha512")
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
		err := VerifyImageChecksum(img, url, "sha512")
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
		err := VerifyImageChecksum(img, url, "sha256")
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
		err := VerifyImageChecksum(img, url, "sha256")
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
		err := VerifyImageChecksum(img, url, "sha256")
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
			// URL must be pinned to a dated build (no moving "latest" symlink)
			// and the checksum URL must be the BSD-style .CHECKSUM companion.
			assert.NotContains(t, img.URL, "latest")
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

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		expected   string
	}{
		{"IPv4 with port", "192.168.1.1:12345", "192.168.1.1"},
		{"IPv6 with port", "[::1]:12345", "::1"},
		{"IPv4 bare", "192.168.1.1", "192.168.1.1"},
		{"IPv6 full with port", "[2001:db8::1]:443", "2001:db8::1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, ClientIP(tt.remoteAddr))
		})
	}
}

func TestAvailableImages_ECSNodeEntry(t *testing.T) {
	img, ok := AvailableImages["spinifex-ecs-node"]
	require.True(t, ok, "spinifex-ecs-node must be in the system image catalog")
	assert.Equal(t, "ecs", img.Tags["spinifex:managed-by"],
		"ECS node image must carry the managed-by=ecs tag the UI guard resolves on")
}
