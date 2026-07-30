package dhcp

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// The caller's budget must be spent retransmitting under one xid, not waiting
// out a single silent read, so the first read is short and the ladder — not the
// read deadline — is what has to cover the window Manager gives an attempt.
func TestRetransmitLadderCoversTheWidestAcquireWindow(t *testing.T) {
	widest := defaultAcquireSchedule[len(defaultAcquireSchedule)-1] + acquireAttemptJitter
	if got := retransmitLadder(nclient4InitialRead, nclient4Tries); got <= widest {
		t.Fatalf("ladder spans %v, want more than the widest attempt window %v — "+
			"the exchange would end on an exhausted ladder with budget left", got, widest)
	}
}

// nclient4 ends a read on whichever of the socket deadline and ctx fires first,
// so a read longer than what the caller has left is dead time: the transmission
// it belongs to can never be retried inside the exchange.
func TestSocketTimeoutNeverOutlivesTheCaller(t *testing.T) {
	c := NewNClient4(5 * time.Second)

	t.Run("ample deadline yields the short initial read", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 32*time.Second)
		defer cancel()
		if got := c.socketTimeout(ctx); got != nclient4InitialRead {
			t.Fatalf("socketTimeout = %v, want the %v initial read", got, nclient4InitialRead)
		}
	})

	t.Run("no deadline yields the same initial read", func(t *testing.T) {
		if got := c.socketTimeout(context.Background()); got != nclient4InitialRead {
			t.Fatalf("socketTimeout = %v, want the %v initial read", got, nclient4InitialRead)
		}
	})

	t.Run("deadline inside the initial read shortens it", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), nclient4InitialRead/4)
		defer cancel()
		got := c.socketTimeout(ctx)
		if got <= 0 || got > nclient4InitialRead/4 {
			t.Fatalf("socketTimeout = %v, want a positive value within the remaining %v",
				got, nclient4InitialRead/4)
		}
	})

	t.Run("expired deadline still yields a positive timeout", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		// nclient4 rejects a non-positive read deadline; ctx.Done() ends the
		// attempt immediately regardless of what is passed here.
		if got := c.socketTimeout(ctx); got != nclient4InitialRead {
			t.Fatalf("socketTimeout = %v, want the %v fallback", got, nclient4InitialRead)
		}
	})
}

// Renew and Release read the chaddr back out of the lease store, so a lease
// written before the chaddr was carried has none. Passing that through puts all
// zeros in chaddr and drops option 61 to its untyped form, and the lease lands
// upstream with no hardware address against it.
func TestResolveHWAddrRepairsAMissingChaddr(t *testing.T) {
	derived, err := DeriveMAC("eipalloc-1234")
	if err != nil {
		t.Fatalf("derive mac: %v", err)
	}

	t.Run("a usable address is kept", func(t *testing.T) {
		got, err := resolveHWAddr("br-wan", "eipalloc-1234", derived, false)
		if err != nil {
			t.Fatalf("resolveHWAddr: %v", err)
		}
		if !bytes.Equal(got, derived) {
			t.Fatalf("hw addr = %s, want the stored %s", got, derived)
		}
	})

	for _, stored := range []net.HardwareAddr{nil, {}, make(net.HardwareAddr, 6)} {
		t.Run("an unusable address is re-derived from the client-id", func(t *testing.T) {
			got, err := resolveHWAddr("br-wan", "eipalloc-1234", stored, false)
			if err != nil {
				t.Fatalf("resolveHWAddr: %v", err)
			}
			// All-zero is six bytes wide and so passes a length check, yet it is
			// the very value that produces an unattributable upstream lease.
			if isZeroMAC(got) {
				t.Fatalf("hw addr = %s, want a real address rather than zeros", got)
			}
			if !bytes.Equal(got, derived) {
				t.Fatalf("hw addr = %s, want the derived %s", got, derived)
			}
		})
	}

	t.Run("no client-id leaves nothing to derive from", func(t *testing.T) {
		if _, err := resolveHWAddr("br-wan", "", nil, false); err == nil {
			t.Fatal("want an error rather than an exchange with a zero chaddr")
		}
	})
}

// resolveHWAddr's useIfaceMAC branch leases with the bind interface's own MAC
// instead of a derived one, for uplinks that drop foreign source MACs. It must
// fail closed on a bad interface rather than fall through to a derived or zero
// chaddr, and must ignore the client-id entirely once an interface MAC exists.
func TestResolveHWAddrUsesTheInterfaceMACWhenConfigured(t *testing.T) {
	t.Run("an unknown interface name errors", func(t *testing.T) {
		if _, err := resolveHWAddr("mulga-no-such-iface0", "eipalloc-1234", nil, true); err == nil {
			t.Fatal("want an error rather than a nil MAC for a nonexistent interface")
		}
	})

	t.Run("an interface with no hardware address errors", func(t *testing.T) {
		iface, err := net.InterfaceByName("lo")
		if err != nil {
			t.Skip("no loopback interface available on this host")
		}
		if len(iface.HardwareAddr) != 0 {
			t.Skip("lo unexpectedly carries a hardware address on this host")
		}
		if _, err := resolveHWAddr("lo", "eipalloc-1234", nil, true); err == nil {
			t.Fatal("want an error rather than an empty MAC")
		}
	})

	t.Run("an interface with a MAC is returned verbatim, ignoring the client-id", func(t *testing.T) {
		ifaces, err := net.Interfaces()
		if err != nil {
			t.Fatalf("list interfaces: %v", err)
		}
		var target *net.Interface
		for i := range ifaces {
			if len(ifaces[i].HardwareAddr) > 0 {
				target = &ifaces[i]
				break
			}
		}
		if target == nil {
			t.Skip("no interface with a hardware address available on this host")
		}
		got, err := resolveHWAddr(target.Name, "eipalloc-1234", nil, true)
		if err != nil {
			t.Fatalf("resolveHWAddr: %v", err)
		}
		if !bytes.Equal(got, target.HardwareAddr) {
			t.Fatalf("hw addr = %s, want the interface's own %s", got, target.HardwareAddr)
		}
	})
}

