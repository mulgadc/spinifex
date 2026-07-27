package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecute(t *testing.T) {
	cfg := Config{
		Name: "test-vm",
	}

	cmd, err := cfg.Execute()

	// Expect error, CPU count required
	assert.Error(t, err)
	assert.ErrorContains(t, err, "cpu count is required")
	assert.Nil(t, cmd)

	cfg.CPUCount = 2

	cmd, err = cfg.Execute()

	// Expect error, Memory required
	assert.Error(t, err)
	assert.ErrorContains(t, err, "memory is required")
	assert.Nil(t, cmd)

	cfg.Memory = 1024

	cmd, err = cfg.Execute()

	// Expect error, at least one drive or kernel image required
	assert.Error(t, err)
	assert.ErrorContains(t, err, "at least one drive or a kernel image is required")
	assert.Nil(t, cmd)

	cfg.Drives = []Drive{
		{
			File:   "disk.img",
			Format: "qcow2",
		},
	}

	cfg.Architecture = "x86_64"
	cmd, err = cfg.Execute()

	// Now expect no error
	assert.NoError(t, err)
	assert.NotNil(t, cmd)

	expectedArgs := []string{
		"-smp", "2",
		"-m", "1024",
		"-drive", "file=disk.img,format=qcow2",
	}

	assert.Contains(t, cmd.Path, "qemu-system-x86_64")
	assert.Equal(t, expectedArgs, cmd.Args[1:])

	// Toggle Instance type to ARM
	cfg.InstanceType = "t4g.micro"
	cfg.Architecture = "arm64"

	cmd, err = cfg.Execute()

	// Now expect no error
	assert.NoError(t, err)
	assert.NotNil(t, cmd)

	assert.Contains(t, cmd.Path, "qemu-system-aarch64")
	assert.Equal(t, expectedArgs, cmd.Args[1:])
}

func TestExecute_IOThreadAndCache(t *testing.T) {
	cfg := Config{
		CPUCount:     2,
		Memory:       4096,
		Architecture: "x86_64",
		IOThreads: []IOThread{
			{ID: "ioth-os"},
		},
		Drives: []Drive{
			{
				File:   "nbd:unix:/run/test.sock",
				Format: "raw",
				If:     "none",
				Media:  "disk",
				ID:     "os",
				Cache:  "none",
			},
		},
		Devices: []Device{
			{Value: "virtio-blk-pci,drive=os,iothread=ioth-os,num-queues=2,bootindex=1"},
		},
	}

	cmd, err := cfg.Execute()
	assert.NoError(t, err)
	assert.NotNil(t, cmd)

	args := cmd.Args[1:]

	// Verify iothread object is present
	assert.Contains(t, args, "-object")
	objectIdx := -1
	for i, a := range args {
		if a == "-object" {
			objectIdx = i
			break
		}
	}
	assert.Greater(t, objectIdx, -1)
	assert.Equal(t, "iothread,id=ioth-os", args[objectIdx+1])

	// Verify iothread appears before drives
	driveIdx := -1
	for i, a := range args {
		if a == "-drive" {
			driveIdx = i
			break
		}
	}
	assert.Greater(t, driveIdx, objectIdx, "iothread object must appear before drives")

	// Verify drive includes cache=none
	assert.Equal(t, "file=nbd:unix:/run/test.sock,format=raw,if=none,media=disk,id=os,cache=none", args[driveIdx+1])

	// Verify device includes iothread and num-queues
	deviceIdx := -1
	for i, a := range args {
		if a == "-device" {
			deviceIdx = i
			break
		}
	}
	assert.Greater(t, deviceIdx, -1)
	assert.Equal(t, "virtio-blk-pci,drive=os,iothread=ioth-os,num-queues=2,bootindex=1", args[deviceIdx+1])
}

func TestExecute_NoCacheWhenEmpty(t *testing.T) {
	cfg := Config{
		CPUCount:     1,
		Memory:       512,
		Architecture: "x86_64",
		Drives: []Drive{
			{
				File:   "disk.img",
				Format: "raw",
				ID:     "d0",
			},
		},
	}

	cmd, err := cfg.Execute()
	assert.NoError(t, err)

	// Verify cache= is NOT in the drive string when Cache is empty
	for i, a := range cmd.Args[1:] {
		if a == "-drive" {
			driveStr := cmd.Args[i+2]
			assert.NotContains(t, driveStr, "cache=")
			break
		}
	}
}

