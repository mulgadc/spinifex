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
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
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
			log.Fatalf("generateMAC(%q): %v", *spinifexID, err)
		}
		fmt.Printf("spinifex virtual MAC for %q: %s\n", *spinifexID, hwAddr)
	case *macStr != "":
		hwAddr, err = net.ParseMAC(*macStr)
		if err != nil {
			log.Fatalf("parse --mac %q: %v", *macStr, err)
		}
	default:
		hwAddr, err = ifaceMAC(*iface)
		if err != nil {
			log.Fatalf("get MAC for %s: %v", *iface, err)
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
			log.Printf("WARNING: failed to set promisc on %s: %v", *iface, err)
		} else {
			fmt.Printf("set %s promisc ON\n\n", *iface)
		}
	}

	// Build nclient4 options — identical to spinifex/services/vpcd/dhcp/nclient4.go.
	opts := []nclient4.ClientOpt{
		nclient4.WithHWAddr(hwAddr),
		nclient4.WithTimeout(*timeout),
		nclient4.WithRetry(*retries),
	}
	if *verbose {
		opts = append(opts, nclient4.WithLogger(nclient4.ShortSummaryLogger{Printfer: log.New(os.Stderr, "[nclient4] ", 0)}))
	}

	client, err := nclient4.New(*iface, opts...)
	if err != nil {
		log.Fatalf("nclient4.New(%s): %v", *iface, err)
	}
	defer client.Close()

	// Identity modifiers — identical to identityModifiers() in vpcd dhcp package.
	var mods []dhcpv4.Modifier
	if cid != "" {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptClientIdentifier([]byte(cid))))
	}
	if *hostname != "" {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptHostName(*hostname)))
	}
	if *vendorClass != "" {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptClassIdentifier(*vendorClass)))
	}

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
