package types

import (
	"encoding/json"
	"sync"
)

type EBSRequests struct {
	Requests []EBSRequest `json:"Requests" mapstructure:"ebs_requests"`
	Mu       sync.Mutex   `json:"-"`
}

type EBSRequest struct {
	Name                string `json:"Name"`
	VolType             string `json:"VolType"`
	Boot                bool   `json:"Boot"`
	EFI                 bool   `json:"EFI"`
	DeleteOnTermination bool   `json:"DeleteOnTermination"`
	NBDURI              string `json:"NBDURI"`     // NBD URI - socket path (nbd:unix:/path.sock) or TCP (nbd://host:port)
	DeviceName          string `json:"DeviceName"` // AWS API device name (e.g. /dev/sdf) for hot-plugged volumes
	// HotplugPort is the PCIe hot-plug root port (hotplug-ebs{N}) a hot-plugged
	// volume occupies. 0 for boot/non-hot-plugged volumes. Persisted so port
	// accounting survives a daemon restart.
	HotplugPort int `json:"HotplugPort,omitempty"`
}

// VolumeTypeGP3 is the only EBS volume type this platform serves.
const VolumeTypeGP3 = "gp3"

const (
	// gp3 IOPS envelope (AWS): 3000 baseline on any size, up to 500 IOPS/GiB,
	// capped at 16000.
	DefaultGP3IOPS = 3000
	MaxGP3IOPS     = 16000
	GP3IOPSPerGiB  = 500

	// gp3 Throughput envelope (AWS): 125 MiB/s baseline, 1000 MiB/s ceiling,
	// flat range independent of volume size.
	DefaultGP3Throughput = 125
	MaxGP3Throughput     = 1000
)

// NBDTransport defines the transport type for NBD connections.
type NBDTransport string

const (
	// NBDTransportSocket uses Unix domain sockets (faster, local only).
	NBDTransportSocket NBDTransport = "socket"
	// NBDTransportTCP uses TCP connections (required for remote/DPU scenarios).
	NBDTransportTCP NBDTransport = "tcp"
)

type EBSMountResponse struct {
	URI     string `json:"URI"`
	Mounted bool   `json:"Mounted"`
	Error   string `json:"Error"`
}

type EBSUnMountResponse struct {
	Volume  string `json:"Volume"`
	Mounted bool   `json:"Mounted"`
	Error   string `json:"Error"`
	// NotFound signals the volume was already unmounted and sealed (a retry
	// after a request that timed out client-side but completed server-side).
	// The caller treats this as a successful, idempotent seal.
	NotFound bool `json:"NotFound"`
}

type EBSSyncRequest struct {
	Volume string `json:"Volume"`
}

type EBSSyncResponse struct {
	Volume string `json:"Volume"`
	Synced bool   `json:"Synced"`
	Error  string `json:"Error"`
}

type EBSDeleteRequest struct {
	Volume string `json:"Volume"`
}

type EBSDeleteResponse struct {
	Volume  string `json:"Volume"`
	Success bool   `json:"Success"`
	Error   string `json:"Error"`
}

// EBSConfigUpdateRequest carries a control-plane VolumeConfig update for an
// encrypted volume. config.json is a sealed VBState; only the master-key holder
// (viperblockd) can reseal it, so the EC2 edge ships the new config here instead
// of rewriting the object directly. VolumeConfig is a marshaled
// viperblock.VolumeConfig (RawMessage keeps this package dependency-free).
type EBSConfigUpdateRequest struct {
	Volume       string          `json:"Volume"`
	VolumeConfig json.RawMessage `json:"VolumeConfig"`
}

type EBSConfigUpdateResponse struct {
	Volume  string `json:"Volume"`
	Success bool   `json:"Success"`
	Error   string `json:"Error"`
}