func TestExecute_MultipleIOThreads(t *testing.T) {
	cfg := Config{
		CPUCount:     4,
		Memory:       8192,
		Architecture: "x86_64",
		IOThreads: []IOThread{
			{ID: "ioth-os"},
			{ID: "ioth-data"},
		},
		Drives: []Drive{
			{
				File:   "nbd:unix:/run/os.sock",
				Format: "raw",
				If:     "none",
				ID:     "os",
				Cache:  "none",
			},
			{
				File:   "nbd:unix:/run/data.sock",
				Format: "raw",
				If:     "none",
				ID:     "data",
				Cache:  "none",
			},
		},
		Devices: []Device{
			{Value: "virtio-blk-pci,drive=os,iothread=ioth-os,num-queues=4,bootindex=1"},
			{Value: "virtio-blk-pci,drive=data,iothread=ioth-data"},
		},
	}

	cmd, err := cfg.Execute()
	assert.NoError(t, err)

	args := cmd.Args[1:]

	// Count iothread objects
	iothreadCount := 0
	for i, a := range args {
		if a == "-object" && i+1 < len(args) {
			if args[i+1] == "iothread,id=ioth-os" || args[i+1] == "iothread,id=ioth-data" {
				iothreadCount++
			}
		}
	}
	assert.Equal(t, 2, iothreadCount)
}

// TestExecute_Blockdev_UnixServer asserts a unix-socket NBD blockdev renders
// as a -blockdev argument, ordered before its referencing -device, and that
// -drive is untouched when Drives is empty — a data volume no longer gets an
// anonymous -drive.
func TestExecute_Blockdev_UnixServer(t *testing.T) {
	cfg := Config{
		CPUCount:     2,
		Memory:       1024,
		Architecture: "x86_64",
		// Execute requires a drive or kernel image; a kernel keeps this test
		// isolated to blockdev/device rendering without an unrelated -drive.
		KernelImage: "/boot/vmlinuz",
		IOThreads:   []IOThread{{ID: "ioth-vol-data-a"}},
		Blockdevs: []Blockdev{
			VolumeBlockdev("nbd-vol-data-a", NBDServerOpts{Type: "unix", Path: "/run/spinifex/nbd/nbd-vol-data-a.sock"}),
		},
		Devices: []Device{
			VolumeBlkDevice("vol-data-a", "nbd-vol-data-a", "ioth-vol-data-a", "hotplug-ebs3"),
		},
	}

	cmd, err := cfg.Execute()
	require.NoError(t, err)

	args := cmd.Args[1:]
	assert.False(t, argExists(args, "-drive"), "a config with only a data volume must emit no -drive")

	blockdevIdx := -1
	for i, a := range args {
		if a == "-blockdev" {
			blockdevIdx = i
			break
		}
	}
	require.Greater(t, blockdevIdx, -1, "-blockdev must be emitted")
	assert.Equal(t, "driver=nbd,node-name=nbd-vol-data-a,server.type=unix,server.path=/run/spinifex/nbd/nbd-vol-data-a.sock,export=", args[blockdevIdx+1])

	deviceIdx := -1
	for i, a := range args {
		if a == "-device" {
			deviceIdx = i
			break
		}
	}
	require.Greater(t, deviceIdx, -1)
	assert.Greater(t, deviceIdx, blockdevIdx, "-blockdev must precede the -device referencing it")
	assert.Equal(t, "virtio-blk-pci,id=vdisk-vol-data-a,drive=nbd-vol-data-a,iothread=ioth-vol-data-a,serial=voldataa,bus=hotplug-ebs3", args[deviceIdx+1])
}

// TestExecute_Blockdev_InetServer asserts the TCP NBD form renders
// server.type=inet with host/port instead of a unix socket path.
func TestExecute_Blockdev_InetServer(t *testing.T) {
	cfg := Config{
		CPUCount:     2,
		Memory:       1024,
		Architecture: "x86_64",
		KernelImage:  "/boot/vmlinuz",
		Blockdevs: []Blockdev{
			VolumeBlockdev("nbd-vol-data-b", NBDServerOpts{Type: "inet", Host: "10.0.0.5", Port: 10809}),
		},
	}

	cmd, err := cfg.Execute()
	require.NoError(t, err)

	assert.Equal(t, "driver=nbd,node-name=nbd-vol-data-b,server.type=inet,server.host=10.0.0.5,server.port=10809,export=",
		argValue(cmd.Args[1:], "-blockdev"))
}

