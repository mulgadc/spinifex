package storevolume

//test:in-package — the host tools, mkfs and NBD copy are swapped for fakes
// through unexported fields so no test formats or writes a real device.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/volume"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ VolumeService = (*volume.VolumeServiceImpl)(nil)

const (
	testAccount = "000000000000"
	testVolume  = "vol-0123456789abcdef0"
	testNode    = "node-a"
	testURI     = "nbd://127.0.0.1:10809"
)

// harness records every call, in order, across the fakes.
type harness struct {
	mu    sync.Mutex
	calls []string

	createState  string
	createErr    error
	publishErr   error
	exportKind   ebsprovider.ExportKind
	unpublishErr error
	writeErr     error
	completeErr  error
	discardErr   error
	mkfsErr      error
	missing      map[string]bool

	completeApplies bool
	unpublishBlocks bool
	state           string

	writtenImage   string
	unpublishCtxOK bool
}

func (h *harness) record(s string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, s)
}

func (h *harness) CreateVolumeForPreparation(_ context.Context, in *ec2.CreateVolumeInput, accountID string) (*ec2.Volume, error) {
	h.record(fmt.Sprintf("create %s %dGiB", accountID, aws.Int64Value(in.Size)))
	if h.createErr != nil {
		return nil, h.createErr
	}
	state := h.createState
	if state == "" {
		state = "creating"
	}
	return &ec2.Volume{VolumeId: aws.String(testVolume), State: aws.String(state)}, nil
}

func (h *harness) CompleteVolumePreparation(_ context.Context, accountID, volumeID string) error {
	h.record("complete " + volumeID)
	if h.completeErr == nil || h.completeApplies {
		h.state = "available"
	}
	return h.completeErr
}

func (h *harness) DiscardVolumePreparation(_ context.Context, accountID, volumeID string) error {
	h.record("discard " + volumeID)
	return h.discardErr
}

func (h *harness) PublishVolume(_ context.Context, req ebsprovider.PublishVolumeRequest) (*ebsprovider.PublishedVolume, error) {
	h.record("publish " + req.VolumeID + " on " + req.NodeID)
	if h.publishErr != nil {
		return nil, h.publishErr
	}
	kind := h.exportKind
	if kind == "" {
		kind = ebsprovider.ExportKindNBD
	}
	export := ebsprovider.VolumeExport{Kind: kind}
	if kind == ebsprovider.ExportKindNBD {
		export.NBD = &ebsprovider.NBDExport{NBDURI: testURI}
	}
	return &ebsprovider.PublishedVolume{VolumeID: req.VolumeID, NodeID: req.NodeID, Export: export}, nil
}

func (h *harness) UnpublishVolume(ctx context.Context, req ebsprovider.UnpublishVolumeRequest) error {
	h.record("unpublish " + req.VolumeID + " on " + req.NodeID)
	h.unpublishCtxOK = ctx.Err() == nil
	if h.unpublishBlocks {
		<-ctx.Done()
		return ctx.Err()
	}
	return h.unpublishErr
}

func (h *harness) preparer(t *testing.T) *Preparer {
	t.Helper()
	return &Preparer{
		Volumes: h, Provider: h, NodeID: testNode, ScratchDir: t.TempDir(),
		lookPath: func(name string) (string, error) {
			if h.missing[name] {
				return "", errors.New("not found")
			}
			return "/fake/" + name, nil
		},
		toolPaths: []string{},
		runMkfs: func(_ context.Context, mkfs, path string) error {
			h.record("mkfs " + filepath.Base(path))
			return h.mkfsErr
		},
		WriteImage: func(_ context.Context, src, uri string, _ io.Writer) error {
			h.record("write " + uri)
			h.writtenImage = src
			return h.writeErr
		},
	}
}

func prepare(t *testing.T, h *harness) (string, error) {
	t.Helper()
	return h.preparer(t).Prepare(context.Background(), Request{AccountID: testAccount, AvailabilityZone: "ap-southeast-2a", SizeGiB: 1})
}

func requireRetained(t *testing.T, err error) {
	t.Helper()
	var retained *RetainedVolumeError
	require.ErrorAs(t, err, &retained)
	assert.Equal(t, testVolume, retained.VolumeID)
}

