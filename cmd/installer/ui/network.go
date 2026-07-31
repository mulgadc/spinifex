package ui

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mulgadc/spinifex/cmd/installer/install"
	"github.com/mulgadc/spinifex/cmd/installer/netprobe"
)

// foldedNIC marks a role with no interface of its own, which collapses onto
// the plane above it (vpc <- lan <- wan).
const foldedNIC = -1

// roleField identifies one editable element on the per-role screen. Which of
// these are actually shown depends on the plane, the addressing mode, whether
// the NIC is wireless, and whether Advanced is on.
type roleField int

const (
	roleFieldNIC roleField = iota
	roleFieldMethod
	roleFieldIP
	roleFieldMask
	roleFieldGateway
	roleFieldDNS
	roleFieldVLAN
	roleFieldMTU
	roleFieldSSID
	roleFieldWiFiPass
)

// roleForm is the editable state for one plane. The three forms are the whole
// of the installer's network configuration.
type roleForm struct {
	plane install.Plane

	// nic indexes into model.nics, or foldedNIC when the role is folded.
	nic   int
	dhcp  bool
	focus roleField

	ip       textinput.Model
	mask     textinput.Model
	gateway  textinput.Model
	dns      textinput.Model
	vlan     textinput.Model
	mtu      textinput.Model
	ssid     textinput.Model
	wifiPass textinput.Model
}

func newRoleForm(plane install.Plane, nic int) roleForm {
	text := func(placeholder string, limit int) textinput.Model {
		in := textinput.New()
		in.Prompt = ""
		in.Placeholder = placeholder
		if limit > 0 {
			in.CharLimit = limit
		}
		return in
	}

	f := roleForm{
		plane:    plane,
		nic:      nic,
		dhcp:     plane == install.PlaneWAN,
		ip:       text(defaultIPPlaceholder(plane), 0),
		mask:     text("255.255.255.0", 0),
		gateway:  text("192.168.1.1", 0),
		dns:      text("1.1.1.1, 8.8.8.8", 0),
		vlan:     text("untagged", 4),
		mtu:      text("1500", 5),
		ssid:     text("Network SSID", 64),
		wifiPass: text("WiFi password", 128),
	}
	f.wifiPass.EchoMode = textinput.EchoPassword
	return f
}

// defaultIPPlaceholder hints the addressing convention for each plane so an
// operator filling in a rack does not have to remember it.
func defaultIPPlaceholder(plane install.Plane) string {
	switch plane {
	case install.PlaneLAN:
		return "10.0.0.3"
	case install.PlaneVPC:
		return "10.1.0.3"
	default:
		return "192.168.1.10"
	}
}

func (f *roleForm) folded() bool { return f.nic == foldedNIC }

// canFold reports whether this role may be left unbound. wan always needs an
// uplink, so only lan and vpc can fold.
func (f *roleForm) canFold() bool { return f.plane != install.PlaneWAN }

// visibleFields returns the fields shown for this form, in tab order. Hiding
// rather than disabling keeps the default screen narrow: a DHCP role shows two
// lines, and VLAN/MTU only appear once Advanced is on.
func (f *roleForm) visibleFields(isWiFi, advanced bool) []roleField {
	if f.folded() {
		return []roleField{roleFieldNIC}
	}
	fields := []roleField{roleFieldNIC, roleFieldMethod}
	if !f.dhcp {
		fields = append(fields, roleFieldIP, roleFieldMask)
		// Default route and resolvers are properties of the node, not of each
		// plane, and wan is the only one that carries either: it holds the
		// default route, so it is the only plane that can reach an upstream
		// resolver. On DHCP the lease supplies both.
		if f.plane == install.PlaneWAN {
			fields = append(fields, roleFieldGateway, roleFieldDNS)
		}
	}
	if advanced {
		fields = append(fields, roleFieldVLAN, roleFieldMTU)
	}
	if isWiFi {
		fields = append(fields, roleFieldSSID, roleFieldWiFiPass)
	}
	return fields
}

