//test:in-package — the wizard model, its screen constants and the key handlers
//under test are all unexported, and the tests read model fields directly to
//assert which screen a key sequence landed on.

package ui

import (
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mulgadc/spinifex/cmd/installer/install"
)

// namedKeys maps the key names the handlers switch on to their tea.KeyType.
// Anything not listed is sent as runes.
var namedKeys = map[string]tea.KeyType{
	"enter":     tea.KeyEnter,
	"esc":       tea.KeyEsc,
	"tab":       tea.KeyTab,
	"shift+tab": tea.KeyShiftTab,
	"up":        tea.KeyUp,
	"down":      tea.KeyDown,
	"left":      tea.KeyLeft,
	"right":     tea.KeyRight,
	" ":         tea.KeySpace,
	"backspace": tea.KeyBackspace,
	"ctrl+c":    tea.KeyCtrlC,
}

// keyMsg builds the tea.KeyMsg for a key name and checks it round-trips, so a
// mis-built message fails here rather than silently exercising the wrong branch.
func keyMsg(t *testing.T, key string) tea.KeyMsg {
	t.Helper()
	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	if kt, ok := namedKeys[key]; ok {
		msg = tea.KeyMsg{Type: kt}
	}
	if got := msg.String(); got != key {
		t.Fatalf("built key %q, which the handlers see as %q", key, got)
	}
	return msg
}

// press feeds keys through Update, which is the only entry point the bubbletea
// runtime uses, and returns the resulting model.
func press(t *testing.T, m model, keys ...string) model {
	t.Helper()
	for _, k := range keys {
		next, _ := m.Update(keyMsg(t, k))
		typed, ok := next.(model)
		if !ok {
			t.Fatalf("Update returned %T, not a model", next)
		}
		m = typed
	}
	return m
}

// typeText sends a string one rune at a time, the way a terminal does.
func typeText(t *testing.T, m model, s string) model {
	t.Helper()
	for _, r := range s {
		m = press(t, m, string(r))
	}
	return m
}

func wizardModel(t *testing.T, diskCount, nicCount int) model {
	t.Helper()
	disks := make([]install.Disk, 0, diskCount)
	for i := range diskCount {
		disks = append(disks, install.Disk{
			Path:             "/dev/sd" + string(rune('a'+i)),
			Bytes:            200 << 30,
			LogicalBlockSize: 512,
		})
	}
	m := newModel(disks, nics(nicCount))
	m.width, m.height = 100, 40
	return m
}

// The wizard has to be completable from the welcome screen using only the keys
// the help lines advertise, and produce the config the operator entered.
func TestWizardWalkthroughProducesTheEnteredConfig(t *testing.T) {
	m := wizardModel(t, 1, 2)

	m = press(t, m, "enter") // welcome → disk
	if m.screen != screenDisk {
		t.Fatalf("screen = %v, want the disk screen", m.screen)
	}

	m = press(t, m, "enter") // preselected disk is valid → erase confirmation
	if m.screen != screenDiskConfirm {
		t.Fatalf("screen = %v, want the erase confirmation", m.screen)
	}

	m = typeText(t, m, "yes")
	m = press(t, m, "enter") // → network roles
	if m.screen != screenNetworkRoles {
		t.Fatalf("screen = %v, want the network roles screen", m.screen)
	}

	m = press(t, m, "down", "down", "down", "enter") // Continue → identity
	if m.screen != screenIdentity {
		t.Fatalf("screen = %v, want the identity screen", m.screen)
	}

	m = typeText(t, m, "node1")
	m = press(t, m, "enter") // → credentials
	if m.screen != screenPassword {
		t.Fatalf("screen = %v, want the credentials screen", m.screen)
	}

	m = typeText(t, m, "admin@example.com")
	m = press(t, m, "enter") // email accepted → password
	m = typeText(t, m, "hunter2hunter2")
	m = press(t, m, "enter") // → confirm password
	m = typeText(t, m, "hunter2hunter2")
	m = press(t, m, "enter") // → final confirmation
	if m.screen != screenConfirm {
		t.Fatalf("screen = %v, want the final confirmation (err: %q)", m.screen, m.validationErr)
	}

	m = press(t, m, "y")
	if m.screen != screenDone {
		t.Fatalf("screen = %v, want done", m.screen)
	}
	if m.result == nil {
		t.Fatal("result is nil after confirming")
	}

	cfg := m.result
	if cfg.Hostname != "node1" {
		t.Errorf("Hostname = %q, want node1", cfg.Hostname)
	}
	if cfg.Email != "admin@example.com" {
		t.Errorf("Email = %q, want admin@example.com", cfg.Email)
	}
	if cfg.RootPassword != "hunter2hunter2" {
		t.Errorf("RootPassword = %q, want what was typed", cfg.RootPassword)
	}
	if !slices.Equal(cfg.Storage.Paths(), []string{"/dev/sda"}) {
		t.Errorf("Storage.Paths() = %v, want the preselected disk", cfg.Storage.Paths())
	}
	if cfg.WAN.Interface != "eno1" {
		t.Errorf("WAN interface = %q, want the first NIC", cfg.WAN.Interface)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the wizard produced a config it would refuse to install: %v", err)
	}
}

