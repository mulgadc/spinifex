/*
Copyright © 2026 Mulga Defense Corporation

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

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
)

// screen represents which step of the wizard is active.
type screen int

const (
	screenWelcome screen = iota
	screenDisk
	screenDiskConfirm
	screenNetworkWAN
	screenNetworkLAN
	screenIdentity
	screenPassword
	screenJoinConfig
	screenConfirm
	screenDone // signals completion; program exits
)

// nicInfo holds display info for a network interface.
type nicInfo struct {
	Name   string
	IsWiFi bool
}

// wanField tracks which element is focused on the WAN network screen.
type wanField int

const (
	wanFieldNIC      wanField = iota // NIC picker
	wanFieldMethod                   // DHCP / Static toggle
	wanFieldIP                       // static only
	wanFieldMask                     // static only
	wanFieldGateway                  // static only
	wanFieldDNS                      // static only
	wanFieldSSID                     // WiFi NIC only
	wanFieldWiFiPass                 // WiFi NIC only
)

// lanField tracks which element is focused on the LAN network screen.
type lanField int

const (
	lanFieldNIC      lanField = iota // NIC picker (WAN NIC shown greyed)
	lanFieldMethod                   // DHCP / Static toggle
	lanFieldIP                       // static only
	lanFieldMask                     // static only
	lanFieldDNS                      // static only
	lanFieldSSID                     // WiFi NIC only
	lanFieldWiFiPass                 // WiFi NIC only
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

	// NIC list (shared between WAN and LAN screens)
	nics []nicInfo

	// WAN network screen
	wanNicCursor      int
	wanNicManualInput textinput.Model // used when no NICs are auto-detected
	wanDHCP           bool
	wanFocus          wanField
	wanIP             textinput.Model
	wanMask           textinput.Model
	wanGateway        textinput.Model
	wanDNS            textinput.Model
	wanSSID           textinput.Model
	wanWiFiPass       textinput.Model

	// LAN network screen (only shown when len(nics) > 1)
	lanNicCursor int
	lanDHCP      bool
	lanFocus     lanField
	lanIP        textinput.Model
	lanMask      textinput.Model
	lanDNS       textinput.Model
	lanSSID      textinput.Model
	lanWiFiPass  textinput.Model

	// Identity
	hostnameInput textinput.Model
	clusterRole   int // 0 = init, 1 = join

	// Join config
	joinIPInput   textinput.Model
	joinPortInput textinput.Model

	// Password
	passwordInput        textinput.Model
	passwordConfirmInput textinput.Model
	passwordFocus        int // 0 = password, 1 = confirm

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

	nics, err := availableNICs()
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

func newModel(disks []diskInfo, nics []nicInfo) model {
	eraseIn := textinput.New()
	eraseIn.Placeholder = "yes"
	eraseIn.CharLimit = 3

	wanNicManualIn := textinput.New()
	wanNicManualIn.Placeholder = "e.g. eth0, enp0s1"
	wanNicManualIn.CharLimit = 32

	wanIPIn := textinput.New()
	wanIPIn.Placeholder = "192.168.1.10"

	wanMaskIn := textinput.New()
	wanMaskIn.Placeholder = "255.255.255.0 or 24"

	wanGWIn := textinput.New()
	wanGWIn.Placeholder = "192.168.1.1"

	wanDNSIn := textinput.New()
	wanDNSIn.Placeholder = "1.1.1.1, 8.8.8.8"

	wanSSIDIn := textinput.New()
	wanSSIDIn.Placeholder = "Network SSID"
	wanSSIDIn.CharLimit = 64

	wanWiFiPassIn := textinput.New()
	wanWiFiPassIn.Placeholder = "WiFi password"
	wanWiFiPassIn.EchoMode = textinput.EchoPassword
	wanWiFiPassIn.CharLimit = 128

	lanIPIn := textinput.New()
	lanIPIn.Placeholder = "10.10.8.2"

	lanMaskIn := textinput.New()
	lanMaskIn.Placeholder = "255.255.255.0 or 24"

	lanDNSIn := textinput.New()
	lanDNSIn.Placeholder = "1.1.1.1, 8.8.8.8"

	lanSSIDIn := textinput.New()
	lanSSIDIn.Placeholder = "Network SSID"
	lanSSIDIn.CharLimit = 64

	lanWiFiPassIn := textinput.New()
	lanWiFiPassIn.Placeholder = "WiFi password"
	lanWiFiPassIn.EchoMode = textinput.EchoPassword
	lanWiFiPassIn.CharLimit = 128

	hostnameIn := textinput.New()
	hostnameIn.Placeholder = "node1"
	hostnameIn.CharLimit = 64

	joinIPIn := textinput.New()
	joinIPIn.Placeholder = "192.168.1.10"

	joinPortIn := textinput.New()
	joinPortIn.Placeholder = "4432"
	joinPortIn.CharLimit = 5

	passIn := textinput.New()
	passIn.Placeholder = "Admin password"
	passIn.EchoMode = textinput.EchoPassword
	passIn.CharLimit = 128

	passConfirmIn := textinput.New()
	passConfirmIn.Placeholder = "Confirm password"
	passConfirmIn.EchoMode = textinput.EchoPassword
	passConfirmIn.CharLimit = 128

	// Initial LAN cursor: first NIC that is not the WAN NIC (cursor 0).
	lanCursor := 0
	if len(nics) > 1 {
		lanCursor = 1 // wanNicCursor starts at 0
	}

	return model{
		screen:               screenWelcome,
		disks:                disks,
		nics:                 nics,
		eraseInput:           eraseIn,
		wanNicManualInput:    wanNicManualIn,
		wanDHCP:              true, // DHCP is the default
		wanIP:                wanIPIn,
		wanMask:              wanMaskIn,
		wanGateway:           wanGWIn,
		wanDNS:               wanDNSIn,
		wanSSID:              wanSSIDIn,
		wanWiFiPass:          wanWiFiPassIn,
		lanNicCursor:         lanCursor,
		lanDHCP:              true, // DHCP is the default
		lanIP:                lanIPIn,
		lanMask:              lanMaskIn,
		lanDNS:               lanDNSIn,
		lanSSID:              lanSSIDIn,
		lanWiFiPass:          lanWiFiPassIn,
		hostnameInput:        hostnameIn,
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
			m.screen = screenNetworkWAN
			m.wanFocus = wanFieldNIC
			m = m.withFocusedWANField()
		case "esc":
			m.screen = screenDisk
			return m, nil
		default:
			var cmd tea.Cmd
			m.eraseInput, cmd = m.eraseInput.Update(msg)
			return m, cmd
		}

	case screenNetworkWAN:
		return m.handleWANKey(key, msg)

	case screenNetworkLAN:
		return m.handleLANKey(key, msg)

	case screenIdentity:
		switch key {
		case "esc":
			m.hostnameInput.Blur()
			if len(m.nics) > 1 {
				m.screen = screenNetworkLAN
				m = m.withFocusedLANField()
			} else {
				m.screen = screenNetworkWAN
				m = m.withFocusedWANField()
			}
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
			m.passwordInput.Focus()
			m.passwordFocus = 0
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
			m.passwordInput.Blur()
			m.passwordConfirmInput.Blur()
			if m.passwordFocus == 0 {
				m.passwordConfirmInput.Focus()
				m.passwordFocus = 1
			} else {
				m.passwordInput.Focus()
				m.passwordFocus = 0
			}
		case "shift+tab", "up":
			m.passwordInput.Blur()
			m.passwordConfirmInput.Blur()
			if m.passwordFocus == 1 {
				m.passwordInput.Focus()
				m.passwordFocus = 0
			} else {
				m.passwordConfirmInput.Focus()
				m.passwordFocus = 1
			}
		case "enter":
			if m.passwordFocus == 0 {
				m.passwordInput.Blur()
				m.passwordConfirmInput.Focus()
				m.passwordFocus = 1
				return m, nil
			}
			pw := m.passwordInput.Value()
			confirm := m.passwordConfirmInput.Value()
			if pw == "" {
				m.validationErr = "Password is required"
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
			m.passwordInput.Blur()
			m.passwordConfirmInput.Blur()
			m.screen = screenIdentity
			m.hostnameInput.Focus()
		default:
			var cmd tea.Cmd
			if m.passwordFocus == 0 {
				m.passwordInput, cmd = m.passwordInput.Update(msg)
			} else {
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
			m.passwordConfirmInput.Focus()
			m.passwordFocus = 1
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
				m.passwordInput.Focus()
				m.passwordFocus = 0
			}
		}
	}

	return m, nil
}

// ── WAN screen key handling ───────────────────────────────────────────────────

func (m model) handleWANKey(key string, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	isWiFi := len(m.nics) > 0 && m.nics[m.wanNicCursor].IsWiFi

	switch key {
	case "tab", "down":
		m.wanFocus = m.wanNextFocus(m.wanFocus, true)
		m = m.withFocusedWANField()
	case "shift+tab", "up":
		m.wanFocus = m.wanNextFocus(m.wanFocus, false)
		m = m.withFocusedWANField()

	case "left", "h":
		switch m.wanFocus {
		case wanFieldNIC:
			if len(m.nics) > 0 && m.wanNicCursor > 0 {
				m.wanNicCursor--
			}
		case wanFieldMethod:
			m.wanDHCP = true
			// If current focus would be invalid in DHCP mode, snap back.
			m.wanFocus = m.wanClampFocus(m.wanFocus)
			m = m.withFocusedWANField()
		}
	case "right", "l":
		switch m.wanFocus {
		case wanFieldNIC:
			if len(m.nics) > 0 && m.wanNicCursor < len(m.nics)-1 {
				m.wanNicCursor++
			}
		case wanFieldMethod:
			m.wanDHCP = false
			m = m.withFocusedWANField()
		}

	case "esc":
		m.screen = screenDiskConfirm
		m = m.withFocusedWANField()
		return m, nil

	case "enter":
		// Always check last-field first: if we're on it, validate and advance screen.
		if m.wanFocus == m.wanLastFocus() {
			if errMsg := m.validateWAN(); errMsg != "" {
				m.validationErr = errMsg
				return m, nil
			}
			if len(m.nics) > 1 {
				m.screen = screenNetworkLAN
				m.lanFocus = lanFieldNIC
				m.lanNicCursor = m.initialLANCursor()
				m = m.withFocusedLANField()
			} else {
				m.screen = screenIdentity
				m.hostnameInput.Focus()
			}
			return m, nil
		}
		// Otherwise advance to the next field.
		m.wanFocus = m.wanNextFocus(m.wanFocus, true)
		m = m.withFocusedWANField()

	default:
		// Forward keystrokes to the active text input.
		switch m.wanFocus {
		case wanFieldIP:
			var cmd tea.Cmd
			m.wanIP, cmd = m.wanIP.Update(msg)
			return m, cmd
		case wanFieldMask:
			var cmd tea.Cmd
			m.wanMask, cmd = m.wanMask.Update(msg)
			return m, cmd
		case wanFieldGateway:
			var cmd tea.Cmd
			m.wanGateway, cmd = m.wanGateway.Update(msg)
			return m, cmd
		case wanFieldDNS:
			var cmd tea.Cmd
			m.wanDNS, cmd = m.wanDNS.Update(msg)
			return m, cmd
		case wanFieldSSID:
			if isWiFi {
				var cmd tea.Cmd
				m.wanSSID, cmd = m.wanSSID.Update(msg)
				return m, cmd
			}
		case wanFieldWiFiPass:
			if isWiFi {
				var cmd tea.Cmd
				m.wanWiFiPass, cmd = m.wanWiFiPass.Update(msg)
				return m, cmd
			}
		case wanFieldNIC:
			if len(m.nics) == 0 {
				var cmd tea.Cmd
				m.wanNicManualInput, cmd = m.wanNicManualInput.Update(msg)
				return m, cmd
			}
		}
	}
	return m, nil
}

// wanNextFocus returns the next valid focus index, skipping fields that are
// hidden in the current DHCP/WiFi configuration.
func (m model) wanNextFocus(current wanField, forward bool) wanField {
	return nextFocusInList[wanField](current, m.wanVisibleFields(), forward)
}

func (m model) wanLastFocus() wanField {
	fields := m.wanVisibleFields()
	return fields[len(fields)-1]
}

func (m model) wanVisibleFields() []wanField {
	isWiFi := len(m.nics) > 0 && m.nics[m.wanNicCursor].IsWiFi
	fields := []wanField{wanFieldNIC, wanFieldMethod}
	if !m.wanDHCP {
		fields = append(fields, wanFieldIP, wanFieldMask, wanFieldGateway, wanFieldDNS)
	}
	if isWiFi {
		fields = append(fields, wanFieldSSID, wanFieldWiFiPass)
	}
	return fields
}

// wanClampFocus snaps the current focus to the nearest visible field (used
// when toggling between DHCP and static removes visible fields).
func (m model) wanClampFocus(current wanField) wanField {
	for _, f := range m.wanVisibleFields() {
		if f >= current {
			return f
		}
	}
	fields := m.wanVisibleFields()
	return fields[len(fields)-1]
}

func (m model) withFocusedWANField() model {
	m.wanIP.Blur()
	m.wanMask.Blur()
	m.wanGateway.Blur()
	m.wanDNS.Blur()
	m.wanSSID.Blur()
	m.wanWiFiPass.Blur()
	m.wanNicManualInput.Blur()
	switch m.wanFocus {
	case wanFieldIP:
		m.wanIP.Focus()
	case wanFieldMask:
		m.wanMask.Focus()
	case wanFieldGateway:
		m.wanGateway.Focus()
	case wanFieldDNS:
		m.wanDNS.Focus()
	case wanFieldSSID:
		m.wanSSID.Focus()
	case wanFieldWiFiPass:
		m.wanWiFiPass.Focus()
	case wanFieldNIC:
		if len(m.nics) == 0 {
			m.wanNicManualInput.Focus()
		}
	}
	return m
}

func (m model) validateWAN() string {
	if len(m.nics) == 0 && strings.TrimSpace(m.wanNicManualInput.Value()) == "" {
		return "Enter interface name (e.g. eth0, enp0s1)"
	}
	if !m.wanDHCP {
		ip := strings.TrimSpace(m.wanIP.Value())
		mask := strings.TrimSpace(m.wanMask.Value())
		gw := strings.TrimSpace(m.wanGateway.Value())
		dns := strings.TrimSpace(m.wanDNS.Value())
		if net.ParseIP(ip) == nil {
			return "Invalid WAN IP address"
		}
		if !validSubnetMask(mask) {
			return "Invalid subnet mask (e.g. 255.255.255.0 or 24)"
		}
		if net.ParseIP(gw) == nil {
			return "Invalid gateway address"
		}
		if dns == "" {
			return "Enter at least one DNS nameserver"
		}
	}
	isWiFi := len(m.nics) > 0 && m.nics[m.wanNicCursor].IsWiFi
	if isWiFi && strings.TrimSpace(m.wanSSID.Value()) == "" {
		return "WiFi SSID is required"
	}
	return ""
}

// ── LAN screen key handling ───────────────────────────────────────────────────

func (m model) handleLANKey(key string, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	isWiFi := len(m.nics) > m.lanNicCursor && m.nics[m.lanNicCursor].IsWiFi

	switch key {
	case "tab", "down":
		m.lanFocus = m.lanNextFocus(m.lanFocus, true)
		m = m.withFocusedLANField()
	case "shift+tab", "up":
		m.lanFocus = m.lanNextFocus(m.lanFocus, false)
		m = m.withFocusedLANField()

	case "left", "h":
		switch m.lanFocus {
		case lanFieldNIC:
			m = m.moveLANCursorLeft()
		case lanFieldMethod:
			m.lanDHCP = true
			m.lanFocus = m.lanClampFocus(m.lanFocus)
			m = m.withFocusedLANField()
		}
	case "right", "l":
		switch m.lanFocus {
		case lanFieldNIC:
			m = m.moveLANCursorRight()
		case lanFieldMethod:
			m.lanDHCP = false
			m = m.withFocusedLANField()
		}

	case "esc":
		m.screen = screenNetworkWAN
		m = m.withFocusedWANField()
		return m, nil

	case "enter":
		if m.lanFocus == m.lanLastFocus() {
			if errMsg := m.validateLAN(); errMsg != "" {
				m.validationErr = errMsg
				return m, nil
			}
			m.screen = screenIdentity
			m.hostnameInput.Focus()
			return m, nil
		}
		m.lanFocus = m.lanNextFocus(m.lanFocus, true)
		m = m.withFocusedLANField()

	default:
		switch m.lanFocus {
		case lanFieldIP:
			var cmd tea.Cmd
			m.lanIP, cmd = m.lanIP.Update(msg)
			return m, cmd
		case lanFieldMask:
			var cmd tea.Cmd
			m.lanMask, cmd = m.lanMask.Update(msg)
			return m, cmd
		case lanFieldDNS:
			var cmd tea.Cmd
			m.lanDNS, cmd = m.lanDNS.Update(msg)
			return m, cmd
		case lanFieldSSID:
			if isWiFi {
				var cmd tea.Cmd
				m.lanSSID, cmd = m.lanSSID.Update(msg)
				return m, cmd
			}
		case lanFieldWiFiPass:
			if isWiFi {
				var cmd tea.Cmd
				m.lanWiFiPass, cmd = m.lanWiFiPass.Update(msg)
				return m, cmd
			}
		}
	}
	return m, nil
}

func (m model) lanNextFocus(current lanField, forward bool) lanField {
	return nextFocusInList[lanField](current, m.lanVisibleFields(), forward)
}

func (m model) lanLastFocus() lanField {
	fields := m.lanVisibleFields()
	return fields[len(fields)-1]
}

func (m model) lanVisibleFields() []lanField {
	isWiFi := len(m.nics) > m.lanNicCursor && m.nics[m.lanNicCursor].IsWiFi
	fields := []lanField{lanFieldNIC, lanFieldMethod}
	if !m.lanDHCP {
		fields = append(fields, lanFieldIP, lanFieldMask, lanFieldDNS)
	}
	if isWiFi {
		fields = append(fields, lanFieldSSID, lanFieldWiFiPass)
	}
	return fields
}

func (m model) lanClampFocus(current lanField) lanField {
	for _, f := range m.lanVisibleFields() {
		if f >= current {
			return f
		}
	}
	fields := m.lanVisibleFields()
	return fields[len(fields)-1]
}

func (m model) withFocusedLANField() model {
	m.lanIP.Blur()
	m.lanMask.Blur()
	m.lanDNS.Blur()
	m.lanSSID.Blur()
	m.lanWiFiPass.Blur()
	switch m.lanFocus {
	case lanFieldIP:
		m.lanIP.Focus()
	case lanFieldMask:
		m.lanMask.Focus()
	case lanFieldDNS:
		m.lanDNS.Focus()
	case lanFieldSSID:
		m.lanSSID.Focus()
	case lanFieldWiFiPass:
		m.lanWiFiPass.Focus()
	}
	return m
}

// initialLANCursor returns the first NIC index that is not the WAN NIC.
func (m model) initialLANCursor() int {
	for i := range m.nics {
		if i != m.wanNicCursor {
			return i
		}
	}
	return 0
}

// moveLANCursorLeft/Right skip the WAN NIC so it can never be selected.
// If no valid position exists in that direction, the cursor stays put.
func (m model) moveLANCursorLeft() model {
	for i := m.lanNicCursor - 1; i >= 0; i-- {
		if i != m.wanNicCursor {
			m.lanNicCursor = i
			return m
		}
	}
	return m
}

func (m model) moveLANCursorRight() model {
	for i := m.lanNicCursor + 1; i < len(m.nics); i++ {
		if i != m.wanNicCursor {
			m.lanNicCursor = i
			return m
		}
	}
	return m
}

func (m model) validateLAN() string {
	if !m.lanDHCP {
		ip := strings.TrimSpace(m.lanIP.Value())
		mask := strings.TrimSpace(m.lanMask.Value())
		dns := strings.TrimSpace(m.lanDNS.Value())
		if net.ParseIP(ip) == nil {
			return "Invalid LAN IP address"
		}
		if !validSubnetMask(mask) {
			return "Invalid subnet mask (e.g. 255.255.255.0 or 24)"
		}
		if dns == "" {
			return "Enter at least one DNS nameserver"
		}
	}
	isWiFi := len(m.nics) > m.lanNicCursor && m.nics[m.lanNicCursor].IsWiFi
	if isWiFi && strings.TrimSpace(m.lanSSID.Value()) == "" {
		return "WiFi SSID is required"
	}
	return ""
}

// nextFocusInList finds current in the list and returns the next/prev entry,
// wrapping around. Used by both WAN and LAN focus helpers.
func nextFocusInList[T ~int](current T, list []T, forward bool) T {
	for i, v := range list {
		if v == current {
			if forward {
				return list[(i+1)%len(list)]
			}
			return list[(i-1+len(list))%len(list)]
		}
	}
	// current not in list (e.g. after a mode change) — return first/last
	if forward {
		return list[0]
	}
	return list[len(list)-1]
}

func (m model) updateActiveInput(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m.screen {
	case screenDiskConfirm:
		var cmd tea.Cmd
		m.eraseInput, cmd = m.eraseInput.Update(msg)
		return m, cmd
	case screenNetworkWAN:
		switch m.wanFocus {
		case wanFieldIP:
			var cmd tea.Cmd
			m.wanIP, cmd = m.wanIP.Update(msg)
			return m, cmd
		case wanFieldMask:
			var cmd tea.Cmd
			m.wanMask, cmd = m.wanMask.Update(msg)
			return m, cmd
		case wanFieldGateway:
			var cmd tea.Cmd
			m.wanGateway, cmd = m.wanGateway.Update(msg)
			return m, cmd
		case wanFieldDNS:
			var cmd tea.Cmd
			m.wanDNS, cmd = m.wanDNS.Update(msg)
			return m, cmd
		case wanFieldSSID:
			var cmd tea.Cmd
			m.wanSSID, cmd = m.wanSSID.Update(msg)
			return m, cmd
		case wanFieldWiFiPass:
			var cmd tea.Cmd
			m.wanWiFiPass, cmd = m.wanWiFiPass.Update(msg)
			return m, cmd
		case wanFieldNIC:
			if len(m.nics) == 0 {
				var cmd tea.Cmd
				m.wanNicManualInput, cmd = m.wanNicManualInput.Update(msg)
				return m, cmd
			}
		}
	case screenNetworkLAN:
		switch m.lanFocus {
		case lanFieldIP:
			var cmd tea.Cmd
			m.lanIP, cmd = m.lanIP.Update(msg)
			return m, cmd
		case lanFieldMask:
			var cmd tea.Cmd
			m.lanMask, cmd = m.lanMask.Update(msg)
			return m, cmd
		case lanFieldDNS:
			var cmd tea.Cmd
			m.lanDNS, cmd = m.lanDNS.Update(msg)
			return m, cmd
		case lanFieldSSID:
			var cmd tea.Cmd
			m.lanSSID, cmd = m.lanSSID.Update(msg)
			return m, cmd
		case lanFieldWiFiPass:
			var cmd tea.Cmd
			m.lanWiFiPass, cmd = m.lanWiFiPass.Update(msg)
			return m, cmd
		}
	case screenIdentity:
		if m.hostnameInput.Focused() {
			var cmd tea.Cmd
			m.hostnameInput, cmd = m.hostnameInput.Update(msg)
			return m, cmd
		}
	case screenPassword:
		var cmd tea.Cmd
		if m.passwordFocus == 0 {
			m.passwordInput, cmd = m.passwordInput.Update(msg)
		} else {
			m.passwordConfirmInput, cmd = m.passwordConfirmInput.Update(msg)
		}
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
	case screenNetworkWAN:
		content = m.viewNetworkWAN(w)
	case screenNetworkLAN:
		content = m.viewNetworkLAN(w)
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

// renderNICPicker renders a horizontal NIC selector. disabledIdx is the index
// to show as greyed/unavailable (-1 to disable none).
func (m model) renderNICPicker(cursor, disabledIdx int, focused bool) string {
	if len(m.nics) == 0 {
		return ""
	}
	var parts []string
	for i, nic := range m.nics {
		tag := "[ETH]"
		if nic.IsWiFi {
			tag = "[WIFI]"
		}
		label := fmt.Sprintf("%s %s", tag, nic.Name)
		switch {
		case i == disabledIdx:
			parts = append(parts, styleMuted.Render("("+label+")"))
		case i == cursor && focused:
			parts = append(parts, styleSelected.Render(" "+label+" "))
		case i == cursor:
			parts = append(parts, styleLabel.Render("["+label+"]"))
		default:
			parts = append(parts, styleMuted.Render(label))
		}
	}
	return strings.Join(parts, "  ")
}

func (m model) viewNetworkWAN(w int) string {
	title := styleTitle.Render("WAN Network  (Step 1 of " + m.networkStepCount() + ")")
	subtitle := styleMuted.Render("The management IP is assigned to a bridge (br-wan) over this NIC.")

	var lines []string
	lines = append(lines, title, subtitle, "")

	// NIC picker
	nicLabel := styleLabel.Render("WAN network interface")
	var nicLine string
	if len(m.nics) == 0 {
		nicLine = m.wanNicManualInput.View()
	} else {
		nicLine = m.renderNICPicker(m.wanNicCursor, -1, m.wanFocus == wanFieldNIC)
	}
	lines = append(lines, nicLabel, nicLine, "")

	// IP method toggle
	methodLabel := styleLabel.Render("IP method")
	methods := []string{"DHCP (automatic)", "Static"}
	dhcpIdx := 0
	if !m.wanDHCP {
		dhcpIdx = 1
	}
	var methodParts []string
	for i, s := range methods {
		if i == dhcpIdx && m.wanFocus == wanFieldMethod {
			methodParts = append(methodParts, styleSelected.Render(" "+s+" "))
		} else if i == dhcpIdx {
			methodParts = append(methodParts, styleLabel.Render("["+s+"]"))
		} else {
			methodParts = append(methodParts, styleMuted.Render(s))
		}
	}
	lines = append(lines, methodLabel, strings.Join(methodParts, "  "), "")

	// Static fields
	if !m.wanDHCP {
		lines = append(lines,
			styleLabel.Render("IP address"), m.wanIP.View(), "",
			styleLabel.Render("Subnet mask"), m.wanMask.View(), "",
			styleLabel.Render("Default gateway"), m.wanGateway.View(), "",
			styleLabel.Render("DNS nameservers"), m.wanDNS.View(), "",
		)
	}

	// WiFi fields
	if len(m.nics) > 0 && m.nics[m.wanNicCursor].IsWiFi {
		lines = append(lines,
			styleLabel.Render("WiFi SSID"), m.wanSSID.View(), "",
			styleLabel.Render("WiFi password"), m.wanWiFiPass.View(), "",
		)
	}

	if m.validationErr != "" {
		lines = append(lines, styleError.Render(m.validationErr), "")
	}
	lines = append(lines, styleHelp.Render("Tab/↑↓ to move • ←/→ to select • Enter to proceed"))

	body := lipgloss.JoinVertical(lipgloss.Left, lines...)
	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center,
		styleBox.Width(min(w-4, 68)).Render(body),
	)
}

func (m model) viewNetworkLAN(w int) string {
	title := styleTitle.Render("LAN Network  (Step 2 of " + m.networkStepCount() + ")")
	subtitle := styleMuted.Render("Internal interface for EC2/VPC and Geneve tunnel traffic (br-lan).")

	var lines []string
	lines = append(lines, title, subtitle, "")

	// NIC picker — WAN NIC shown greyed
	nicLabel := styleLabel.Render("LAN network interface")
	nicLine := m.renderNICPicker(m.lanNicCursor, m.wanNicCursor, m.lanFocus == lanFieldNIC)
	lines = append(lines, nicLabel, nicLine, "")

	// IP method toggle
	methodLabel := styleLabel.Render("IP method")
	methods := []string{"DHCP (automatic)", "Static"}
	dhcpIdx := 0
	if !m.lanDHCP {
		dhcpIdx = 1
	}
	var methodParts []string
	for i, s := range methods {
		if i == dhcpIdx && m.lanFocus == lanFieldMethod {
			methodParts = append(methodParts, styleSelected.Render(" "+s+" "))
		} else if i == dhcpIdx {
			methodParts = append(methodParts, styleLabel.Render("["+s+"]"))
		} else {
			methodParts = append(methodParts, styleMuted.Render(s))
		}
	}
	lines = append(lines, methodLabel, strings.Join(methodParts, "  "), "")

	// Static fields (no gateway for LAN)
	if !m.lanDHCP {
		lines = append(lines,
			styleLabel.Render("IP address"), m.lanIP.View(), "",
			styleLabel.Render("Subnet mask"), m.lanMask.View(), "",
			styleLabel.Render("DNS nameservers"), m.lanDNS.View(), "",
		)
	}

	// WiFi fields
	if len(m.nics) > m.lanNicCursor && m.nics[m.lanNicCursor].IsWiFi {
		lines = append(lines,
			styleLabel.Render("WiFi SSID"), m.lanSSID.View(), "",
			styleLabel.Render("WiFi password"), m.lanWiFiPass.View(), "",
		)
	}

	if m.validationErr != "" {
		lines = append(lines, styleError.Render(m.validationErr), "")
	}
	lines = append(lines, styleHelp.Render("Tab/↑↓ to move • ←/→ to select • Enter to proceed"))

	body := lipgloss.JoinVertical(lipgloss.Left, lines...)
	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center,
		styleBox.Width(min(w-4, 68)).Render(body),
	)
}

func (m model) networkStepCount() string {
	if len(m.nics) > 1 {
		return "2"
	}
	return "1"
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
	title := styleTitle.Render("Root Password")
	passLabel := styleLabel.Render("Password")
	confirmLabel := styleLabel.Render("Confirm password")

	var lines []string
	lines = append(lines, title, "", passLabel, m.passwordInput.View(), "", confirmLabel, m.passwordConfirmInput.View())
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

	wanNet := "DHCP"
	if !cfg.WANDHCPMode {
		wanNet = fmt.Sprintf("%s/%s via %s", cfg.WANAddress, cfg.WANMask, cfg.WANGateway)
	}

	summary := []struct{ k, v string }{
		{"Disk", cfg.Disk},
		{"WAN interface", fmt.Sprintf("%s → br-wan", cfg.WANInterface)},
		{"WAN address", wanNet},
	}
	if cfg.LANInterface != "" {
		lanNet := "DHCP"
		if !cfg.LANDHCPMode {
			lanNet = fmt.Sprintf("%s/%s", cfg.LANAddress, cfg.LANMask)
		}
		summary = append(summary,
			struct{ k, v string }{"LAN interface", fmt.Sprintf("%s → br-lan", cfg.LANInterface)},
			struct{ k, v string }{"LAN address", lanNet},
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

	// WAN
	if len(m.nics) > m.wanNicCursor {
		cfg.WANInterface = m.nics[m.wanNicCursor].Name
		if m.nics[m.wanNicCursor].IsWiFi {
			cfg.WANWiFiSSID = strings.TrimSpace(m.wanSSID.Value())
			cfg.WANWiFiPass = m.wanWiFiPass.Value()
		}
	} else {
		cfg.WANInterface = strings.TrimSpace(m.wanNicManualInput.Value())
	}
	cfg.WANDHCPMode = m.wanDHCP
	if !m.wanDHCP {
		cfg.WANAddress = strings.TrimSpace(m.wanIP.Value())
		cfg.WANMask = strings.TrimSpace(m.wanMask.Value())
		cfg.WANGateway = strings.TrimSpace(m.wanGateway.Value())
		cfg.WANDNS = parseDNS(m.wanDNS.Value())
	}

	// LAN (only when 2+ NICs)
	if len(m.nics) > 1 && m.lanNicCursor < len(m.nics) {
		cfg.LANInterface = m.nics[m.lanNicCursor].Name
		cfg.LANDHCPMode = m.lanDHCP
		if !m.lanDHCP {
			cfg.LANAddress = strings.TrimSpace(m.lanIP.Value())
			cfg.LANMask = strings.TrimSpace(m.lanMask.Value())
			cfg.LANDNS = parseDNS(m.lanDNS.Value())
		}
		if m.nics[m.lanNicCursor].IsWiFi {
			cfg.LANWiFiSSID = strings.TrimSpace(m.lanSSID.Value())
			cfg.LANWiFiPass = m.lanWiFiPass.Value()
		}
	}

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

func availableNICs() ([]nicInfo, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var nics []nicInfo
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		nics = append(nics, nicInfo{
			Name:   iface.Name,
			IsWiFi: isWiFiNIC(iface.Name),
		})
	}
	return nics, nil
}

// isWiFiNIC returns true if the interface has a wireless subdirectory in sysfs.
func isWiFiNIC(name string) bool {
	_, err := os.Stat("/sys/class/net/" + name + "/wireless")
	return err == nil
}
