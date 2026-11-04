//go:build lambdatestlink

package host

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	hostcommand "github.com/mulgadc/spinifex/spinifex/runtime/host/command"
	"github.com/stretchr/testify/require"
)

// fakeHost stands in for sysfs and the privileged commands: ip creates and
// deletes link directories, ovs-vsctl reports a port owner.
type fakeHost struct {
	sysfs     string
	calls     []string
	ovsOwner  string
	failCmd   string
	nextIndex string
}

func newFakeHost(t *testing.T) *fakeHost {
	f := &fakeHost{sysfs: t.TempDir(), nextIndex: "41"}
	orig := ownedTapSysfs
	ownedTapSysfs = f.sysfs
	t.Cleanup(func() { ownedTapSysfs = orig })
	t.Cleanup(hostcommand.SetSudoCommandForTest(func(name string, args ...string) *exec.Cmd {
		call := name + " " + strings.Join(args, " ")
		f.calls = append(f.calls, call)
		if f.failCmd != "" && strings.Contains(call, f.failCmd) {
			return exec.Command("/bin/false")
		}
		switch {
		case name == "ip" && args[0] == "tuntap":
			f.link(t, args[3], f.nextIndex, "")
		case name == "ip" && slices.Contains(args, "alias"):
			i := slices.Index(args, "alias")
			require.NoError(t, os.WriteFile(filepath.Join(f.sysfs, args[3], "ifalias"), []byte(args[i+1]+"\n"), 0o600))
		case name == "ip" && args[0] == "link" && args[1] == "del":
			require.NoError(t, os.RemoveAll(filepath.Join(f.sysfs, args[3])))
		case name == "ovs-vsctl" && slices.Contains(args, "get"):
			return exec.Command("/bin/echo", f.ovsOwner)
		}
		return exec.Command("/bin/true")
	}))
	return f
}

func (f *fakeHost) link(t *testing.T, name, index, alias string) {
	dir := filepath.Join(f.sysfs, name)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ifindex"), []byte(index+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ifalias"), []byte(alias+"\n"), 0o600))
}

func (f *fakeHost) ran(sub string) bool {
	return slices.ContainsFunc(f.calls, func(c string) bool { return strings.Contains(c, sub) })
}

func TestCreateOwnedTap_RefusesAnExistingLinkWithoutTouchingIt(t *testing.T) {
	f := newFakeHost(t)
	f.link(t, "lm1", "7", "someone-else")
	_, err := NewOVSPlumber().CreateOwnedTap(vm.TapSpec{Name: "lm1", Bridge: "br-mgmt"}, "lambda-a")
	require.ErrorIs(t, err, ErrTapExists)
	require.ErrorIs(t, err, ErrTapNotCreated)
	require.Empty(t, f.calls, "an existing link must not be deleted, replaced or detached")
}

func TestCreateOwnedTap_CreatesWithoutReplacing(t *testing.T) {
	f := newFakeHost(t)
	tap, err := NewOVSPlumber().CreateOwnedTap(vm.TapSpec{Name: "lm1", Bridge: "br-mgmt", MTU: 1400}, "lambda-a")
	require.NoError(t, err)
	require.Equal(t, OwnedTap{Name: "lm1", IfIndex: 41, Owner: "lambda-a"}, tap)
	require.True(t, f.ran("add-port br-mgmt lm1 -- set Interface lm1 external_ids:spinifex-owner=lambda-a"))
	require.True(t, f.ran("alias lambda-a up mtu 1400"))
	require.False(t, f.ran("del-port"), "creation never removes an existing port")
	require.False(t, f.ran("--may-exist"))
	require.False(t, f.ran("link del"))
}

func TestCreateOwnedTap_OnlyARefusalClaimsNothingCreated(t *testing.T) {
	f := newFakeHost(t)
	_, err := NewOVSPlumber().CreateOwnedTap(vm.TapSpec{Name: "lm1", Bridge: "br-mgmt"}, "bad owner")
	require.ErrorIs(t, err, ErrTapNotCreated)
	require.Empty(t, f.calls)

	f = newFakeHost(t)
	f.failCmd = "tuntap add"
	_, err = NewOVSPlumber().CreateOwnedTap(vm.TapSpec{Name: "lm1", Bridge: "br-mgmt"}, "lambda-a")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrTapNotCreated, "a failed add may still have created the link")

	f = newFakeHost(t)
	f.nextIndex = "not-a-number"
	tap, err := NewOVSPlumber().CreateOwnedTap(vm.TapSpec{Name: "lm2", Bridge: "br-mgmt"}, "lambda-a")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrTapNotCreated, "a link exists that the caller cannot identify")
	require.Empty(t, tap.Name)
	require.False(t, f.ran("link del"))
}

func TestCreateOwnedTap_FailedAttachRemovesOnlyItsOwnLink(t *testing.T) {
	f := newFakeHost(t)
	f.failCmd = "add-port"
	_, err := NewOVSPlumber().CreateOwnedTap(vm.TapSpec{Name: "lm1", Bridge: "br-mgmt"}, "lambda-a")
	require.Error(t, err)
	require.True(t, f.ran("link del dev lm1"))
	require.False(t, f.ran("del-port"), "a port this call failed to add is never removed")
}

func TestRemoveOwnedTap_LeavesAnythingItDidNotCreate(t *testing.T) {
	cases := map[string]struct {
		index, alias, ovsOwner string
		wantDelPort            bool
	}{
		"other OVS owner":  {index: "41", alias: "lambda-a", ovsOwner: `"other"`},
		"replaced link":    {index: "99", alias: "lambda-a", ovsOwner: `"lambda-a"`, wantDelPort: true},
		"other link alias": {index: "41", alias: "other", ovsOwner: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeHost(t)
			f.link(t, "lm1", tc.index, tc.alias)
			f.ovsOwner = tc.ovsOwner
			err := NewOVSPlumber().RemoveOwnedTap(OwnedTap{Name: "lm1", IfIndex: 41, Owner: "lambda-a"})
			require.Error(t, err)
			require.False(t, f.ran("link del"))
			require.Equal(t, tc.wantDelPort, f.ran("del-port"))
		})
	}
}

func TestRemoveOwnedTap_RemovesItsOwnTap(t *testing.T) {
	f := newFakeHost(t)
	f.link(t, "lm1", "41", "lambda-a")
	f.ovsOwner = `"lambda-a"`
	require.NoError(t, NewOVSPlumber().RemoveOwnedTap(OwnedTap{Name: "lm1", IfIndex: 41, Owner: "lambda-a"}))
	require.True(t, f.ran("del-port lm1"))
	require.True(t, f.ran("link del dev lm1"))

	// Already gone is success, and touches nothing.
	f.calls, f.ovsOwner = nil, ""
	require.NoError(t, NewOVSPlumber().RemoveOwnedTap(OwnedTap{Name: "lm1", IfIndex: 41, Owner: "lambda-a"}))
	require.False(t, f.ran("del-port"))
	require.False(t, f.ran("link del"))
}

func TestRemoveOwnedTap_OVSReadFailureRemovesNothing(t *testing.T) {
	f := newFakeHost(t)
	f.link(t, "lm1", "41", "lambda-a")
	f.failCmd = "get Interface"
	err := NewOVSPlumber().RemoveOwnedTap(OwnedTap{Name: "lm1", IfIndex: 41, Owner: "lambda-a"})
	require.Error(t, err)
	require.False(t, f.ran("del-port"))
	require.False(t, f.ran("link del"))
	require.False(t, errors.Is(err, ErrTapExists))
}