// input returns the textinput backing a field, or nil for the non-text fields.
func (f *roleForm) input(field roleField) *textinput.Model {
	switch field {
	case roleFieldIP:
		return &f.ip
	case roleFieldMask:
		return &f.mask
	case roleFieldGateway:
		return &f.gateway
	case roleFieldDNS:
		return &f.dns
	case roleFieldVLAN:
		return &f.vlan
	case roleFieldMTU:
		return &f.mtu
	case roleFieldSSID:
		return &f.ssid
	case roleFieldWiFiPass:
		return &f.wifiPass
	default:
		return nil
	}
}

// blurAll drops focus from every input so only the focused field has a cursor.
func (f *roleForm) blurAll() {
	for _, field := range []roleField{
		roleFieldIP, roleFieldMask, roleFieldGateway, roleFieldDNS,
		roleFieldVLAN, roleFieldMTU, roleFieldSSID, roleFieldWiFiPass,
	} {
		f.input(field).Blur()
	}
}

// focusCurrent gives the cursor to the focused field, if it is a text field.
func (f *roleForm) focusCurrent() {
	f.blurAll()
	if in := f.input(f.focus); in != nil {
		in.Focus()
	}
}

// toRole converts the form into the install package's role model.
func (f *roleForm) toRole(nics []netprobe.NIC) install.NetworkRole {
	if f.folded() || f.nic >= len(nics) {
		return install.NetworkRole{}
	}
	role := install.NetworkRole{
		Interface: nics[f.nic].Name,
		DHCPMode:  f.dhcp,
		VLAN:      atoiOr(f.vlan.Value(), 0),
		MTU:       atoiOr(f.mtu.Value(), 0),
	}
	if !f.dhcp {
		role.Address = strings.TrimSpace(f.ip.Value())
		role.Mask = strings.TrimSpace(f.mask.Value())
		role.DNS = parseDNS(f.dns.Value())
		if f.plane == install.PlaneWAN {
			role.Gateway = strings.TrimSpace(f.gateway.Value())
		}
	}
	if nics[f.nic].IsWiFi {
		role.WiFiSSID = strings.TrimSpace(f.ssid.Value())
		role.WiFiPass = f.wifiPass.Value()
	}
	return role
}

func atoiOr(s string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fallback
	}
	return n
}

// ── Roles overview screen ─────────────────────────────────────────────────────

// continueRow is the cursor position below the three role rows.
const continueRow = 3

func (m model) handleRolesKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "up", "k":
		if m.roleCursor > 0 {
			m.roleCursor--
		}
	case "down", "j":
		if m.roleCursor < continueRow {
			m.roleCursor++
		}
	case "a":
		// Advanced reveals VLAN and MTU on every role at once, rather than
		// per-role, so the operator flips it on for the whole node.
		m.advanced = !m.advanced
		m.validationErr = ""
	case "enter":
		if m.roleCursor == continueRow {
			if err := m.buildConfig().Validate(); err != nil {
				m.validationErr = capitalise(err.Error())
				return m, nil
			}
			m.validationErr = ""
			m.screen = screenIdentity
			m.hostnameInput.Focus()
			return m, nil
		}
		m.validationErr = ""
		m.screen = screenNetworkRole
		m.roles[m.roleCursor].focus = roleFieldNIC
		m.roles[m.roleCursor].blurAll()
	case "esc":
		m.screen = screenDiskConfirm
		m.eraseInput.Focus()
	}
	return m, nil
}

func (m model) viewNetworkRoles(w int) string {
	title := styleTitle.Render("Network Planes")
	subtitle := styleMuted.Render("A role left unbound folds onto the plane above it.")

	adv := styleMuted.Render("Advanced: off")
	if m.advanced {
		adv = styleSuccess.Render("Advanced: on")
	}

	header := fmt.Sprintf("  %-*s%-*s", colRole, "ROLE", colIface, "INTERFACE")
	if m.advanced {
		header += fmt.Sprintf("%-*s%-*s", colVLAN, "VLAN", colMTU, "MTU")
	}
	header += "ADDRESS"

	lines := []string{title, subtitle, "", adv, "", styleMuted.Render(header)}

	for i := range m.roles {
		row := m.renderRoleRow(i)
		if i == m.roleCursor {
			lines = append(lines, styleSelected.Render("> "+row))
		} else {
			lines = append(lines, "  "+row)
		}
		if detail := m.roleHardware(i, bodyWidth(w)); detail != "" {
			lines = append(lines, detail)
		}
	}

	lines = append(lines, "")
	cont := "  Continue"
	if m.roleCursor == continueRow {
		cont = styleSelected.Render("> Continue")
	}
	lines = append(lines, cont)

	if m.validationErr != "" {
		lines = append(lines, "", styleError.Render(m.validationErr))
	}
	lines = append(lines, styleHelp.Render("↑/↓ move • Enter edit • a advanced • Esc back"))

	body := lipgloss.JoinVertical(lipgloss.Left, lines...)
	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center,
		styleBox.Width(min(w-4, 78)).Render(body),
	)
}

