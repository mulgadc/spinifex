// Package natsserve is the provider-neutral NATS server for the
// ebs.provider.v1.* wire contract defined by spinifex/ebsprovider. It
// depends on nothing but the ebsprovider.EBSProvider interface, so any implementation of that interface can be served over NATS by calling Serve.
package natsserve

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/mulgadc/spinifex/spinifex/ebsprovider"
	"github.com/nats-io/nats.go"
)

// DefaultQueueGroup is the NATS queue group Serve subscribes the
// multi-node-shared subjects (every subject except the per-node
// publish/unpublish pair) under by default, matching viperblockd's production subscriptions.
const DefaultQueueGroup = "spinifex-workers"

// Options configures Serve's subscription behavior.
type Options struct {
	// QueueGroup names the queue group the shared subjects (Capabilities,
	// CreateVolume, GetVolume, ExpandVolume, DeleteVolume, DeleteSnapshot,
	// CopySnapshot, and the snapshot-create wildcard) subscribe under. Empty defaults to DefaultQueueGroup; set NoQueueGroup to opt out.
	QueueGroup string

	// NoQueueGroup subscribes the shared subjects plainly instead of under
	// a queue group, e.g. for a single-instance server where load balancing
	// across workers does not apply.
	NoQueueGroup bool

	// NodeID scopes PublishVolume/UnpublishVolume to this node's own
	// subjects (ebsprovider.PublishSubject / UnpublishSubject), matching a
	// production daemon that can only mount volumes on its own node. Left empty (the default), Serve instead subscribes the wildcard "ebs.provider.v1.*.mount"/"unmount" subjects, answering requests for any node.
	NodeID string
}

// Serve subscribes every subject spinifex/ebsprovider/nats.go defines,
// except owner-routed subjects (see the comment in the function body), and
// delegates each to provider. The returned stop function unsubscribes everything Serve registered; it is safe to call more than once.
func Serve(ctx context.Context, nc *nats.Conn, provider ebsprovider.EBSProvider, opts Options) (stop func(), err error) {
	// Owner-subject routing (ebsprovider.OwnerSubject and its per-verb
	// wrappers) is not served: it routes a request to the node holding a
	// volume's live engine via a per-node mounted-volume registry the EBSProvider interface does not model, so it stays viperblockd-specific.
	queueGroup := opts.QueueGroup
	if queueGroup == "" {
		queueGroup = DefaultQueueGroup
	}
	if opts.NoQueueGroup {
		queueGroup = ""
	}

	var subs []*nats.Subscription
	cleanup := func() {
		for _, sub := range subs {
			_ = sub.Unsubscribe()
		}
	}

	subscribeShared := func(subject string, handler nats.MsgHandler) error {
		var sub *nats.Subscription
		var subErr error
		if queueGroup != "" {
			sub, subErr = nc.QueueSubscribe(subject, queueGroup, handler)
		} else {
			sub, subErr = nc.Subscribe(subject, handler)
		}
		if subErr != nil {
			return fmt.Errorf("subscribe to %s: %w", subject, subErr)
		}
		subs = append(subs, sub)
		return nil
	}

	// Publish/Unpublish are never queue-grouped: each subject already
	// addresses exactly one node (or, in the wildcard case, is answered by
	// the one backing provider standing in for every node), so load balancing across a group has nothing to add.
	subscribePlain := func(subject string, handler nats.MsgHandler) error {
		sub, subErr := nc.Subscribe(subject, handler)
		if subErr != nil {
			return fmt.Errorf("subscribe to %s: %w", subject, subErr)
		}
		subs = append(subs, sub)
		return nil
	}

	steps := []struct {
		subject string
		handler nats.MsgHandler
	}{
		{ebsprovider.CapabilitiesSubject, handleCapabilities(ctx, provider)},
		{ebsprovider.CreateVolumeSubject, handleCreateVolume(ctx, provider)},
		{ebsprovider.GetVolumeSubject, handleGetVolume(ctx, provider)},
		{ebsprovider.ExpandVolumeSubject, handleExpandVolume(ctx, provider)},
		{ebsprovider.DeleteVolumeSubject, handleDeleteVolume(ctx, provider)},
		{ebsprovider.DeleteSnapshotSubject, handleDeleteSnapshot(ctx, provider)},
		{ebsprovider.CopySnapshotSubject, handleCopySnapshot(ctx, provider)},
		{ebsprovider.SnapshotCreateSubjectPrefix + "*", handleCreateSnapshot(ctx, nc, provider)},
	}
	for _, step := range steps {
		if err := subscribeShared(step.subject, step.handler); err != nil {
			cleanup()
			return nil, err
		}
	}

	if opts.NodeID == "" {
		if err := subscribePlain("ebs.provider.v1.*.mount", handlePublishVolume(ctx, provider)); err != nil {
			cleanup()
			return nil, err
		}
		if err := subscribePlain("ebs.provider.v1.*.unmount", handleUnpublishVolume(ctx, provider)); err != nil {
			cleanup()
			return nil, err
		}
	} else {
		publishSubject, err := ebsprovider.PublishSubject(opts.NodeID)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("build publish subject: %w", err)
		}
		if err := subscribePlain(publishSubject, handlePublishVolume(ctx, provider)); err != nil {
			cleanup()
			return nil, err
		}
		unpublishSubject, err := ebsprovider.UnpublishSubject(opts.NodeID)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("build unpublish subject: %w", err)
		}
		if err := subscribePlain(unpublishSubject, handleUnpublishVolume(ctx, provider)); err != nil {
			cleanup()
			return nil, err
		}
	}

	if err := nc.Flush(); err != nil {
		cleanup()
		return nil, fmt.Errorf("flush subscriptions: %w", err)
	}

	var once sync.Once
	return func() { once.Do(cleanup) }, nil
}

