// Package ui presents the interactive installer TUI using bubbletea and lipgloss.
package ui

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mulgadc/spinifex/cmd/installer/branding"
	"github.com/mulgadc/spinifex/cmd/installer/install"
	"github.com/mulgadc/spinifex/cmd/installer/netprobe"
	"github.com/mulgadc/spinifex/spinifex/admin"
)

// screen represents which step of the wizard is active.
type screen int

const (
	screenWelcome screen = iota
	screenDisk
	screenZFSOptions
	screenDiskConfirm
	screenNetworkRoles
	screenNetworkRole
	screenIdentity
	screenPassword
	screenConfirm
	screenDone // signals completion; program exits
)

// model is the top-level bubbletea model for the installer wizard.
type model struct {
	screen screen
	width  int
	height int

	// Storage. The filesystem selector and the disk list share one screen, so
	// storageCursor indexes rows: fsRow, then one row per disk.
	//
	// selected holds indexes into disks in the order they were picked, which is
	// what pairs members for RAID10 — so it is a slice, not a set.
	disks         []install.Disk
	storageCursor int
	selected      []int
	fsCursor      int
	zfsCursor     int
	zfs           install.ZFSOpts
	eraseInput    textinput.Model

	// Detected interfaces, shared by every role screen.
	nics []netprobe.NIC

	// Network planes. roleCursor indexes roles, or continueRow for the
	// Continue action; advanced reveals VLAN and MTU on every role.
	roles      [3]roleForm
	roleCursor int
	advanced   bool

	// Identity
	hostnameInput textinput.Model

	// Credentials (email + password)
	emailInput           textinput.Model
	passwordInput        textinput.Model
	passwordConfirmInput textinput.Model
	credsFocus           int // 0 = email, 1 = password, 2 = confirm

	// Accumulated validation error shown on current screen
	validationErr string

	// Final result — set when screenDone is reached
	result *install.Config
	err    error
}

// Run launches the bubbletea program connected to ttyPath and returns the
// completed Config when the user finishes the wizard.
func Run(ttyPath string) (*install.Config, error) {
	disks, err := install.ListDisks()
	if err != nil {
		return nil, fmt.Errorf("listing disks: %w", err)
	}
	if len(disks) == 0 {
		return nil, errors.New("no block devices found")
	}

	nics, err := netprobe.Probe()
	if err != nil {
		return nil, fmt.Errorf("listing network interfaces: %w", err)
	}

	m := newModel(disks, nics)

	var opts []tea.ProgramOption
	opts = append(opts, tea.WithAltScreen())

	if ttyPath != "" {
		tty, err := os.OpenFile(ttyPath, os.O_RDWR, 0)
		if err != nil {
			// Requested TTY unavailable (e.g. serial console selected but no
			// serial port present). Fall back to tty1 rather than aborting so
			// the installer remains usable on the display.
			slog.Warn("ui: could not open requested TTY, falling back to tty1", "tty", ttyPath, "err", err)
			if tty, err = os.OpenFile("/dev/tty1", os.O_RDWR, 0); err != nil {
				return nil, fmt.Errorf("open fallback console /dev/tty1: %w", err)
			}
		}
		opts = append(opts, tea.WithInput(tty), tea.WithOutput(tty))
	}

	p := tea.NewProgram(m, opts...)
	final, err := p.Run()
	if err != nil {
		return nil, err
	}

	fm, ok := final.(model)
	if !ok {
		return nil, errors.New("unexpected model type")
	}
	if fm.err != nil {
		return nil, fm.err
	}
	return fm.result, nil
}

