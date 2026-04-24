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

// Package install performs the disk installation steps after the UI has
// collected configuration.
package install

// Config holds all values collected by the installer UI.
type Config struct {
	// Disk is the block device path to install onto (e.g. /dev/sda).
	Disk string

	// WAN interface — always present. The management IP lives on br-wan (a Linux
	// bridge over the NIC), not on the physical NIC itself. This allows OVN/OVS
	// to safely attach to the bridge without disrupting host connectivity.
	WANInterface string
	WANDHCPMode  bool
	WANAddress   string   // empty if WANDHCPMode
	WANMask      string   // empty if WANDHCPMode
	WANGateway   string   // empty if WANDHCPMode
	WANDNS       []string // empty if WANDHCPMode; e.g. ["1.1.1.1", "8.8.8.8"]
	WANWiFiSSID  string   // non-empty if a WiFi NIC was selected
	WANWiFiPass  string   // non-empty if a WiFi NIC was selected

	// LAN interface — only set when 2+ NICs are present. The Geneve encap IP
	// for OVN tunnels lives on br-lan. When empty, the WAN address is used.
	LANInterface string
	LANDHCPMode  bool
	LANAddress   string
	LANMask      string
	LANDNS       []string
	LANWiFiSSID  string
	LANWiFiPass  string

	// Node identity
	Hostname string

	// ClusterRole is "init" or "join".
	ClusterRole string

	// JoinAddr is the primary node address (host:port) when ClusterRole is "join".
	JoinAddr string

	// CA certificate (PEM), optional.
	HasCACert bool
	CACert    string

	// RootPassword is the password to set for the root account on the installed system.
	RootPassword string

	// Email is the operator's email address, used by the call-home telemetry
	// endpoint to notify of important system updates and security advisories.
	// Required on interactive installs; may be empty on headless/CI installs
	// when SPINIFEX_EMAIL is not supplied on the kernel cmdline.
	Email string
}
