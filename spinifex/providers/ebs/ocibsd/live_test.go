package ocibsd_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/providers/cloud/oci"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/conformance"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/natsserve"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/ocibsd"
)

// The live run reads API-key credentials from the local OCI CLI config and
// drives iscsiadm on the instance over SSH; see docs/oci-integration/README.md.
const (
	liveEnv          = "SPINIFEX_OCIBSD_LIVE"
	liveCompartment  = "SPINIFEX_OCIBSD_COMPARTMENT_ID"
	liveAD           = "SPINIFEX_OCIBSD_AVAILABILITY_DOMAIN"
	liveInstance     = "SPINIFEX_OCIBSD_INSTANCE_ID"
	liveSSHHost      = "SPINIFEX_OCIBSD_SSH_HOST"
	liveSSHUser      = "SPINIFEX_OCIBSD_SSH_USER"
	liveSSHKey       = "SPINIFEX_OCIBSD_SSH_KEY"
	liveKnownHosts   = "SPINIFEX_OCIBSD_SSH_KNOWN_HOSTS"
	liveClusterID    = "SPINIFEX_OCIBSD_CLUSTER_ID"
	liveOCIProfile   = "SPINIFEX_OCIBSD_OCI_PROFILE"
	liveNodeID       = "ocibsd-live"
	liveAZ           = "live-1a"
	liveNATSDeadline = 10 * time.Minute
)

type liveEnvironment struct {
	compartment, ad, instance, clusterID string
	client                               oci.BlockClient
	ssh                                  *sshRunner
}

// liveSetup skips unless the live run is asked for, and fails on any input it
// is missing rather than running against a half-configured tenancy.
func liveSetup(t *testing.T) *liveEnvironment {
	t.Helper()
	if os.Getenv(liveEnv) == "" {
		t.Skipf("skipping: set %s=1, with the inputs in docs/oci-integration/README.md, to run against real OCI", liveEnv)
	}
	need := func(name string) string {
		value := os.Getenv(name)
		require.NotEmptyf(t, value, "%s is required for the live run", name)
		return value
	}
	env := &liveEnvironment{
		compartment: need(liveCompartment), ad: need(liveAD), instance: need(liveInstance),
		clusterID: os.Getenv(liveClusterID),
	}
	if env.clusterID == "" {
		b := make([]byte, 16)
		_, err := rand.Read(b)
		require.NoError(t, err)
		env.clusterID = hex.EncodeToString(b)
	}
	t.Logf("live run: cluster ID %s, instance %s, availability domain %s", env.clusterID, env.instance, env.ad)

	client, err := oci.NewConfigFileBlockClient("", os.Getenv(liveOCIProfile))
	require.NoError(t, err)
	env.client = client
	env.ssh = newSSHRunner(t, need(liveSSHHost), os.Getenv(liveSSHUser), need(liveSSHKey), os.Getenv(liveKnownHosts))
	return env
}

// provider builds and starts a Provider for the instance, with iscsiadm and
// the device check run on it over SSH.
func (e *liveEnvironment) provider(t *testing.T) *ocibsd.Provider {
	t.Helper()
	initiator := ocibsd.NewISCSIAdm(e.ssh)
	initiator.Stat = e.ssh.statDevice
	p, err := ocibsd.New(ocibsd.Config{
		ClusterID: e.clusterID, CompartmentID: e.compartment,
		AvailabilityDomain: e.ad, AvailabilityZone: liveAZ,
		NodeID: liveNodeID, InstanceID: e.instance, InstanceAvailabilityDomain: e.ad,
		Client: e.client, Initiator: initiator,
	})
	require.NoError(t, err)
	require.NoError(t, p.Start(t.Context()))
	return p
}

func runPrefix(kind string) string {
	return kind + strconv.FormatInt(time.Now().Unix(), 36) + "-"
}

func TestLiveConformanceInProcess(t *testing.T) {
	env := liveSetup(t)
	conformance.RunSuiteWithConfig(t, func(t *testing.T) ebsprovider.EBSProvider {
		t.Helper()
		return env.provider(t)
	}, conformance.SuiteConfig{NamePrefix: runPrefix("lp"), NodeID: liveNodeID})
}

func TestLiveConformanceOverNATS(t *testing.T) {
	env := liveSetup(t)
	conformance.RunSuiteWithConfig(t, func(t *testing.T) ebsprovider.EBSProvider {
		t.Helper()
		_, conn := testutil.StartTestNATS(t)
		// t.Context is cancelled before cleanups run, and the suite's teardown
		// is served through this server, so it needs a context that outlives it.
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		stop, err := natsserve.Serve(ctx, conn, env.provider(t), natsserve.Options{NodeID: liveNodeID})
		require.NoError(t, err)
		t.Cleanup(stop)
		return ebsprovider.NewNATSProvider(conn, liveNATSDeadline)
	}, conformance.SuiteConfig{NamePrefix: runPrefix("ln"), NodeID: liveNodeID})
}