func newModel(disks []install.Disk, nics []netprobe.NIC) model {
	eraseIn := textinput.New()
	eraseIn.Placeholder = "yes"
	eraseIn.CharLimit = 3

	hostnameIn := textinput.New()
	hostnameIn.Placeholder = "node1"
	hostnameIn.CharLimit = 64

	emailIn := textinput.New()
	emailIn.Placeholder = "admin@mydomain.com"
	emailIn.CharLimit = 254 // RFC 5321 upper bound
	emailIn.Width = 40

	passIn := textinput.New()
	passIn.Placeholder = "Admin password"
	passIn.EchoMode = textinput.EchoPassword
	passIn.CharLimit = 128

	passConfirmIn := textinput.New()
	passConfirmIn.Placeholder = "Confirm password"
	passConfirmIn.EchoMode = textinput.EchoPassword
	passConfirmIn.CharLimit = 128

	// Pre-fill the roles from the NIC count: one NIC folds everything onto
	// wan, two dedicates the second to lan with vpc folded onto it, and three
	// or more give each plane its own interface.
	lanNIC, vpcNIC := foldedNIC, foldedNIC
	if len(nics) > 1 {
		lanNIC = 1
	}
	if len(nics) > 2 {
		vpcNIC = 2
	}
	roles := [3]roleForm{
		newRoleForm(install.PlaneWAN, 0),
		newRoleForm(install.PlaneLAN, lanNIC),
		newRoleForm(install.PlaneVPC, vpcNIC),
	}

	// The first non-live, non-removable disk is preselected so a single-disk
	// machine needs no storage input at all.
	var preselected []int
	for i, d := range disks {
		if !d.LiveMedia && !d.Removable {
			preselected = []int{i}
			break
		}
	}

	return model{
		screen:               screenWelcome,
		disks:                disks,
		selected:             preselected,
		nics:                 nics,
		eraseInput:           eraseIn,
		roles:                roles,
		hostnameInput:        hostnameIn,
		emailInput:           emailIn,
		passwordInput:        passIn,
		passwordConfirmInput: passConfirmIn,
	}
}

// ── Styles ────────────────────────────────────────────────────────────────────

var (
	styleLogo = lipgloss.NewStyle().
			Foreground(branding.ColorPrimary).
			Bold(true)

	styleTitle = lipgloss.NewStyle().
			Foreground(branding.ColorPrimary).
			Bold(true).
			MarginBottom(1)

	styleSubtitle = lipgloss.NewStyle().
			Foreground(branding.ColorMuted)

	styleBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(branding.ColorBorder).
			Padding(1, 2)

	styleSelected = lipgloss.NewStyle().
			Foreground(branding.ColorBackground).
			Background(branding.ColorPrimary).
			Bold(true)

	styleWarning = lipgloss.NewStyle().
			Foreground(branding.ColorWarning).
			Bold(true)

	styleError = lipgloss.NewStyle().
			Foreground(branding.ColorError)

	styleMuted = lipgloss.NewStyle().
			Foreground(branding.ColorMuted)

	styleSuccess = lipgloss.NewStyle().
			Foreground(branding.ColorSuccess)

	styleLabel = lipgloss.NewStyle().
			Foreground(branding.ColorAccent).
			Bold(true)

	styleHelp = lipgloss.NewStyle().
			Foreground(branding.ColorMuted).
			MarginTop(1)
)

// ── Init / Update / View ──────────────────────────────────────────────────────

