package viperblock

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fakeNbdkitEnv = "SPINIFEX_TEST_FAKE_NBDKIT"

// quietWindow is how long a test waits to see that a request has not been
// served. Only a wrong dispatch can end the wait early.
const quietWindow = 300 * time.Millisecond

// TestFakeNbdkitProcess is the nbdkit that installFakeNbdkit puts on PATH. It
// listens on the TCP port a mount gives it and exits on SIGTERM.
func TestFakeNbdkitProcess(t *testing.T) {
	if os.Getenv(fakeNbdkitEnv) != "1" {
		t.Skip("runs only as the fake nbdkit a dispatch test starts")
	}
	args := flag.Args()
	port := ""
	for i, arg := range args {
		if arg == "-p" && i+1 < len(args) {
			port = args[i+1]
		}
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		os.Exit(1)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	select {
	case <-term:
	case <-time.After(time.Minute):
	}
	os.Exit(0)
}

// installFakeNbdkit makes a TCP mount start TestFakeNbdkitProcess instead of
// nbdkit, so a publish can succeed without the plugin.
func installFakeNbdkit(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run='^TestFakeNbdkitProcess$' -- \"$@\"\n", exe)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nbdkit"), []byte(script), 0o755)) //nolint:gosec // the fake has to be executable
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(fakeNbdkitEnv, "1")
}

// heldOpens holds every engine open in constructVB until release is closed.
type heldOpens struct {
	arrived chan string
	release chan struct{}
	once    sync.Once
}

func holdEngineOpens(t *testing.T, cfg *Config, open func(ctx context.Context, volume string) (*viperblock.VB, int, error)) *heldOpens {
	t.Helper()
	h := &heldOpens{arrived: make(chan string, 64), release: make(chan struct{})}
	cfg.constructVB = func(ctx context.Context, volume string) (*viperblock.VB, int, error) {
		h.arrived <- volume
		<-h.release
		return open(ctx, volume)
	}
	t.Cleanup(h.releaseAll)
	return h
}

func (h *heldOpens) releaseAll() { h.once.Do(func() { close(h.release) }) }

// expectOpen waits for the next engine open and returns its volume.
func (h *heldOpens) expectOpen(t *testing.T) string {
	t.Helper()
	select {
	case volume := <-h.arrived:
		return volume
	case <-time.After(5 * time.Second):
		t.Fatal("no engine open arrived")
		return ""
	}
}

// expectNoOpen fails if an engine open arrives within the quiet window.
func (h *heldOpens) expectNoOpen(t *testing.T, why string) {
	t.Helper()
	select {
	case volume := <-h.arrived:
		t.Fatalf("%s: an open of %s arrived", why, volume)
	case <-time.After(quietWindow):
	}
}

// failedOpen ends a held open without a mount, so no nbdkit is started.
func failedOpen(context.Context, string) (*viperblock.VB, int, error) {
	return nil, 0, fmt.Errorf("open: %w", viperblock.ErrStateBackendUnavailable)
}

type natsReply struct {
	msg *nats.Msg
	err error
}

func requestAsync(nc *nats.Conn, subject string, body []byte, timeout time.Duration) <-chan natsReply {
	ch := make(chan natsReply, 1)
	go func() {
		msg, err := nc.Request(subject, body, timeout)
		ch <- natsReply{msg: msg, err: err}
	}()
	return ch
}

func publishAsync(t *testing.T, nc *nats.Conn, cfg *Config, volume string, timeout time.Duration) <-chan natsReply {
	t.Helper()
	subject, err := ebsprovider.PublishSubject(cfg.NodeName)
	require.NoError(t, err)
	body, err := json.Marshal(ebsprovider.PublishVolumeRequest{
		Versioned: ebsprovider.Versioned{SchemaVersion: ebsprovider.SchemaVersion}, VolumeID: volume, NodeID: cfg.NodeName,
	})
	require.NoError(t, err)
	return requestAsync(nc, subject, body, timeout)
}

