package ui

import (
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/cmd/installer/install"
	"github.com/mulgadc/spinifex/cmd/installer/netprobe"
)

func nics(n int) []netprobe.NIC {
	out := make([]netprobe.NIC, 0, n)
	names := []string{"eno1", "ens1f0np0", "ens1f1np1", "ens2f0np0"}
	for i := range n {
		out = append(out, netprobe.NIC{Name: names[i], Speed: "25Gbps", Vendor: "Mellanox", State: "online"})
	}
	return out
}

// The pre-fill is the whole of the simple path: an operator with a
// conventional node should be able to press Continue without editing anything.
func TestNewModelPrefillsRolesByNICCount(t *testing.T) {
	tests := []struct {
		name             string
		count            int
		wantLAN, wantVPC int
	}{
		{"one NIC folds everything onto wan", 1, foldedNIC, foldedNIC},
		{"two NICs dedicate the second to lan, vpc folds", 2, 1, foldedNIC},
		{"three NICs give each plane its own", 3, 1, 2},
		{"four NICs still only bind three", 4, 1, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel([]install.Disk{{Path: "/dev/sda"}}, nics(tt.count))

			if got := m.roles[0].nic; got != 0 {
				t.Errorf("wan nic = %d, want 0", got)
			}
			if got := m.roles[1].nic; got != tt.wantLAN {
				t.Errorf("lan nic = %d, want %d", got, tt.wantLAN)
			}
			if got := m.roles[2].nic; got != tt.wantVPC {
				t.Errorf("vpc nic = %d, want %d", got, tt.wantVPC)
			}
		})
	}
}