func TestPrepare_FormatsBeforeAllocatingThenReleasesOnlyAfterUnpublish(t *testing.T) {
	h := &harness{}
	p := h.preparer(t)
	id, err := p.Prepare(context.Background(), Request{AccountID: testAccount, AvailabilityZone: "ap-southeast-2a", SizeGiB: 1})
	require.NoError(t, err)
	assert.Equal(t, testVolume, id)
	assert.Equal(t, []string{
		"mkfs ext4.raw",
		"create 000000000000 1GiB",
		"publish " + testVolume + " on " + testNode,
		"write " + testURI,
		"unpublish " + testVolume + " on " + testNode,
		"complete " + testVolume,
	}, h.calls)

	assert.True(t, strings.HasPrefix(h.writtenImage, p.ScratchDir), "the image must come from private scratch")
	entries, err := os.ReadDir(p.ScratchDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "the scratch image must be removed")
}

func TestPrepare_MissingToolAllocatesNothing(t *testing.T) {
	for _, tool := range []string{"mkfs.ext4", "qemu-img"} {
		t.Run(tool, func(t *testing.T) {
			h := &harness{missing: map[string]bool{tool: true}}
			_, err := prepare(t, h)
			require.ErrorIs(t, err, ErrToolMissing)
			assert.Contains(t, err.Error(), tool)
			assert.Empty(t, h.calls)
		})
	}
}

func TestPrepare_MissingImageWriterAllocatesNothing(t *testing.T) {
	h := &harness{}
	p := h.preparer(t)
	p.WriteImage = nil
	_, err := p.Prepare(context.Background(), Request{AccountID: testAccount, AvailabilityZone: "ap-southeast-2a", SizeGiB: 1})
	require.ErrorContains(t, err, "image writer")
	assert.Empty(t, h.calls)
}

func TestPrepare_FindsSbinToolOffThePath(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mkfs.ext4"), []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "qemu-img"), []byte("not executable"), 0o644))
	p := &Preparer{lookPath: func(string) (string, error) { return "", errors.New("not found") }, toolPaths: []string{dir}}

	got, err := p.findTool("mkfs.ext4")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "mkfs.ext4"), got)
	_, err = p.findTool("qemu-img")
	require.ErrorIs(t, err, ErrToolMissing, "a file that is not executable is not the tool")
}

// The copy runs qemu-img by name, so a qemu-img found only in sbin must not
// pass the check that runs before anything is allocated.
func TestPrepare_QemuImgOnlyInSbinAllocatesNothing(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "qemu-img"), []byte("#!/bin/sh\n"), 0o755))
	h := &harness{missing: map[string]bool{"qemu-img": true}}
	p := h.preparer(t)
	p.toolPaths = []string{dir}
	_, err := p.Prepare(context.Background(), Request{AccountID: testAccount, AvailabilityZone: "ap-southeast-2a", SizeGiB: 1})
	require.ErrorIs(t, err, ErrToolMissing)
	assert.Contains(t, err.Error(), "qemu-img")
	assert.Empty(t, h.calls)
}

func TestPrepare_FailedFormatAllocatesNothing(t *testing.T) {
	h := &harness{mkfsErr: errors.New("mkfs exploded")}
	_, err := prepare(t, h)
	require.ErrorContains(t, err, "mkfs exploded")
	assert.Equal(t, []string{"mkfs ext4.raw"}, h.calls)
}

func TestPrepare_FailedCreateTouchesNoVolume(t *testing.T) {
	h := &harness{createErr: errors.New("no capacity")}
	_, err := prepare(t, h)
	require.ErrorContains(t, err, "no capacity")
	assert.Equal(t, []string{"mkfs ext4.raw", "create 000000000000 1GiB"}, h.calls)
}

// A volume that does not come back creating may not be this call's alone, so
// nothing is published, written or deleted.
func TestPrepare_VolumeNotBornCreatingIsNeverWritten(t *testing.T) {
	h := &harness{createState: "available"}
	_, err := prepare(t, h)
	requireRetained(t, err)
	assert.Equal(t, []string{"mkfs ext4.raw", "create 000000000000 1GiB"}, h.calls)
}

