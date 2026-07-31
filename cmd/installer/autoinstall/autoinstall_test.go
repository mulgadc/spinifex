package autoinstall

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/cmd/installer/install"
)

const gib = 1 << 30

// setEnv clears every SPINIFEX_* variable inherited from the test runner before
// applying want, so one case cannot leak a kernel-cmdline value into the next.
func setEnv(t *testing.T, want map[string]string) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "SPINIFEX_") {
			t.Setenv(k, "")
		}
	}
	for k, v := range want {
		t.Setenv(k, v)
	}
}

// withDisks replaces the block-device scan. The clone matters: candidateDisks
// filters with slices.DeleteFunc, which would otherwise mutate the fixture.
func withDisks(t *testing.T, disks []install.Disk, err error) {
	t.Helper()
	prev := listDisks
	listDisks = func() ([]install.Disk, error) { return slices.Clone(disks), err }
	t.Cleanup(func() { listDisks = prev })
}

func mkDisk(path string, bytes int64) install.Disk {
	return install.Disk{
		Path:              path,
		ByID:              "/dev/disk/by-id/test-" + strings.TrimPrefix(path, "/dev/"),
		Bytes:             bytes,
		Model:             "TESTDISK",
		LogicalBlockSize:  512,
		PhysicalBlockSize: 512,
		Content:           "empty",
	}
}

func TestLoadReturnsNilWhenNotAuto(t *testing.T) {
	setEnv(t, nil)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Fatal("expected nil config when SPINIFEX_AUTO is not set")
	}
}

