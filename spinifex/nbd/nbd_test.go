package nbd

import (
	"strconv"
	"testing"
)

func TestBuildArgs_TCPTransport(t *testing.T) {
	cfg := &NBDKitConfig{
		Port:       10809,
		PidFile:    "/tmp/nbd.pid",
		PluginPath: "/usr/lib/nbdkit/plugins/vb.so",
		UseTCP:     true,
		Size:       1073741824,
		Volume:     "vol-abc123",
		Bucket:     "my-bucket",
		Region:     "us-east-1",
		AccessKey:  "AKIA123",
		SecretKey:  "secret",
		BaseDir:    "/data",
		Host:       "localhost:9000",
		CacheSize:  256,
		ShardWAL:   true,
	}

	args, err := cfg.buildArgs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{
		"-f",
		"--pidfile", "/tmp/nbd.pid",
		"-p", "10809",
		"/usr/lib/nbdkit/plugins/vb.so",
		"size=1073741824",
		"volume=vol-abc123",
		"bucket=my-bucket",
		"region=us-east-1",
		"access_key=AKIA123",
		"secret_key=secret",
		"base_dir=/data",
		"host=localhost:9000",
		"cache_size=256",
		"shardwal=true",
	}

	assertArgs(t, expected, args)
}

func TestBuildArgs_UnixSocketTransport(t *testing.T) {
	cfg := &NBDKitConfig{
		Socket:     "/tmp/nbd.sock",
		PidFile:    "/tmp/nbd.pid",
		PluginPath: "/usr/lib/nbdkit/plugins/vb.so",
		UseTCP:     false,
		Size:       536870912,
		Volume:     "vol-def456",
		Bucket:     "bucket-2",
		Region:     "eu-west-1",
		AccessKey:  "AKIA456",
		SecretKey:  "topsecret",
		BaseDir:    "/mnt/data",
		Host:       "10.0.0.1:9000",
		CacheSize:  128,
		ShardWAL:   false,
	}

	args, err := cfg.buildArgs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{
		"-f",
		"--pidfile", "/tmp/nbd.pid",
		"--unix", "/tmp/nbd.sock",
		"/usr/lib/nbdkit/plugins/vb.so",
		"size=536870912",
		"volume=vol-def456",
		"bucket=bucket-2",
		"region=eu-west-1",
		"access_key=AKIA456",
		"secret_key=topsecret",
		"base_dir=/mnt/data",
		"host=10.0.0.1:9000",
		"cache_size=128",
		"shardwal=false",
	}

	assertArgs(t, expected, args)
}

func TestBuildArgs_SocketTransport_MissingSocket(t *testing.T) {
	cfg := &NBDKitConfig{
		PidFile:    "/tmp/nbd.pid",
		PluginPath: "/usr/lib/nbdkit/plugins/vb.so",
		UseTCP:     false,
		Socket:     "", // empty socket path
	}

	_, err := cfg.buildArgs()
	if err == nil {
		t.Fatal("expected error for missing socket path, got nil")
	}

	want := "socket path is required when not using TCP transport"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestBuildArgs_Verbose(t *testing.T) {
	cfg := &NBDKitConfig{
		Socket:     "/tmp/nbd.sock",
		PidFile:    "/tmp/nbd.pid",
		PluginPath: "/usr/lib/nbdkit/plugins/vb.so",
		Verbose:    true,
	}

	args, err := cfg.buildArgs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// -v should appear right after plugin path
	pluginIdx := indexOf(args, cfg.PluginPath)
	if pluginIdx < 0 {
		t.Fatal("plugin path not found in args")
	}
	if pluginIdx+1 >= len(args) || args[pluginIdx+1] != "-v" {
		t.Errorf("expected -v after plugin path, got args: %v", args)
	}
}

func TestBuildArgs_NotVerbose(t *testing.T) {
	cfg := &NBDKitConfig{
		Socket:     "/tmp/nbd.sock",
		PidFile:    "/tmp/nbd.pid",
		PluginPath: "/usr/lib/nbdkit/plugins/vb.so",
		Verbose:    false,
	}

	args, err := cfg.buildArgs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, arg := range args {
		if arg == "-v" {
			t.Error("expected -v to be absent when Verbose=false")
		}
	}
}

func TestBuildArgs_TCPPortValue(t *testing.T) {
	tests := []struct {
		port int
		want string
	}{
		{10809, "10809"},
		{0, "0"},
		{65535, "65535"},
	}

	for _, tt := range tests {
		t.Run("port_"+tt.want, func(t *testing.T) {
			cfg := &NBDKitConfig{
				UseTCP:     true,
				Port:       tt.port,
				PidFile:    "/tmp/nbd.pid",
				PluginPath: "/plugin.so",
			}

			args, err := cfg.buildArgs()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			pIdx := indexOf(args, "-p")
			if pIdx < 0 || pIdx+1 >= len(args) {
				t.Fatal("-p flag not found in args")
			}
			if args[pIdx+1] != strconv.Itoa(tt.port) {
				t.Errorf("port = %q, want %q", args[pIdx+1], tt.want)
			}
		})
	}
}

func TestBuildArgs_ArgOrdering(t *testing.T) {
	cfg := &NBDKitConfig{
		Socket:     "/tmp/nbd.sock",
		PidFile:    "/tmp/nbd.pid",
		PluginPath: "/plugin.so",
		Verbose:    true,
		Volume:     "vol-test",
	}

	args, err := cfg.buildArgs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// -f must be first
	if args[0] != "-f" {
		t.Errorf("first arg = %q, want -f", args[0])
	}

	// --pidfile before transport args
	pidIdx := indexOf(args, "--pidfile")
	unixIdx := indexOf(args, "--unix")
	pluginIdx := indexOf(args, cfg.PluginPath)
	verboseIdx := indexOf(args, "-v")
	volumeIdx := indexOf(args, "volume=vol-test")

	if pidIdx < 0 || unixIdx < 0 || pluginIdx < 0 || verboseIdx < 0 || volumeIdx < 0 {
		t.Fatalf("missing expected args in: %v", args)
	}

	// Order: -f, --pidfile, transport, plugin, -v, plugin-args
	if pidIdx >= unixIdx {
		t.Error("--pidfile should come before --unix")
	}
	if unixIdx >= pluginIdx {
		t.Error("--unix should come before plugin path")
	}
	if pluginIdx >= verboseIdx {
		t.Error("plugin path should come before -v")
	}
	if verboseIdx >= volumeIdx {
		t.Error("-v should come before plugin args")
	}
}

func assertArgs(t *testing.T, expected, got []string) {
	t.Helper()
	if len(expected) != len(got) {
		t.Fatalf("args length = %d, want %d\ngot:  %v\nwant: %v", len(got), len(expected), got, expected)
	}
	for i := range expected {
		if expected[i] != got[i] {
			t.Errorf("args[%d] = %q, want %q", i, got[i], expected[i])
		}
	}
}

func indexOf(args []string, val string) int {
	for i, a := range args {
		if a == val {
			return i
		}
	}
	return -1
}