// renderRoleRow renders one line of the overview table. A folded role names
// the plane it collapsed onto rather than showing a blank, so the operator can
// always see where the traffic will actually go.
func (m model) renderRoleRow(i int) string {
	f := &m.roles[i]
	name := fmt.Sprintf("%-5s", f.plane)

	if f.folded() {
		_, landed := m.buildConfig().Resolve(f.plane)
		return fmt.Sprintf("%-*s%s", colRole, name, styleMuted.Render("folds onto "+string(landed)))
	}

	iface := styleMuted.Render("—")
	if f.nic < len(m.nics) {
		iface = nicSummary(m.nics[f.nic])
	}

	addr := "DHCP"
	if !f.dhcp {
		addr = formatCIDR(f.ip.Value(), f.mask.Value())
		if addr == "" {
			addr = styleWarning.Render("not set")
		}
	}

	row := padCell(name, colRole) + padCell(iface, colIface)
	if m.advanced {
		row += padCell(valueOr(f.vlan.Value(), "—"), colVLAN) + padCell(valueOr(f.mtu.Value(), "—"), colMTU)
	}
	return row + addr
}

// roleHardware is the vendor/model line beneath a role row, indented under the
// interface column. It is what tells an operator that wan really is the 25gbe
// Mellanox and not the onboard 1gbe.
func (m model) roleHardware(i, width int) string {
	f := &m.roles[i]
	if f.folded() || f.nic >= len(m.nics) {
		return ""
	}
	indent := 2 + colRole
	return strings.Repeat(" ", indent) + styleMuted.Render(nicHardware(m.nics[f.nic], width-indent))
}

// nicSummary renders name, speed and link state, colouring the state so a port
// with no cable stands out rather than having to be read for.
func nicSummary(n netprobe.NIC) string {
	state := styleWarning.Render(n.State)
	if n.Carrier {
		state = styleSuccess.Render(n.State)
	}
	speed := n.Speed
	if speed == "" {
		speed = "—"
	}
	return fmt.Sprintf("%s  %s  %s", n.Name, speed, state)
}

// nicHardware is the detail line: what the card is, plus the predictable name
// it also answers to, which is the one written on cabling notes. The alt name
// is dropped rather than truncated when the two will not fit together.
func nicHardware(n netprobe.NIC, width int) string {
	hw := n.Hardware()
	const separator = "  ·  "
	if n.AltName != "" && len([]rune(hw))+len(separator)+len([]rune(n.AltName)) <= width {
		return hw + separator + n.AltName
	}
	return truncate(hw, width)
}

// bodyWidth is the text width inside the framed body. Detail lines are trimmed
// to it rather than left to wrap, which would push the table out of alignment.
func bodyWidth(w int) int {
	return min(w-4, 78) - 4
}

// padCell pads a cell to width by display width, which the %-*s verb cannot do
// once a cell carries the ANSI escapes of a style.
func padCell(s string, width int) string {
	return lipgloss.PlaceHorizontal(width, lipgloss.Left, s)
}

// Column widths for the overview table, shared by the header and the rows.
const (
	colRole  = 6
	colIface = 32
	colVLAN  = 7
	colMTU   = 7
)

// focusMark gives the focused text field a visible caret without every field
// rendering its own prompt.
func focusMark(focused bool) string {
	if focused {
		return styleLabel.Render("> ")
	}
	return "  "
}

// formatCIDR renders an address and mask the way an operator writes them,
// falling back to the raw mask when it is not dotted-decimal.
func formatCIDR(addr, mask string) string {
	addr = strings.TrimSpace(addr)
	mask = strings.TrimSpace(mask)
	if addr == "" {
		return ""
	}
	if mask == "" {
		return addr
	}
	if ip := net.ParseIP(mask); ip != nil && ip.To4() != nil {
		if ones, bits := net.IPMask(ip.To4()).Size(); bits != 0 {
			return fmt.Sprintf("%s/%d", addr, ones)
		}
	}
	return addr + "/" + strings.TrimPrefix(mask, "/")
}