func respond(msg *nats.Msg, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		slog.Error("ebs.provider: failed to marshal response", "subject", msg.Subject, "err", err)
		return
	}
	if err := msg.Respond(data); err != nil {
		slog.Error("ebs.provider: failed to send response", "subject", msg.Subject, "err", err)
	}
}

// badRequestError maps a JSON decode failure to the wire error shape,
// mirroring viperblockd's badRequestError: a malformed request body is the
// caller's mistake, not a server-side failure.
func badRequestError(err error) *ebsprovider.ProviderError {
	return &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeInvalidArgument, Message: fmt.Sprintf("bad request: %v", err)}
}

func versionError(got uint16) *ebsprovider.ProviderError {
	return &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeUnsupportedVersion, Message: fmt.Sprintf("unsupported schema version %d, want %d", got, ebsprovider.SchemaVersion)}
}

func invalidArgumentError(format string, args ...any) *ebsprovider.ProviderError {
	return &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeInvalidArgument, Message: fmt.Sprintf(format, args...)}
}

func internalError(format string, args ...any) *ebsprovider.ProviderError {
	return &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeInternal, Message: fmt.Sprintf(format, args...)}
}

// classifyError maps a provider error to the wire ProviderError shape,
// using only the sentinel errors and ErrorCode values errors.go exports:
// it has no forward (error -> ProviderError) mapper of its own to call.
func classifyError(err error) *ebsprovider.ProviderError {
	switch {
	case errors.Is(err, ebsprovider.ErrAlreadyExists):
		return &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeAlreadyExists, Message: err.Error()}
	case errors.Is(err, ebsprovider.ErrInvalidArgument):
		return &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeInvalidArgument, Message: err.Error()}
	case errors.Is(err, ebsprovider.ErrNotFound):
		return &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeNotFound, Message: err.Error()}
	case errors.Is(err, ebsprovider.ErrUnsupportedVersion):
		return &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeUnsupportedVersion, Message: err.Error()}
	case errors.Is(err, ebsprovider.ErrVolumeInUse):
		return &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeVolumeInUse, Message: err.Error()}
	case errors.Is(err, ebsprovider.ErrUnsupportedCapability):
		return &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeUnsupportedCap, Message: err.Error()}
	default:
		return &ebsprovider.ProviderError{Code: ebsprovider.ErrorCodeInternal, Message: err.Error()}
	}
}

func handleCapabilities(ctx context.Context, provider ebsprovider.EBSProvider) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var req ebsprovider.GetCapabilitiesRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			respond(msg, ebsprovider.GetCapabilitiesResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
			return
		}
		if req.SchemaVersion != ebsprovider.SchemaVersion {
			respond(msg, ebsprovider.GetCapabilitiesResponse{Versioned: ebsprovider.NewVersioned(), Error: versionError(req.SchemaVersion)})
			return
		}
		resp, err := provider.GetCapabilities(ctx, req)
		if err != nil {
			respond(msg, ebsprovider.GetCapabilitiesResponse{Versioned: ebsprovider.NewVersioned(), Error: classifyError(err)})
			return
		}
		respond(msg, *resp)
	}
}