func (m model) Init() tea.Cmd {
	return textinput.Blink
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		m.validationErr = ""
		switch msg.String() {
		case "ctrl+c":
			m.err = errors.New("installation cancelled")
			return m, tea.Quit
		}
		return m.handleKey(msg)
	}

	// Forward to active input
	return m.updateActiveInput(msg)
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch m.screen {
	case screenWelcome:
		if key == "enter" || key == " " {
			m.screen = screenDisk
		}

	case screenDisk:
		return m.handleDiskKey(key)

	case screenZFSOptions:
		return m.handleZFSOptionsKey(key)

	case screenDiskConfirm:
		switch key {
		case "enter":
			if strings.ToLower(strings.TrimSpace(m.eraseInput.Value())) != "yes" {
				m.validationErr = "Type 'yes' to confirm disk erasure"
				return m, nil
			}
			m.screen = screenNetworkRoles
			m.roleCursor = 0
		case "esc":
			m.screen = screenDisk
			return m, nil
		default:
			var cmd tea.Cmd
			m.eraseInput, cmd = m.eraseInput.Update(msg)
			return m, cmd
		}

	case screenNetworkRoles:
		return m.handleRolesKey(key)

	case screenNetworkRole:
		return m.handleRoleKey(key, msg)

	case screenIdentity:
		switch key {
		case "esc":
			m.hostnameInput.Blur()
			m.screen = screenNetworkRoles
		case "enter":
			if strings.TrimSpace(m.hostnameInput.Value()) == "" {
				m.validationErr = "Hostname is required"
				m.hostnameInput.Focus()
				return m, nil
			}
			m.screen = screenPassword
			m.emailInput.Focus()
			m.credsFocus = 0
		default:
			if m.hostnameInput.Focused() {
				var cmd tea.Cmd
				m.hostnameInput, cmd = m.hostnameInput.Update(msg)
				return m, cmd
			}
		}

	case screenPassword:
		switch key {
		case "tab", "down":
			m = m.setCredsFocus((m.credsFocus + 1) % 3)
		case "shift+tab", "up":
			m = m.setCredsFocus((m.credsFocus + 2) % 3)
		case "enter":
			// On email field: validate, then advance to password.
			if m.credsFocus == 0 {
				if err := admin.ValidateEmail(m.emailInput.Value()); err != nil {
					m.validationErr = err.Error()
					return m, nil
				}
				m.validationErr = ""
				m = m.setCredsFocus(1)
				return m, nil
			}
			// On password field: just advance (defer validation to confirm).
			if m.credsFocus == 1 {
				m = m.setCredsFocus(2)
				return m, nil
			}
			// On confirm: validate email (again, in case user tabbed past),
			// then validate password + match.
			if err := admin.ValidateEmail(m.emailInput.Value()); err != nil {
				m.validationErr = err.Error()
				m = m.setCredsFocus(0)
				return m, nil
			}
			pw := m.passwordInput.Value()
			confirm := m.passwordConfirmInput.Value()
			if pw == "" {
				m.validationErr = "Password is required"
				m = m.setCredsFocus(1)
				return m, nil
			}
			if pw != confirm {
				m.validationErr = "Passwords do not match"
				return m, nil
			}
			m.validationErr = ""
			m.screen = screenConfirm
		case "esc":
			m.emailInput.Blur()
			m.passwordInput.Blur()
			m.passwordConfirmInput.Blur()
			m.screen = screenIdentity
			m.hostnameInput.Focus()
		default:
			var cmd tea.Cmd
			switch m.credsFocus {
			case 0:
				m.emailInput, cmd = m.emailInput.Update(msg)
			case 1:
				m.passwordInput, cmd = m.passwordInput.Update(msg)
			default:
				m.passwordConfirmInput, cmd = m.passwordConfirmInput.Update(msg)
			}
			return m, cmd
		}

	case screenConfirm:
		switch key {
		case "enter", "y", "Y":
			m.result = m.buildConfig()
			m.screen = screenDone
			return m, tea.Quit
		case "n", "N":
			m.err = errors.New("installation cancelled")
			return m, tea.Quit
		case "esc":
			m.screen = screenPassword
			m = m.setCredsFocus(0)
		}
	}

	return m, nil
}

// setCredsFocus moves focus among the three credential inputs (email,
// password, confirm) and ensures exactly one is focused. Returns the
// updated model — callers must reassign (m = m.setCredsFocus(...)).
// Value receiver keeps model's method set consistent with the other
// bubbletea View/buildConfig methods (avoids golangci-lint recvcheck).
func (m model) setCredsFocus(i int) model {
	m.emailInput.Blur()
	m.passwordInput.Blur()
	m.passwordConfirmInput.Blur()
	switch i {
	case 0:
		m.emailInput.Focus()
	case 1:
		m.passwordInput.Focus()
	default:
		m.passwordConfirmInput.Focus()
		i = 2
	}
	m.credsFocus = i
	return m
}

