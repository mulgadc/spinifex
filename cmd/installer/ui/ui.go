// Package ui presents the interactive installer TUI using bubbletea and lipgloss.
package ui

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
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
	screenDiskConfirm
	screenNetworkRoles
	screenNetworkRole
	screenIdentity
	screenPassword
	screenJoinConfig
	screenConfirm
	screenDone // signals completion; program exits
)

// model is the top-level bubbletea model for the installer wizard.
type model struct {
	screen screen
	width  int
	height int

	// Disk selection
	disks      []diskInfo
	diskCursor int
	eraseInput textinput.Model

	// Detected interfaces, shared by every role screen.
	nics []netprobe.NIC

	// Network planes. roleCursor indexes roles, or continueRow for the
	// Continue action; advanced reveals VLAN and MTU on every role.
	roles      [3]roleForm
	roleCursor int
	advanced   bool

	// Identity
	hostnameInput textinput.Model
	clusterRole   int // 0 = init, 1 = join

	// Join config
	joinIPInput   textinput.Model
	joinPortInput textinput.Model

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
	disks, err := availableDisks()
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

func newModel(disks []diskInfo, nics []netprobe.NIC) model {
	eraseIn := textinput.New()
	eraseIn.Placeholder = "yes"
	eraseIn.CharLimit = 3

	hostnameIn := textinput.New()
	hostnameIn.Placeholder = "node1"
	hostnameIn.CharLimit = 64

	joinIPIn := textinput.New()
	joinIPIn.Placeholder = "192.168.1.10"

	joinPortIn := textinput.New()
	joinPortIn.Placeholder = "4432"
	joinPortIn.CharLimit = 5

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

	return model{
		screen:               screenWelcome,
		disks:                disks,
		nics:                 nics,
		eraseInput:           eraseIn,
		roles:                roles,
		hostnameInput:        hostnameIn,
		emailInput:           emailIn,
		passwordInput:        passIn,
		passwordConfirmInput: passConfirmIn,
		joinIPInput:          joinIPIn,
		joinPortInput:        joinPortIn,
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
		switch key {
		case "up", "k":
			if m.diskCursor > 0 {
				m.diskCursor--
			}
		case "down", "j":
			if m.diskCursor < len(m.disks)-1 {
				m.diskCursor++
			}
		case "enter":
			m.screen = screenDiskConfirm
			m.eraseInput.Focus()
			m.eraseInput.SetValue("")
		}

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
		case "tab", "down":
			if m.hostnameInput.Focused() {
				m.hostnameInput.Blur()
			} else {
				m.hostnameInput.Focus()
			}
		case "left", "right":
			if m.hostnameInput.Focused() {
				var cmd tea.Cmd
				m.hostnameInput, cmd = m.hostnameInput.Update(msg)
				return m, cmd
			}
			if key == "left" {
				m.clusterRole = 0
			} else {
				m.clusterRole = 1
			}
		case "enter":
			if m.hostnameInput.Focused() {
				if strings.TrimSpace(m.hostnameInput.Value()) == "" {
					m.validationErr = "Hostname is required"
					return m, nil
				}
				m.hostnameInput.Blur()
				return m, nil
			}
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
			if m.clusterRole == 1 {
				m.screen = screenJoinConfig
				m.joinIPInput.Focus()
			} else {
				m.screen = screenConfirm
			}
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

	case screenJoinConfig:
		switch key {
		case "tab", "down":
			if m.joinIPInput.Focused() {
				m.joinIPInput.Blur()
				m.joinPortInput.Focus()
			} else {
				m.joinPortInput.Blur()
				m.joinIPInput.Focus()
			}
		case "enter":
			if m.joinIPInput.Focused() {
				m.joinIPInput.Blur()
				m.joinPortInput.Focus()
				return m, nil
			}
			joinIP := strings.TrimSpace(m.joinIPInput.Value())
			if net.ParseIP(joinIP) == nil {
				m.validationErr = "Invalid primary node IP"
				return m, nil
			}
			m.screen = screenConfirm
		case "esc":
			m.joinIPInput.Blur()
			m.joinPortInput.Blur()
			m.screen = screenPassword
			m = m.setCredsFocus(2)
		default:
			var cmd tea.Cmd
			if m.joinIPInput.Focused() {
				m.joinIPInput, cmd = m.joinIPInput.Update(msg)
			} else {
				m.joinPortInput, cmd = m.joinPortInput.Update(msg)
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
			if m.clusterRole == 1 {
				m.screen = screenJoinConfig
				m.joinIPInput.Focus()
				m.joinPortInput.Blur()
			} else {
				m.screen = screenPassword
				m = m.setCredsFocus(0)
			}
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
	case screenJoinConfig:
		content = m.viewJoinConfig(w)
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

func (m model) viewDisk(w int) string {
	title := styleTitle.Render("Select Installation Disk")
	subtitle := styleMuted.Render("All data on the selected disk will be permanently erased.")

	var rows []string
	for i, d := range m.disks {
		line := fmt.Sprintf("  %-20s  %-8s  %s", d.Path, d.Size, d.Model)
		if i == m.diskCursor {
			line = styleSelected.Render("> " + line[2:])
		} else {
			line = styleMuted.Render(line)
		}
		rows = append(rows, line)
	}

	help := styleHelp.Render("↑/↓ to select • Enter to confirm")
	body := lipgloss.JoinVertical(lipgloss.Left, append([]string{title, subtitle, ""}, append(rows, "", help)...)...)

	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center,
		styleBox.Width(min(w-4, 72)).Render(body),
	)
}

func (m model) viewDiskConfirm(w int) string {
	title := styleTitle.Render("Confirm Disk Erasure")
	disk := styleLabel.Render(m.disks[m.diskCursor].Path)
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

	roleLabel := styleLabel.Render("Cluster role")
	roles := []string{"Initialize new cluster", "Join existing cluster"}
	var roleParts []string
	for i, r := range roles {
		if i == m.clusterRole && !m.hostnameInput.Focused() {
			roleParts = append(roleParts, styleSelected.Render(" "+r+" "))
		} else if i == m.clusterRole {
			roleParts = append(roleParts, styleLabel.Render("["+r+"]"))
		} else {
			roleParts = append(roleParts, styleMuted.Render(r))
		}
	}

	var lines []string
	lines = append(lines, title, "", hostnameLabel, m.hostnameInput.View(), "", roleLabel, strings.Join(roleParts, "  "))
	if m.validationErr != "" {
		lines = append(lines, "", styleError.Render(m.validationErr))
	}
	lines = append(lines, styleHelp.Render("Tab to toggle focus • ←/→ to select role • Enter to proceed"))

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

func (m model) viewJoinConfig(w int) string {
	title := styleTitle.Render("Join Existing Cluster")
	ipLabel := styleLabel.Render("Primary node IP")
	portLabel := styleLabel.Render("Formation port")

	var lines []string
	lines = append(lines, title, "", ipLabel, m.joinIPInput.View(), "", portLabel, m.joinPortInput.View())
	if m.validationErr != "" {
		lines = append(lines, "", styleError.Render(m.validationErr))
	}
	lines = append(lines, styleHelp.Render("Tab to move • Enter to proceed • Esc to go back"))

	body := lipgloss.JoinVertical(lipgloss.Left, lines...)
	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center,
		styleBox.Width(min(w-4, 64)).Render(body),
	)
}

func (m model) viewConfirm(w int) string {
	title := styleTitle.Render("Confirm Installation")

	cfg := m.buildConfig()
	role := "Initialize new cluster"
	if cfg.ClusterRole == "join" {
		role = fmt.Sprintf("Join cluster at %s", cfg.JoinAddr)
	}

	summary := []struct{ k, v string }{{"Disk", cfg.Disk}}
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
		struct{ k, v string }{"Cluster role", role},
	)
	if cfg.HasCACert {
		summary = append(summary, struct{ k, v string }{"CA certificate", "provided"})
	}

	var rows []string
	for _, s := range summary {
		rows = append(rows, fmt.Sprintf("  %s%-20s%s  %s",
			styleLabel.Render(""), styleLabel.Render(s.k), "", s.v))
	}

	warning := styleWarning.Render("This will erase " + cfg.Disk + " and begin installation.")

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
	cfg := &install.Config{}
	if len(m.disks) > m.diskCursor {
		cfg.Disk = m.disks[m.diskCursor].Path
	}

	cfg.WAN = m.roles[0].toRole(m.nics)
	cfg.LAN = m.roles[1].toRole(m.nics)
	cfg.VPC = m.roles[2].toRole(m.nics)

	cfg.Hostname = strings.TrimSpace(m.hostnameInput.Value())
	if m.clusterRole == 0 {
		cfg.ClusterRole = "init"
	} else {
		cfg.ClusterRole = "join"
		port := strings.TrimSpace(m.joinPortInput.Value())
		if port == "" {
			port = "4432"
		}
		cfg.JoinAddr = net.JoinHostPort(strings.TrimSpace(m.joinIPInput.Value()), port)
	}
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

// diskInfo holds display info for a block device.
type diskInfo struct {
	Path  string
	Size  string
	Model string
}

func availableDisks() ([]diskInfo, error) {
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil, err
	}
	var disks []diskInfo
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") {
			continue
		}
		d := diskInfo{Path: "/dev/" + name}
		d.Size = readSysBlockFile(name, "size")
		if d.Size != "" {
			d.Size = formatSectors(d.Size)
		}
		d.Model = strings.TrimSpace(readSysBlockFile(name, "device/model"))
		disks = append(disks, d)
	}
	return disks, nil
}

func readSysBlockFile(dev, file string) string {
	data, err := os.ReadFile("/sys/block/" + dev + "/" + file)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func formatSectors(sectors string) string {
	var n int64
	if _, err := fmt.Sscan(sectors, &n); err != nil {
		return ""
	}
	bytes := n * 512
	switch {
	case bytes >= 1<<40:
		return fmt.Sprintf("%.1fT", float64(bytes)/(1<<40))
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(bytes)/(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(bytes)/(1<<20))
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

// validSubnetMask accepts dotted-decimal (255.255.255.0) or CIDR prefix (/24 or 24).
func validSubnetMask(s string) bool {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "/")
	var prefix int
	if _, err := fmt.Sscan(s, &prefix); err == nil && len(s) <= 2 {
		return prefix >= 0 && prefix <= 32
	}
	return net.ParseIP(s) != nil
}