func TestLoadBuildsConfigFromEnv(t *testing.T) {
	withDisks(t, []install.Disk{mkDisk("/dev/vdb", 40*gib)}, nil)
	setEnv(t, map[string]string{
		"SPINIFEX_AUTO":            "1",
		"SPINIFEX_PASSWORD":        "s3cret",
		"SPINIFEX_HOSTNAME":        "node-7",
		"SPINIFEX_EMAIL":           "  ops@example.com  ",
		"SPINIFEX_WAN_IFACE":       "eno1",
		"SPINIFEX_WAN_DNS":         "1.1.1.1,8.8.8.8",
		"SPINIFEX_LAN_IFACE":       "eno2",
		"SPINIFEX_LAN_VLAN":        "100",
		"SPINIFEX_LAN_MTU":         "9000",
		"SPINIFEX_GPU_PASSTHROUGH": "1",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Hostname != "node-7" || cfg.RootPassword != "s3cret" {
		t.Errorf("identity = %q/%q", cfg.Hostname, cfg.RootPassword)
	}
	if cfg.Email != "ops@example.com" {
		t.Errorf("Email = %q, want trimmed", cfg.Email)
	}
	if cfg.Storage.FS != install.FSExt4 || len(cfg.Storage.Disks) != 1 {
		t.Errorf("Storage = %v / %d disks", cfg.Storage.FS, len(cfg.Storage.Disks))
	}
	if cfg.WAN.Interface != "eno1" || !cfg.WAN.DHCPMode {
		t.Errorf("WAN = %+v, want eno1 on DHCP", cfg.WAN)
	}
	if !slices.Equal(cfg.WAN.DNS, []string{"1.1.1.1", "8.8.8.8"}) {
		t.Errorf("WAN.DNS = %v", cfg.WAN.DNS)
	}
	if cfg.LAN.Interface != "eno2" || cfg.LAN.VLAN != 100 || cfg.LAN.MTU != 9000 {
		t.Errorf("LAN = %+v", cfg.LAN)
	}
	// Only the wan plane installs a default route.
	if cfg.LAN.Gateway != "" {
		t.Errorf("LAN.Gateway = %q, want cleared", cfg.LAN.Gateway)
	}
	if !cfg.VPC.Folded() {
		t.Errorf("VPC = %+v, want folded when no _IFACE is set", cfg.VPC)
	}
	if !cfg.GPUPassthrough {
		t.Error("GPUPassthrough not enabled")
	}
	if cfg.ClusterRole != "init" {
		t.Errorf("ClusterRole = %q, want init by default", cfg.ClusterRole)
	}
}

func TestLoadDefaultsHostname(t *testing.T) {
	withDisks(t, []install.Disk{mkDisk("/dev/vdb", 40*gib)}, nil)
	setEnv(t, map[string]string{
		"SPINIFEX_AUTO":      "1",
		"SPINIFEX_PASSWORD":  "s3cret",
		"SPINIFEX_WAN_IFACE": "eno1",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Hostname != "spinifex-node" {
		t.Errorf("Hostname = %q, want spinifex-node", cfg.Hostname)
	}
}

func TestLoadRequiresPassword(t *testing.T) {
	setEnv(t, map[string]string{"SPINIFEX_AUTO": "1"})

	cfg, err := Load()
	if err == nil {
		t.Fatalf("expected error, got config %+v", cfg)
	}
	if !strings.Contains(err.Error(), "SPINIFEX_PASSWORD") {
		t.Errorf("err = %v, want it to name SPINIFEX_PASSWORD", err)
	}
	if !strings.HasPrefix(err.Error(), "autoinstall config:") {
		t.Errorf("err = %v, want the autoinstall wrapper", err)
	}
}

func TestBuildConfigStaticWAN(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{
			name: "full static",
			env: map[string]string{
				"SPINIFEX_WAN_MODE": "static",
				"SPINIFEX_WAN_IP":   "10.0.0.5",
				"SPINIFEX_WAN_MASK": "255.255.255.0",
				"SPINIFEX_WAN_GW":   "10.0.0.1",
			},
		},
		{
			name: "missing gateway",
			env: map[string]string{
				"SPINIFEX_WAN_MODE": "static",
				"SPINIFEX_WAN_IP":   "10.0.0.5",
				"SPINIFEX_WAN_MASK": "255.255.255.0",
			},
			wantErr: "SPINIFEX_WAN_GW required",
		},
		{
			name:    "missing address",
			env:     map[string]string{"SPINIFEX_WAN_MODE": "static"},
			wantErr: "SPINIFEX_WAN_IP and SPINIFEX_WAN_MASK required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withDisks(t, []install.Disk{mkDisk("/dev/vdb", 40*gib)}, nil)
			env := map[string]string{
				"SPINIFEX_PASSWORD":  "s3cret",
				"SPINIFEX_WAN_IFACE": "eno1",
			}
			maps.Copy(env, tt.env)
			setEnv(t, env)

			cfg, err := buildConfig()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildConfig: %v", err)
			}
			if cfg.WAN.DHCPMode {
				t.Error("WAN.DHCPMode = true, want static")
			}
			if cfg.WAN.Address != "10.0.0.5" || cfg.WAN.Gateway != "10.0.0.1" {
				t.Errorf("WAN = %+v", cfg.WAN)
			}
		})
	}
}

func TestBuildConfigClusterRole(t *testing.T) {
	tests := []struct {
		name         string
		env          map[string]string
		wantErr      string
		wantRole     string
		wantJoin     string
		wantSkipForm bool
	}{
		{
			name:     "join with address",
			env:      map[string]string{"SPINIFEX_ROLE": "JOIN", "SPINIFEX_JOIN_ADDR": "10.0.0.1:9999"},
			wantRole: "join",
			wantJoin: "10.0.0.1:9999",
		},
		{
			name:    "join without address",
			env:     map[string]string{"SPINIFEX_ROLE": "join"},
			wantErr: "SPINIFEX_JOIN_ADDR required",
		},
		{
			// Formation is the provisioning controller's job here, so the join
			// address is not the installer's to demand.
			name:         "join deferred to the provisioner",
			env:          map[string]string{"SPINIFEX_ROLE": "join", "SPINIFEX_SKIP_FORMATION": "1"},
			wantRole:     "join",
			wantSkipForm: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withDisks(t, []install.Disk{mkDisk("/dev/vdb", 40*gib)}, nil)
			env := map[string]string{
				"SPINIFEX_PASSWORD":  "s3cret",
				"SPINIFEX_WAN_IFACE": "eno1",
			}
			maps.Copy(env, tt.env)
			setEnv(t, env)

			cfg, err := buildConfig()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildConfig: %v", err)
			}
			if cfg.ClusterRole != tt.wantRole {
				t.Errorf("ClusterRole = %q, want %q", cfg.ClusterRole, tt.wantRole)
			}
			if cfg.JoinAddr != tt.wantJoin {
				t.Errorf("JoinAddr = %q, want %q", cfg.JoinAddr, tt.wantJoin)
			}
			if cfg.SkipFormation != tt.wantSkipForm {
				t.Errorf("SkipFormation = %v, want %v", cfg.SkipFormation, tt.wantSkipForm)
			}
		})
	}
}