// Esc has to walk back the way it came, or an operator who overshoots is stuck
// with a wizard they can only cancel out of.
func TestEscapeWalksBackThroughTheScreens(t *testing.T) {
	m := wizardModel(t, 1, 2)
	m = press(t, m, "enter") // disk
	m = press(t, m, "enter") // erase confirmation

	for _, step := range []struct {
		key  string
		want screen
	}{
		{"esc", screenDisk},
		{"esc", screenWelcome},
	} {
		m = press(t, m, step.key)
		if m.screen != step.want {
			t.Fatalf("after esc: screen = %v, want %v", m.screen, step.want)
		}
	}
}

// Ctrl+C is handled before any screen sees the key, so it works everywhere.
func TestCtrlCCancelsWithAnError(t *testing.T) {
	m := wizardModel(t, 1, 1)
	next, cmd := m.Update(keyMsg(t, "ctrl+c"))
	final, ok := next.(model)
	if !ok {
		t.Fatalf("Update returned %T", next)
	}
	if final.err == nil || !strings.Contains(final.err.Error(), "cancelled") {
		t.Fatalf("err = %v, want a cancellation", final.err)
	}
	if cmd == nil {
		t.Error("ctrl+c must return tea.Quit")
	}
}

// A window resize has to reach the model, since every view lays out against it.
func TestWindowResizeIsStored(t *testing.T) {
	m := model{}
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 50})
	resized, ok := next.(model)
	if !ok {
		t.Fatalf("Update returned %T", next)
	}
	if resized.width != 120 || resized.height != 50 {
		t.Errorf("size = %dx%d, want 120x50", resized.width, resized.height)
	}
}

// The erase prompt is the last point of no return, so anything other than an
// explicit "yes" has to keep the operator on it.
func TestEraseConfirmationRequiresYes(t *testing.T) {
	m := wizardModel(t, 1, 1)
	m = press(t, m, "enter", "enter") // → erase confirmation

	m = typeText(t, m, "no")
	m = press(t, m, "enter")
	if m.screen != screenDiskConfirm {
		t.Fatalf("screen = %v, want to stay on the confirmation", m.screen)
	}
	if !strings.Contains(m.validationErr, "yes") {
		t.Errorf("validationErr = %q, want it to say what to type", m.validationErr)
	}
	if !strings.Contains(m.viewDiskConfirm(100), m.validationErr) {
		t.Error("the confirmation screen must show the error it set")
	}
}

// The filesystem selector and the disk list share one screen, so the cursor has
// to move between them and stop at both ends.
func TestDiskScreenCursorStaysInRange(t *testing.T) {
	m := wizardModel(t, 2, 1)
	m.screen = screenDisk

	m = press(t, m, "up", "up")
	if m.storageCursor != fsRow {
		t.Errorf("storageCursor = %d, want it clamped to the filesystem row", m.storageCursor)
	}

	m = press(t, m, "down", "down", "down", "down")
	if m.storageCursor != len(m.disks) {
		t.Errorf("storageCursor = %d, want it clamped to the last disk", m.storageCursor)
	}
}