// ── Storage ───────────────────────────────────────────────────────────────────

// fsType is the currently highlighted filesystem.
func (m model) fsType() install.FSType { return install.AllFSTypes[m.fsCursor] }

// storage assembles the disk configuration from the current selection.
func (m model) storage() install.DiskConfig {
	cfg := install.DiskConfig{FS: m.fsType(), ZFS: m.zfs}
	for _, i := range m.selected {
		if i < len(m.disks) {
			cfg.Disks = append(cfg.Disks, m.disks[i])
		}
	}
	return cfg
}

// fsRow is the filesystem selector, which sits above the disk list on the same
// screen: the choice and the disks it constrains have to be visible together,
// or the operator picks RAIDZ-1 and only then learns three disks are needed.
const fsRow = 0

// handleDiskKey drives the storage screen. Space toggles a disk and the order
// they are picked in is kept, because that is what pairs mirrors under RAID10.
func (m model) handleDiskKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "up", "k":
		m.storageCursor = max(m.storageCursor-1, fsRow)
	case "down", "j":
		m.storageCursor = min(m.storageCursor+1, len(m.disks))
	case "left", "h":
		if m.storageCursor == fsRow {
			m.fsCursor = wrap(m.fsCursor-1, len(install.AllFSTypes))
			m.validationErr = ""
		}
	case "right", "l":
		if m.storageCursor == fsRow {
			m.fsCursor = wrap(m.fsCursor+1, len(install.AllFSTypes))
			m.validationErr = ""
		}
	case " ", "x":
		if m.storageCursor != fsRow {
			m = m.toggleDisk(m.storageCursor - 1)
		}
	case "a":
		if m.fsType().IsZFS() {
			m.zfsCursor = 0
			m.screen = screenZFSOptions
		}
	case "esc":
		m.validationErr = ""
		m.screen = screenWelcome
	case "enter":
		// Validated before the confirmation prompt so a bad selection is a
		// message on this screen, not a failure part-way through the install.
		if err := m.storage().Validate(); err != nil {
			m.validationErr = err.Error()
			return m, nil
		}
		m.validationErr = ""
		m.screen = screenDiskConfirm
		m.eraseInput.Focus()
		m.eraseInput.SetValue("")
	}
	return m, nil
}

// toggleDisk adds or removes a disk from the selection. The slice is cloned
// because the model is copied by value and would otherwise share its backing
// array with the version bubbletea still holds.
func (m model) toggleDisk(i int) model {
	sel := slices.Clone(m.selected)
	if at := slices.Index(sel, i); at >= 0 {
		sel = slices.Delete(sel, at, at+1)
	} else {
		sel = append(sel, i)
	}
	m.selected = sel
	m.validationErr = ""
	return m
}

// zfsOption is one row on the advanced screen. Each holds an ordered choice
// list whose first entry is the computed default, shown as "auto".
type zfsOption struct {
	label string
	get   func(*install.ZFSOpts) *int
	text  func(*install.ZFSOpts) *string
	ints  []int
	strs  []string
	note  string
}

var zfsOptions = []zfsOption{
	{label: "ashift", get: func(o *install.ZFSOpts) *int { return &o.Ashift },
		ints: []int{0, 9, 12, 13}, note: "sector size exponent; cannot be changed after the pool is created"},
	{label: "compression", text: func(o *install.ZFSOpts) *string { return &o.Compress },
		strs: []string{"", "lz4", "zstd", "gzip", "off"}, note: "lz4 is faster than the disks it is feeding"},
	{label: "checksum", text: func(o *install.ZFSOpts) *string { return &o.Checksum },
		strs: []string{"", "on", "fletcher4", "sha256", "blake3"}, note: "on selects the pool's default algorithm"},
	{label: "copies", get: func(o *install.ZFSOpts) *int { return &o.Copies },
		ints: []int{0, 1, 2, 3}, note: "extra copies of every block, on top of any RAID redundancy"},
	{label: "ARC max", get: func(o *install.ZFSOpts) *int { return &o.ARCMaxMiB },
		ints: []int{0, 1024, 2048, 4096, 8192, 16384},
		note: "memory the cache may hold; it is subtracted from what instances can be given"},
}

