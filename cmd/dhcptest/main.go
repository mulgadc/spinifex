// dhcptest is a diagnostic tool that runs a DHCP DORA handshake using the
// same nclient4 library and options that spinifex-vpcd uses for gateway pool
// leases. Run it on a node to verify DHCP works at the library level before
// debugging the full spinifex stack.
//
// Usage (must run as root — needs AF_PACKET):
//
//	sudo dhcptest --iface=br-wan
//	sudo dhcptest --iface=br-wan --mac=02:00:00:7c:b7:b9 --promisc
//	sudo dhcptest --iface=br-wan --spinifex-id=gateway-wan1
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/mulgadc/spinifex/spinifex/network/external/dhcp"
)

func main() {
	iface := flag.String("iface", "br-wan", "Interface/bridge to bind the DHCP client to")
	macStr := flag.String("mac", "", "Hardware address for DORA (chaddr + AF_PACKET filter). If empty, uses the real interface MAC.")
	spinifexID := flag.String("spinifex-id", "", "Generate a spinifex virtual MAC from this resource ID (e.g. 'gateway-wan1'). Overrides --mac.")
	clientID := flag.String("client-id", "", "DHCP option 61 client identifier. Defaults to --spinifex-id or --mac value.")
	hostname := flag.String("hostname", "", "DHCP option 12 hostname")
	vendorClass := flag.String("vendor-class", "mulga-spinifex-gw", "DHCP option 60 vendor class identifier")
	promisc := flag.Bool("promisc", false, "Enable promiscuous mode on the interface before the test (mirrors vpcd's setBridgePromisc)")
	timeout := flag.Duration("timeout", 10*time.Second, "Per-attempt DHCP timeout")
	retries := flag.Int("retries", 3, "Number of DORA attempts before giving up")
	verbose := flag.Bool("verbose", false, "Print full DHCP packet contents")
	flag.Parse()

	// Resolve the hardware address to use.
	var hwAddr net.HardwareAddr
	var err error

	switch {
	case *spinifexID != "":
		hwAddr, err = net.ParseMAC(generateMAC(*spinifexID))
		if err != nil {
			slog.Error("Could not parse generated MAC", "spinifex-id", *spinifexID, "error", err)
			os.Exit(1)
		}
		fmt.Printf("spinifex virtual MAC for %q: %s\n", *spinifexID, hwAddr)
	case *macStr != "":
		hwAddr, err = net.ParseMAC(*macStr)
		if err != nil {
			slog.Error("Could not parse --mac", "mac", *macStr, "error", err)
			os.Exit(1)
		}
	default:
		hwAddr, err = ifaceMAC(*iface)
		if err != nil {
			slog.Error("Could not get MAC for interface", "iface", *iface, "error", err)
			os.Exit(1)
		}
		fmt.Printf("using real interface MAC: %s\n", hwAddr)
	}

	// Resolve client-id: default to spinifex-id, then MAC string.
	cid := *clientID
	if cid == "" {
		if *spinifexID != "" {
			cid = *spinifexID
		} else {
			cid = hwAddr.String()
		}
	}

	fmt.Printf("iface:        %s\n", *iface)
	fmt.Printf("hw-addr:      %s\n", hwAddr)
	fmt.Printf("client-id:    %s\n", cid)
	fmt.Printf("hostname:     %s\n", *hostname)
	fmt.Printf("vendor-class: %s\n", *vendorClass)
	fmt.Printf("promisc:      %v\n", *promisc)
	fmt.Printf("timeout:      %s  retries: %d\n", *timeout, *retries)
	fmt.Println()

	if *promisc {
		if err := setPromisc(*iface, true); err != nil {
			slog.Warn("Failed to set promisc on interface", "iface", *iface, "error", err)
		} else {
			fmt.Printf("set %s promisc ON\n\n", *iface)
		}
	}

	// Build nclient4 options — standalone DHCP probe for upstream-router debugging.
	opts := []nclient4.ClientOpt{
		nclient4.WithHWAddr(hwAddr),
		nclient4.WithTimeout(*timeout),
		nclient4.WithRetry(*retries),
	}
	if *verbose {
		opts = append(opts, nclient4.WithLogger(nclient4.ShortSummaryLogger{Printfer: slogPrintfer{}}))
	}

	client, err := nclient4.New(*iface, opts...)
	if err != nil {
		slog.Error("Could not create nclient4 client", "iface", *iface, "error", err)
		os.Exit(1)
	}
	defer client.Close()

	// Identity modifiers — client-id / hostname / vendor-class for upstream lease
	// tagging. Built by the vpcd code path so the probe cannot drift from it.
	mods := dhcp.IdentityModifiers(cid, *hostname, *vendorClass, hwAddr)

	fmt.Println("Sending DHCP Discover...")
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout*time.Duration(*retries+1))
	defer cancel()

	lease, err := client.Request(ctx, mods...)
	if err != nil {
		fmt.Printf("\nFAILED after %s: %v\n", time.Since(start).Round(time.Millisecond), err)
		os.Exit(1)
	}

	fmt.Printf("SUCCESS in %s\n\n", time.Since(start).Round(time.Millisecond))
	ack := lease.ACK
	fmt.Printf("  Your IP:     %s\n", ack.YourIPAddr)
	fmt.Printf("  Subnet:      %s\n", ack.SubnetMask())
	fmt.Printf("  Gateway:     %v\n", ack.Router())
	fmt.Printf("  DNS:         %v\n", ack.DNS())
	fmt.Printf("  Server ID:   %s\n", ack.ServerIdentifier())
	fmt.Printf("  Lease time:  %s\n", ack.IPAddressLeaseTime(24*time.Hour))
	if *verbose {
		fmt.Printf("\n--- Full ACK ---\n%s\n", ack.Summary())
	}
}

// slogPrintfer adapts nclient4's Printfer to slog. nclient4 only emits through
// it under --verbose, so the packet summaries log at info level to stay visible.
type slogPrintfer struct{}

func (slogPrintfer) Printf(format string, v ...any) {
	slog.Info(fmt.Sprintf(format, v...), "component", "nclient4")
}

// generateMAC is the same deterministic hash used by vpcd's topology.go.
// It produces a locally-administered MAC (02:00:00:xx:xx:xx) from a resource ID.
func generateMAC(resourceID string) string {
	h := uint32(0)
	for _, c := range resourceID {
		h = h*31 + uint32(c)
	}
	return fmt.Sprintf("02:00:00:%02x:%02x:%02x", (h>>16)&0xff, (h>>8)&0xff, h&0xff)
}

func ifaceMAC(name string) (net.HardwareAddr, error) {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	return ifc.HardwareAddr, nil
}

func setPromisc(iface string, on bool) error {
	state := "on"
	if !on {
		state = "off"
	}
	out, err := exec.Command("ip", "link", "set", iface, "promisc", state).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