func unpublishAsync(t *testing.T, nc *nats.Conn, cfg *Config, volume string) <-chan natsReply {
	t.Helper()
	subject, err := ebsprovider.UnpublishSubject(cfg.NodeName)
	require.NoError(t, err)
	body, err := json.Marshal(ebsprovider.UnpublishVolumeRequest{
		Versioned: ebsprovider.Versioned{SchemaVersion: ebsprovider.SchemaVersion}, VolumeID: volume, NodeID: cfg.NodeName,
	})
	require.NoError(t, err)
	return requestAsync(nc, subject, body, 10*time.Second)
}

func awaitReply(t *testing.T, ch <-chan natsReply) *nats.Msg {
	t.Helper()
	select {
	case r := <-ch:
		require.NoError(t, r.err)
		return r.msg
	case <-time.After(15 * time.Second):
		t.Fatal("no reply")
		return nil
	}
}

func expectNoReply(t *testing.T, ch <-chan natsReply, why string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal(why)
	case <-time.After(quietWindow):
	}
}

func decodePublish(t *testing.T, msg *nats.Msg) ebsprovider.PublishVolumeResponse {
	t.Helper()
	var resp ebsprovider.PublishVolumeResponse
	require.NoError(t, json.Unmarshal(msg.Data, &resp))
	return resp
}

// tcpDispatchProvider is a provider on TCP transport whose mounts start the
// fake nbdkit, with every engine open held.
func tcpDispatchProvider(t *testing.T) (*Config, *nats.Conn, *heldOpens) {
	t.Helper()
	installFakeNbdkit(t)
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	cfg.NBDTransport = NBDTransportTCP
	held := holdEngineOpens(t, cfg, fileBackedConstructVB(t))
	return cfg, startProviderSubjects(t, cfg, natsURL), held
}

// assertNotExported checks that this node holds neither an export nor a lease
// for volume.
func assertNotExported(t *testing.T, cfg *Config, volume string) {
	t.Helper()
	_, mounted := findMountedVolume(cfg, volume)
	assert.False(t, mounted, "%s is still exported", volume)
	_, held := cfg.leases.currentOwner(t.Context(), volume)
	assert.False(t, held, "%s is still leased", volume)
}

// An unpublish that arrives while a publish of the same volume is in flight
// waits for it, so it tears down the export that publish makes.
func TestProviderDispatch_UnpublishWaitsForTheVolumesPublish(t *testing.T) {
	cfg, nc, held := tcpDispatchProvider(t)

	publish := publishAsync(t, nc, cfg, "vol-a", 10*time.Second)
	require.Equal(t, "vol-a", held.expectOpen(t))

	unpublish := unpublishAsync(t, nc, cfg, "vol-a")
	expectNoReply(t, unpublish, "the unpublish replied while the publish of the same volume was still opening it")

	held.releaseAll()
	published := decodePublish(t, awaitReply(t, publish))
	require.Nil(t, published.Error)
	var unpublished ebsprovider.UnpublishVolumeResponse
	require.NoError(t, json.Unmarshal(awaitReply(t, unpublish).Data, &unpublished))
	require.Nil(t, unpublished.Error)
	assert.True(t, unpublished.TornDown, "the unpublish must tear down the export the publish made")
	assertNotExported(t, cfg, "vol-a")
}

// A caller whose publish timed out sends a rollback unpublish. The publish is
// still running, and the rollback must not leave behind the export it makes.
func TestProviderDispatch_RollbackAfterDelayedPublishLeavesNoExport(t *testing.T) {
	cfg, nc, held := tcpDispatchProvider(t)

	publish := publishAsync(t, nc, cfg, "vol-a", quietWindow)
	require.Equal(t, "vol-a", held.expectOpen(t))
	r := <-publish
	require.ErrorIs(t, r.err, nats.ErrTimeout)

	rollback := unpublishAsync(t, nc, cfg, "vol-a")
	expectNoReply(t, rollback, "the rollback replied before the publish it rolls back had finished")

	held.releaseAll()
	awaitReply(t, rollback)
	assertNotExported(t, cfg, "vol-a")
}