// value renders the current setting, or "auto" for the computed default.
func (z zfsOption) value(o *install.ZFSOpts) string {
	if z.get != nil {
		n := *z.get(o)
		if n == 0 {
			return "auto"
		}
		if z.label == "ARC max" {
			return fmt.Sprintf("%d MiB", n)
		}
		return strconv.Itoa(n)
	}
	if s := *z.text(o); s != "" {
		return s
	}
	return "auto"
}

// cycle steps the option forwards or backwards through its choices.
func (z zfsOption) cycle(o *install.ZFSOpts, delta int) {
	if z.get != nil {
		dst := z.get(o)
		i := slices.Index(z.ints, *dst)
		*dst = z.ints[wrap(i+delta, len(z.ints))]
		return
	}
	dst := z.text(o)
	i := slices.Index(z.strs, *dst)
	*dst = z.strs[wrap(i+delta, len(z.strs))]
}

func wrap(i, n int) int { return ((i % n) + n) % n }

func (m model) handleZFSOptionsKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "up", "k":
		m.zfsCursor = max(m.zfsCursor-1, 0)
	case "down", "j":
		m.zfsCursor = min(m.zfsCursor+1, len(zfsOptions)-1)
	case "left", "h":
		zfsOptions[m.zfsCursor].cycle(&m.zfs, -1)
	case "right", "l":
		zfsOptions[m.zfsCursor].cycle(&m.zfs, 1)
	case "enter", "esc":
		m.screen = screenDisk
	}
	return m, nil
}

func (m model) updateActiveInput(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m.screen {
	case screenDiskConfirm:
		var cmd tea.Cmd
		m.eraseInput, cmd = m.eraseInput.Update(msg)
		return m, cmd
	case screenIdentity:
		var cmd tea.Cmd
		m.hostnameInput, cmd = m.hostnameInput.Update(msg)
		return m, cmd
	}
	return m, nil
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m model) View() string {
	w := m.width
	if w == 0 {
		w = 80
	}

	var content string
	switch m.screen {
	case screenWelcome:
		content = m.viewWelcome(w)
	case screenDisk:
		content = m.viewDisk(w)
	case screenZFSOptions:
		content = m.viewZFSOptions(w)
	case screenDiskConfirm:
		content = m.viewDiskConfirm(w)
	case screenNetworkRoles:
		content = m.viewNetworkRoles(w)
	case screenNetworkRole:
		content = m.viewNetworkRole(w)
	case screenIdentity:
		content = m.viewIdentity(w)
	case screenPassword:
		content = m.viewPassword(w)
	case screenConfirm:
		content = m.viewConfirm(w)
	case screenDone:
		content = m.viewDone(w)
	}

	return content
}

func (m model) viewWelcome(w int) string {
	logo := styleLogo.Render(branding.Logo)
	subtitle := styleSubtitle.Render(branding.Subtitle)
	publisher := styleMuted.Render(branding.Publisher)

	warning := styleWarning.Render("WARNING: Installation will erase the selected disk entirely.")
	help := styleHelp.Render("Press Enter to begin")

	body := lipgloss.JoinVertical(lipgloss.Center,
		logo,
		subtitle,
		publisher,
		"",
		warning,
		"",
		help,
	)

	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center,
		styleBox.Width(min(w-4, 72)).Render(body),
	)
}

