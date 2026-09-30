package exoscale

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scripted replays captured exo output and records the argv it was given.
type scripted struct {
	stdout, stderr []byte
	err            error
	args           []string
}

func (s *scripted) run(_ context.Context, _ string, args []string) ([]byte, []byte, error) {
	s.args = args
	return s.stdout, s.stderr, s.err
}

func testdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return b
}

func newScripted(t *testing.T, s *scripted) *cliClient {
	t.Helper()
	c, err := NewCLIClient(CLIConfig{Zone: "de-fra-1", Account: "spx"})
	require.NoError(t, err)
	cc := c.(*cliClient)
	cc.run = s.run
	return cc
}

func TestCreateElasticIP_DecodesCapturedOutput(t *testing.T) {
	s := &scripted{stdout: testdata(t, "create.json")}
	e, err := newScripted(t, s).CreateElasticIP(context.Background(), "spinifex:x:eipalloc-1")
	require.NoError(t, err)
	assert.Equal(t, "9d86594b-1bb0-4908-ae28-5870f961aaef", e.ID)
	assert.Equal(t, "194.182.171.24", e.Address.String())
	assert.Equal(t, "spinifex:capture:1", e.Description)

	// Credentials come only from the named config file and account, and the
	// description travels as one argv element however it is spelled.
	assert.Equal(t, []string{
		"--config", DefaultConfigFile, "--output-format", "json", "--quiet", "--use-account", "spx",
		"compute", "elastic-ip", "create", "--description", "spinifex:x:eipalloc-1", "-z", "de-fra-1",
	}, s.args)
}

func TestListElasticIPs_DecodesCapturedOutput(t *testing.T) {
	s := &scripted{stdout: testdata(t, "list.json")}
	eips, err := newScripted(t, s).ListElasticIPs(context.Background())
	require.NoError(t, err)
	require.Len(t, eips, 5)
	assert.Equal(t, "mulga1", eips[0].Description)
	assert.Equal(t, "89.145.162.53", eips[0].Address.String())
}

func TestListElasticIPs_SkipsOtherZones(t *testing.T) {
	s := &scripted{stdout: []byte(`[{"id":"a","ip_address":"1.2.3.4","zone":"ch-gva-2"},{"id":"b","ip_address":"1.2.3.5","zone":"de-fra-1"}]`)}
	eips, err := newScripted(t, s).ListElasticIPs(context.Background())
	require.NoError(t, err)
	require.Len(t, eips, 1)
	assert.Equal(t, "b", eips[0].ID)
}

func TestCreateElasticIP_RejectsOutputWithoutAddress(t *testing.T) {
	s := &scripted{stdout: []byte(`{"id":"a","ip_address":""}`)}
	_, err := newScripted(t, s).CreateElasticIP(context.Background(), "d")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unparseable address")
}

func TestClassify_CapturedErrors(t *testing.T) {
	exitErr := errors.New("exit status 1")
	cases := []struct {
		file string
		want error
	}{
		{"quota.stderr", ErrQuotaExceeded},
		{"notfound.stderr", ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			s := &scripted{stderr: testdata(t, tc.file), err: exitErr}
			_, err := newScripted(t, s).CreateElasticIP(context.Background(), "d")
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func TestClassify_UnknownErrorKeepsMessageAndCause(t *testing.T) {
	exitErr := errors.New("exit status 1")
	s := &scripted{stderr: []byte("error: invalid API key\n"), err: exitErr}
	err := newScripted(t, s).DeleteElasticIP(context.Background(), "a")
	require.ErrorIs(t, err, exitErr)
	assert.NotErrorIs(t, err, ErrNotFound)
	assert.Contains(t, err.Error(), "invalid API key")
}

func TestScrubEnv_DropsCredentialVariables(t *testing.T) {
	got := scrubEnv([]string{"PATH=/usr/bin", "EXOSCALE_API_KEY=k", "EXOSCALE_API_SECRET=s", "EXOSCALE_CONFIG=/x", "HOME=/h"})
	assert.Equal(t, []string{"PATH=/usr/bin", "HOME=/h"}, got)
}

// A binary that exits non-zero is reported, not mistaken for empty output.
func TestExecRun_RealProcessFailure(t *testing.T) {
	sh, err := exec.LookPath("sh")
	require.NoError(t, err)
	c, err := NewCLIClient(CLIConfig{Zone: "de-fra-1", Binary: sh})
	require.NoError(t, err)
	_, err = c.Version(context.Background())
	require.Error(t, err)
}

// The instance comes before the EIP in both commands. Swapped, exo reports the
// instance as not found, which Release would read as already detached.
func TestAttachDetach_PassInstanceThenEIP(t *testing.T) {
	s := &scripted{}
	c := newScripted(t, s)
	require.NoError(t, c.AttachElasticIP(context.Background(), "inst-1", "eip-1"))
	assert.Equal(t, []string{"compute", "instance", "elastic-ip", "attach", "inst-1", "eip-1", "-z", "de-fra-1"}, s.args[7:])
	require.NoError(t, c.DetachElasticIP(context.Background(), "inst-1", "eip-1"))
	assert.Equal(t, []string{"compute", "instance", "elastic-ip", "detach", "inst-1", "eip-1", "-z", "de-fra-1"}, s.args[7:])
}

func TestDetach_NotFoundIsTheSentinel(t *testing.T) {
	s := &scripted{stderr: testdata(t, "notfound.stderr"), err: errors.New("exit status 1")}
	err := newScripted(t, s).DetachElasticIP(context.Background(), "inst-1", "eip-1")
	assert.ErrorIs(t, err, ErrNotFound)
}