func valueOr(v, fallback string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return fallback
}

// ── Per-role editor screen ────────────────────────────────────────────────────

func (m model) handleRoleKey(key string, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := &m.roles[m.roleCursor]
	fields := f.visibleFields(m.nicIsWiFi(f.nic), m.advanced)
	pos := fieldIndex(fields, f.focus)

	switch key {
	case "esc":
		f.blurAll()
		m.screen = screenNetworkRoles
		m.validationErr = ""
		return m, nil

	case "enter":
		if err := m.validateRole(m.roleCursor); err != "" {
			m.validationErr = err
			return m, nil
		}
		f.blurAll()
		m.screen = screenNetworkRoles
		m.validationErr = ""
		return m, nil

	case "tab", "down":
		pos = (pos + 1) % len(fields)
		f.focus = fields[pos]
		f.focusCurrent()
		return m, nil

	case "shift+tab", "up":
		pos = (pos - 1 + len(fields)) % len(fields)
		f.focus = fields[pos]
		f.focusCurrent()
		return m, nil

	case "left", "right":
		// Only the two selector fields consume arrows; text fields need them
		// for cursor movement.
		switch f.focus {
		case roleFieldNIC:
			return m.cycleNIC(m.roleCursor, key == "right"), nil
		case roleFieldMethod:
			f.dhcp = key == "left"
			f.focusCurrent()
			return m, nil
		}
	}

	if in := f.input(f.focus); in != nil {
		var cmd tea.Cmd
		*in, cmd = in.Update(msg)
		return m, cmd
	}
	return m, nil
}

// cycleNIC steps the interface selection, including the fold option for lan
// and vpc. Wrapping keeps a single key usable on a node with one NIC.
func (m model) cycleNIC(idx int, forward bool) model {
	f := &m.roles[idx]
	low := 0
	if f.canFold() {
		low = foldedNIC
	}
	high := len(m.nics) - 1

	if forward {
		if f.nic >= high {
			f.nic = low
		} else {
			f.nic++
		}
		return m
	}
	if f.nic <= low {
		f.nic = high
	} else {
		f.nic--
	}
	return m
}

func (m model) nicIsWiFi(idx int) bool {
	return idx >= 0 && idx < len(m.nics) && m.nics[idx].IsWiFi
}

func (m model) viewNetworkRole(w int) string {
	f := &m.roles[m.roleCursor]
	title := styleTitle.Render(fmt.Sprintf("Configure %s plane", strings.ToUpper(string(f.plane))))
	subtitle := styleMuted.Render(planeDescription(f.plane))

	lines := []string{title, subtitle, ""}
	for _, field := range f.visibleFields(m.nicIsWiFi(f.nic), m.advanced) {
		lines = append(lines, m.renderRoleField(f, field, bodyWidth(w)), "")
	}

	if m.validationErr != "" {
		lines = append(lines, styleError.Render(m.validationErr), "")
	}
	lines = append(lines, styleHelp.Render("Tab/↑↓ move • ←/→ change • Enter done • Esc cancel"))

	body := lipgloss.JoinVertical(lipgloss.Left, lines...)
	return lipgloss.Place(w, m.height, lipgloss.Center, lipgloss.Center,
		styleBox.Width(min(w-4, 78)).Render(body),
	)
}

// planeDescription explains what rides each plane, so the choice is not just a
// three-letter name.
func planeDescription(p install.Plane) string {
	switch p {
	case install.PlaneWAN:
		return "Public ingress and egress, EIPs and NAT (br-wan)."
	case install.PlaneLAN:
		return "Storage replication, NATS mesh and OVN control (br-lan)."
	default:
		return "Geneve tunnels carrying EC2 traffic between nodes (br-vpc)."
	}
}

// roleLabelWidth is the field-label column on the editor screen, shared so a
// wrapped value can indent to line up underneath it.
const roleLabelWidth = 14