// Left/right only cycle the filesystem while the cursor is on that row, and
// they wrap, so one key reaches every topology.
func TestFilesystemCyclesOnlyOnItsOwnRow(t *testing.T) {
	m := wizardModel(t, 2, 1)
	m.screen = screenDisk

	m = press(t, m, "left")
	if got := m.fsType(); got != install.AllFSTypes[len(install.AllFSTypes)-1] {
		t.Errorf("fsType = %v, want the cycle to wrap backwards to the last", got)
	}
	m = press(t, m, "right")
	if got := m.fsType(); got != install.AllFSTypes[0] {
		t.Errorf("fsType = %v, want the cycle to wrap forwards to the first", got)
	}

	m = press(t, m, "down") // onto the first disk
	before := m.fsType()
	m = press(t, m, "left", "right")
	if m.fsType() != before {
		t.Errorf("fsType changed to %v while the cursor was on a disk", m.fsType())
	}
}

// Space toggles the disk under the cursor, and r cycles its role — the two keys
// the storage screen exists for.
func TestDiskScreenSelectionKeys(t *testing.T) {
	m := wizardModel(t, 2, 1)
	m.screen = screenDisk

	// The second disk is unselected; the first arrives preselected as os.
	m = press(t, m, "down", "down", " ")
	if !slices.Contains(m.selected, 1) {
		t.Fatalf("selected = %v, want the second disk added", m.selected)
	}

	roleBefore := m.roleOf[slices.Index(m.selected, 1)]
	m = press(t, m, "r")
	if got := m.roleOf[slices.Index(m.selected, 1)]; got == roleBefore {
		t.Errorf("role stayed %q after r", got)
	}

	m = press(t, m, " ")
	if slices.Contains(m.selected, 1) {
		t.Fatalf("selected = %v, want the second disk removed again", m.selected)
	}
	if len(m.roleOf) != len(m.selected) {
		t.Errorf("roleOf (%d) and selected (%d) drifted", len(m.roleOf), len(m.selected))
	}
}

// A selection that cannot be installed has to be reported on the screen the
// operator can still fix it on, not part-way through the install.
func TestDiskScreenRefusesAnInvalidSelection(t *testing.T) {
	m := wizardModel(t, 2, 1)
	m.screen = screenDisk
	m.fsCursor = slices.Index(install.AllFSTypes, install.FSZFSRAID1) // needs 2 disks

	m = press(t, m, "enter")
	if m.screen != screenDisk {
		t.Fatalf("screen = %v, want to stay on the disk screen", m.screen)
	}
	if m.validationErr == "" {
		t.Fatal("an invalid selection must set a validation error")
	}
	if !strings.Contains(m.viewDisk(100), m.validationErr) {
		t.Error("the disk screen must render the error it set")
	}

	// Selecting the second disk satisfies the mirror and clears the way.
	m = press(t, m, "down", "down", " ", "enter")
	if m.screen != screenDiskConfirm {
		t.Fatalf("screen = %v, want to advance once the mirror has two disks", m.screen)
	}
}

// The ZFS options screen is only reachable on a pool, since its settings have
// no meaning on ext4.
func TestZFSOptionsScreenIsZFSOnly(t *testing.T) {
	m := wizardModel(t, 2, 1)
	m.screen = screenDisk

	m = press(t, m, "a")
	if m.screen != screenDisk {
		t.Fatalf("screen = %v, want a to do nothing on ext4", m.screen)
	}

	m.fsCursor = slices.Index(install.AllFSTypes, install.FSZFSRAID1)
	m = press(t, m, "a")
	if m.screen != screenZFSOptions {
		t.Fatalf("screen = %v, want the ZFS options screen", m.screen)
	}
}