func TestPrepare_FailedCopyUnpublishesThenDiscards(t *testing.T) {
	h := &harness{writeErr: errors.New("qemu-img convert: broken pipe")}
	_, err := prepare(t, h)
	require.ErrorContains(t, err, "broken pipe")
	require.ErrorContains(t, err, "discarded")
	var retained *RetainedVolumeError
	assert.NotErrorAs(t, err, &retained)
	assert.Equal(t, []string{
		"mkfs ext4.raw", "create 000000000000 1GiB",
		"publish " + testVolume + " on " + testNode, "write " + testURI,
		"unpublish " + testVolume + " on " + testNode, "discard " + testVolume,
	}, h.calls)
}

func TestPrepare_NonNBDExportIsNotWrittenAndIsDiscarded(t *testing.T) {
	h := &harness{exportKind: "iscsi"}
	_, err := prepare(t, h)
	require.ErrorContains(t, err, "not published as an nbd export")
	assert.NotContains(t, h.calls, "write "+testURI)
	assert.Equal(t, "discard "+testVolume, h.calls[len(h.calls)-1])
}

// An in-use or already-exists refusal may mean another export of the volume
// is live, and unpublish names only volume and node, so it is never sent.
func TestPrepare_PublishRefusedForAnotherExportSendsNoUnpublish(t *testing.T) {
	for name, perr := range map[string]error{
		"volume in use":  &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeVolumeInUse, Message: "in use"},
		"already exists": &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeAlreadyExists, Message: "exists"},
		"wrapped":        fmt.Errorf("nats provider: %w", &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeVolumeInUse, Message: "in use"}),
	} {
		t.Run(name, func(t *testing.T) {
			h := &harness{publishErr: perr}
			_, err := prepare(t, h)
			requireRetained(t, err)
			require.ErrorContains(t, err, "export not touched")
			assert.Equal(t, []string{
				"mkfs ext4.raw", "create 000000000000 1GiB",
				"publish " + testVolume + " on " + testNode,
			}, h.calls, "no unpublish, write, discard or release")
		})
	}
}

// Any other publish error may still land, so the volume is kept with its ID
// reported even when the best-effort unpublish succeeds.
func TestPrepare_AmbiguousPublishRetainsTheVolume(t *testing.T) {
	for name, perr := range map[string]error{
		"deadline":         context.DeadlineExceeded,
		"unavailable":      &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeUnavailable, Message: "lease busy"},
		"internal":         &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeInternal, Message: "mount half done"},
		"invalid argument": &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeInvalidArgument, Message: "bad node"},
		"not found":        &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeNotFound, Message: "no volume"},
	} {
		t.Run(name, func(t *testing.T) {
			h := &harness{publishErr: perr}
			_, err := prepare(t, h)
			requireRetained(t, err)
			assert.Equal(t, []string{
				"mkfs ext4.raw", "create 000000000000 1GiB",
				"publish " + testVolume + " on " + testNode,
				"unpublish " + testVolume + " on " + testNode,
			}, h.calls, "no write, no discard, no release")
		})
	}
}

func TestPrepare_FailedUnpublishRetainsTheVolume(t *testing.T) {
	for name, writeErr := range map[string]error{"after a good copy": nil, "after a failed copy": errors.New("short write")} {
		t.Run(name, func(t *testing.T) {
			h := &harness{writeErr: writeErr, unpublishErr: errors.New("nats: timeout")}
			_, err := prepare(t, h)
			requireRetained(t, err)
			require.ErrorContains(t, err, "nats: timeout")
			require.ErrorContains(t, err, "retained in creating", "the release never ran, so the state is known")
			last := h.calls[len(h.calls)-1]
			assert.Equal(t, "unpublish "+testVolume+" on "+testNode, last, "no discard or release after an unconfirmed unpublish")
		})
	}
}

// A failed release write may still have landed, leaving the volume available
// to others, so it is never discarded and its state is reported as unknown.
func TestPrepare_FailedReleaseRetainsWithUnknownState(t *testing.T) {
	for name, applied := range map[string]bool{"write not applied": false, "write applied, then error": true} {
		t.Run(name, func(t *testing.T) {
			h := &harness{completeErr: errors.New("object store: request failed"), completeApplies: applied}
			_, err := prepare(t, h)
			requireRetained(t, err)
			var retained *RetainedVolumeError
			require.ErrorAs(t, err, &retained)
			assert.True(t, retained.StateUnknown)
			require.ErrorContains(t, err, "unknown state (creating or available)")
			assert.NotContains(t, err.Error(), "retained in creating")
			assert.Equal(t, "complete "+testVolume, h.calls[len(h.calls)-1], "no discard after an uncertain release")
			if applied {
				assert.Equal(t, "available", h.state, "the fake really did release it, which is why discard would be wrong")
			}
		})
	}
}