func handleCreateVolume(ctx context.Context, provider ebsprovider.EBSProvider) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var req ebsprovider.CreateVolumeRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			respond(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
			return
		}
		if req.SchemaVersion != ebsprovider.SchemaVersion {
			respond(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: versionError(req.SchemaVersion)})
			return
		}
		vol, err := provider.CreateVolume(ctx, req)
		if err != nil {
			respond(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: classifyError(err)})
			return
		}
		respond(msg, ebsprovider.CreateVolumeResponse{Versioned: ebsprovider.NewVersioned(), Volume: vol})
	}
}

func handleGetVolume(ctx context.Context, provider ebsprovider.EBSProvider) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var req ebsprovider.GetVolumeRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			respond(msg, ebsprovider.GetVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
			return
		}
		if req.SchemaVersion != ebsprovider.SchemaVersion {
			respond(msg, ebsprovider.GetVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: versionError(req.SchemaVersion)})
			return
		}
		vol, err := provider.GetVolume(ctx, req)
		if err != nil {
			respond(msg, ebsprovider.GetVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: classifyError(err)})
			return
		}
		respond(msg, ebsprovider.GetVolumeResponse{Versioned: ebsprovider.NewVersioned(), Volume: vol})
	}
}

func handleExpandVolume(ctx context.Context, provider ebsprovider.EBSProvider) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var req ebsprovider.ExpandVolumeRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			respond(msg, ebsprovider.ExpandVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
			return
		}
		if req.SchemaVersion != ebsprovider.SchemaVersion {
			respond(msg, ebsprovider.ExpandVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: versionError(req.SchemaVersion)})
			return
		}
		vol, err := provider.ExpandVolume(ctx, req)
		if err != nil {
			respond(msg, ebsprovider.ExpandVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: classifyError(err)})
			return
		}
		respond(msg, ebsprovider.ExpandVolumeResponse{Versioned: ebsprovider.NewVersioned(), Volume: vol})
	}
}

func handleDeleteVolume(ctx context.Context, provider ebsprovider.EBSProvider) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var req ebsprovider.DeleteVolumeRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			respond(msg, ebsprovider.DeleteVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
			return
		}
		if req.SchemaVersion != ebsprovider.SchemaVersion {
			respond(msg, ebsprovider.DeleteVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: versionError(req.SchemaVersion)})
			return
		}
		if err := provider.DeleteVolume(ctx, req); err != nil {
			respond(msg, ebsprovider.DeleteVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: classifyError(err)})
			return
		}
		respond(msg, ebsprovider.DeleteVolumeResponse{Versioned: ebsprovider.NewVersioned()})
	}
}

func handleDeleteSnapshot(ctx context.Context, provider ebsprovider.EBSProvider) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var req ebsprovider.DeleteSnapshotRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			respond(msg, ebsprovider.DeleteSnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
			return
		}
		if req.SchemaVersion != ebsprovider.SchemaVersion {
			respond(msg, ebsprovider.DeleteSnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: versionError(req.SchemaVersion)})
			return
		}
		if err := provider.DeleteSnapshot(ctx, req); err != nil {
			respond(msg, ebsprovider.DeleteSnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: classifyError(err)})
			return
		}
		respond(msg, ebsprovider.DeleteSnapshotResponse{Versioned: ebsprovider.NewVersioned()})
	}
}

func handleCopySnapshot(ctx context.Context, provider ebsprovider.EBSProvider) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var req ebsprovider.CopySnapshotRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			respond(msg, ebsprovider.CopySnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
			return
		}
		if req.SchemaVersion != ebsprovider.SchemaVersion {
			respond(msg, ebsprovider.CopySnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: versionError(req.SchemaVersion)})
			return
		}
		snap, err := provider.CopySnapshot(ctx, req)
		if err != nil {
			respond(msg, ebsprovider.CopySnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: classifyError(err)})
			return
		}
		respond(msg, ebsprovider.CopySnapshotResponse{Versioned: ebsprovider.NewVersioned(), Snapshot: snap})
	}
}

