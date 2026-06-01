package dhcp

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// enableBridgePromisc puts iface into IFF_PROMISC and returns a release
// callback the caller defers. Reference-counted across concurrent DORAs
// on the same bridge so the first caller flips PROMISC on and the last
// caller flips it off — concurrent leases don't fight over flag state.
//
// Rationale: nclient4 sends DHCPDISCOVER with the derived 02:xx:xx:xx:xx:xx
// chaddr (see DeriveMAC) which doesn't match the bridge's own MAC. Many
// upstream DHCP servers reply unicast to chaddr regardless of the
// broadcast flag, so the OFFER egresses the Linux bridge with a destination
// MAC the bridge has never learned. Without IFF_PROMISC the kernel
// AF_PACKET socket bound to the bridge drops those frames in software and
// the DORA times out with "no matching response packet received". With
// PROMISC the bridge surfaces every received frame to the socket and
// nclient4 sees the OFFER.
//
// On failure (typically EPERM when vpcd lost CAP_NET_ADMIN) the function
// returns the error so the caller can decide whether to proceed — the
// bridge may already be in PROMISC via operator config.
var enableBridgePromisc = func(iface string) (func() error, error) {
	if iface == "" {
		return nil, errors.New("promisc: iface required")
	}
	promiscMu.Lock()
	defer promiscMu.Unlock()

	if promiscRefs[iface] > 0 {
		promiscRefs[iface]++
		return makePromiscRelease(iface), nil
	}
	if err := setPromiscFn(iface, true); err != nil {
		return nil, fmt.Errorf("promisc: enable on %s: %w", iface, err)
	}
	promiscRefs[iface] = 1
	return makePromiscRelease(iface), nil
}

func makePromiscRelease(iface string) func() error {
	return func() error {
		promiscMu.Lock()
		defer promiscMu.Unlock()
		n := promiscRefs[iface]
		if n <= 0 {
			delete(promiscRefs, iface)
			return nil
		}
		n--
		if n > 0 {
			promiscRefs[iface] = n
			return nil
		}
		delete(promiscRefs, iface)
		if err := setPromiscFn(iface, false); err != nil {
			return fmt.Errorf("promisc: disable on %s: %w", iface, err)
		}
		return nil
	}
}

// setPromiscFn is the syscall sink, swappable in tests to assert
// ref-count behaviour without root or a real interface.
var setPromiscFn = setPromisc

var (
	promiscMu   sync.Mutex
	promiscRefs = map[string]int{}
)

// ifReq mirrors `struct ifreq` from <net/if.h> — 16-byte name padded out
// to the same total size as the kernel expects. Only the flags overlay
// is read/written here.
type ifReq struct {
	name  [unix.IFNAMSIZ]byte
	flags uint16
	_pad  [22]byte
}

// setPromisc toggles IFF_PROMISC on iface via SIOCGIFFLAGS / SIOCSIFFLAGS.
// Requires CAP_NET_ADMIN (granted to spinifex-vpcd via AmbientCapabilities
// in the systemd unit).
func setPromisc(iface string, on bool) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return fmt.Errorf("open socket: %w", err)
	}
	defer unix.Close(fd)

	var req ifReq
	if len(iface) >= unix.IFNAMSIZ {
		return fmt.Errorf("iface name too long: %q", iface)
	}
	copy(req.name[:], iface)

	if errno := ioctl(fd, unix.SIOCGIFFLAGS, unsafe.Pointer(&req)); errno != 0 {
		return fmt.Errorf("SIOCGIFFLAGS: %w", errno)
	}
	if on {
		if req.flags&unix.IFF_PROMISC != 0 {
			return nil
		}
		req.flags |= unix.IFF_PROMISC
	} else {
		if req.flags&unix.IFF_PROMISC == 0 {
			return nil
		}
		req.flags &^= unix.IFF_PROMISC
	}
	if errno := ioctl(fd, unix.SIOCSIFFLAGS, unsafe.Pointer(&req)); errno != 0 {
		return fmt.Errorf("SIOCSIFFLAGS: %w", errno)
	}
	return nil
}

func ioctl(fd int, req uint, arg unsafe.Pointer) syscall.Errno {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(arg))
	return errno
}