// TestExecute_NoBlockdevWhenEmpty asserts a config with no Blockdevs emits no
// -blockdev flag at all — the common case (boot-only, or no EBS requests).
func TestExecute_NoBlockdevWhenEmpty(t *testing.T) {
	cfg := Config{
		CPUCount:     1,
		Memory:       512,
		Architecture: "x86_64",
		Drives:       []Drive{{File: "disk.img", Format: "raw"}},
	}

	cmd, err := cfg.Execute()
	require.NoError(t, err)

	assert.False(t, argExists(cmd.Args[1:], "-blockdev"))
}

// argValue returns the value following flag in args, or "" if not found.
func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func argExists(args []string, flag string) bool {
	return slices.Contains(args, flag)
}

// allArgValues returns all values following flag (every occurrence).
func allArgValues(args []string, flag string) []string {
	var out []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

func TestExecute_DriveAndKernelInvariant(t *testing.T) {
	base := Config{
		CPUCount:     2,
		Memory:       1024,
		Architecture: "x86_64",
	}

	t.Run("drives-only is OK", func(t *testing.T) {
		cfg := base
		cfg.Drives = []Drive{{File: "disk.img", Format: "raw"}}
		cmd, err := cfg.Execute()
		assert.NoError(t, err)
		assert.NotNil(t, cmd)
	})

	t.Run("kernel-only is OK", func(t *testing.T) {
		cfg := base
		cfg.KernelImage = "/boot/vmlinuz"
		cmd, err := cfg.Execute()
		assert.NoError(t, err)
		assert.NotNil(t, cmd)
	})

	t.Run("both-empty returns error", func(t *testing.T) {
		cfg := base
		cmd, err := cfg.Execute()
		assert.Error(t, err)
		assert.ErrorContains(t, err, "at least one drive or a kernel image")
		assert.Nil(t, cmd)
	})
}

func TestResetNodeLocalState(t *testing.T) {
	v := &VM{
		ID:                    "i-abc123",
		MetadataServerAddress: "127.0.0.1:9999",
		Status:                StateRunning,
	}

	v.ResetNodeLocalState()

	assert.Empty(t, v.MetadataServerAddress)
	assert.NotNil(t, v.QMPClient)
	// ID and Status should be unchanged
	assert.Equal(t, "i-abc123", v.ID)
	assert.Equal(t, StateRunning, v.Status)
}

func TestExecute_SerialSocketAndConsoleLog(t *testing.T) {
	t.Run("both set emits chardev and serial", func(t *testing.T) {
		cfg := Config{
			CPUCount:       1,
			Memory:         512,
			Architecture:   "x86_64",
			SerialSocket:   "/run/serial.sock",
			ConsoleLogPath: "/var/log/console.log",
			Drives:         []Drive{{File: "disk.img", Format: "raw"}},
		}

		cmd, err := cfg.Execute()
		assert.NoError(t, err)

		args := cmd.Args[1:]
		chardev := argValue(args, "-chardev")
		assert.Contains(t, chardev, "socket,id=console0")
		assert.Contains(t, chardev, "path=/run/serial.sock")
		assert.Contains(t, chardev, "logfile=/var/log/console.log")
		assert.Equal(t, "chardev:console0", argValue(args, "-serial"))
	})

	// Boundary: the production guard requires BOTH fields. Flipping && to ||
	// would silently emit invalid -chardev args; these subtests catch that.
	t.Run("serial socket alone emits nothing", func(t *testing.T) {
		cfg := Config{
			CPUCount:     1,
			Memory:       512,
			Architecture: "x86_64",
			SerialSocket: "/run/serial.sock",
			Drives:       []Drive{{File: "disk.img", Format: "raw"}},
		}

		cmd, err := cfg.Execute()
		assert.NoError(t, err)

		args := cmd.Args[1:]
		assert.Empty(t, argValue(args, "-chardev"))
		assert.Empty(t, argValue(args, "-serial"))
	})

	t.Run("console log alone emits nothing", func(t *testing.T) {
		cfg := Config{
			CPUCount:       1,
			Memory:         512,
			Architecture:   "x86_64",
			ConsoleLogPath: "/var/log/console.log",
			Drives:         []Drive{{File: "disk.img", Format: "raw"}},
		}

		cmd, err := cfg.Execute()
		assert.NoError(t, err)

		args := cmd.Args[1:]
		assert.Empty(t, argValue(args, "-chardev"))
		assert.Empty(t, argValue(args, "-serial"))
	})
}

func TestExecute_MicrovmFileChardev(t *testing.T) {
	cfg := Config{
		CPUCount:       1,
		Memory:         512,
		Architecture:   "x86_64",
		MachineType:    "microvm,x-option-roms=off",
		ConsoleLogPath: "/var/log/console.log",
		KernelImage:    "/boot/vmlinuz",
	}

	cmd, err := cfg.Execute()
	assert.NoError(t, err)
	require.NotNil(t, cmd)

	args := cmd.Args[1:]
	chardev := argValue(args, "-chardev")
	assert.Equal(t, "file,id=console0,path=/var/log/console.log", chardev)
	// -serial chardev:console0 attaches to the isa-serial=on device (ttyS0).
	assert.Equal(t, "chardev:console0", argValue(args, "-serial"))
	// No explicit isa-serial device addition (would create ttyS1, not ttyS0).
	devs := strings.Join(allArgValues(args, "-device"), " ")
	assert.NotContains(t, devs, "isa-serial")
}

func TestExecute_ARM64_Q35(t *testing.T) {
	cfg := Config{
		CPUCount:     1,
		Memory:       512,
		Architecture: "arm64",
		MachineType:  "q35",
		Drives:       []Drive{{File: "disk.img", Format: "raw"}},
	}

	cmd, err := cfg.Execute()

	assert.NoError(t, err)
	require.NotNil(t, cmd)
	args := cmd.Args[1:]
	assert.Contains(t, cmd.Path, "qemu-system-aarch64")
	assert.Equal(t, "virt", argValue(args, "-M"))
	// Firmware is no longer auto-loaded via -bios on arm64 q35; it flows
	// from cfg.UseUEFI as pflash CODE+VARS instead.
	assert.False(t, argExists(args, "-bios"), "-bios must not be emitted")
}

// installFakeFirmware seeds a temp directory with code+vars files matching the
// pflash split-file shape and swaps FirmwarePathCandidates so tests don't
// depend on whatever firmware happens to be installed on the host.
func installFakeFirmware(t *testing.T, arch string, varsBytes []byte) (codePath, varsPath string) {
	t.Helper()
	dir := t.TempDir()
	codePath = filepath.Join(dir, "CODE.fd")
	varsPath = filepath.Join(dir, "VARS.fd")
	require.NoError(t, os.WriteFile(codePath, []byte("fake-code"), 0o644))
	require.NoError(t, os.WriteFile(varsPath, varsBytes, 0o644))

	orig := FirmwarePathCandidates
	FirmwarePathCandidates = map[string][]FirmwareCandidate{
		arch: {{Code: codePath, VarsTemplate: varsPath}},
	}
	t.Cleanup(func() { FirmwarePathCandidates = orig })
	return codePath, varsPath
}

func TestExecute_x86_64_UEFI(t *testing.T) {
	codePath, _ := installFakeFirmware(t, "x86_64", make([]byte, 540_672))

	cfg := Config{
		CPUCount:     2,
		Memory:       1024,
		Architecture: "x86_64",
		MachineType:  "q35",
		UseUEFI:      true,
		Drives:       []Drive{{File: "disk.img", Format: "raw", If: "none", ID: "os"}},
	}

	cmd, err := cfg.Execute()
	require.NoError(t, err)
	require.NotNil(t, cmd)
	args := cmd.Args[1:]

	driveArgs := allArgValues(args, "-drive")
	require.GreaterOrEqual(t, len(driveArgs), 1)
	assert.Equal(t, fmt.Sprintf("file=%s,format=raw,if=pflash,unit=0,readonly=on", codePath), driveArgs[0],
		"first -drive must be pflash CODE readonly unit=0")
	assert.False(t, argExists(args, "-bios"), "-bios must not be emitted under UEFI")
}

func TestExecute_x86_64_UEFI_MissingFirmware(t *testing.T) {
	orig := FirmwarePathCandidates
	FirmwarePathCandidates = map[string][]FirmwareCandidate{
		"x86_64": {{Code: "/nonexistent/CODE.fd", VarsTemplate: "/nonexistent/VARS.fd"}},
	}
	t.Cleanup(func() { FirmwarePathCandidates = orig })

	cfg := Config{
		CPUCount:     1,
		Memory:       512,
		Architecture: "x86_64",
		MachineType:  "q35",
		UseUEFI:      true,
		Drives:       []Drive{{File: "disk.img", Format: "raw"}},
	}

	cmd, err := cfg.Execute()
	assert.Nil(t, cmd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "x86_64", "error must name the architecture so operators know which firmware to install")
}

func TestExecute_ARM64_UEFI_pflash(t *testing.T) {
	codePath, _ := installFakeFirmware(t, "arm64", make([]byte, 67_108_864))

	cfg := Config{
		CPUCount:     1,
		Memory:       512,
		Architecture: "arm64",
		MachineType:  "q35",
		UseUEFI:      true,
		Drives:       []Drive{{File: "nbd:unix:/tmp/efi.sock", Format: "raw", If: "pflash", Unit: 1}},
	}

	cmd, err := cfg.Execute()
	require.NoError(t, err)
	require.NotNil(t, cmd)
	args := cmd.Args[1:]
	assert.Contains(t, cmd.Path, "qemu-system-aarch64")
	assert.Equal(t, "virt", argValue(args, "-M"))

	driveArgs := allArgValues(args, "-drive")
	require.Len(t, driveArgs, 2, "expect pflash CODE + VARS")
	assert.Equal(t, fmt.Sprintf("file=%s,format=raw,if=pflash,unit=0,readonly=on", codePath), driveArgs[0])
	assert.Equal(t, "file=nbd:unix:/tmp/efi.sock,format=raw,if=pflash,unit=1", driveArgs[1])
}

func TestExecute_MissingArchitecture(t *testing.T) {
	cfg := Config{
		CPUCount: 1,
		Memory:   512,
		Drives:   []Drive{{File: "disk.img", Format: "raw"}},
	}

	cmd, err := cfg.Execute()
	assert.Error(t, err)
	assert.Nil(t, cmd)
	assert.Contains(t, err.Error(), "architecture missing")
}

func TestExecute_KVMAndCPUType(t *testing.T) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm missing: %v", err)
	}

	cfg := Config{
		CPUCount:     2,
		Memory:       1024,
		Architecture: "x86_64",
		EnableKVM:    true,
		CPUType:      "host",
		Drives:       []Drive{{File: "disk.img", Format: "raw"}},
	}

	cmd, err := cfg.Execute()
	assert.NoError(t, err)

	args := cmd.Args[1:]
	assert.True(t, argExists(args, "-enable-kvm"))
	assert.Equal(t, "host", argValue(args, "-cpu"))
}

