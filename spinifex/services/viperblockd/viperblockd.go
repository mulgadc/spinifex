package viperblockd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mulgadc/bluebottle/pkg/masterkey"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/mulgadc/viperblock/viperblock/backends/s3"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const viperblockdTracerName = "github.com/mulgadc/spinifex/spinifex/services/viperblockd"

// endSpanWithResponseError marks span failed when respErr is non-empty, then ends it.
func endSpanWithResponseError(span trace.Span, respErr string) {
	if respErr != "" {
		span.RecordError(errors.New(respErr))
		span.SetStatus(codes.Error, respErr)
	}
	span.End()
}

// loadStateRetryAttempts / loadStateRetryBaseDelay tune the mount-time retry
// loop (5 attempts at 200ms * 1.5^n ≈ 3.7s; well under the 30s NATS timeout).
const (
	loadStateRetryAttempts  = 5
	loadStateRetryBaseDelay = 200 * time.Millisecond
)

// retryLoadState invokes loadFn with exponential backoff (delay * 3/2 each step)
// on ErrStateBackendUnavailable only; other errors return immediately.
func retryLoadState(volume string, attempts int, baseDelay time.Duration, sleep func(time.Duration), loadFn func() error) error {
	delay := baseDelay
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		err = loadFn()
		if err == nil {
			if attempt > 1 {
				slog.Info("LoadState succeeded after retry",
					"volume", volume, "attempt", attempt)
			}
			return nil
		}
		if !errors.Is(err, viperblock.ErrStateBackendUnavailable) {
			return err
		}
		if attempt == attempts {
			break
		}
		slog.Warn("LoadState transient failure, retrying",
			"volume", volume, "attempt", attempt, "delay", delay, "err", err)
		sleep(delay)
		delay = delay * 3 / 2
	}
	return fmt.Errorf("LoadState exhausted %d retries: %w", attempts, err)
}

// loadStateWithRetry calls vb.LoadState with the production retry budget
// (loadStateRetryAttempts / loadStateRetryBaseDelay).
func loadStateWithRetry(vb *viperblock.VB, volume string) error {
	return retryLoadState(volume, loadStateRetryAttempts, loadStateRetryBaseDelay, time.Sleep, vb.LoadState)
}

var serviceName = "viperblock"

type MountedVolume struct {
	Name      string
	Port      int    // TCP port (when using TCP transport)
	Socket    string // Unix socket path (when using socket transport)
	NBDURI    string // Full NBD URI (nbd:unix:/path.sock or nbd://host:port)
	PID       int
	VB        *viperblock.VB     // Reference to viperblock instance for state sync/flush
	ConfigSub *nats.Subscription // Per-volume config-update subscription (ebs.config.{volumeID})
}
type Config struct {
	ConfigPath     string
	PluginPath     string
	Debug          bool
	NatsHost       string
	NatsToken      string
	NatsCACert     string
	MountedVolumes []MountedVolume
	S3Host         string
	Bucket         string
	Region         string
	AccessKey      string
	SecretKey      string
	BaseDir        string

	// NodeName identifies this node in the cluster (e.g. "node1").
	// Used for node-specific NATS topics: ebs.{NodeName}.mount / ebs.{NodeName}.unmount.
	// If empty, falls back to generic ebs.mount / ebs.unmount with queue group (single-node compat).
	NodeName string

	// NBDTransport controls the transport type: "socket" (default) or "tcp"
	// Socket is faster for local connections, TCP required for remote/DPU scenarios
	NBDTransport types.NBDTransport

	// ShardWAL enables sharded WAL for mounted volumes (default false)
	ShardWAL bool

	// GCEnabled turns on viperblock chunk garbage collection for every VB this
	// service constructs: the nbdkit plugin backing each mounted volume, and
	// the short-lived detached VBs opened for config updates and sealing.
	// Default false, matching ShardWAL.
	GCEnabled bool

	// EncryptionKeyFile is the path to the shared AES-256 master key for at-rest
	// encryption. Empty → cleartext mode (legacy).
	EncryptionKeyFile string

	masterKey *masterkey.Key

	// sealVolume overrides how a detached volume is sealed to predastore.
	// Nil means sealVolumeVB, the real seal. Tests that need a seal to FAIL
	// inject here rather than pointing S3Host at an unreachable endpoint: the
	// real failure only arrives once the S3 client exhausts its jittered
	// backoff, which takes anywhere from one to five seconds and so decides the
	// test on where the dice land relative to the caller's deadline.
	sealVolume func(volumeName string) error

	mu sync.Mutex
}