func (m model) renderRoleField(f *roleForm, field roleField, width int) string {
	focused := f.focus == field
	label := styleLabel.Render(fmt.Sprintf("%-*s", roleLabelWidth, roleFieldLabel(field)))

	switch field {
	case roleFieldNIC:
		return label + m.renderNICChoice(f, focused, width)

	case roleFieldMethod:
		return label + renderToggle([]string{"DHCP (automatic)", "Static"}, boolToIdx(!f.dhcp), focused)

	default:
		if in := f.input(field); in != nil {
			return label + focusMark(focused) + in.View()
		}
		return label
	}
}

func roleFieldLabel(field roleField) string {
	switch field {
	case roleFieldNIC:
		return "Interface"
	case roleFieldMethod:
		return "IP method"
	case roleFieldIP:
		return "IP address"
	case roleFieldMask:
		return "Subnet mask"
	case roleFieldGateway:
		return "Gateway"
	case roleFieldDNS:
		return "DNS"
	case roleFieldVLAN:
		return "VLAN id"
	case roleFieldMTU:
		return "MTU"
	case roleFieldSSID:
		return "WiFi SSID"
	default:
		return "WiFi password"
	}
}

// renderNICChoice shows the selected interface on the field line and its
// hardware identity on a line of its own beneath. Vendor and model are what
// make a port identifiable, and they do not fit alongside the selector.
func (m model) renderNICChoice(f *roleForm, focused bool, width int) string {
	var text, detail string
	switch {
	case f.folded():
		_, landed := m.buildConfig().Resolve(f.plane)
		text = fmt.Sprintf("(fold onto %s)", landed)
	case f.nic < len(m.nics):
		text = nicSummary(m.nics[f.nic])
		detail = nicHardware(m.nics[f.nic], width-roleLabelWidth-2)
	default:
		text = "(none detected)"
	}

	if focused {
		text = styleSelected.Render(" ← " + text + " → ")
	} else {
		text = styleLabel.Render("[ " + text + " ]")
	}
	if detail == "" {
		return text
	}
	return text + "\n" + strings.Repeat(" ", roleLabelWidth+2) + styleMuted.Render(detail)
}

func renderToggle(options []string, selected int, focused bool) string {
	var parts []string
	for i, opt := range options {
		switch {
		case i == selected && focused:
			parts = append(parts, styleSelected.Render(" "+opt+" "))
		case i == selected:
			parts = append(parts, styleLabel.Render("["+opt+"]"))
		default:
			parts = append(parts, styleMuted.Render(opt))
		}
	}
	return strings.Join(parts, "  ")
}

func boolToIdx(b bool) int {
	if b {
		return 1
	}
	return 0
}

func fieldIndex(fields []roleField, want roleField) int {
	for i, f := range fields {
		if f == want {
			return i
		}
	}
	return 0
}

// validateRole checks one plane's entries. Cross-role rules (a shared NIC
// without distinct VLAN ids, an unbound wan) belong to install.Config.Validate
// and run when the operator continues.
func (m model) validateRole(idx int) string {
	f := &m.roles[idx]
	if f.folded() {
		return ""
	}
	if len(m.nics) == 0 {
		return "No network interfaces detected"
	}

	if !f.dhcp {
		if net.ParseIP(strings.TrimSpace(f.ip.Value())) == nil {
			return "Enter a valid IP address"
		}
		if !validSubnetMask(f.mask.Value()) {
			return "Enter a dotted-decimal subnet mask, e.g. 255.255.255.0 or 255.255.0.0"
		}
		if f.plane == install.PlaneWAN && net.ParseIP(strings.TrimSpace(f.gateway.Value())) == nil {
			return "Enter a valid gateway address"
		}
	}

	if v := strings.TrimSpace(f.vlan.Value()); v != "" {
		id, err := strconv.Atoi(v)
		if err != nil || id < 1 || id > 4094 {
			return "VLAN id must be between 1 and 4094"
		}
	}
	if v := strings.TrimSpace(f.mtu.Value()); v != "" {
		mtu, err := strconv.Atoi(v)
		if err != nil || mtu < 68 || mtu > 9216 {
			return "MTU must be between 68 and 9216"
		}
	}
	if m.nicIsWiFi(f.nic) && strings.TrimSpace(f.ssid.Value()) == "" {
		return "Enter the WiFi network SSID"
	}
	return ""
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