// matchedDiskCount is the size of the largest group of usable disks within the
// same-size tolerance, which is the number a redundant pool could be built from.
func (m model) matchedDiskCount() int {
	best := 0
	for _, ref := range m.disks {
		if ref.LiveMedia || ref.Removable {
			continue
		}
		n := 0
		for _, d := range m.disks {
			if d.LiveMedia || d.Removable {
				continue
			}
			if install.SizesWithinTolerance(ref, d) {
				n++
			}
		}
		best = max(best, n)
	}
	return best
}

func (m model) viewDisk(w int) string {
	title := styleTitle.Render("Select Installation Disk")
	subtitle := styleMuted.Render("All data on the selected disks will be permanently erased.")

	fs := m.fsType()
	req := "single disk"
	if n := fs.MinDisks(); n > 1 {
		req = fmt.Sprintf("%d+ disks, same size", n)
	} else if fs.IsZFS() {
		req = "1+ disks"
	}
	fsLine := fmt.Sprintf("  Filesystem   ‹ %-14s ›   %s", fs.Label(), req)
	if m.storageCursor == fsRow {
		fsLine = styleSelected.Render("> " + strings.TrimPrefix(fsLine, "  "))
	} else {
		fsLine = styleMuted.Render(fsLine)
	}
	rows := []string{fsLine, ""}

	for i, d := range m.disks {
		marker := "[ ]"
		if at := slices.Index(m.selected, i); at >= 0 {
			// Numbered, not ticked: the order is what pairs mirrors.
			marker = fmt.Sprintf("[%d]", at+1)
		}
		note := d.Content
		switch {
		case d.LiveMedia:
			note = "installer boot media"
		case d.Removable:
			note += ", removable"
		}
		line := fmt.Sprintf("  %s %-16s %-8s %-20s %s", marker, d.Path, d.SizeHuman(), truncate(d.Model, 20), note)
		if m.storageCursor == i+1 {
			line = styleSelected.Render("> " + strings.TrimPrefix(line, "  "))
		} else {
			line = styleMuted.Render(line)
		}
		rows = append(rows, line)
	}

	rows = append(rows, "", m.geometryPreview())

	// Only suggested, never applied: which disks may be erased is the operator's
	// call, and a machine with matched spares is not necessarily offering them.
	if n := m.matchedDiskCount(); n >= 2 && !fs.IsZFS() {
		rows = append(rows, styleMuted.Render(fmt.Sprintf(
			"  %d disks of matching size are present — a ZFS pool would survive a disk failure.", n)))
	}
	for _, warn := range m.storage().Warnings() {
		rows = append(rows, styleWarning.Render("  "+warn))
	}
	if m.validationErr != "" {
		rows = append(rows, styleError.Render("  "+m.validationErr))
	}

	keys := "↑/↓ to move • ←/→ filesystem • Space to select disk • Enter to continue"
	if fs.IsZFS() {
		keys = "↑/↓ to move • ←/→ filesystem • Space to select disk • A for ZFS options • Enter to continue"
	}
	body := lipgloss.JoinVertical(lipgloss.Left,
		append([]string{title, subtitle, ""}, append(rows, "", styleHelp.Render(keys))...)...)

	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center,
		styleBox.Width(min(w-4, 96)).Render(body),
	)
}

// geometryPreview states what the current selection actually builds, so the
// cost of the redundancy choice is visible before it is committed to.
func (m model) geometryPreview() string {
	cfg := m.storage()
	if len(cfg.Disks) == 0 {
		return styleMuted.Render("  No disks selected.")
	}
	// Until the topology can be built there is no capacity to quote, so state
	// what is missing instead of a figure derived from too few members.
	if !cfg.Buildable() {
		return styleMuted.Render(fmt.Sprintf("  %s %s — %d selected.",
			cfg.FS.Label(), cfg.Requirement(), len(cfg.Disks)))
	}
	line := fmt.Sprintf("  %s across %d disk(s) — %s usable",
		cfg.FS.Label(), len(cfg.Disks), humanBytes(cfg.UsableBytes()))
	if n := cfg.Tolerated(); n > 0 {
		line += fmt.Sprintf(", survives %d disk failure(s)", n)
	}
	return styleLabel.Render(line)
}

