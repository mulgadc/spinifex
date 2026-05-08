package viperblockd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/nbd"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/mulgadc/viperblock/viperblock"
	"github.com/mulgadc/viperblock/viperblock/backends/s3"

	"github.com/nats-io/nats.go"
)

var serviceName = "viperblock"

type MountedVolume struct {
	Name        string
	Port        int    // TCP port (when using TCP transport)
	Socket      string // Unix socket path (when using socket transport)
	NBDURI      string // Full NBD URI (nbd:unix:/path.sock or nbd://host:port)
	PID         int
	VB          *viperblock.VB     // Reference to viperblock instance for state sync/flush
	SnapshotSub *nats.Subscription // Per-volume snapshot subscription (ebs.snapshot.{volumeID})
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

	mu sync.Mutex
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

// makeSnapshotHandler returns a NATS handler for volume-specific snapshot requests (ebs.snapshot.{volumeID}).
func makeSnapshotHandler(vb *viperblock.VB, volumeName string) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var snapRequest types.EBSSnapshotRequest
		if err := json.Unmarshal(msg.Data, &snapRequest); err != nil {
			slog.Error("Failed to unmarshal ebs.snapshot message", "volume", volumeName, "err", err)
			respondJSON(msg, types.EBSSnapshotResponse{Error: fmt.Sprintf("bad request: %v", err)})
			return
		}

		slog.Info("ebs.snapshot: processing snapshot request", "volume", volumeName, "snapshotId", snapRequest.SnapshotID)

		snapResponse := types.EBSSnapshotResponse{SnapshotID: snapRequest.SnapshotID}

		if _, err := vb.CreateSnapshot(snapRequest.SnapshotID); err != nil {
			snapResponse.Error = fmt.Sprintf("snapshot failed: %v", err)
			slog.Error("ebs.snapshot: CreateSnapshot failed", "volume", volumeName, "snapshotId", snapRequest.SnapshotID, "err", err)
		} else {
			snapResponse.Success = true
			slog.Info("ebs.snapshot: snapshot created", "volume", volumeName, "snapshotId", snapRequest.SnapshotID)
		}

		respondJSON(msg, snapResponse)
	}
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
	// Connect to NATS
	nc, err := utils.ConnectNATSWithRetry(admin.DialTarget(cfg.NatsHost), cfg.NatsToken, cfg.NatsCACert)
	if err != nil {
		slog.Error("Failed to connect to NATS", "err", err)
		return err
	}

	slog.Info("Viperblock config", "shardwal", cfg.ShardWAL)

	if cfg.NodeName != "" {
		slog.Info("Waiting for EBS events", "node", cfg.NodeName)
	} else {
		slog.Info("Waiting for EBS events (single-node mode)")
	}

	if _, err := nc.QueueSubscribe("ebs.delete", "spinifex-workers", func(msg *nats.Msg) {
		slog.Info("Received ebs.delete message", "data", string(msg.Data))

		var ebsRequest types.EBSDeleteRequest
		if err := json.Unmarshal(msg.Data, &ebsRequest); err != nil {
			slog.Error("Failed to unmarshal ebs.delete message", "err", err)
			respondJSON(msg, types.EBSDeleteResponse{Error: fmt.Sprintf("bad request: %v", err)})
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
			// Unsubscribe from volume-specific snapshot topic
			if matched.SnapshotSub != nil {
				if err := matched.SnapshotSub.Unsubscribe(); err != nil {
					slog.Error("Failed to unsubscribe snapshot topic", "volume", ebsRequest.Volume, "err", err)
				}
			}
			// Stop WAL syncer and kill nbdkit process
			if matched.VB != nil {
				matched.VB.StopWALSyncer()
			}
			if err := utils.KillProcess(matched.PID); err != nil {
				slog.Error("Failed to kill nbdkit process", "pid", matched.PID, "err", err)
			}

			// Remove the socket file if using socket transport
			if matched.Socket != "" {
				slog.Info("Removing socket file", "socket", matched.Socket)
				if err := os.Remove(matched.Socket); err != nil && !os.IsNotExist(err) {
					slog.Error("Failed to delete nbd socket", "err", err, "socket", matched.Socket)
				}
			}

			slog.Info("ebs.delete: cleaned up mounted volume", "volume", ebsRequest.Volume, "pid", matched.PID)
		} else {
			// Volume not mounted is expected for "available" volumes
			slog.Info("ebs.delete: volume not mounted (expected for available volumes)", "volume", ebsRequest.Volume)
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
		slog.Info("Received message", "data", string(msg.Data))

		// Parse the message
		var ebsRequest types.EBSRequest
		if err := json.Unmarshal(msg.Data, &ebsRequest); err != nil {
			slog.Error("Failed to unmarshal ebs.unmount message", "err", err)
			respondJSON(msg, types.EBSUnMountResponse{Error: fmt.Sprintf("bad request: %v", err)})
			return
		}

		// Find the volume and extract references while holding the lock,
		// then release before calling VB.Close() (which does heavy S3 I/O).
		var ebsResponse types.EBSUnMountResponse
		var matched MountedVolume
		var matchIdx = -1
		cfg.mu.Lock()
		for i, volume := range cfg.MountedVolumes {
			if volume.Name == ebsRequest.Name {
				matched = volume
				matchIdx = i
				// Remove from slice while we hold the lock
				cfg.MountedVolumes = append(cfg.MountedVolumes[:i], cfg.MountedVolumes[i+1:]...)
				break
			}
		}
		cfg.mu.Unlock()

		if matchIdx >= 0 {
			ebsResponse = types.EBSUnMountResponse{
				Volume:  matched.Name,
				Mounted: false,
			}

			// Unsubscribe from volume-specific snapshot topic
			if matched.SnapshotSub != nil {
				if err := matched.SnapshotSub.Unsubscribe(); err != nil {
					slog.Error("Failed to unsubscribe snapshot topic", "volume", ebsRequest.Name, "err", err)
				}
			}

			// Clean up the VB instance's background goroutine.
			// This VB is state-only (LoadState/sync) — actual I/O is in the nbdkit plugin process.
			if matched.VB != nil {
				matched.VB.StopWALSyncer()
			}

			if err := utils.KillProcess(matched.PID); err != nil {
				slog.Error("Failed to kill nbdkit process", "pid", matched.PID, "err", err)
			}

			// Remove the socket file if using socket transport
			if matched.Socket != "" {
				slog.Info("Removing socket file", "socket", matched.Socket)
				if err := os.Remove(matched.Socket); err != nil && !os.IsNotExist(err) {
					slog.Error("Failed to delete nbd socket", "err", err, "socket", matched.Socket)
				}
			}
		}

		if matchIdx < 0 {
			ebsResponse = types.EBSUnMountResponse{
				Volume: ebsRequest.Name,
				Error:  fmt.Sprintf("Volume %s not found", ebsRequest.Name),
			}
		}

		respondAndPublish(msg, nc, "ebs.unmount.response", ebsResponse)
	}); err != nil {
		return fmt.Errorf("failed to subscribe to %s: %w", unmountTopic, err)
	}

	if _, err := nc.QueueSubscribe("ebs.sync", "spinifex-workers", func(msg *nats.Msg) {
		slog.Info("Received ebs.sync message", "data", string(msg.Data))

		var syncRequest types.EBSSyncRequest
		if err := json.Unmarshal(msg.Data, &syncRequest); err != nil {
			slog.Error("Failed to unmarshal ebs.sync message", "err", err)
			respondJSON(msg, types.EBSSyncResponse{Error: fmt.Sprintf("bad request: %v", err)})
			return
		}

		syncResponse := types.EBSSyncResponse{Volume: syncRequest.Volume}

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
			slog.Warn("ebs.sync: volume not found", "volume", syncRequest.Volume)
		} else if err := foundVB.LoadState(); err != nil {
			syncResponse.Error = fmt.Sprintf("failed to reload state: %v", err)
			slog.Error("ebs.sync: LoadState failed", "volume", syncRequest.Volume, "err", err)
		} else {
			syncResponse.Synced = true
			slog.Info("ebs.sync: state reloaded", "volume", syncRequest.Volume,
				"volumeSize", foundVB.GetVolumeSize())
		}

		respondJSON(msg, syncResponse)
	}); err != nil {
		return fmt.Errorf("failed to subscribe to ebs.sync: %w", err)
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
		slog.Info("Received message:", "data", string(msg.Data))

		// Parse the message
		var ebsRequest types.EBSRequest
		if err := json.Unmarshal(msg.Data, &ebsRequest); err != nil {
			slog.Error("Failed to unmarshal ebs.mount message", "err", err)
			respondJSON(msg, types.EBSMountResponse{Error: fmt.Sprintf("bad request: %v", err)})
			return
		}

		slog.Info("ebs.mount", "request", ebsRequest)

		var ebsResponse types.EBSMountResponse
		ebsResponse.Mounted = false

		s3cfg := s3.S3Config{
			VolumeName: ebsRequest.Name,
			Bucket:     cfg.Bucket,
			Region:     cfg.Region,
			AccessKey:  cfg.AccessKey,
			SecretKey:  cfg.SecretKey,
			Host:       admin.DialTarget(cfg.S3Host),
		}

		// TODO: Improve based on system availability. Default 128MB cache
		defaultCache := (128 * 1024 * 1024) / int(viperblock.DefaultBlockSize)

		vbconfig := viperblock.VB{
			VolumeName: ebsRequest.Name,
			VolumeSize: 1, // Workaround, calculated on LoadState()
			BaseDir:    cfg.BaseDir,
			Cache: viperblock.Cache{
				Config: viperblock.CacheConfig{
					// TODO: Improve, based on system memory
					Size: defaultCache,
				},
			},
			VolumeConfig: viperblock.VolumeConfig{},
		}

		vb, err := viperblock.New(&vbconfig, "s3", s3cfg)

		// Enable 128MB cache for main volumes, disable for cloudinit/efi (small, rarely read)
		// This cacheSize is passed to nbdkit plugin (separate viperblock instance)
		var nbdCacheSize int
		if strings.HasSuffix(ebsRequest.Name, "-cloudinit") || strings.HasSuffix(ebsRequest.Name, "-efi") {
			slog.Info("Disabling cache for auxiliary volume", "volume", ebsRequest.Name)
			if err := vb.SetCacheSize(0, 0); err != nil {
				slog.Error("Failed to set cache size", "err", err)
			}
			nbdCacheSize = 0
		} else {
			slog.Info("Enabling 128MB cache for main volume", "volume", ebsRequest.Name, "blocks", defaultCache)
			if err := vb.SetCacheSize(defaultCache, 0); err != nil {
				slog.Error("Failed to set cache size", "err", err)
			}
			nbdCacheSize = defaultCache
		}

		if err != nil {
			ebsResponse.Error = fmt.Sprintf("Failed to connect to Viperblock store: %v", err)
			respondAndPublish(msg, nc, "ebs.mount.response", ebsResponse)
			return
		}

		if cfg.Debug {
			vb.SetDebug(true)
		}

		// Initialize the backend
		err = vb.Backend.Init()

		if err != nil {
			ebsResponse.Error = err.Error()
			respondAndPublish(msg, nc, "ebs.mount.response", ebsResponse)
			return
		}

		// Next, connect to the volume and confirm the state
		// First, fetch the state from the remote backend
		err = vb.LoadState()

		if err != nil {
			ebsResponse.Error = err.Error()
			respondAndPublish(msg, nc, "ebs.mount.response", ebsResponse)
			return
		}

		// Next, mount the volume using nbdkit

		// Determine transport type (default to socket)
		useTCP := cfg.NBDTransport == types.NBDTransportTCP

		var nbdURI string
		var nbdSocket string
		var nbdPort int

		if useTCP {
			// TCP transport - find a free port
			portStr, err := viperblock.FindFreePort()
			if err != nil {
				ebsResponse.Error = err.Error()
				respondAndPublish(msg, nc, "ebs.mount.response", ebsResponse)
				return
			}

			// Parse the port from the address
			parts := strings.Split(portStr, ":")
			nbdPort, err = strconv.Atoi(parts[len(parts)-1])
			if err != nil {
				slog.Error("Failed to convert port to int", "err", err)
				ebsResponse.Error = fmt.Sprintf("failed to parse port: %v", err)
				respondAndPublish(msg, nc, "ebs.mount.response", ebsResponse)
				return
			}

			nbdURI = utils.FormatNBDTCPURI("127.0.0.1", nbdPort)
			slog.Info("Mounting volume (TCP)", "name", ebsRequest.Name, "port", nbdPort, "uri", nbdURI)
		} else {
			// Unix socket transport (default) - generate unique socket path
			nbdSocket, err = utils.GenerateUniqueSocketFile(ebsRequest.Name)
			if err != nil {
				ebsResponse.Error = err.Error()
				respondAndPublish(msg, nc, "ebs.mount.response", ebsResponse)
				return
			}

			nbdURI = utils.FormatNBDSocketURI(nbdSocket)
			slog.Info("Mounting volume (socket)", "name", ebsRequest.Name, "socket", nbdSocket, "uri", nbdURI)
		}

		// Generate PID file for nbdkit process
		nbdPidFile, err := utils.GeneratePidFile(fmt.Sprintf("nbdkit-vol-%s", ebsRequest.Name))
		if err != nil {
			slog.Error("Failed to generate nbdkit pid file", "err", err)
			ebsResponse.Error = fmt.Sprintf("failed to generate pid file: %v", err)
			respondAndPublish(msg, nc, "ebs.mount.response", ebsResponse)
			return
		}

		nbdConfig := nbd.NBDKitConfig{
			Port:       nbdPort,
			Socket:     nbdSocket,
			UseTCP:     useTCP,
			PidFile:    nbdPidFile,
			PluginPath: cfg.PluginPath,
			BaseDir:    cfg.BaseDir,
			Host:       admin.DialTarget(cfg.S3Host),
			Verbose:    false,
			Size:       utils.SafeUint64ToInt64(vb.GetVolumeSize()),
			Volume:     ebsRequest.Name,
			Bucket:     cfg.Bucket,
			Region:     cfg.Region,
			AccessKey:  cfg.AccessKey,
			SecretKey:  cfg.SecretKey,
			CacheSize:  nbdCacheSize,
			ShardWAL:   cfg.ShardWAL,
		}

		// Create a unique error channel for this specific mount request
		processChan := make(chan int, 1)
		exitChan := make(chan int, 1)

		// TODO: Improve, use a process manager to track the (multiple) nbdkit process
		go func() {
			slog.Debug("Executing nbdkit")

			cmd, err := nbdConfig.Execute()
			if err != nil {
				slog.Error("Failed to execute nbdkit", "err", err)
				// Signal error (no PID) to parent goroutine
				processChan <- 0
				return
			}

			pid := cmd.Process.Pid
			// Signal successful startup w/ PID
			processChan <- pid

			err = cmd.Wait()

			if err != nil {
				slog.Error("Failed to wait for nbdkit", "err", err)
				exitChan <- 1
				return
			}

			exitCode := cmd.ProcessState.ExitCode()

			exitChan <- exitCode

			slog.Error("NBDKit exited", "code", exitCode)
		}()

		// Wait for startup result
		pid := <-processChan

		if pid == 0 {
			ebsResponse.Error = "Failed to start nbdkit"
			respondAndPublish(msg, nc, "ebs.mount.response", ebsResponse)
			return
		}

		// Wait for 1 second to confirm nbdkit is running
		time.Sleep(1 * time.Second)

		// Any exit within the first second means NBDKit failed to stay up.
		select {
		case exitErr := <-exitChan:
			ebsResponse.Error = fmt.Sprintf("nbdkit exited unexpectedly (code=%d)", exitErr)
			respondAndPublish(msg, nc, "ebs.mount.response", ebsResponse)
			return
		default:
			// nbdkit is still running after 1 second, which means it started successfully
			slog.Info("NBDKit started successfully and is running")
		}

		// NBDKit creates the socket with its own umask (typically 0755).
		// The daemon (different user, same group) needs write access to connect.
		if nbdSocket != "" {
			if err := os.Chmod(nbdSocket, 0770); err != nil { //nolint:gosec // socket needs group-write for cross-service access
				slog.Warn("Failed to chmod NBD socket", "socket", nbdSocket, "err", err)
			}
		}

		ebsResponse.Mounted = true
		ebsResponse.URI = nbdURI

		// Subscribe to volume-specific snapshot topic so requests route to this node
		snapSub, err := nc.Subscribe(fmt.Sprintf("ebs.snapshot.%s", ebsRequest.Name), makeSnapshotHandler(vb, ebsRequest.Name))
		if err != nil {
			slog.Error("Failed to subscribe to volume snapshot topic", "volume", ebsRequest.Name, "err", err)
		}

		cfg.mu.Lock()
		cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{
			Name:        ebsRequest.Name,
			Port:        nbdPort,
			Socket:      nbdSocket,
			NBDURI:      nbdURI,
			PID:         pid,
			VB:          vb,
			SnapshotSub: snapSub,
		})
		cfg.mu.Unlock()

		respondAndPublish(msg, nc, "ebs.mount.response", ebsResponse)
		slog.Debug("Sent ebs.mount response")
	}); err != nil {
		return fmt.Errorf("failed to subscribe to %s: %w", mountTopic, err)
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

	for _, volume := range volumes {
		if volume.VB != nil {
			volume.VB.StopWALSyncer()
		}
		slog.Info("Killing nbdkit process", "pid", volume.PID)
		if err := utils.KillProcess(volume.PID); err != nil {
			slog.Error("Failed to kill nbdkit process", "pid", volume.PID, "err", err)
		}
	}

	return nil
}