// handleCreateSnapshot serves the SnapshotCreateSubjectPrefix wildcard by
// replying immediately with a pending Snapshot plus an OperationID, then
// running the blocking EBSProvider.CreateSnapshot call in the background and publishing its result: a transport-level wrapping, so no async form is needed in the interface itself.
func handleCreateSnapshot(ctx context.Context, nc *nats.Conn, provider ebsprovider.EBSProvider) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var req ebsprovider.CreateSnapshotRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			respond(msg, ebsprovider.CreateSnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
			return
		}
		if req.SchemaVersion != ebsprovider.SchemaVersion {
			respond(msg, ebsprovider.CreateSnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: versionError(req.SchemaVersion)})
			return
		}

		// The wildcard subject carries the source volume ID as its own
		// token (SnapshotCreateSubjectPrefix + volumeID); a mismatch with
		// the request body means the two disagree about which volume this create is for.
		if subjectVolumeID, ok := strings.CutPrefix(msg.Subject, ebsprovider.SnapshotCreateSubjectPrefix); ok && subjectVolumeID != req.VolumeID {
			respond(msg, ebsprovider.CreateSnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: invalidArgumentError("volume id %q in subject does not match request volume id %q", subjectVolumeID, req.VolumeID)})
			return
		}

		completionSubject, err := ebsprovider.SnapshotCompletionSubject(req.SnapshotID)
		if err != nil {
			respond(msg, ebsprovider.CreateSnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: classifyError(err)})
			return
		}
		operationID, err := newOperationID()
		if err != nil {
			respond(msg, ebsprovider.CreateSnapshotResponse{Versioned: ebsprovider.NewVersioned(), Error: internalError("generate operation id: %v", err)})
			return
		}

		respond(msg, ebsprovider.CreateSnapshotResponse{
			Versioned:         ebsprovider.NewVersioned(),
			OperationID:       operationID,
			CompletionSubject: completionSubject,
			Snapshot: &ebsprovider.Snapshot{
				ID:             req.SnapshotID,
				SourceVolumeID: req.VolumeID,
				State:          ebsprovider.SnapshotStatePending,
			},
		})

		go completeCreateSnapshot(ctx, nc, provider, req, operationID, completionSubject)
	}
}

func completeCreateSnapshot(ctx context.Context, nc *nats.Conn, provider ebsprovider.EBSProvider, req ebsprovider.CreateSnapshotRequest, operationID, completionSubject string) {
	completion := ebsprovider.CreateSnapshotResponse{
		Versioned:         ebsprovider.NewVersioned(),
		OperationID:       operationID,
		CompletionSubject: completionSubject,
	}

	snapshot, err := provider.CreateSnapshot(ctx, req)
	if err != nil {
		completion.Error = classifyError(err)
	} else {
		completion.Snapshot = snapshot
	}

	data, err := json.Marshal(completion)
	if err != nil {
		slog.Error("ebs.provider.snapshot.create: failed to marshal completion", "volume", req.VolumeID, "snapshot", req.SnapshotID, "err", err)
		return
	}
	if err := nc.Publish(completionSubject, data); err != nil {
		slog.Error("ebs.provider.snapshot.create: failed to publish completion", "subject", completionSubject, "err", err)
	}
}

// newOperationID mints a random operation ID without depending on anything
// beyond the standard library, keeping natsserve's only non-stdlib
// dependencies ebsprovider and nats.go.
func newOperationID() (string, error) {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return "op-" + hex.EncodeToString(b), nil
}

func handlePublishVolume(ctx context.Context, provider ebsprovider.EBSProvider) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var req ebsprovider.PublishVolumeRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			respond(msg, ebsprovider.PublishVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
			return
		}
		if req.SchemaVersion != ebsprovider.SchemaVersion {
			respond(msg, ebsprovider.PublishVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: versionError(req.SchemaVersion)})
			return
		}
		pub, err := provider.PublishVolume(ctx, req)
		if err != nil {
			respond(msg, ebsprovider.PublishVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: classifyError(err)})
			return
		}
		respond(msg, ebsprovider.PublishVolumeResponse{Versioned: ebsprovider.NewVersioned(), Published: pub})
	}
}

func handleUnpublishVolume(ctx context.Context, provider ebsprovider.EBSProvider) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var req ebsprovider.UnpublishVolumeRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			respond(msg, ebsprovider.UnpublishVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: badRequestError(err)})
			return
		}
		if req.SchemaVersion != ebsprovider.SchemaVersion {
			respond(msg, ebsprovider.UnpublishVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: versionError(req.SchemaVersion)})
			return
		}
		if err := provider.UnpublishVolume(ctx, req); err != nil {
			respond(msg, ebsprovider.UnpublishVolumeResponse{Versioned: ebsprovider.NewVersioned(), Error: classifyError(err)})
			return
		}
		respond(msg, ebsprovider.UnpublishVolumeResponse{Versioned: ebsprovider.NewVersioned()})
	}
}
