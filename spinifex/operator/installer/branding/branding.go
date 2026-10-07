// Package branding centralises Mulga/Spinifex visual identity constants used
// by the installer TUI.
//
// TODO: Replace placeholder values with official Mulga/Spinifex assets once
// the brand guide and logo files are located.
package branding

import "github.com/charmbracelet/lipgloss"

// Logo is the ASCII art rendered on the installer welcome screen.
// TODO: Replace with official Spinifex/Mulga ASCII logo.
const Logo = `
  ███████╗██████╗ ██╗███╗   ██╗██╗███████╗███████╗██╗  ██╗
  ██╔════╝██╔══██╗██║████╗  ██║██║██╔════╝██╔════╝╚██╗██╔╝
  ███████╗██████╔╝██║██╔██╗ ██║██║█████╗  █████╗   ╚███╔╝
  ╚════██║██╔═══╝ ██║██║╚██╗██║██║██╔══╝  ██╔══╝   ██╔██╗
  ███████║██║     ██║██║ ╚████║██║██║     ███████╗██╔╝ ██╗
  ╚══════╝╚═╝     ╚═╝╚═╝  ╚═══╝╚═╝╚═╝     ╚══════╝╚═╝  ╚═╝`

// Subtitle is displayed beneath the logo on the welcome screen.
const Subtitle = "Bare-Metal Node Provisioner"

// Publisher is shown in the welcome screen footer.
const Publisher = "Mulga Defense Corporation"

// Mulga color palette.
// TODO: Replace with official hex values from the Mulga brand guide.
const (
	ColorPrimary    = lipgloss.Color("#00A3E0") // Mulga blue (placeholder)
	ColorAccent     = lipgloss.Color("#FF6B00") // Mulga orange (placeholder)
	ColorBackground = lipgloss.Color("#0D1117") // Dark background
	ColorSurface    = lipgloss.Color("#161B22") // Slightly lighter surface
	ColorText       = lipgloss.Color("#E6EDF3") // Light text
	ColorMuted      = lipgloss.Color("#8B949E") // Muted / secondary text
	ColorSuccess    = lipgloss.Color("#3FB950") // Green
	ColorWarning    = lipgloss.Color("#D29922") // Amber
	ColorError      = lipgloss.Color("#F85149") // Red
	ColorBorder     = lipgloss.Color("#30363D") // Border
)