// Cleanup ignores caller cancellation but is bounded, so a provider that
// never answers cannot hang the caller.
func TestPrepare_StuckUnpublishIsBounded(t *testing.T) {
	h := &harness{unpublishBlocks: true}
	p := h.preparer(t)
	p.cleanupTimeout = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	p.WriteImage = func(context.Context, string, string, io.Writer) error {
		cancel()
		return context.Canceled
	}
	done := make(chan error, 1)
	go func() {
		_, err := p.Prepare(ctx, Request{AccountID: testAccount, AvailabilityZone: "ap-southeast-2a", SizeGiB: 1})
		done <- err
	}()
	select {
	case err := <-done:
		requireRetained(t, err)
		require.ErrorIs(t, err, context.DeadlineExceeded, "unpublish ends on its own bound, not the caller's cancel")
		assert.NotContains(t, h.calls, "discard "+testVolume)
	case <-time.After(10 * time.Second):
		t.Fatal("Prepare hung on a stuck unpublish")
	}
}

func TestPrepare_FailedDiscardRetainsTheVolume(t *testing.T) {
	h := &harness{writeErr: errors.New("short write"), discardErr: errors.New("provider unreachable")}
	_, err := prepare(t, h)
	requireRetained(t, err)
	require.ErrorContains(t, err, "provider unreachable")
}

// Unpublish is what closes the export, so a caller cancelling mid-copy must
// not make it skip that step.
func TestPrepare_CancelledCopyStillUnpublishes(t *testing.T) {
	h := &harness{}
	ctx, cancel := context.WithCancel(context.Background())
	p := h.preparer(t)
	p.WriteImage = func(context.Context, string, string, io.Writer) error {
		cancel()
		return context.Canceled
	}
	_, err := p.Prepare(ctx, Request{AccountID: testAccount, AvailabilityZone: "ap-southeast-2a", SizeGiB: 1})
	require.Error(t, err)
	assert.Contains(t, h.calls, "unpublish "+testVolume+" on "+testNode)
	assert.True(t, h.unpublishCtxOK, "unpublish must run with a live context")
	assert.Equal(t, "discard "+testVolume, h.calls[len(h.calls)-1])
}

func TestPrepare_RejectsIncompleteRequestBeforeAnything(t *testing.T) {
	for name, req := range map[string]Request{
		"no account": {AvailabilityZone: "ap-southeast-2a", SizeGiB: 1},
		"no zone":    {AccountID: testAccount, SizeGiB: 1},
		"zero size":  {AccountID: testAccount, AvailabilityZone: "ap-southeast-2a"},
	} {
		t.Run(name, func(t *testing.T) {
			h := &harness{}
			_, err := h.preparer(t).Prepare(context.Background(), req)
			require.Error(t, err)
			assert.Empty(t, h.calls)
		})
	}
}

// The real formatter produces an ext4 superblock the guest's e2fsck accepts.
func TestBuildImage_RealMkfsWritesExt4(t *testing.T) {
	mkfs, err := (&Preparer{}).findTool("mkfs.ext4")
	if err != nil {
		t.Skip("mkfs.ext4 not installed")
	}
	path := filepath.Join(t.TempDir(), "ext4.raw")
	require.NoError(t, buildImage(context.Background(), runMkfsExt4, mkfs, path, 1))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, int64(1<<30), info.Size())
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	magic := make([]byte, 2)
	_, err = f.ReadAt(magic, 1024+0x38)
	require.NoError(t, err)
	assert.Equal(t, uint16(0xEF53), binary.LittleEndian.Uint16(magic))
	if fsck, err := (&Preparer{}).findTool("e2fsck"); err == nil {
		out, err := exec.Command(fsck, "-n", "-f", path).CombinedOutput()
		require.NoError(t, err, "e2fsck must find the new filesystem clean: %s", out)
	}

	require.Error(t, buildImage(context.Background(), runMkfsExt4, mkfs, path, 1), "an existing file is never reused")
}