// seal persists volumeName's block map to predastore, honouring a test's
// injected seal if there is one.
func (cfg *Config) seal(volumeName string) error {
	if cfg.sealVolume != nil {
		return cfg.sealVolume(volumeName)
	}
	return sealVolumeVB(cfg, volumeName)
}

type Service struct {
	Config *Config
}

//  nbdkit -p 10812 --pidfile /tmp/vb-vol-1.pid ./lib/nbdkit-viperblock-plugin.so -v -f size=67108864 volume=vol-2 bucket=predastore region=ap-southeast-2 access_key="X" secret_key="Y" base_dir="/tmp/vb/" host="https://127.0.0.1:8443" cache_size=0

func New(config any) (svc *Service, err error) {
	cfg, ok := config.(*Config)
	if !ok {
		return nil, fmt.Errorf("invalid config type for viperblockd service")
	}
	svc = &Service{
		Config: cfg,
	}

	return svc, nil
}

// applyConfigUpdate writes a control-plane VolumeConfig onto a viperblock
// instance and reseals its state. For encrypted volumes SaveState recomputes the
// AES-GCM tag under the volume's current StateSeqNum, so the caller MUST own the
// volume exclusively (live mounted VB, or a freshly opened detached one) to keep
// the GCM nonce unique.
func applyConfigUpdate(vb *viperblock.VB, req types.EBSConfigUpdateRequest) error {
	var vc viperblock.VolumeConfig
	if err := json.Unmarshal(req.VolumeConfig, &vc); err != nil {
		return fmt.Errorf("unmarshal VolumeConfig: %w", err)
	}
	vb.VolumeConfig = vc
	// Reconcile grow-only volume size (mirrors the EC2 handler merge path).
	if sz := vc.VolumeMetadata.SizeGiB * 1024 * 1024 * 1024; sz > vb.VolumeSize {
		vb.VolumeSize = sz
	}
	return vb.SaveState()
}

// makeConfigUpdateHandler returns a NATS handler for volume-specific config
// updates (ebs.config.{volumeID}). It runs against the live mounted VB, which is
// the single writer that owns the volume's StateSeqNum.
func makeConfigUpdateHandler(vb *viperblock.VB, volumeName string) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var req types.EBSConfigUpdateRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			slog.Error("Failed to unmarshal ebs.config message", "volume", volumeName, "err", err)
			respondJSON(msg, types.EBSConfigUpdateResponse{Volume: volumeName, Error: fmt.Sprintf("bad request: %v", err)})
			return
		}
		if err := applyConfigUpdate(vb, req); err != nil {
			slog.Error("ebs.config: live VB update failed", "volume", volumeName, "err", err)
			respondJSON(msg, types.EBSConfigUpdateResponse{Volume: volumeName, Error: err.Error()})
			return
		}
		slog.Info("ebs.config: live VB state updated", "volume", volumeName)
		respondJSON(msg, types.EBSConfigUpdateResponse{Volume: volumeName, Success: true})
	}
}

// openVolumeVB constructs and opens an existing viperblock volume with its
// config state loaded (LoadState) but NOT its block map. Construction mirrors
// the ebs.mount path so encrypted volumes open with the master key and matching
// encryption state. Callers that Close() the VB MUST go through
// openLoadedVolumeVB instead, so the block map is restored before Close()
// flushes it back to predastore.
func openVolumeVB(cfg *Config, volumeName string) (*viperblock.VB, error) {
	s3cfg := s3.S3Config{
		VolumeName: volumeName,
		Bucket:     cfg.Bucket,
		Region:     cfg.Region,
		AccessKey:  cfg.AccessKey,
		SecretKey:  cfg.SecretKey,
		Host:       admin.DialTarget(cfg.S3Host),
	}
	vbconfig := viperblock.VB{
		VolumeName:        volumeName,
		VolumeSize:        1, // Recalculated on LoadState.
		BaseDir:           cfg.BaseDir,
		VolumeConfig:      viperblock.VolumeConfig{},
		MasterKey:         cfg.masterKey,
		EncryptionEnabled: cfg.masterKey != nil,
		GCEnabled:         cfg.GCEnabled,
	}
	vb, err := viperblock.New(&vbconfig, "s3", s3cfg)
	if err != nil {
		return nil, fmt.Errorf("new viperblock: %w", err)
	}

	// New starts the chunk uploader and WAL syncer, so a failure below must
	// release them: the caller gets no handle to stop them with.
	opened := false
	defer func() {
		if !opened {
			vb.StopChunkUploader()
			vb.StopWALSyncer()
		}
	}()

	if err := vb.Backend.Init(); err != nil {
		return nil, fmt.Errorf("backend init: %w", err)
	}
	if err := loadStateWithRetry(vb, volumeName); err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	opened = true
	return vb, nil
}

