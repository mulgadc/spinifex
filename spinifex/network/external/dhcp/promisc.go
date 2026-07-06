package dhcp

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// enableBridgePromisc puts iface into IFF_PROMISC and returns a release callback.
// Reference-counted so concurrent DORAs share one flag; needed because servers
// unicast OFFERs to the derived chaddr, which the bridge drops without PROMISC.
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
	//nolint:gosec // G115: req is a fixed ioctl request constant, not attacker-influenced.
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(arg))
	return errno
}