// Every option defaults to auto and cycles through its own choice list, with
// the cursor clamped to the rows that exist.
func TestZFSOptionsCycleTheirValues(t *testing.T) {
	m := wizardModel(t, 2, 1)
	m.screen = screenZFSOptions

	if got := zfsOptions[0].value(&m.zfs); got != "auto" {
		t.Errorf("ashift starts at %q, want auto", got)
	}
	m = press(t, m, "right")
	if got := zfsOptions[0].value(&m.zfs); got == "auto" {
		t.Error("right must move ashift off auto")
	}
	m = press(t, m, "left")
	if got := zfsOptions[0].value(&m.zfs); got != "auto" {
		t.Errorf("ashift = %q after cycling back, want auto", got)
	}

	m = press(t, m, "up", "up")
	if m.zfsCursor != 0 {
		t.Errorf("zfsCursor = %d, want it clamped to the first row", m.zfsCursor)
	}
	for range len(zfsOptions) + 2 {
		m = press(t, m, "down")
	}
	if m.zfsCursor != len(zfsOptions)-1 {
		t.Errorf("zfsCursor = %d, want it clamped to the last row", m.zfsCursor)
	}

	// The compression choices are strings rather than ints, so they exercise
	// the other half of cycle.
	m.zfsCursor = 1
	m = press(t, m, "right")
	if m.zfs.Compress == "" {
		t.Error("right must set a compression algorithm")
	}

	m = press(t, m, "enter")
	if m.screen != screenDisk {
		t.Fatalf("screen = %v, want enter to return to the disk screen", m.screen)
	}
}