// isAuxVolume reports whether a volume is an -efi auxiliary volume. Auxiliary
// volumes are recreated on launch and carry no durable guest data, so they
// never need sealing to predastore.
func isAuxVolume(volumeName string) bool {
	return strings.HasSuffix(volumeName, "-efi")
}

// validVolumeName reports whether volume is safe to use as a single path
// component under BaseDir: non-empty, no separator, and not "." or "..".
func validVolumeName(volume string) bool {
	return volume != "" && volume != "." && volume != ".." && filepath.Base(volume) == volume
}

// localVolumeDir validates volume and baseDir, then returns baseDir/volume.
// Every handler that turns a wire-supplied volume name into a filesystem path
// (mount, unmount, delete, config) must go through this: the name arrives
// unmarshalled straight from a NATS message, so an empty or ".."-laden value
// must never reach a filesystem call. The character checks in validVolumeName
// already rule out an escape, but the path is still resolved and checked
// against baseDir as a second, independent guard.
func localVolumeDir(baseDir, volume string) (string, error) {
	if baseDir == "" {
		return "", fmt.Errorf("empty base directory")
	}
	if !validVolumeName(volume) {
		return "", fmt.Errorf("invalid volume name %q", volume)
	}

	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("resolve base directory: %w", err)
	}
	dir := filepath.Join(absBase, volume)

	rel, err := filepath.Rel(absBase, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("volume name %q escapes base directory", volume)
	}
	return dir, nil
}

// volumeNeedsSeal reports whether an unmounted volume has local viperblock
// state under baseDir/<volume> to flush. A node that never held the local
// WAL has nothing to seal. Callers handle auxiliary volumes separately (see
// isAuxVolume).
func volumeNeedsSeal(volumeName, baseDir string) bool {
	dir, err := localVolumeDir(baseDir, volumeName)
	if err != nil {
		slog.Warn("volumeNeedsSeal: rejecting invalid volume name", "volume", volumeName, "err", err)
		return false
	}
	_, err = os.Stat(dir)
	return err == nil
}

// sealReceiptSuffix names the file the nbdkit plugin leaves at
// baseDir/<volume>.sealed after a successful seal.
const sealReceiptSuffix = ".sealed"

// sealReceipt is the fixed shape the nbdkit plugin writes after a successful
// seal. PID is diagnostic only: it is never used to judge staleness, since a
// receipt is cleared at mount instead (see clearStaleSealReceipt).
type sealReceipt struct {
	Volume   string    `json:"volume"`
	PID      int       `json:"pid"`
	SealedAt time.Time `json:"sealed_at"`
}

// sealReceiptPath returns the receipt path for a volume under baseDir.
func sealReceiptPath(baseDir, volume string) string {
	return filepath.Join(baseDir, volume+sealReceiptSuffix)
}

// consumeSealReceipt reads and deletes baseDir/<volume>.sealed, reporting
// whether a valid receipt was there. It never fails the unmount: the seal it
// attests to already happened, so a missing or unreadable receipt only costs
// the caller a WARN.
func consumeSealReceipt(baseDir, volume string) bool {
	path := sealReceiptPath(baseDir, volume)

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("failed to read seal receipt", "volume", volume, "path", path, "err", err)
		}
		return false
	}

	// Delete unconditionally, even if the contents below turn out to be
	// invalid, so a malformed receipt cannot linger and be misread later.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to remove seal receipt", "volume", volume, "path", path, "err", err)
	}

	var receipt sealReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		slog.Warn("malformed seal receipt", "volume", volume, "path", path, "err", err)
		return false
	}
	if receipt.Volume != volume || receipt.SealedAt.IsZero() {
		slog.Warn("seal receipt missing required fields", "volume", volume, "path", path, "receipt", receipt)
		return false
	}
	return true
}

// clearStaleSealReceipt removes any seal receipt left by a previous mount of
// this volume. Staleness is handled here, at mount, rather than by matching
// PIDs at unmount: once this runs, any receipt found at the next unmount can
// only have come from the plugin instance this mount is about to start.
func clearStaleSealReceipt(baseDir, volume string) {
	path := sealReceiptPath(baseDir, volume)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Warn("failed to clear stale seal receipt", "volume", volume, "path", path, "err", err)
	}
}