// Hiding rather than disabling is what keeps the default screen narrow.
func TestVisibleFields(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*roleForm)
		isWiFi   bool
		advanced bool
		want     []roleField
	}{
		{
			name:   "wan on DHCP asks two questions",
			mutate: func(f *roleForm) { f.dhcp = true },
			want:   []roleField{roleFieldNIC, roleFieldMethod},
		},
		{
			name:   "wan static adds a gateway",
			mutate: func(f *roleForm) { f.dhcp = false },
			want:   []roleField{roleFieldNIC, roleFieldMethod, roleFieldIP, roleFieldMask, roleFieldGateway, roleFieldDNS},
		},
		{
			name:     "advanced reveals VLAN and MTU",
			mutate:   func(f *roleForm) { f.dhcp = true },
			advanced: true,
			want:     []roleField{roleFieldNIC, roleFieldMethod, roleFieldVLAN, roleFieldMTU},
		},
		{
			name:   "wireless adds credentials",
			mutate: func(f *roleForm) { f.dhcp = true },
			isWiFi: true,
			want:   []roleField{roleFieldNIC, roleFieldMethod, roleFieldSSID, roleFieldWiFiPass},
		},
		{
			name:   "a folded role has nothing to configure",
			mutate: func(f *roleForm) { f.nic = foldedNIC },
			want:   []roleField{roleFieldNIC},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRoleForm(install.PlaneWAN, 0)
			tt.mutate(&f)
			got := f.visibleFields(tt.isWiFi, tt.advanced)

			if len(got) != len(tt.want) {
				t.Fatalf("visibleFields() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("visibleFields() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// A non-wan role must be able to reach the folded state, and wan must not.
func TestCycleNICFoldReachableOnlyOffWAN(t *testing.T) {
	m := newModel([]install.Disk{{Path: "/dev/sda"}}, nics(2))

	// lan: 1 -> 0 -> folded -> wraps to the last NIC.
	m.roles[1].nic = 1
	for _, want := range []int{0, foldedNIC, 1} {
		m = m.cycleNIC(1, false)
		if m.roles[1].nic != want {
			t.Fatalf("lan nic = %d, want %d", m.roles[1].nic, want)
		}
	}

	// wan cycles only through real interfaces.
	m.roles[0].nic = 0
	for range 4 {
		m = m.cycleNIC(0, false)
		if m.roles[0].nic == foldedNIC {
			t.Fatal("wan reached the folded state, but it always needs an uplink")
		}
	}
}

func TestToRoleReadsVLANAndMTU(t *testing.T) {
	f := newRoleForm(install.PlaneLAN, 0)
	f.dhcp = false
	f.ip.SetValue("10.0.0.2")
	f.mask.SetValue("255.255.0.0")
	f.vlan.SetValue("10")
	f.mtu.SetValue("9000")

	role := f.toRole(nics(1))

	if role.VLAN != 10 || role.MTU != 9000 {
		t.Errorf("VLAN/MTU = %d/%d, want 10/9000", role.VLAN, role.MTU)
	}
	if role.Link() != "eno1.10" {
		t.Errorf("Link() = %q, want eno1.10", role.Link())
	}
	// Only wan carries a default route, even if a gateway was typed earlier.
	f.gateway.SetValue("10.0.0.1")
	if got := f.toRole(nics(1)).Gateway; got != "" {
		t.Errorf("lan Gateway = %q, want empty", got)
	}
}

func TestToRoleFoldedIsEmpty(t *testing.T) {
	f := newRoleForm(install.PlaneVPC, foldedNIC)
	if !f.toRole(nics(2)).Folded() {
		t.Error("a folded form must produce a folded role")
	}
}

func TestValidateRole(t *testing.T) {
	tests := []struct {
		name    string
		plane   install.Plane
		mutate  func(*roleForm)
		wantErr string
	}{
		{"valid static lan", install.PlaneLAN, func(f *roleForm) {
			f.dhcp = false
			f.ip.SetValue("10.0.0.2")
			f.mask.SetValue("255.255.0.0")
		}, ""},
		{"bad ip", install.PlaneLAN, func(f *roleForm) {
			f.dhcp = false
			f.ip.SetValue("not-an-ip")
			f.mask.SetValue("255.255.0.0")
		}, "valid IP"},
		{"wan needs a gateway", install.PlaneWAN, func(f *roleForm) {
			f.dhcp = false
			f.ip.SetValue("216.218.163.99")
			f.mask.SetValue("255.255.255.224")
		}, "gateway"},
		{"VLAN out of range", install.PlaneLAN, func(f *roleForm) {
			f.dhcp = true
			f.vlan.SetValue("9000")
		}, "VLAN id must be"},
		{"MTU out of range", install.PlaneLAN, func(f *roleForm) {
			f.dhcp = true
			f.mtu.SetValue("70000")
		}, "MTU must be"},
		{"folded roles skip validation", install.PlaneVPC, func(f *roleForm) {
			f.nic = foldedNIC
			f.ip.SetValue("nonsense")
		}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel([]install.Disk{{Path: "/dev/sda"}}, nics(3))
			m.roles[0] = newRoleForm(tt.plane, 0)
			tt.mutate(&m.roles[0])

			got := m.validateRole(0)
			if tt.wantErr == "" {
				if got != "" {
					t.Fatalf("validateRole() = %q, want no error", got)
				}
				return
			}
			if !strings.Contains(got, tt.wantErr) {
				t.Fatalf("validateRole() = %q, want it to mention %q", got, tt.wantErr)
			}
		})
	}
}

// Continue must refuse a layout install.Config.Validate would reject, rather
// than failing later during the install itself.
func TestContinueBlocksOnSharedNICWithoutVLANs(t *testing.T) {
	m := newModel([]install.Disk{{Path: "/dev/sda"}}, nics(2))
	m.screen = screenNetworkRoles
	m.roleCursor = continueRow
	m.roles[1].nic = 0 // lan onto the wan NIC, both untagged

	next, _ := m.handleRolesKey("enter")
	got := next.(model)

	if got.screen != screenNetworkRoles {
		t.Error("expected to stay on the roles screen when the layout is invalid")
	}
	if !strings.Contains(got.validationErr, "both bind eno1") {
		t.Errorf("validationErr = %q, want it to name the shared interface", got.validationErr)
	}

	// Distinct VLAN ids make the same pairing legal.
	got.roles[0].vlan.SetValue("30")
	got.roles[1].vlan.SetValue("10")
	next, _ = got.handleRolesKey("enter")
	if next.(model).screen != screenIdentity {
		t.Error("distinct VLAN ids should allow two roles to share one NIC")
	}
}

func TestAdvancedToggle(t *testing.T) {
	m := newModel([]install.Disk{{Path: "/dev/sda"}}, nics(3))
	m.screen = screenNetworkRoles

	next, _ := m.handleRolesKey("a")
	if !next.(model).advanced {
		t.Fatal("'a' should turn Advanced on")
	}
	next, _ = next.(model).handleRolesKey("a")
	if next.(model).advanced {
		t.Fatal("'a' should turn Advanced back off")
	}
}

// The default route and the resolvers are node-level, and wan is the only plane
// that carries either, so the east-west editors must not ask for them. Their
// bridges are link-local and activate manually — a resolver pinned there is
// unreachable, and a gateway would fight the real default route.
func TestVisibleFieldsOmitsRoutingOffWAN(t *testing.T) {
	for _, plane := range []install.Plane{install.PlaneLAN, install.PlaneVPC} {
		t.Run(string(plane), func(t *testing.T) {
			f := newRoleForm(plane, 1)
			f.dhcp = false
			got := f.visibleFields(false, false)

			want := []roleField{roleFieldNIC, roleFieldMethod, roleFieldIP, roleFieldMask}
			if len(got) != len(want) {
				t.Fatalf("visibleFields() = %v, want %v", got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("visibleFields() = %v, want %v", got, want)
				}
			}
		})
	}
}

// The mask field must accept only what netgen can consume. It previously
// advertised and accepted a prefix length, which passed the TUI and then failed
// the install itself with "invalid mask: 24".
func TestValidSubnetMaskRejectsPrefixLength(t *testing.T) {
	for _, bad := range []string{"24", "/24", "8", "", "255.255.0", "255.0.255.0", "::1"} {
		if validSubnetMask(bad) {
			t.Errorf("validSubnetMask(%q) = true, want false", bad)
		}
	}
	for _, good := range []string{"255.255.255.0", "255.255.0.0", "255.0.0.0", "255.255.255.252"} {
		if !validSubnetMask(good) {
			t.Errorf("validSubnetMask(%q) = false, want true", good)
		}
	}
}

// The hardware detail is the point of the NIC table: an operator has to be able
// to tell which physical port each plane landed on. It gets a line of its own,
// under the interface column, rather than being squeezed into the row.
func TestRoleHardwareRendersOnItsOwnLine(t *testing.T) {
	m := newModel([]install.Disk{{Path: "/dev/sda"}}, []netprobe.NIC{{
		Name: "ens1f0np0", Vendor: "Mellanox Technologies", Model: "MT27710",
		AltName: "enp132s0f0np0", Speed: "25 Gbps", Carrier: true, State: "online",
	}})

	view := m.viewNetworkRoles(100)
	if !strings.Contains(view, "ens1f0np0  25 Gbps  online") {
		t.Errorf("expected the identity line in:\n%s", view)
	}
	detail := m.roleHardware(0, bodyWidth(100))
	if !strings.Contains(detail, "Mellanox Technologies MT27710") {
		t.Errorf("roleHardware = %q, want the vendor and model", detail)
	}
	if !strings.Contains(detail, "enp132s0f0np0") {
		t.Errorf("roleHardware = %q, want the alternative name", detail)
	}
	// Every line has to fit the box, or lipgloss wraps it and the columns shear.
	for line := range strings.SplitSeq(view, "\n") {
		if len([]rune(line)) > 100 {
			t.Errorf("line exceeds the terminal width: %q", line)
		}
	}
}

// A narrow box drops the alternative name before it truncates the model, since
// half a model name identifies nothing.
func TestNICHardwareDropsAltNameBeforeTruncating(t *testing.T) {
	n := netprobe.NIC{Vendor: "Broadcom Inc. and subsidiaries", Model: "NetXtreme BCM5720", AltName: "enp1s0f1"}

	if got := nicHardware(n, 60); got != "Broadcom Inc. and subsidiaries NetXtreme BCM5720" {
		t.Errorf("nicHardware(60) = %q, want the name without the alt name", got)
	}
	if got := nicHardware(n, 20); got != "Broadcom Inc. and s…" {
		t.Errorf("nicHardware(20) = %q, want a marked truncation", got)
	}
}

// A role bound to a port with no hardware database entry still gets something
// to identify the card by, never the bare word "unknown".
func TestRoleHardwareFallsBackToDriver(t *testing.T) {
	m := newModel([]install.Disk{{Path: "/dev/sda"}}, []netprobe.NIC{{
		Name: "eno1", Driver: "bnxt_en", DeviceID: "14e4:165f", State: "no cable",
	}})

	if got := m.roleHardware(0, bodyWidth(100)); !strings.Contains(got, "bnxt_en [14e4:165f]") {
		t.Errorf("roleHardware = %q, want the driver and device id", got)
	}
}

// A folded role has no interface, so it must not render an orphan detail line
// under the plane it collapsed onto.
func TestRoleHardwareEmptyWhenFolded(t *testing.T) {
	m := newModel([]install.Disk{{Path: "/dev/sda"}}, nics(1))
	if got := m.roleHardware(1, bodyWidth(100)); got != "" {
		t.Errorf("roleHardware for a folded role = %q, want empty", got)
	}
}