func (m model) viewZFSOptions(w int) string {
	title := styleTitle.Render("ZFS Options")
	subtitle := styleMuted.Render("Defaults are computed from the selected disks and this machine's memory.")

	var rows []string
	for i, opt := range zfsOptions {
		line := fmt.Sprintf("  %-14s %-12s", opt.label, opt.value(&m.zfs))
		if i == m.zfsCursor {
			rows = append(rows, styleSelected.Render("> "+strings.TrimPrefix(line, "  ")))
			rows = append(rows, styleMuted.Render("    "+opt.note))
			continue
		}
		rows = append(rows, styleMuted.Render(line))
	}

	help := styleHelp.Render("↑/↓ to move • ←/→ to change • Enter to go back")
	body := lipgloss.JoinVertical(lipgloss.Left, append([]string{title, subtitle, ""}, append(rows, "", help)...)...)

	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center,
		styleBox.Width(min(w-4, 78)).Render(body),
	)
}

func (m model) viewDiskConfirm(w int) string {
	title := styleTitle.Render("Confirm Disk Erasure")
	disk := styleLabel.Render(strings.Join(m.storage().Paths(), ", "))
	msg := fmt.Sprintf("All data on %s will be permanently erased.\nType 'yes' to confirm:", disk)

	var lines []string
	lines = append(lines, title, msg, "", m.eraseInput.View())
	if m.validationErr != "" {
		lines = append(lines, "", styleError.Render(m.validationErr))
	}
	lines = append(lines, styleHelp.Render("Enter to confirm • Esc to go back"))

	body := lipgloss.JoinVertical(lipgloss.Left, lines...)
	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center,
		styleBox.Width(min(w-4, 64)).Render(body),
	)
}

func (m model) viewIdentity(w int) string {
	title := styleTitle.Render("Node Identity")

	hostnameLabel := styleLabel.Render("Hostname")

	var lines []string
	lines = append(lines, title, "", hostnameLabel, m.hostnameInput.View())
	if m.validationErr != "" {
		lines = append(lines, "", styleError.Render(m.validationErr))
	}
	lines = append(lines, styleHelp.Render("Enter to proceed • Esc to go back"))

	body := lipgloss.JoinVertical(lipgloss.Left, lines...)
	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center,
		styleBox.Width(min(w-4, 64)).Render(body),
	)
}

func (m model) viewPassword(w int) string {
	title := styleTitle.Render("Administrator Credentials")
	emailLabel := styleLabel.Render("Email")
	emailHelp := styleHelp.Render("Used to notify of important system updates to Spinifex or security announcements")
	passLabel := styleLabel.Render("Password")
	confirmLabel := styleLabel.Render("Confirm password")

	var lines []string
	lines = append(lines,
		title, "",
		emailLabel, m.emailInput.View(), emailHelp, "",
		passLabel, m.passwordInput.View(), "",
		confirmLabel, m.passwordConfirmInput.View(),
	)
	if m.validationErr != "" {
		lines = append(lines, "", styleError.Render(m.validationErr))
	}
	lines = append(lines, "", styleHelp.Render("Tab to move • Enter to proceed • Esc to go back"))

	body := lipgloss.JoinVertical(lipgloss.Left, lines...)
	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center,
		styleBox.Width(min(w-4, 64)).Render(body),
	)
}