// openLoadedVolumeVB opens a detached volume and fully restores its state for a
// short-lived operation that ends in Close(): LoadState + LoadBlockState +
// RecoverLocalWALs. Skipping LoadBlockState would leave an empty in-memory block
// map that Close() then flushes over the good checkpoint in predastore — silent
// data loss (a reattach then finds an empty map, bad superblock). The caller
// MUST Close the returned VB; on error the WAL syncer is stopped and no VB is
// returned. The caller MUST ensure no nbdkit process is writing the shared
// BaseDir first (post-KillProcess, or volume detached).
func openLoadedVolumeVB(cfg *Config, volumeName string) (*viperblock.VB, error) {
	vb, err := openVolumeVB(cfg, volumeName)
	if err != nil {
		return nil, err
	}
	if err := vb.LoadBlockState(); err != nil {
		vb.StopWALSyncer()
		return nil, fmt.Errorf("load block state: %w", err)
	}
	// RecoverLocalWALs is fail-closed on integrity errors and persists recovered
	// state itself; on failure retain the local WAL (no Close) for retry.
	if err := vb.RecoverLocalWALs(); err != nil {
		vb.StopWALSyncer()
		return nil, fmt.Errorf("recover local WALs: %w", err)
	}
	return vb, nil
}

// sealVolumeVB persists a detached volume's block->object map to predastore.
// The runtime nbdkit plugin is the only path that flushes the map on close and
// it does not reliably fire on detach, so without this seal a reattach on a
// node lacking the local WAL finds no checkpoint (bad superblock). It mirrors
// the plugin's recover sequence (LoadBlockState + RecoverLocalWALs replay
// un-sealed chunk WALs) then Close()s to flush the map.
func sealVolumeVB(cfg *Config, volumeName string) error {
	vb, err := openLoadedVolumeVB(cfg, volumeName)
	if err != nil {
		return err
	}
	// Close removes local files only after the predastore writes succeed, so a
	// failed seal leaves the WAL intact rather than losing data.
	if err := vb.Close(); err != nil {
		return fmt.Errorf("seal close: %w", err)
	}
	return nil
}

// respondJSON marshals data and sends it as a NATS response. On marshal
// failure a raw JSON error string is sent instead.
func respondJSON(msg *nats.Msg, data any) {
	response, err := json.Marshal(data)
	if err != nil {
		slog.Error("Failed to marshal response", "type", fmt.Sprintf("%T", data), "err", err)
		_ = msg.Respond([]byte(`{"Error":"internal marshal failure"}`))
		return
	}
	if err := msg.Respond(response); err != nil {
		slog.Error("Failed to respond to NATS request", "err", err)
	}
}

// respondAndPublish is like respondJSON but also publishes the marshaled
// response to the given NATS topic (used for ebs.mount.response etc.).
func respondAndPublish(msg *nats.Msg, nc *nats.Conn, topic string, data any) {
	response, err := json.Marshal(data)
	if err != nil {
		slog.Error("Failed to marshal response", "type", fmt.Sprintf("%T", data), "err", err)
		_ = msg.Respond([]byte(`{"Error":"internal marshal failure"}`))
		return
	}
	if err := msg.Respond(response); err != nil {
		slog.Error("Failed to respond to NATS request", "err", err)
	}
	if err := nc.Publish(topic, response); err != nil {
		slog.Error("Failed to publish response", "topic", topic, "err", err)
	}
}

func (svc *Service) Start() (int, error) {
	if err := utils.WritePidFileTo(svc.Config.BaseDir, serviceName, os.Getpid()); err != nil {
		return 0, fmt.Errorf("write pid file: %w", err)
	}
	err := launchService(svc.Config)

	if err != nil {
		slog.Error("Failed to launch service", "err", err)
		return 0, err
	}

	return os.Getpid(), nil
}

func (svc *Service) Stop() (err error) {
	return utils.StopProcessAt(svc.Config.BaseDir, serviceName)
}

func (svc *Service) Status() (string, error) {
	return utils.ServiceStatus(svc.Config.BaseDir, serviceName)
}

func (svc *Service) Shutdown() (err error) {
	return svc.Stop()
}

func (svc *Service) Reload() (err error) {
	return nil
}