func TestBuildConfigRejectsDuplicatePlaneBinding(t *testing.T) {
	withDisks(t, []install.Disk{mkDisk("/dev/vdb", 40*gib)}, nil)
	setEnv(t, map[string]string{
		"SPINIFEX_PASSWORD":  "s3cret",
		"SPINIFEX_WAN_IFACE": "eno1",
		"SPINIFEX_LAN_IFACE": "eno1",
	})

	if _, err := buildConfig(); err == nil || !strings.Contains(err.Error(), "network roles") {
		t.Fatalf("err = %v, want a network roles validation failure", err)
	}
}

func TestBuildConfigPropagatesStorageError(t *testing.T) {
	setEnv(t, map[string]string{
		"SPINIFEX_PASSWORD":  "s3cret",
		"SPINIFEX_WAN_IFACE": "eno1",
		"SPINIFEX_FS":        "btrfs",
	})

	_, err := buildConfig()
	if err == nil || !strings.HasPrefix(err.Error(), "storage:") {
		t.Fatalf("err = %v, want a storage: prefix", err)
	}
}

func TestParseRole(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    install.NetworkRole
		wantErr string
	}{
		{
			name: "dhcp by default",
			want: install.NetworkRole{Interface: "eno1", DHCPMode: true},
		},
		{
			name: "vlan and mtu",
			env:  map[string]string{"SPINIFEX_LAN_VLAN": "42", "SPINIFEX_LAN_MTU": "9000"},
			want: install.NetworkRole{Interface: "eno1", DHCPMode: true, VLAN: 42, MTU: 9000},
		},
		{
			name: "dns list is comma separated",
			env:  map[string]string{"SPINIFEX_LAN_DNS": "1.1.1.1,9.9.9.9"},
			want: install.NetworkRole{Interface: "eno1", DHCPMode: true, DNS: []string{"1.1.1.1", "9.9.9.9"}},
		},
		{
			name: "static",
			env: map[string]string{
				"SPINIFEX_LAN_MODE": "STATIC",
				"SPINIFEX_LAN_IP":   "192.168.1.2",
				"SPINIFEX_LAN_MASK": "255.255.255.0",
				"SPINIFEX_LAN_GW":   "192.168.1.1",
			},
			want: install.NetworkRole{
				Interface: "eno1",
				Address:   "192.168.1.2",
				Mask:      "255.255.255.0",
				Gateway:   "192.168.1.1",
			},
		},
		{
			name:    "static without mask",
			env:     map[string]string{"SPINIFEX_LAN_MODE": "static", "SPINIFEX_LAN_IP": "192.168.1.2"},
			wantErr: "SPINIFEX_LAN_IP and SPINIFEX_LAN_MASK required",
		},
		{
			name:    "unparsable vlan",
			env:     map[string]string{"SPINIFEX_LAN_VLAN": "trunk"},
			wantErr: "SPINIFEX_LAN_VLAN",
		},
		{
			name:    "unparsable mtu",
			env:     map[string]string{"SPINIFEX_LAN_MTU": "jumbo"},
			wantErr: "SPINIFEX_LAN_MTU",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, tt.env)

			got, err := parseRole("LAN", "eno1")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRole: %v", err)
			}
			if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", tt.want) {
				t.Errorf("role = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseZFSOpts(t *testing.T) {
	t.Run("unset leaves the computed defaults", func(t *testing.T) {
		setEnv(t, nil)
		got, err := parseZFSOpts()
		if err != nil {
			t.Fatalf("parseZFSOpts: %v", err)
		}
		if got != (install.ZFSOpts{}) {
			t.Errorf("opts = %+v, want zero", got)
		}
	})

	t.Run("all tunables", func(t *testing.T) {
		setEnv(t, map[string]string{
			"SPINIFEX_ZFS_ASHIFT":      " 12 ",
			"SPINIFEX_ZFS_COPIES":      "2",
			"SPINIFEX_ZFS_ARC_MAX_MIB": "4096",
			"SPINIFEX_ZFS_COMPRESS":    " lz4 ",
			"SPINIFEX_ZFS_CHECKSUM":    "blake3",
		})
		got, err := parseZFSOpts()
		if err != nil {
			t.Fatalf("parseZFSOpts: %v", err)
		}
		want := install.ZFSOpts{Ashift: 12, Copies: 2, ARCMaxMiB: 4096, Compress: "lz4", Checksum: "blake3"}
		if got != want {
			t.Errorf("opts = %+v, want %+v", got, want)
		}
	})

	for _, env := range []string{"SPINIFEX_ZFS_ASHIFT", "SPINIFEX_ZFS_COPIES", "SPINIFEX_ZFS_ARC_MAX_MIB"} {
		t.Run("unparsable "+env, func(t *testing.T) {
			setEnv(t, map[string]string{env: "auto"})
			if _, err := parseZFSOpts(); err == nil || !strings.Contains(err.Error(), env) {
				t.Fatalf("err = %v, want it to name %s", err, env)
			}
		})
	}
}

