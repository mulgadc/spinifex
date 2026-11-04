//go:build lambdatestlink

package host

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	hostcommand "github.com/mulgadc/spinifex/spinifex/runtime/host/command"
)

// ErrTapExists means a link with the requested name already exists; nothing
// was changed.
var ErrTapExists = errors.New("tap name already in use")

// ErrTapNotCreated wraps a CreateOwnedTap failure that left no link behind.
// Any other failure may have left one.
var ErrTapNotCreated = errors.New("no tap created")

// ownedTapSysfs is a var so tests can stand in a fake /sys/class/net.
var ownedTapSysfs = "/sys/class/net"

const ownerExternalID = "spinifex-owner"

// OwnedTap identifies a tap CreateOwnedTap made: a later remove acts only on a
// link with the same ifindex and alias, and an OVS port with the same owner.
type OwnedTap struct {
	Name    string
	IfIndex int
	Owner   string
}

// CreateOwnedTap creates and attaches a new tap, refusing to replace a link or
// OVS port of the same name. The check-then-create gap remains, so callers use
// names no other creator would pick.
func (p *OVSPlumber) CreateOwnedTap(spec vm.TapSpec, owner string) (OwnedTap, error) {
	if owner == "" || strings.ContainsAny(owner, " \t\n\"'") {
		return OwnedTap{}, fmt.Errorf("%w: invalid owner", ErrTapNotCreated)
	}
	if _, err := os.Lstat(filepath.Join(ownedTapSysfs, spec.Name)); err == nil {
		return OwnedTap{}, fmt.Errorf("%w: %w: %s", ErrTapNotCreated, ErrTapExists, spec.Name)
	}
	addArgs := []string{"tuntap", "add", "dev", spec.Name, "mode", "tap"}
	if uid := os.Geteuid(); uid != 0 {
		addArgs = append(addArgs, "user", strconv.Itoa(uid), "group", strconv.Itoa(os.Getegid()))
	}
	if out, err := hostcommand.SudoCommand("ip", addArgs...).CombinedOutput(); err != nil {
		// The add may have taken effect before failing, so this is not ErrTapNotCreated.
		return OwnedTap{}, fmt.Errorf("create tap %s: %s: %w", spec.Name, strings.TrimSpace(string(out)), err)
	}
	idx, err := readIfIndex(spec.Name)
	if err != nil {
		// Without its ifindex the new link cannot be told apart, so it stays.
		return OwnedTap{}, fmt.Errorf("created tap %s but cannot read its ifindex; left in place: %w", spec.Name, err)
	}
	tap := OwnedTap{Name: spec.Name, IfIndex: idx, Owner: owner}
	upArgs := []string{"link", "set", "dev", spec.Name, "alias", owner, "up"}
	if spec.MTU > 0 {
		upArgs = append(upArgs, "mtu", strconv.Itoa(spec.MTU))
	}
	if out, err := hostcommand.SudoCommand("ip", upArgs...).CombinedOutput(); err != nil {
		return tap, errors.Join(fmt.Errorf("bring up tap %s: %s: %w", spec.Name, strings.TrimSpace(string(out)), err), p.RemoveOwnedTap(tap))
	}
	// No --may-exist: a port record of the same name fails the add.
	addPort := []string{"add-port", spec.Bridge, spec.Name, "--", "set", "Interface", spec.Name,
		fmt.Sprintf("external_ids:%s=%s", ownerExternalID, owner)}
	if out, err := hostcommand.SudoCommand("ovs-vsctl", addPort...).CombinedOutput(); err != nil {
		return tap, errors.Join(fmt.Errorf("add tap %s to %s: %s: %w", spec.Name, spec.Bridge, strings.TrimSpace(string(out)), err), p.removeOwnedLink(tap))
	}
	return tap, nil
}

// RemoveOwnedTap removes the OVS port and kernel link only where each still
// carries tap's owner; anything else under that name is left alone. A link
// already gone is success.
func (p *OVSPlumber) RemoveOwnedTap(tap OwnedTap) error {
	out, err := hostcommand.SudoCommand("ovs-vsctl", "--if-exists", "get", "Interface", tap.Name,
		"external_ids:"+ownerExternalID).CombinedOutput()
	if err != nil {
		return fmt.Errorf("read OVS owner of %s: %s: %w", tap.Name, strings.TrimSpace(string(out)), err)
	}
	switch got := strings.Trim(strings.TrimSpace(string(out)), `"`); got {
	case tap.Owner:
		if out, err := hostcommand.SudoCommand("ovs-vsctl", "--if-exists", "del-port", tap.Name).CombinedOutput(); err != nil {
			return fmt.Errorf("remove OVS port %s: %s: %w", tap.Name, strings.TrimSpace(string(out)), err)
		}
	case "":
	default:
		return fmt.Errorf("OVS port %s belongs to another owner; left in place", tap.Name)
	}
	return p.removeOwnedLink(tap)
}

func (p *OVSPlumber) removeOwnedLink(tap OwnedTap) error {
	idx, err := readIfIndex(tap.Name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read ifindex of %s: %w", tap.Name, err)
	}
	alias, err := os.ReadFile(filepath.Join(ownedTapSysfs, tap.Name, "ifalias"))
	if err != nil {
		return fmt.Errorf("read alias of %s: %w", tap.Name, err)
	}
	if idx != tap.IfIndex || strings.TrimSpace(string(alias)) != tap.Owner {
		return fmt.Errorf("link %s is not the tap this owner created; left in place", tap.Name)
	}
	if out, err := deleteTapLink(tap.Name).CombinedOutput(); err != nil {
		return fmt.Errorf("delete tap %s: %s: %w", tap.Name, strings.TrimSpace(string(out)), err)
	}
	slog.Info("Owned tap removed", "tap", tap.Name)
	return nil
}

func readIfIndex(name string) (int, error) {
	b, err := os.ReadFile(filepath.Join(ownedTapSysfs, name, "ifindex"))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}