func launchService(cfg *Config) (err error) {
	nc, err := utils.ConnectNATSWithRetry(admin.DialTarget(cfg.NatsHost), cfg.NatsToken, cfg.NatsCACert)
	if err != nil {
		slog.Error("Failed to connect to NATS", "err", err)
		return err
	}

	if cfg.EncryptionKeyFile != "" {
		mkey, err := masterkey.LoadShared(cfg.EncryptionKeyFile)
		if err != nil {
			return fmt.Errorf("load viperblock encryption key %s: %w", cfg.EncryptionKeyFile, err)
		}
		cfg.masterKey = mkey
		slog.Info("Viperblock at-rest encryption enabled", "key_fingerprint", mkey.Fingerprint)
	} else {
		slog.Warn("Viperblock at-rest encryption disabled (no EncryptionKeyFile configured)")
	}

	slog.Info("Viperblock config", "shardwal", cfg.ShardWAL, "gc_enabled", cfg.GCEnabled)

	if cfg.NodeName != "" {
		slog.Info("Waiting for EBS events", "node", cfg.NodeName)
	} else {
		slog.Info("Waiting for EBS events (single-node mode)")
	}

	if _, err := nc.QueueSubscribe("ebs.delete", "spinifex-workers", func(msg *nats.Msg) {
		ctx, span := utils.StartConsumerSpan(msg)
		defer span.End()
		slog.InfoContext(ctx, "Received ebs.delete message")

		var ebsRequest types.EBSDeleteRequest
		if err := json.Unmarshal(msg.Data, &ebsRequest); err != nil {
			slog.ErrorContext(ctx, "Failed to unmarshal ebs.delete message", "err", err)
			utils.MarkSpanError(span, err)
			respondJSON(msg, types.EBSDeleteResponse{Error: fmt.Sprintf("bad request: %v", err)})
			return
		}

		// Reject before touching anything: an empty or path-traversing name
		// must never reach the RemoveAll below, which is otherwise wide
		// enough to wipe BaseDir itself or escape it entirely.
		localPath, err := localVolumeDir(cfg.BaseDir, ebsRequest.Volume)
		if err != nil {
			slog.ErrorContext(ctx, "ebs.delete: refusing invalid volume name", "volume", ebsRequest.Volume, "err", err)
			utils.MarkSpanError(span, err)
			respondJSON(msg, types.EBSDeleteResponse{Volume: ebsRequest.Volume, Error: fmt.Sprintf("invalid volume name: %v", err)})
			return
		}

		response := types.EBSDeleteResponse{Volume: ebsRequest.Volume, Success: true}

		// Find and clean up the mounted volume if it exists
		cfg.mu.Lock()
		var matched MountedVolume
		matchIdx := -1
		for i, volume := range cfg.MountedVolumes {
			if volume.Name == ebsRequest.Volume {
				matched = volume
				matchIdx = i
				cfg.MountedVolumes = append(cfg.MountedVolumes[:i], cfg.MountedVolumes[i+1:]...)
				break
			}
		}
		cfg.mu.Unlock()

		if matchIdx >= 0 {
			// Unsubscribe from volume-specific config-update topic
			if matched.ConfigSub != nil {
				if err := matched.ConfigSub.Unsubscribe(); err != nil {
					slog.ErrorContext(ctx, "Failed to unsubscribe config topic", "volume", ebsRequest.Volume, "err", err)
				}
			}
			// Stop background goroutines and kill nbdkit process
			if matched.VB != nil {
				matched.VB.StopChunkUploader()
				matched.VB.StopWALSyncer()
			}
			if err := utils.KillProcess(matched.PID); err != nil {
				slog.ErrorContext(ctx, "Failed to kill nbdkit process", "pid", matched.PID, "err", err)
			}

			// Remove the socket file if using socket transport
			if matched.Socket != "" {
				slog.InfoContext(ctx, "Removing socket file", "socket", matched.Socket)
				if err := os.Remove(matched.Socket); err != nil && !os.IsNotExist(err) {
					slog.ErrorContext(ctx, "Failed to delete nbd socket", "err", err, "socket", matched.Socket)
				}
			}

			slog.InfoContext(ctx, "ebs.delete: cleaned up mounted volume", "volume", ebsRequest.Volume, "pid", matched.PID)
		} else {
			// Volume not mounted is expected for "available" volumes
			slog.InfoContext(ctx, "ebs.delete: volume not mounted (expected for available volumes)", "volume", ebsRequest.Volume)
		}

		// Delete is permanent: remove the on-disk WAL/checkpoint cache
		// regardless of mount-tracking state. -efi volumes never go through
		// the unmount seal (isAuxVolume skips it, they carry no durable
		// data), and a main volume's seal can have been skipped after a
		// failed flush (e.g. disk full), so this is the only guaranteed
		// cleanup point for both. localPath was validated above.
		if err := os.RemoveAll(localPath); err != nil {
			slog.ErrorContext(ctx, "ebs.delete: failed to remove local volume directory",
				"volume", ebsRequest.Volume, "path", localPath, "err", err)
		} else {
			slog.InfoContext(ctx, "ebs.delete: removed local volume directory",
				"volume", ebsRequest.Volume, "path", localPath)
		}

		respondJSON(msg, response)
	}); err != nil {
		return fmt.Errorf("failed to subscribe to ebs.delete: %w", err)
	}

	// Subscribe to node-specific unmount topic if NodeName is set, otherwise fall back to generic queue group
	unmountTopic := "ebs.unmount"
	if cfg.NodeName != "" {
		unmountTopic = fmt.Sprintf("ebs.%s.unmount", cfg.NodeName)
	}
	unmountSubscribe := func(topic string, handler nats.MsgHandler) (*nats.Subscription, error) {
		if cfg.NodeName != "" {
			return nc.Subscribe(topic, handler)
		}
		return nc.QueueSubscribe(topic, "spinifex-workers", handler)
	}
	if _, err := unmountSubscribe(unmountTopic, func(msg *nats.Msg) {
		ctx, span := utils.StartConsumerSpan(msg)
		defer span.End()
		slog.InfoContext(ctx, "Received message")

		var ebsRequest types.EBSRequest
		if err := json.Unmarshal(msg.Data, &ebsRequest); err != nil {
			slog.ErrorContext(ctx, "Failed to unmarshal ebs.unmount message", "err", err)
			utils.MarkSpanError(span, err)
			respondJSON(msg, types.EBSUnMountResponse{Error: fmt.Sprintf("bad request: %v", err)})
			return
		}

		ebsResponse, _ := unmountVolume(ctx, cfg, ebsRequest.Name)
		respondAndPublish(msg, nc, "ebs.unmount.response", ebsResponse)
	}); err != nil {
		return fmt.Errorf("failed to subscribe to %s: %w", unmountTopic, err)
	}

	if _, err := nc.QueueSubscribe("ebs.sync", "spinifex-workers", func(msg *nats.Msg) {
		ctx, span := utils.StartConsumerSpan(msg)
		defer span.End()
		slog.InfoContext(ctx, "Received ebs.sync message")

		var syncRequest types.EBSSyncRequest
		if err := json.Unmarshal(msg.Data, &syncRequest); err != nil {
			slog.ErrorContext(ctx, "Failed to unmarshal ebs.sync message", "err", err)
			utils.MarkSpanError(span, err)
			respondJSON(msg, types.EBSSyncResponse{Error: fmt.Sprintf("bad request: %v", err)})
			return
		}

		syncResponse := types.EBSSyncResponse{Volume: syncRequest.Volume}
		defer func() {
			if syncResponse.Error != "" {
				utils.MarkSpanError(span, errors.New(syncResponse.Error))
			}
		}()

		// Find the mounted volume and reload its state from the backend
		cfg.mu.Lock()
		var foundVB *viperblock.VB
		for _, volume := range cfg.MountedVolumes {
			if volume.Name == syncRequest.Volume && volume.VB != nil {
				foundVB = volume.VB
				break
			}
		}
		cfg.mu.Unlock()

		if foundVB == nil {
			syncResponse.Error = fmt.Sprintf("volume %s not mounted or has no VB instance", syncRequest.Volume)
			slog.WarnContext(ctx, "ebs.sync: volume not found", "volume", syncRequest.Volume)
		} else if err := foundVB.LoadState(); err != nil {
			syncResponse.Error = fmt.Sprintf("failed to reload state: %v", err)
			slog.ErrorContext(ctx, "ebs.sync: LoadState failed", "volume", syncRequest.Volume, "err", err)
		} else {
			syncResponse.Synced = true
			slog.InfoContext(ctx, "ebs.sync: state reloaded", "volume", syncRequest.Volume,
				"volumeSize", foundVB.GetVolumeSize())
		}

		respondJSON(msg, syncResponse)
	}); err != nil {
		return fmt.Errorf("failed to subscribe to ebs.sync: %w", err)
	}

	// ebs.config is the fallback for encrypted-volume config updates whose
	// per-volume ebs.config.{volumeID} topic had no responder (volume not
	// mounted anywhere). A detached volume has no live writer, so any worker may
	// open it exclusively and reseal. A mount that raced in is still handled by
	// preferring the live VB when this node happens to own it.
	if _, err := nc.QueueSubscribe("ebs.config", "spinifex-workers", func(msg *nats.Msg) {
		ctx, span := utils.StartConsumerSpan(msg)
		defer span.End()

		var req types.EBSConfigUpdateRequest
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			slog.ErrorContext(ctx, "Failed to unmarshal ebs.config message", "err", err)
			utils.MarkSpanError(span, err)
			respondJSON(msg, types.EBSConfigUpdateResponse{Error: fmt.Sprintf("bad request: %v", err)})
			return
		}

		// Reject before the not-live branch below can hand this name to
		// openLoadedVolumeVB, which derives a BaseDir/<volume> path.
		if !validVolumeName(req.Volume) {
			err := fmt.Errorf("invalid volume name %q", req.Volume)
			slog.ErrorContext(ctx, "ebs.config: refusing invalid volume name", "volume", req.Volume)
			utils.MarkSpanError(span, err)
			respondJSON(msg, types.EBSConfigUpdateResponse{Volume: req.Volume, Error: err.Error()})
			return
		}

		cfg.mu.Lock()
		var live *viperblock.VB
		for _, volume := range cfg.MountedVolumes {
			if volume.Name == req.Volume && volume.VB != nil {
				live = volume.VB
				break
			}
		}
		cfg.mu.Unlock()

		if live != nil {
			if err := applyConfigUpdate(live, req); err != nil {
				slog.ErrorContext(ctx, "ebs.config: live VB update failed", "volume", req.Volume, "err", err)
				utils.MarkSpanError(span, err)
				respondJSON(msg, types.EBSConfigUpdateResponse{Volume: req.Volume, Error: err.Error()})
				return
			}
			slog.InfoContext(ctx, "ebs.config: live VB state updated (fallback path)", "volume", req.Volume)
			respondJSON(msg, types.EBSConfigUpdateResponse{Volume: req.Volume, Success: true})
			return
		}

		vb, err := openLoadedVolumeVB(cfg, req.Volume)
		if err != nil {
			slog.ErrorContext(ctx, "ebs.config: failed to open detached volume", "volume", req.Volume, "err", err)
			utils.MarkSpanError(span, err)
			respondJSON(msg, types.EBSConfigUpdateResponse{Volume: req.Volume, Error: fmt.Sprintf("open volume: %v", err)})
			return
		}
		applyErr := applyConfigUpdate(vb, req)
		if closeErr := vb.Close(); closeErr != nil {
			slog.ErrorContext(ctx, "ebs.config: VB close failed", "volume", req.Volume, "err", closeErr)
		}
		if applyErr != nil {
			slog.ErrorContext(ctx, "ebs.config: detached volume update failed", "volume", req.Volume, "err", applyErr)
			utils.MarkSpanError(span, applyErr)
			respondJSON(msg, types.EBSConfigUpdateResponse{Volume: req.Volume, Error: applyErr.Error()})
			return
		}
		slog.InfoContext(ctx, "ebs.config: detached volume state updated", "volume", req.Volume)
		respondJSON(msg, types.EBSConfigUpdateResponse{Volume: req.Volume, Success: true})
	}); err != nil {
		return fmt.Errorf("failed to subscribe to ebs.config: %w", err)
	}

	// Note: ebs.snapshot is handled per-volume via ebs.snapshot.{volumeID} topics,
	// subscribed at mount time and unsubscribed at unmount time. This ensures
	// snapshot requests are routed to the node that owns the volume.

	// Subscribe to node-specific mount topic if NodeName is set, otherwise fall back to generic queue group
	mountTopic := "ebs.mount"
	if cfg.NodeName != "" {
		mountTopic = fmt.Sprintf("ebs.%s.mount", cfg.NodeName)
	}
	mountSubscribe := func(topic string, handler nats.MsgHandler) (*nats.Subscription, error) {
		if cfg.NodeName != "" {
			return nc.Subscribe(topic, handler)
		}
		return nc.QueueSubscribe(topic, "spinifex-workers", handler)
	}
	if _, err := mountSubscribe(mountTopic, func(msg *nats.Msg) {
		ctx, span := utils.StartConsumerSpan(msg)
		defer span.End()
		slog.InfoContext(ctx, "Received message:")

		var ebsRequest types.EBSRequest
		if err := json.Unmarshal(msg.Data, &ebsRequest); err != nil {
			slog.ErrorContext(ctx, "Failed to unmarshal ebs.mount message", "err", err)
			utils.MarkSpanError(span, err)
			respondJSON(msg, types.EBSMountResponse{Error: fmt.Sprintf("bad request: %v", err)})
			return
		}

		slog.InfoContext(ctx, "ebs.mount", "request", ebsRequest)

		// Reject before any local path is derived from the name: this is the
		// first point an unvalidated wire name would otherwise reach the
		// filesystem (clearStaleSealReceipt) and, further down, viperblock's
		// own BaseDir/<volume> layout.
		if !validVolumeName(ebsRequest.Name) {
			err := fmt.Errorf("invalid volume name %q", ebsRequest.Name)
			slog.ErrorContext(ctx, "ebs.mount: refusing invalid volume name", "volume", ebsRequest.Name)
			utils.MarkSpanError(span, err)
			respondJSON(msg, types.EBSMountResponse{Error: err.Error()})
			return
		}

		ebsResponse, _ := mountVolume(ctx, cfg, nc, ebsRequest.Name)
		respondAndPublish(msg, nc, "ebs.mount.response", ebsResponse)
		if ebsResponse.Mounted {
			slog.Debug("Sent ebs.mount response")
		}
	}); err != nil {
		return fmt.Errorf("failed to subscribe to %s: %w", mountTopic, err)
	}

	if err := registerProviderSubjects(cfg, nc); err != nil {
		return err
	}

	// Create a channel to receive shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Wait for shutdown signal
	<-sigChan
	slog.Info("Shutting down gracefully...")

	nc.Close()

	// Snapshot mounted volumes and clear the list while holding the lock,
	// then flush/kill outside the lock (VB.Close does heavy I/O).
	cfg.mu.Lock()
	volumes := make([]MountedVolume, len(cfg.MountedVolumes))
	copy(volumes, cfg.MountedVolumes)
	cfg.MountedVolumes = nil
	cfg.mu.Unlock()

	shutdownVolumes(volumes, nbdkitInUse)

	return nil
}