func TestExecute_FullConfig(t *testing.T) {
	cfg := Config{
		Name:           "full-vm",
		PIDFile:        "/run/vm.pid",
		QMPSocket:      "/run/vm.sock",
		NoGraphic:      true,
		MachineType:    "q35",
		ConsoleLogPath: "/var/log/vm.log",
		SerialSocket:   "/run/serial.sock",
		CPUCount:       4,
		Memory:         8192,
		Architecture:   "x86_64",
		IOThreads:      []IOThread{{ID: "io0"}},
		Drives: []Drive{
			{File: "nbd:unix:/run/os.sock", Format: "raw", If: "none", ID: "os", Cache: "none"},
		},
		Devices: []Device{{Value: "virtio-blk-pci,drive=os"}},
		NetDevs: []NetDev{{Value: "user,id=net0"}},
	}

	cmd, err := cfg.Execute()
	assert.NoError(t, err)
	assert.NotNil(t, cmd)
	assert.Contains(t, cmd.Path, "qemu-system-x86_64")

	args := cmd.Args[1:]
	assert.Equal(t, "/run/vm.pid", argValue(args, "-pidfile"))
	assert.Equal(t, "unix:/run/vm.sock,server,nowait", argValue(args, "-qmp"))
	assert.Equal(t, "none", argValue(args, "-display"))
	assert.Equal(t, "q35", argValue(args, "-M"))
	assert.Equal(t, "4", argValue(args, "-smp"))
	assert.Equal(t, "8192", argValue(args, "-m"))
	assert.Contains(t, argValue(args, "-chardev"), "logfile=/var/log/vm.log")
	assert.Equal(t, "chardev:console0", argValue(args, "-serial"))
	assert.Equal(t, "iothread,id=io0", argValue(args, "-object"))
	assert.Equal(t, "user,id=net0", argValue(args, "-netdev"))
}