func TestResolveStorage(t *testing.T) {
	four := []install.Disk{
		mkDisk("/dev/vdb", 40*gib), mkDisk("/dev/vdc", 40*gib),
		mkDisk("/dev/vdd", 40*gib), mkDisk("/dev/vde", 40*gib),
	}

	tests := []struct {
		name      string
		disks     []install.Disk
		env       map[string]string
		wantFS    install.FSType
		wantPaths []string
		wantErr   string
	}{
		{
			name:      "ext4 single disk by default",
			disks:     four[:1],
			wantFS:    install.FSExt4,
			wantPaths: []string{"/dev/vdb"},
		},
		{
			name:      "explicit member list preserves order",
			disks:     four,
			env:       map[string]string{"SPINIFEX_FS": "zfs-raid10", "SPINIFEX_DISKS": "vde,/dev/vdd,vdc, vdb "},
			wantFS:    install.FSZFSRAID10,
			wantPaths: []string{"/dev/vde", "/dev/vdd", "/dev/vdc", "/dev/vdb"},
		},
		{
			name:    "unknown filesystem",
			disks:   four,
			env:     map[string]string{"SPINIFEX_FS": "btrfs"},
			wantErr: "unknown filesystem",
		},
		{
			// Choosing which disks to erase is not an unattended decision.
			name:    "multi-disk pool without an explicit list",
			disks:   four,
			env:     map[string]string{"SPINIFEX_FS": "zfs-raid1"},
			wantErr: "set SPINIFEX_DISKS to an explicit comma-separated list",
		},
		{
			name:    "member that is not present",
			disks:   four[:1],
			env:     map[string]string{"SPINIFEX_DISKS": "vdz"},
			wantErr: `"vdz" is not an available disk`,
		},
		{
			name:    "member list that fails validation",
			disks:   four,
			env:     map[string]string{"SPINIFEX_DISKS": "vdb,vdc"},
			wantErr: "supports a single disk only",
		},
		{
			name:    "bad zfs tunable",
			disks:   four[:1],
			env:     map[string]string{"SPINIFEX_ZFS_ASHIFT": "big"},
			wantErr: "SPINIFEX_ZFS_ASHIFT",
		},
		{
			name:    "scan failure",
			disks:   nil,
			env:     map[string]string{"SPINIFEX_DISKS": "vdb"},
			wantErr: "sysfs unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var scanErr error
			if tt.disks == nil {
				scanErr = errors.New("sysfs unavailable")
			}
			withDisks(t, tt.disks, scanErr)
			setEnv(t, tt.env)

			got, err := resolveStorage()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveStorage: %v", err)
			}
			if got.FS != tt.wantFS {
				t.Errorf("FS = %v, want %v", got.FS, tt.wantFS)
			}
			if !slices.Equal(got.Paths(), tt.wantPaths) {
				t.Errorf("Paths = %v, want %v", got.Paths(), tt.wantPaths)
			}
		})
	}
}

func TestResolveDisk(t *testing.T) {
	small := mkDisk("/dev/vdb", 40*gib)
	large := mkDisk("/dev/vdc", 400*gib)
	nvme := mkDisk("/dev/nvme0n1", 200*gib)

	tests := []struct {
		name     string
		disks    []install.Disk
		target   string
		wantPath string
		wantErr  string
	}{
		{name: "auto with one candidate", disks: []install.Disk{small}, target: "auto", wantPath: "/dev/vdb"},
		{name: "empty target is auto", disks: []install.Disk{small}, wantPath: "/dev/vdb"},
		{
			name:    "auto with several candidates",
			disks:   []install.Disk{small, large},
			target:  "auto",
			wantErr: "expected exactly one disk, found 2",
		},
		{name: "largest", disks: []install.Disk{small, large, nvme}, target: "LARGEST", wantPath: "/dev/vdc"},
		{name: "smallest", disks: []install.Disk{large, small, nvme}, target: " smallest ", wantPath: "/dev/vdb"},
		{name: "nvme", disks: []install.Disk{small, large, nvme}, target: "nvme", wantPath: "/dev/nvme0n1"},
		{
			name:    "nvme with none present",
			disks:   []install.Disk{small, large},
			target:  "nvme",
			wantErr: "expected exactly one NVMe disk, found 0",
		},
		{name: "exact path", disks: []install.Disk{small, large}, target: "/dev/vdc", wantPath: "/dev/vdc"},
		{
			name:    "path that is not present",
			disks:   []install.Disk{small},
			target:  "/dev/vdz",
			wantErr: "is not an available disk",
		},
		{name: "no candidates at all", disks: []install.Disk{}, target: "largest", wantErr: "no non-removable disks found"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withDisks(t, tt.disks, nil)

			got, err := resolveDisk(tt.target)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveDisk(%q): %v", tt.target, err)
			}
			if got.Path != tt.wantPath {
				t.Errorf("path = %q, want %q", got.Path, tt.wantPath)
			}
		})
	}
}