// shutdownVolumes flushes each mounted volume's WAL on SIGTERM but only reaps
// nbdkit for volumes with no attached guest (inUse false). Killing an nbdkit a
// guest is still writing through corrupts that guest's filesystem; the graceful
// drain (or unmount) path owns reaping in-use nbdkit after the guest is gone.
//
// The reap itself fans out one goroutine per idle volume so the wall-clock
// cost is bounded by the slowest single nbdkit's utils.KillProcess grace, not
// the sum across every mounted volume — the caller returns (and the process
// exits) only once every goroutine below has finished.
func shutdownVolumes(volumes []MountedVolume, inUse func(MountedVolume) bool) {
	var wg sync.WaitGroup
	for _, volume := range volumes {
		if volume.VB != nil {
			volume.VB.StopChunkUploader()
			volume.VB.StopWALSyncer()
		}
		if inUse(volume) {
			slog.Warn("nbdkit still serving a guest; leaving it for the drain/unmount path",
				"pid", volume.PID, "name", volume.Name, "socket", volume.Socket)
			continue
		}
		wg.Add(1)
		go func(volume MountedVolume) {
			defer wg.Done()
			slog.Info("Killing idle nbdkit process", "pid", volume.PID, "name", volume.Name)
			if err := utils.KillProcess(volume.PID); err != nil {
				slog.Error("Failed to kill nbdkit process", "pid", volume.PID, "err", err)
			}
		}(volume)
	}
	wg.Wait()
}

// nbdkitInUse best-effort reports whether nbdkit's NBD endpoint still has a
// connected client (a guest). On any uncertainty it returns true so the
// shutdown path never tears a backing store out from under a running guest.
func nbdkitInUse(vol MountedVolume) bool {
	if vol.Socket == "" {
		// TCP transport: cannot cheaply confirm idle — assume in use.
		return true
	}
	out, err := exec.Command("ss", "-H", "-x", "-a").Output()
	if err != nil {
		return true
	}
	// ss -H rows are: <netid> <state> <recvq> <sendq> <local-addr> ...
	// LISTEN is the idle server socket; ESTAB means a client is attached.
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[1] == "ESTAB" && strings.Contains(line, vol.Socket) {
			return true
		}
	}
	return false
}