// One volume with work queued on it does not stop a publish of another.
func TestProviderDispatch_OtherVolumesProgressWhileOneIsBusy(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	held := holdEngineOpens(t, cfg, failedOpen)
	nc := startProviderSubjects(t, cfg, natsURL)

	queued := []<-chan natsReply{publishAsync(t, nc, cfg, "vol-a", 10*time.Second)}
	require.Equal(t, "vol-a", held.expectOpen(t))
	queued = append(queued,
		unpublishAsync(t, nc, cfg, "vol-a"),
		publishAsync(t, nc, cfg, "vol-a", 10*time.Second),
		publishAsync(t, nc, cfg, "vol-a", 10*time.Second))

	queued = append(queued, publishAsync(t, nc, cfg, "vol-b", 10*time.Second))
	assert.Equal(t, "vol-b", held.expectOpen(t), "a second open of vol-a overlapped the first, or vol-b waited for vol-a")

	held.releaseAll()
	for _, ch := range queued {
		awaitReply(t, ch)
	}
}

// Under TCP transport, a free port is only reserved once nbdkit binds it, so
// two starts at the same time could share one. Publishes run one at a time.
func TestProviderDispatch_TCPPublishesStartOneAtATime(t *testing.T) {
	cfg, nc, held := tcpDispatchProvider(t)

	publishA := publishAsync(t, nc, cfg, "vol-a", 10*time.Second)
	require.Equal(t, "vol-a", held.expectOpen(t))
	publishB := publishAsync(t, nc, cfg, "vol-b", 10*time.Second)
	held.expectNoOpen(t, "a TCP publish started while another was opening")

	held.releaseAll()
	require.Equal(t, "vol-b", held.expectOpen(t))
	endpoints := map[string]string{}
	for volume, ch := range map[string]<-chan natsReply{"vol-a": publishA, "vol-b": publishB} {
		resp := decodePublish(t, awaitReply(t, ch))
		require.Nil(t, resp.Error)
		require.NotNil(t, resp.Published)
		require.NotNil(t, resp.Published.Export.NBD)
		mv, ok := findMountedVolume(cfg, volume)
		require.True(t, ok)
		assert.Equal(t, mv.NBDURI, resp.Published.Export.NBD.NBDURI, "the reply for %s names another volume's export", volume)
		endpoints[volume] = mv.NBDURI
	}
	assert.NotEqual(t, endpoints["vol-a"], endpoints["vol-b"])

	for _, volume := range []string{"vol-a", "vol-b"} {
		awaitReply(t, unpublishAsync(t, nc, cfg, volume))
		assertNotExported(t, cfg, volume)
	}
}

// A stop or detach does not wait behind publishes of other volumes, even when
// every publish slot is taken.
func TestProviderDispatch_UnpublishDoesNotWaitForOtherVolumes(t *testing.T) {
	_, natsURL := setupEmbeddedNATS(t)
	cfg := setupTestConfig(t, natsURL)
	held := holdEngineOpens(t, cfg, failedOpen)
	nc := startProviderSubjects(t, cfg, natsURL)

	const slots = 8
	var publishes []<-chan natsReply
	for i := range slots {
		publishes = append(publishes, publishAsync(t, nc, cfg, fmt.Sprintf("vol-held%d", i), 10*time.Second))
	}
	for range slots {
		held.expectOpen(t)
	}

	resp := unpublishAsync(t, nc, cfg, "vol-other")
	select {
	case r := <-resp:
		require.NoError(t, r.err)
	case <-time.After(5 * time.Second):
		t.Fatal("the unpublish waited for publishes of other volumes")
	}

	held.releaseAll()
	for _, ch := range publishes {
		awaitReply(t, ch)
	}
}
