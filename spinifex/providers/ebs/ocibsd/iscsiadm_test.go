package ocibsd_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs/ocibsd"
)

// exitError is a failed command's error, carrying its exit status as an
// *exec.ExitError does.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitError) ExitCode() int { return int(e) }

// scriptedRunner records each argv and answers from replies, keyed by the
// argv's first matching substring; an argv with no reply succeeds empty.
type scriptedRunner struct {
	calls   []string
	replies map[string]reply
}

type reply struct {
	out string
	err error
}

func (r *scriptedRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	argv := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, argv)
	for match, rep := range r.replies {
		if strings.Contains(argv, match) {
			return []byte(rep.out), rep.err
		}
	}
	return nil, nil
}

var target = ocibsd.Target{IQN: "iqn.2015-12.com.oracleiaas:vol-1", IPv4: "169.254.2.2", Port: 3260}

const nodeArgs = "iscsiadm -m node -T iqn.2015-12.com.oracleiaas:vol-1 -p 169.254.2.2:3260"

func present(string) (os.FileInfo, error) { return nil, nil }

func newISCSIAdm(r *scriptedRunner, stat func(string) (os.FileInfo, error)) *ocibsd.ISCSIAdm {
	a := ocibsd.NewISCSIAdm(r)
	a.Stat, a.DeviceTimeout, a.DevicePoll = stat, 50*time.Millisecond, time.Millisecond
	return a
}

func TestLoginWritesTheNodeRecordBeforeLoggingIn(t *testing.T) {
	cases := []struct {
		name    string
		replies map[string]reply
	}{
		{name: "a new session"},
		{name: "a session already open", replies: map[string]reply{" -l": {err: exitError(15)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &scriptedRunner{replies: tc.replies}
			var statted string
			device, err := newISCSIAdm(r, func(path string) (os.FileInfo, error) { statted = path; return nil, nil }).Login(context.Background(), target)
			require.NoError(t, err)
			assert.Equal(t, ocibsd.ByPathDevice(target), device)
			assert.Equal(t, device, statted)
			assert.Equal(t, []string{
				nodeArgs + " -o new",
				nodeArgs + " -o update -n node.startup -v manual",
				nodeArgs + " -o update -n node.session.timeo.replacement_timeout -v 120",
				nodeArgs + " -l",
			}, r.calls)
		})
	}
}

func TestLoginFailures(t *testing.T) {
	cases := []struct {
		name      string
		replies   map[string]reply
		stat      func(string) (os.FileInfo, error)
		want      string
		wantCalls int
	}{
		{
			name:      "a record that cannot be created logs nothing in",
			replies:   map[string]reply{"-o new": {err: exitError(6)}},
			want:      "create node record",
			wantCalls: 1,
		},
		{
			name:      "no login without the short replacement timeout",
			replies:   map[string]reply{"replacement_timeout": {err: exitError(6)}},
			want:      "set node.session.timeo.replacement_timeout",
			wantCalls: 3,
		},
		{
			name:      "a refused login",
			replies:   map[string]reply{" -l": {err: exitError(24)}},
			want:      "log in",
			wantCalls: 4,
		},
		{
			name:      "a device that never appears",
			stat:      func(string) (os.FileInfo, error) { return nil, fs.ErrNotExist },
			want:      "did not appear",
			wantCalls: 4,
		},
		{
			name:      "a device that cannot be read",
			stat:      func(string) (os.FileInfo, error) { return nil, fs.ErrPermission },
			want:      "permission denied",
			wantCalls: 4,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &scriptedRunner{replies: tc.replies}
			stat := tc.stat
			if stat == nil {
				stat = present
			}
			_, err := newISCSIAdm(r, stat).Login(context.Background(), target)
			require.ErrorContains(t, err, tc.want)
			assert.Len(t, r.calls, tc.wantCalls)
		})
	}
}

func TestLogout(t *testing.T) {
	cases := []struct {
		name    string
		replies map[string]reply
		want    string
	}{
		{name: "an open session"},
		{name: "no session and no record", replies: map[string]reply{" -u": {err: exitError(21)}, "-o delete": {err: exitError(21)}}},
		{name: "a failed logout", replies: map[string]reply{" -u": {err: exitError(10)}}, want: "log out"},
		{name: "a record that cannot be deleted", replies: map[string]reply{"-o delete": {err: exitError(6)}}, want: "delete node record"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &scriptedRunner{replies: tc.replies}
			err := newISCSIAdm(r, present).Logout(context.Background(), target)
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []string{nodeArgs + " -u", nodeArgs + " -o delete"}, r.calls)
		})
	}
}

func TestSessions(t *testing.T) {
	cases := []struct {
		name string
		rep  reply
		want []ocibsd.Target
		err  string
	}{
		{
			name: "every readable session",
			rep: reply{out: "tcp: [1] 169.254.2.2:3260,1 iqn.2015-12.com.oracleiaas:vol-1 (non-flash)\n" +
				"garbage\n" +
				"tcp: [2] 10.0.0.5:3261,1 iqn.2003-01.org.linux-iscsi.san:lun0 (non-flash)\n" +
				"tcp: [3] 10.0.0.6:notaport,1 iqn.2003-01.org.linux-iscsi.san:lun1 (non-flash)\n"},
			want: []ocibsd.Target{target, {IQN: "iqn.2003-01.org.linux-iscsi.san:lun0", IPv4: "10.0.0.5", Port: 3261}},
		},
		{name: "no sessions", rep: reply{out: "iscsiadm: No active sessions.", err: exitError(21)}},
		{name: "iscsid unreachable", rep: reply{err: exitError(20)}, err: "list sessions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &scriptedRunner{replies: map[string]reply{"-m session": tc.rep}}
			got, err := newISCSIAdm(r, present).Sessions(context.Background())
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, []string{"iscsiadm -m session"}, r.calls)
		})
	}
}

func TestExecRunnerKeepsTheExitCode(t *testing.T) {
	_, err := ocibsd.ExecRunner{}.Run(context.Background(), "false")
	var coded interface{ ExitCode() int }
	require.ErrorAs(t, err, &coded)
	assert.Equal(t, 1, coded.ExitCode())
}