// The screen has to show what each option is currently set to, since that is
// the only place the pool's geometry is stated before it is created.
func TestZFSOptionsViewShowsValuesAndTheFocusedNote(t *testing.T) {
	m := wizardModel(t, 2, 1)
	m.screen = screenZFSOptions
	m.zfs.Compress = "zstd"

	got := m.viewZFSOptions(100)
	for _, want := range []string{"ashift", "auto", "compression", "zstd", zfsOptions[0].note} {
		if !strings.Contains(got, want) {
			t.Errorf("ZFS options screen is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, zfsOptions[1].note) {
		t.Error("only the focused row's note should be shown")
	}
}

// Credentials are three fields on one screen, so focus has to cycle both ways
// and land somewhere sensible after a validation failure.
func TestCredentialsFocusCyclesBothWays(t *testing.T) {
	m := wizardModel(t, 1, 1)
	m.screen = screenPassword
	m = m.setCredsFocus(0)

	m = press(t, m, "tab")
	if m.credsFocus != 1 || !m.passwordInput.Focused() {
		t.Fatalf("credsFocus = %d, want the password field focused", m.credsFocus)
	}
	m = press(t, m, "tab", "tab")
	if m.credsFocus != 0 || !m.emailInput.Focused() {
		t.Fatalf("credsFocus = %d, want the cycle to wrap to email", m.credsFocus)
	}
	m = press(t, m, "shift+tab")
	if m.credsFocus != 2 || !m.passwordConfirmInput.Focused() {
		t.Fatalf("credsFocus = %d, want shift+tab to wrap backwards to confirm", m.credsFocus)
	}
}

// Each credential rule has to stop the wizard on the screen and put the cursor
// on the field at fault, not just refuse to advance.
func TestCredentialValidation(t *testing.T) {
	tests := []struct {
		name       string
		email      string
		password   string
		confirm    string
		wantErr    string
		wantFocus  int
		wantScreen screen
	}{
		{
			name: "rejects a malformed email", email: "not-an-email",
			password: "hunter2hunter2", confirm: "hunter2hunter2",
			wantErr: "email", wantFocus: 0, wantScreen: screenPassword,
		},
		{
			name: "rejects an empty password", email: "admin@example.com",
			wantErr: "Password is required", wantFocus: 1, wantScreen: screenPassword,
		},
		{
			name: "rejects a mismatch", email: "admin@example.com",
			password: "hunter2hunter2", confirm: "hunter2hunter3",
			wantErr: "do not match", wantFocus: 2, wantScreen: screenPassword,
		},
		{
			name: "accepts a matching pair", email: "admin@example.com",
			password: "hunter2hunter2", confirm: "hunter2hunter2",
			wantScreen: screenConfirm,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := wizardModel(t, 1, 1)
			m.screen = screenPassword
			m.emailInput.SetValue(tt.email)
			m.passwordInput.SetValue(tt.password)
			m.passwordConfirmInput.SetValue(tt.confirm)
			m = m.setCredsFocus(2) // enter on confirm runs every rule

			m = press(t, m, "enter")

			if m.screen != tt.wantScreen {
				t.Fatalf("screen = %v, want %v (err %q)", m.screen, tt.wantScreen, m.validationErr)
			}
			if tt.wantErr == "" {
				if m.validationErr != "" {
					t.Fatalf("validationErr = %q, want none", m.validationErr)
				}
				return
			}
			if !strings.Contains(strings.ToLower(m.validationErr), strings.ToLower(tt.wantErr)) {
				t.Errorf("validationErr = %q, want it to mention %q", m.validationErr, tt.wantErr)
			}
			if m.credsFocus != tt.wantFocus {
				t.Errorf("credsFocus = %d, want the cursor on field %d", m.credsFocus, tt.wantFocus)
			}
			if !strings.Contains(m.viewPassword(100), m.validationErr) {
				t.Error("the credentials screen must render the error it set")
			}
		})
	}
}

// The hostname ends up in the config and in DNS, so an empty one has to be
// caught on the screen rather than installed.
func TestHostnameIsRequired(t *testing.T) {
	m := wizardModel(t, 1, 1)
	m.screen = screenIdentity
	m.hostnameInput.Focus()

	m = press(t, m, "enter")
	if m.screen != screenIdentity {
		t.Fatalf("screen = %v, want to stay on identity", m.screen)
	}
	if !strings.Contains(m.validationErr, "Hostname") {
		t.Errorf("validationErr = %q, want it to name the field", m.validationErr)
	}
	if !m.hostnameInput.Focused() {
		t.Error("the hostname field must keep focus after a failed submit")
	}
	if !strings.Contains(m.viewIdentity(100), m.validationErr) {
		t.Error("the identity screen must render the error it set")
	}
}

// Tab order follows the visible fields, which change with the addressing mode,
// so it has to be derived from them rather than fixed.
func TestRoleEditorTabsThroughVisibleFieldsOnly(t *testing.T) {
	m := wizardModel(t, 1, 2)
	m.screen = screenNetworkRole
	m.roleCursor = 0 // wan, DHCP by default

	m = press(t, m, "tab")
	if m.roles[0].focus != roleFieldMethod {
		t.Fatalf("focus = %v, want the IP method field", m.roles[0].focus)
	}
	m = press(t, m, "tab")
	if m.roles[0].focus != roleFieldNIC {
		t.Fatalf("focus = %v, want the DHCP form to wrap after two fields", m.roles[0].focus)
	}

	// Switching to static reveals the address fields, and tab must reach them.
	m = press(t, m, "tab", "right")
	if m.roles[0].dhcp {
		t.Fatal("right on the method field must select static addressing")
	}
	m = press(t, m, "tab")
	if m.roles[0].focus != roleFieldIP || !m.roles[0].ip.Focused() {
		t.Fatalf("focus = %v, want the IP field focused", m.roles[0].focus)
	}
	m = press(t, m, "shift+tab")
	if m.roles[0].focus != roleFieldMethod {
		t.Fatalf("focus = %v, want shift+tab to go back", m.roles[0].focus)
	}
}

// A focused text field has to receive typed runes, since that is the only way
// an address is entered.
func TestRoleEditorTypesIntoTheFocusedField(t *testing.T) {
	m := wizardModel(t, 1, 2)
	m.screen = screenNetworkRole
	m.roles[0].dhcp = false
	m.roles[0].focus = roleFieldIP
	m.roles[0].focusCurrent()

	m = typeText(t, m, "192.168.1.10")
	if got := m.roles[0].ip.Value(); got != "192.168.1.10" {
		t.Fatalf("ip = %q, want what was typed", got)
	}
	if got := m.roles[0].mask.Value(); got != "" {
		t.Errorf("mask = %q, want the keys to reach only the focused field", got)
	}
}

// The interface selector wraps and offers the fold option on lan and vpc only,
// so a single-NIC node stays configurable with one key.
func TestNICSelectorWrapsAndFoldsWhereAllowed(t *testing.T) {
	m := wizardModel(t, 1, 2)
	m.screen = screenNetworkRole

	// wan cannot fold, so it wraps across the real NICs only.
	m.roleCursor = 0
	m = press(t, m, "left")
	if m.roles[0].nic != len(m.nics)-1 {
		t.Fatalf("wan nic = %d, want it to wrap to the last NIC", m.roles[0].nic)
	}
	m = press(t, m, "right")
	if m.roles[0].nic != 0 {
		t.Fatalf("wan nic = %d, want it to wrap back to the first", m.roles[0].nic)
	}

	// lan may fold, so the cycle includes the unbound position.
	m.roleCursor = 1
	m.roles[1].nic = 0
	m = press(t, m, "left")
	if m.roles[1].nic != foldedNIC {
		t.Fatalf("lan nic = %d, want the fold position", m.roles[1].nic)
	}
	if !m.roles[1].folded() {
		t.Error("the lan role should read as folded")
	}
}

// Per-role validation runs on Enter and keeps the operator on the editor, since
// an invalid address here fails the install after the disk is partitioned.
func TestRoleEditorValidatesBeforeLeaving(t *testing.T) {
	m := wizardModel(t, 1, 2)
	m.screen = screenNetworkRole
	m.roleCursor = 0
	m.roles[0].dhcp = false
	m.roles[0].ip.SetValue("not-an-ip")

	m = press(t, m, "enter")
	if m.screen != screenNetworkRole {
		t.Fatalf("screen = %v, want to stay on the role editor", m.screen)
	}
	if !strings.Contains(m.validationErr, "IP address") {
		t.Errorf("validationErr = %q, want it to name the field", m.validationErr)
	}
	if !strings.Contains(m.viewNetworkRole(100), m.validationErr) {
		t.Error("the role editor must render the error it set")
	}

	// A prefix length is rejected here because netgen parses the mask with
	// net.ParseIP and would fail the install itself.
	m.roles[0].ip.SetValue("192.168.1.10")
	m.roles[0].mask.SetValue("24")
	m = press(t, m, "enter")
	if !strings.Contains(m.validationErr, "dotted-decimal") {
		t.Errorf("validationErr = %q, want the mask rule", m.validationErr)
	}

	m.roles[0].mask.SetValue("255.255.255.0")
	m.roles[0].gateway.SetValue("192.168.1.1")
	m = press(t, m, "enter")
	if m.screen != screenNetworkRoles {
		t.Fatalf("screen = %v, want a valid role to return to the overview (err %q)", m.screen, m.validationErr)
	}
}

// Esc abandons the editor without validating, so a half-typed address is not a
// trap the operator has to finish before backing out.
func TestRoleEditorEscapeSkipsValidation(t *testing.T) {
	m := wizardModel(t, 1, 2)
	m.screen = screenNetworkRole
	m.roles[0].dhcp = false
	m.roles[0].ip.SetValue("192.")

	m = press(t, m, "esc")
	if m.screen != screenNetworkRoles {
		t.Fatalf("screen = %v, want the overview", m.screen)
	}
	if m.validationErr != "" {
		t.Errorf("validationErr = %q, want esc to clear it", m.validationErr)
	}
}

// The editor shows only the fields that apply, so the DHCP form stays two lines
// and Advanced is what reveals VLAN and MTU.
func TestRoleEditorViewShowsTheApplicableFields(t *testing.T) {
	m := wizardModel(t, 1, 3)
	m.screen = screenNetworkRole
	m.roleCursor = 0

	dhcpView := m.viewNetworkRole(100)
	if !strings.Contains(dhcpView, "DHCP") || !strings.Contains(dhcpView, "WAN") {
		t.Errorf("DHCP view is missing the plane or the method:\n%s", dhcpView)
	}
	for _, hidden := range []string{"IP address", "Subnet mask", "VLAN id"} {
		if strings.Contains(dhcpView, hidden) {
			t.Errorf("DHCP view should not show %q:\n%s", hidden, dhcpView)
		}
	}

	m.roles[0].dhcp = false
	m.advanced = true
	staticView := m.viewNetworkRole(100)
	for _, want := range []string{"IP address", "Subnet mask", "Gateway", "DNS", "VLAN id", "MTU"} {
		if !strings.Contains(staticView, want) {
			t.Errorf("static advanced view is missing %q:\n%s", want, staticView)
		}
	}

	// Gateway and DNS belong to the plane holding the default route, so a
	// static lan must not offer them.
	m.roleCursor = 1
	m.roles[1].nic = 1
	m.roles[1].dhcp = false
	lanView := m.viewNetworkRole(100)
	for _, hidden := range []string{"Gateway", "DNS"} {
		if strings.Contains(lanView, hidden) {
			t.Errorf("static lan view should not show %q:\n%s", hidden, lanView)
		}
	}
}

// A folded role names the plane it landed on, on both screens, so the operator
// can see where the traffic actually goes before committing.
func TestFoldedRolesAreNamedNotBlank(t *testing.T) {
	m := wizardModel(t, 1, 1) // one NIC folds lan and vpc onto wan

	overview := m.viewNetworkRoles(100)
	if !strings.Contains(overview, "folds onto") {
		t.Errorf("overview does not state where the folded roles land:\n%s", overview)
	}

	m.screen = screenNetworkRole
	m.roleCursor = 2
	if got := m.viewNetworkRole(100); !strings.Contains(got, "fold onto") {
		t.Errorf("role editor does not offer the fold plainly:\n%s", got)
	}

	m.screen = screenConfirm
	if got := m.viewConfirm(100); !strings.Contains(got, "folded onto") {
		t.Errorf("confirmation does not state the folded planes:\n%s", got)
	}
}

// Continue is refused while the config would fail, and the message is the one
// install.Config.Validate produced.
func TestRolesOverviewRefusesAnInvalidConfig(t *testing.T) {
	m := wizardModel(t, 1, 2)
	m.screen = screenNetworkRoles
	m.roles[1].nic = 0 // lan on the same NIC as wan, no VLAN to separate them
	m.roleCursor = continueRow

	m = press(t, m, "enter")
	if m.screen != screenNetworkRoles {
		t.Fatalf("screen = %v, want to stay on the overview", m.screen)
	}
	if !strings.Contains(m.validationErr, "VLAN") {
		t.Errorf("validationErr = %q, want the shared-NIC rule", m.validationErr)
	}
	// The message is wrapped to the box, so match its opening rather than
	// the whole line.
	if !strings.Contains(m.viewNetworkRoles(100), "both bind eno1") {
		t.Error("the overview must render the error it set")
	}

	// a toggles Advanced for the whole node rather than per role.
	m = press(t, m, "a")
	if !m.advanced {
		t.Error("a must turn Advanced on")
	}
	if !strings.Contains(m.viewNetworkRoles(100), "Advanced: on") {
		t.Error("the overview must show Advanced is on")
	}
}

// The final screen is the last chance to catch a wrong disk or address, so the
// summary has to carry what was entered.
func TestConfirmScreenSummarisesTheEnteredConfig(t *testing.T) {
	m := wizardModel(t, 1, 3)
	m.screen = screenConfirm
	m.hostnameInput.SetValue("node7")
	m.roles[1].dhcp = false
	m.roles[1].ip.SetValue("10.0.0.3")
	m.roles[1].mask.SetValue("255.255.255.0")

	got := m.viewConfirm(100)
	for _, want := range []string{"node7", "/dev/sda", "ext4", "eno1", "10.0.0.3/255.255.255.0", "br-lan", "will erase"} {
		if !strings.Contains(got, want) {
			t.Errorf("confirmation is missing %q:\n%s", want, got)
		}
	}
}

// n on the confirmation cancels rather than installing.
func TestConfirmScreenCancelsOnN(t *testing.T) {
	m := wizardModel(t, 1, 1)
	m.screen = screenConfirm

	m = press(t, m, "n")
	if m.err == nil {
		t.Fatal("n must set the cancellation error")
	}
	if m.result != nil {
		t.Error("a cancelled install must produce no config")
	}
}

// View dispatches on the screen, so each one has to render its own content
// rather than falling through to an empty frame.
func TestViewRendersEveryScreen(t *testing.T) {
	tests := []struct {
		screen screen
		want   string
	}{
		{screenWelcome, "Enter to begin"},
		{screenDisk, "Select Installation Disk"},
		{screenZFSOptions, "ZFS Options"},
		{screenDiskConfirm, "Confirm Disk Erasure"},
		{screenNetworkRoles, "Network Planes"},
		{screenNetworkRole, "Configure WAN plane"},
		{screenIdentity, "Node Identity"},
		{screenPassword, "Administrator Credentials"},
		{screenConfirm, "Confirm Installation"},
		{screenDone, "Installation complete"},
	}

	for _, tt := range tests {
		m := wizardModel(t, 1, 2)
		m.screen = tt.screen
		if got := m.View(); !strings.Contains(got, tt.want) {
			t.Errorf("screen %v does not render %q:\n%s", tt.screen, tt.want, got)
		}
	}
}

// The disk screen has to state what the selection actually builds, since the
// cost of the redundancy choice is otherwise invisible until after the install.
func TestDiskViewStatesTheResultingGeometry(t *testing.T) {
	m := wizardModel(t, 3, 1)
	m.screen = screenDisk

	// ext4 with a role per drive: the mountpoints are the summary that matters.
	m = press(t, m, "down", "down", " ", "down", " ")
	if got := m.viewDisk(100); !strings.Contains(got, "/var/lib/spinifex") {
		t.Errorf("ext4 view does not state the role layout:\n%s", got)
	}
	// Matched spares are suggested, never applied.
	if got := m.viewDisk(100); !strings.Contains(got, "survive a disk failure") {
		t.Errorf("view does not suggest the pool the matched disks allow:\n%s", got)
	}

	// A mirror with too few disks quotes the requirement, not a capacity.
	m = wizardModel(t, 3, 1)
	m.screen = screenDisk
	m.fsCursor = slices.Index(install.AllFSTypes, install.FSZFSRAID1)
	if got := m.viewDisk(100); !strings.Contains(got, "1 selected") {
		t.Errorf("view does not state what the mirror is missing:\n%s", got)
	}

	// Once buildable it quotes usable capacity and the failures tolerated.
	m = press(t, m, "down", "down", " ")
	got := m.viewDisk(100)
	if !strings.Contains(got, "usable") || !strings.Contains(got, "survives 1 disk failure") {
		t.Errorf("view does not quote the pool geometry:\n%s", got)
	}
	if !strings.Contains(got, "A for ZFS options") {
		t.Errorf("the ZFS help line should replace the role key:\n%s", got)
	}
}

// The overview names the card behind each port, which is what distinguishes the
// 25gbe uplink from the onboard 1gbe.
func TestRolesOverviewNamesTheHardware(t *testing.T) {
	m := wizardModel(t, 1, 2)
	m.nics[0].AltName = "enp1s0f0"
	m.advanced = true

	got := m.viewNetworkRoles(100)
	for _, want := range []string{"eno1", "25Gbps", "Mellanox", "enp1s0f0", "VLAN", "MTU"} {
		if !strings.Contains(got, want) {
			t.Errorf("overview is missing %q:\n%s", want, got)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{512, "512B"},
		{2 << 20, "2.0M"},
		{3 << 30, "3.0G"},
		{2 << 40, "2.0T"},
	}
	for _, tt := range tests {
		if got := humanBytes(tt.bytes); got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

// The overview writes an address the way an operator does, and falls back to
// the raw mask rather than dropping it when it is not dotted-decimal.
func TestFormatCIDR(t *testing.T) {
	tests := []struct {
		name       string
		addr, mask string
		want       string
	}{
		{"dotted mask becomes a prefix", "10.0.0.3", "255.255.255.0", "10.0.0.3/24"},
		{"short mask keeps its prefix", "10.0.0.3", "255.255.0.0", "10.0.0.3/16"},
		{"no mask yet", "10.0.0.3", "", "10.0.0.3"},
		{"no address is blank", "", "255.255.255.0", ""},
		{"unparseable mask is shown as typed", "10.0.0.3", "24", "10.0.0.3/24"},
		{"whitespace is trimmed", " 10.0.0.3 ", " 255.255.255.0 ", "10.0.0.3/24"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatCIDR(tt.addr, tt.mask); got != tt.want {
				t.Errorf("formatCIDR(%q, %q) = %q, want %q", tt.addr, tt.mask, got, tt.want)
			}
		})
	}
}

// Messages that are not keys reach the focused input, which is what keeps the
// cursor blinking on the two single-field screens.
func TestNonKeyMessagesReachTheActiveInput(t *testing.T) {
	m := wizardModel(t, 1, 1)
	m.screen = screenIdentity
	m.hostnameInput.Focus()

	next, _ := m.Update(struct{ tea.Msg }{})
	if _, ok := next.(model); !ok {
		t.Fatalf("Update returned %T", next)
	}

	m.screen = screenWelcome
	if _, cmd := m.Update(struct{ tea.Msg }{}); cmd != nil {
		t.Error("a screen with no input must issue no command")
	}
}
