package dhcp

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// socketTimeout must never cap the caller's budget, because nclient4 ends an
// attempt on whichever of the two deadlines fires first.
func TestSocketTimeoutTracksContextDeadline(t *testing.T) {
	c := NewNClient4(5 * time.Second)

	t.Run("no deadline falls back to the configured timeout", func(t *testing.T) {
		if got := c.socketTimeout(context.Background()); got != 5*time.Second {
			t.Fatalf("socketTimeout = %v, want 5s", got)
		}
	})

	t.Run("longer deadline is honoured rather than capped", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 32*time.Second)
		defer cancel()
		got := c.socketTimeout(ctx)
		if got <= 5*time.Second {
			t.Fatalf("socketTimeout = %v, want the remaining ~32s, not the 5s fallback", got)
		}
		// Strictly beyond the caller's deadline so ctx.Done() reports the timeout.
		if got <= 32*time.Second {
			t.Fatalf("socketTimeout = %v, want more than the remaining 32s", got)
		}
	})

	t.Run("shorter deadline shortens the read", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		got := c.socketTimeout(ctx)
		if got <= time.Second || got > 2*time.Second {
			t.Fatalf("socketTimeout = %v, want just beyond the remaining 1s", got)
		}
	})

	t.Run("expired deadline yields a positive timeout", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		// nclient4 rejects a non-positive read deadline; ctx.Done() ends the
		// attempt immediately regardless of what is passed here.
		if got := c.socketTimeout(ctx); got != 5*time.Second {
			t.Fatalf("socketTimeout = %v, want the 5s fallback", got)
		}
	})
}

// Option 61 carries a leading hardware-type byte. Omitting it makes the server
// consume the identifier's first character as the type, which is how leases end
// up in the upstream table with no hardware address at all.
func TestClientIDOptionCarriesHardwareType(t *testing.T) {
	hw, err := net.ParseMAC("02:0c:46:55:cf:ed")
	if err != nil {
		t.Fatalf("parse mac: %v", err)
	}

	t.Run("ethernet address is typed 1 and sent verbatim", func(t *testing.T) {
		got := clientIDOption("dhcp-gw-lrp-vpc-abc", hw).Value.ToBytes()
		want := append([]byte{0x01}, hw...)
		if !bytes.Equal(got, want) {
			t.Fatalf("client-id = %v, want %v", got, want)
		}
	})

	t.Run("without a hardware address the id is typed 0", func(t *testing.T) {
		got := clientIDOption("eni-1234", nil).Value.ToBytes()
		want := append([]byte{0x00}, "eni-1234"...)
		if !bytes.Equal(got, want) {
			t.Fatalf("client-id = %v, want %v", got, want)
		}
	})

	t.Run("no leading byte is mistaken for a hardware type", func(t *testing.T) {
		// 'd' is 0x64, so the unfixed encoding announced hardware type 100.
		got := clientIDOption("dhcp-gw-lrp-vpc-abc", hw).Value.ToBytes()
		if got[0] == 'd' {
			t.Fatalf("client-id starts with the identifier text, not a type byte: %v", got)
		}
	})
}

// The readable identity moves to options 12 and 60 once option 61 carries the
// chaddr, so a lease stays attributable in the upstream table.
func TestIdentityModifiersAlwaysCarryReadableIdentity(t *testing.T) {
	hw, err := net.ParseMAC("02:0c:46:55:cf:ed")
	if err != nil {
		t.Fatalf("parse mac: %v", err)
	}

	build := func(clientID, hostname, vendorClass string) *dhcpv4.DHCPv4 {
		msg, err := dhcpv4.New(IdentityModifiers(clientID, hostname, vendorClass, hw)...)
		if err != nil {
			t.Fatalf("build message: %v", err)
		}
		return msg
	}

	t.Run("hostname defaults to the client-id", func(t *testing.T) {
		msg := build("dhcp-gw-lrp-vpc-abc", "", "")
		if got := msg.HostName(); got != "dhcp-gw-lrp-vpc-abc" {
			t.Fatalf("hostname = %q, want the client-id", got)
		}
	})

	t.Run("vendor class defaults so leases are identifiable as ours", func(t *testing.T) {
		msg := build("eni-1234", "", "")
		if got := msg.ClassIdentifier(); got != defaultVendorClass {
			t.Fatalf("vendor class = %q, want %q", got, defaultVendorClass)
		}
	})

	t.Run("explicit values win over the defaults", func(t *testing.T) {
		msg := build("eni-1234", "host-a", "acme")
		if got := msg.HostName(); got != "host-a" {
			t.Fatalf("hostname = %q, want host-a", got)
		}
		if got := msg.ClassIdentifier(); got != "acme" {
			t.Fatalf("vendor class = %q, want acme", got)
		}
	})
}