func (m model) viewConfirm(w int) string {
	title := styleTitle.Render("Confirm Installation")

	cfg := m.buildConfig()

	summary := []struct{ k, v string }{
		{"Filesystem", cfg.Storage.FS.Label()},
		{"Disks", strings.Join(cfg.Storage.Paths(), ", ")},
	}
	// Folded roles are shown explicitly rather than omitted, so the operator
	// sees which plane a collapsed role landed on before committing.
	for _, p := range []install.Plane{install.PlaneWAN, install.PlaneLAN, install.PlaneVPC} {
		var role install.NetworkRole
		switch p {
		case install.PlaneWAN:
			role = cfg.WAN
		case install.PlaneLAN:
			role = cfg.LAN
		case install.PlaneVPC:
			role = cfg.VPC
		}
		name := strings.ToUpper(string(p))
		if role.Folded() {
			_, landed := cfg.Resolve(p)
			summary = append(summary, struct{ k, v string }{name, fmt.Sprintf("folded onto %s", landed)})
			continue
		}
		addr := "DHCP"
		if !role.DHCPMode {
			addr = role.Address + "/" + role.Mask
			if role.Gateway != "" {
				addr += " via " + role.Gateway
			}
		}
		summary = append(summary,
			struct{ k, v string }{name + " interface", fmt.Sprintf("%s → %s", role.Link(), p.Bridge())},
			struct{ k, v string }{name + " address", addr},
		)
	}
	summary = append(summary,
		struct{ k, v string }{"Hostname", cfg.Hostname},
	)
	if cfg.HasCACert {
		summary = append(summary, struct{ k, v string }{"CA certificate", "provided"})
	}

	var rows []string
	for _, s := range summary {
		rows = append(rows, fmt.Sprintf("  %s%-20s%s  %s",
			styleLabel.Render(""), styleLabel.Render(s.k), "", s.v))
	}

	warning := styleWarning.Render("This will erase " + strings.Join(cfg.Storage.Paths(), ", ") + " and begin installation.")

	body := lipgloss.JoinVertical(lipgloss.Left,
		title, "",
		strings.Join(rows, "\n"), "",
		warning, "",
		styleHelp.Render("Enter/Y to install • N to cancel • Esc to go back"),
	)
	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center,
		styleBox.Width(min(w-4, 72)).Render(body),
	)
}

func (m model) viewDone(w int) string {
	body := lipgloss.JoinVertical(lipgloss.Center,
		styleSuccess.Render("Installation complete."),
		"",
		styleMuted.Render("The system will reboot shortly."),
	)
	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center,
		styleBox.Width(min(w-4, 48)).Render(body),
	)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (m model) buildConfig() *install.Config {
	cfg := &install.Config{Storage: m.storage()}

	cfg.WAN = m.roles[0].toRole(m.nics)
	cfg.LAN = m.roles[1].toRole(m.nics)
	cfg.VPC = m.roles[2].toRole(m.nics)

	cfg.Hostname = strings.TrimSpace(m.hostnameInput.Value())
	cfg.RootPassword = m.passwordInput.Value()
	cfg.Email = strings.TrimSpace(m.emailInput.Value())
	return cfg
}

// parseDNS splits a comma-separated DNS string into individual nameserver entries.
func parseDNS(raw string) []string {
	var out []string
	for s := range strings.SplitSeq(raw, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// humanBytes renders a capacity for the geometry preview.
func humanBytes(b int64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1fT", float64(b)/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// truncate shortens a field so a long drive or NIC model does not wrap the
// table it sits in. It counts runes, since vendor strings are not all ASCII.
func truncate(s string, n int) string {
	r := []rune(s)
	if n <= 1 || len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// validSubnetMask accepts dotted-decimal only (255.255.255.0). Prefix length is
// deliberately rejected: netgen converts the mask with net.ParseIP, so a "24"
// accepted here passed validation and then failed the install itself with
// "invalid mask: 24" — after the disk had already been partitioned.
func validSubnetMask(s string) bool {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil || ip.To4() == nil {
		return false
	}
	_, bits := net.IPMask(ip.To4()).Size()
	// Size reports zero bits for a non-contiguous mask, which netgen rejects too.
	return bits != 0
}