// TestLiveSizeUnits checks that OCI's GB is 2^30 bytes: the device the
// instance sees must be SizeGB gibibytes and hold the requested capacity.
func TestLiveSizeUnits(t *testing.T) {
	env := liveSetup(t)
	p := env.provider(t)
	ctx := t.Context()
	volumeID := "vol-" + runPrefix("lsz") + "units"
	capacity := 51*int64(1<<30) + 1

	v, err := p.CreateVolume(ctx, ebsprovider.CreateVolumeRequest{
		Versioned: latest, VolumeID: volumeID, CapacityRange: ebsprovider.CapacityRange{RequiredBytes: capacity},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, p.DeleteVolume(context.Background(), ebsprovider.DeleteVolumeRequest{Versioned: latest, VolumeID: volumeID, Handle: v.Handle}))
	})
	assert.Equal(t, capacity, v.CapacityBytes)

	got, err := env.client.GetVolume(ctx, v.Handle)
	require.NoError(t, err)
	t.Logf("requested %d bytes; OCI reports %d GB; provider reports %d bytes", capacity, got.SizeGB, v.CapacityBytes)
	assert.Equal(t, ocibsd.SizeGB(capacity), got.SizeGB)

	published, err := p.PublishVolume(ctx, ebsprovider.PublishVolumeRequest{Versioned: latest, VolumeID: volumeID, Handle: v.Handle, NodeID: liveNodeID})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := p.UnpublishVolume(context.Background(), ebsprovider.UnpublishVolumeRequest{Versioned: latest, VolumeID: volumeID, Handle: v.Handle, NodeID: liveNodeID})
		assert.NoError(t, err)
	})
	require.NotNil(t, published.Export.Block)
	assert.Equal(t, uint64(capacity), published.Export.Block.SizeBytes)

	out, err := env.ssh.Run(ctx, "blockdev", "--getsize64", published.Export.Block.DevicePath)
	require.NoError(t, err)
	deviceBytes, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	require.NoError(t, err)
	t.Logf("device %s on the instance is %d bytes", published.Export.Block.DevicePath, deviceBytes)
	assert.Equal(t, got.SizeGB*int64(1<<30), deviceBytes, "OCI's GB must be 2^30 bytes")
	assert.GreaterOrEqual(t, deviceBytes, capacity, "the device must hold the size the guest is told")
}

// TestLiveRetryTokenReplay recreates a deleted volume under the same ID, and
// so the same retry token, which OCI replays for a day: the provider must
// never answer with the deleted volume.
func TestLiveRetryTokenReplay(t *testing.T) {
	env := liveSetup(t)
	p := env.provider(t)
	ctx := t.Context()
	volumeID := "vol-" + runPrefix("lrt") + "replay"
	req := ebsprovider.CreateVolumeRequest{Versioned: latest, VolumeID: volumeID, CapacityRange: ebsprovider.CapacityRange{RequiredBytes: 1 << 30}}

	first, err := p.CreateVolume(ctx, req)
	require.NoError(t, err)
	require.NoError(t, p.DeleteVolume(ctx, ebsprovider.DeleteVolumeRequest{Versioned: latest, VolumeID: volumeID, Handle: first.Handle}))
	deleted, err := env.client.GetVolume(ctx, first.Handle)
	require.NoError(t, err)
	t.Logf("first volume %s is %s after delete", first.Handle, deleted.State)

	second, err := p.CreateVolume(ctx, req)
	if err != nil {
		t.Logf("recreate refused: %v", err)
		assert.Contains(t, err.Error(), first.Handle, "the refusal should name the replayed volume")
		return
	}
	t.Cleanup(func() {
		assert.NoError(t, p.DeleteVolume(context.Background(), ebsprovider.DeleteVolumeRequest{Versioned: latest, VolumeID: volumeID, Handle: second.Handle}))
	})
	t.Logf("recreate answered volume %s", second.Handle)
	assert.NotEqual(t, first.Handle, second.Handle, "the provider answered with the deleted volume")
	live, err := env.client.GetVolume(ctx, second.Handle)
	require.NoError(t, err)
	assert.False(t, live.State.Gone(), "the provider answered with a %s volume", live.State)
}

// sshRunner is a CommandRunner that runs each command under sudo on the
// instance. ssh exits with the remote command's status, so ExitCode survives.
type sshRunner struct {
	base []string
}

var _ ocibsd.CommandRunner = (*sshRunner)(nil)

func newSSHRunner(t *testing.T, host, user, key, knownHosts string) *sshRunner {
	t.Helper()
	if user == "" {
		user = "opc"
	}
	if knownHosts == "" {
		knownHosts = filepath.Join(t.TempDir(), "known_hosts")
	}
	control := t.TempDir()
	t.Cleanup(func() {
		_ = exec.Command("ssh", "-o", "ControlPath="+filepath.Join(control, "c"), "-O", "exit", user+"@"+host).Run()
	})
	return &sshRunner{base: []string{
		"-i", key, "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=15",
		"-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile=" + knownHosts,
		"-o", "ControlMaster=auto", "-o", "ControlPath=" + filepath.Join(control, "c"), "-o", "ControlPersist=120",
		user + "@" + host,
	}}
}

func (r *sshRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	words := []string{"sudo", shellQuote(name)}
	for _, a := range args {
		words = append(words, shellQuote(a))
	}
	argv := append(append([]string(nil), r.base...), "--", strings.Join(words, " "))
	out, err := exec.CommandContext(ctx, "ssh", argv...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("ssh %s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// statDevice is ISCSIAdm's device check run on the instance: a missing block
// device reads as fs.ErrNotExist, as os.Stat reports it locally.
func (r *sshRunner) statDevice(path string) (os.FileInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := r.Run(ctx, "test", "-b", path)
	if err != nil && exitStatus(err) == 1 {
		return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
	}
	return nil, err
}

func exitStatus(err error) int {
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return -1
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