// A server matches a RELEASE on chaddr, so it has to be the address the lease
// was taken with. Re-resolving a UseIfaceMAC lease against an interface whose
// MAC has since changed would send one the binding never had, and the server
// drops such a RELEASE without a word.
func TestReleaseHWAddrComesFromTheLease(t *testing.T) {
	stored, err := net.ParseMAC("02:6e:c3:e4:67:f3")
	if err != nil {
		t.Fatalf("parse mac: %v", err)
	}

	t.Run("a stored chaddr wins over the live interface MAC", func(t *testing.T) {
		got, err := releaseHWAddr(&Lease{
			Bridge: "mulga-no-such-iface0", ClientID: "dhcp-gw-lrp-vpc-1",
			HWAddr: stored, UseIfaceMAC: true,
		})
		if err != nil {
			t.Fatalf("releaseHWAddr: %v", err)
		}
		if !bytes.Equal(got, stored) {
			t.Fatalf("hw addr = %s, want the lease's own %s", got, stored)
		}
	})

	t.Run("a lease with no chaddr falls back to the derived one", func(t *testing.T) {
		got, err := releaseHWAddr(&Lease{Bridge: "br-wan", ClientID: "dhcp-gw-lrp-vpc-1"})
		if err != nil {
			t.Fatalf("releaseHWAddr: %v", err)
		}
		want, err := DeriveMAC("dhcp-gw-lrp-vpc-1")
		if err != nil {
			t.Fatalf("derive mac: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("hw addr = %s, want the derived %s", got, want)
		}
	})

	t.Run("an all-zero chaddr is not passed through", func(t *testing.T) {
		got, err := releaseHWAddr(&Lease{
			Bridge: "br-wan", ClientID: "dhcp-gw-lrp-vpc-1",
			HWAddr: net.HardwareAddr{0, 0, 0, 0, 0, 0},
		})
		if err != nil {
			t.Fatalf("releaseHWAddr: %v", err)
		}
		if isZeroMAC(got) {
			t.Fatal("want a repaired chaddr rather than all zeros")
		}
	})
}

// rawDHCPv4Message returns a syntactically valid, arbitrary DHCPv4 wire
// message for use as a Lease's RawOffer/RawACK. Renew and Release only need
// something reconstructNClient4Lease can parse before chaddr resolution runs.
func rawDHCPv4Message(t *testing.T) []byte {
	t.Helper()
	msg, err := dhcpv4.New()
	if err != nil {
		t.Fatalf("build dhcpv4 message: %v", err)
	}
	return msg.ToBytes()
}

// Acquire, Renew and Release each resolve a chaddr before opening a socket, so
// all three fail the same way against a bridge that cannot exist — proof they
// share one resolution path rather than three independently maintained ones.
// nclient4.New rejects an unknown interface name itself, so this needs no
// privileges and no live DHCP server.
func TestAcquireRenewReleaseFailToOpenAnUnknownBridge(t *testing.T) {
	const noSuchBridge = "mulga-no-such-br0"
	c := NewNClient4(time.Second)

	derived, err := DeriveMAC("eipalloc-1234")
	if err != nil {
		t.Fatalf("derive mac: %v", err)
	}
	lease := &Lease{
		Bridge:   noSuchBridge,
		ClientID: "eipalloc-1234",
		HWAddr:   derived,
		RawOffer: rawDHCPv4Message(t),
		RawACK:   rawDHCPv4Message(t),
	}

	t.Run("Acquire", func(t *testing.T) {
		_, err := c.Acquire(context.Background(), AcquireRequest{
			Bridge:   noSuchBridge,
			ClientID: "eipalloc-1234",
		})
		if err == nil || !strings.Contains(err.Error(), noSuchBridge) {
			t.Fatalf("Acquire err = %v, want an error mentioning %s", err, noSuchBridge)
		}
	})

	t.Run("Renew", func(t *testing.T) {
		_, err := c.Renew(context.Background(), lease)
		if err == nil || !strings.Contains(err.Error(), noSuchBridge) {
			t.Fatalf("Renew err = %v, want an error mentioning %s", err, noSuchBridge)
		}
	})

	t.Run("Release", func(t *testing.T) {
		if err := c.Release(context.Background(), lease); err == nil || !strings.Contains(err.Error(), noSuchBridge) {
			t.Fatalf("Release err = %v, want an error mentioning %s", err, noSuchBridge)
		}
	})
}

// A lease persisted before the chaddr was carried has neither a hardware
// address nor a client-id to derive one from. Renew and Release must refuse
// it before opening a socket rather than exchange with a zero chaddr — the
// same class of bug option 61 was fixed to stop, reached by a different route.
func TestRenewReleaseRefuseALeaseWithNoResolvableChaddr(t *testing.T) {
	c := NewNClient4(time.Second)
	lease := &Lease{
		Bridge:   "br-wan",
		ClientID: "",
		HWAddr:   nil,
		RawOffer: rawDHCPv4Message(t),
		RawACK:   rawDHCPv4Message(t),
	}

	t.Run("Renew", func(t *testing.T) {
		if _, err := c.Renew(context.Background(), lease); err == nil {
			t.Fatal("want an error rather than a renew with a zero chaddr")
		}
	})

	t.Run("Release", func(t *testing.T) {
		if err := c.Release(context.Background(), lease); err == nil {
			t.Fatal("want an error rather than a release with a zero chaddr")
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