func TestCandidateDisksExcludesRemovableAndLiveMedia(t *testing.T) {
	fixed := mkDisk("/dev/vdb", 40*gib)
	usb := mkDisk("/dev/sdz", 16*gib)
	usb.Removable = true
	boot := mkDisk("/dev/sdy", 16*gib)
	boot.LiveMedia = true
	withDisks(t, []install.Disk{usb, fixed, boot}, nil)

	got, err := candidateDisks()
	if err != nil {
		t.Fatalf("candidateDisks: %v", err)
	}
	if len(got) != 1 || got[0].Path != "/dev/vdb" {
		t.Fatalf("candidates = %v, want only /dev/vdb", got)
	}
}

func TestCandidateDisksPropagatesScanError(t *testing.T) {
	withDisks(t, nil, errors.New("read /sys/block: denied"))
	if _, err := candidateDisks(); err == nil {
		t.Fatal("expected the scan error to propagate")
	}
}

func TestPickBySizeRejectsEmptySet(t *testing.T) {
	if _, err := pickBySize(nil, true); err == nil {
		t.Fatal("expected an error for an empty disk set")
	}
}

func TestPickBySizeLeavesInputOrderIntact(t *testing.T) {
	disks := []install.Disk{mkDisk("/dev/vdb", 40*gib), mkDisk("/dev/vdc", 400*gib)}
	if _, err := pickBySize(disks, false); err != nil {
		t.Fatalf("pickBySize: %v", err)
	}
	if disks[0].Path != "/dev/vdb" {
		t.Errorf("input reordered: %v", disks)
	}
}

func TestDiskList(t *testing.T) {
	got := diskList([]install.Disk{mkDisk("/dev/vdb", 40*gib), mkDisk("/dev/vdc", 2048*gib)})
	want := "  /dev/vdb (40.0G, empty)\n  /dev/vdc (2.0T, empty)"
	if got != want {
		t.Errorf("diskList =\n%s\nwant\n%s", got, want)
	}
}

func TestResolveNICExplicitName(t *testing.T) {
	got, err := resolveNIC("eno1", "")
	if err != nil {
		t.Fatalf("resolveNIC: %v", err)
	}
	if got != "eno1" {
		t.Errorf("nic = %q, want eno1", got)
	}
}

// The auto path reads the host's real interfaces, so assert the selection rules
// rather than a specific name.
func TestResolveNICAutoSkipsVirtualInterfaces(t *testing.T) {
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("no interface list on this host: %v", err)
	}

	got, err := resolveNIC("auto", "")
	if err != nil {
		if len(ifaces) > 0 {
			t.Logf("no physical NIC among %d interfaces: %v", len(ifaces), err)
		}
		return
	}

	idx := slices.IndexFunc(ifaces, func(i net.Interface) bool { return i.Name == got })
	if idx < 0 {
		t.Fatalf("resolveNIC returned %q, which is not a host interface", got)
	}
	sel := ifaces[idx]
	if sel.Flags&net.FlagLoopback != 0 || sel.Flags&net.FlagBroadcast == 0 || len(sel.HardwareAddr) == 0 {
		t.Errorf("selected %q with flags %v and MAC %q", got, sel.Flags, sel.HardwareAddr)
	}
	for _, pfx := range virtualNICPrefixes {
		if strings.HasPrefix(got, pfx) {
			t.Errorf("selected virtual interface %q (prefix %q)", got, pfx)
		}
	}
}

func TestResolveNICAutoHonoursExclude(t *testing.T) {
	first, err := resolveNIC("auto", "")
	if err != nil {
		t.Skipf("no physical NIC on this host: %v", err)
	}
	got, err := resolveNIC("", first)
	if err == nil && got == first {
		t.Errorf("resolveNIC returned the excluded interface %q", first)
	}
}
