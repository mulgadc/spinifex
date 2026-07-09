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

// NBDTransport defines the transport type for NBD connections
type NBDTransport string

const (
	// NBDTransportSocket uses Unix domain sockets (faster, local only)
	NBDTransportSocket NBDTransport = "socket"
	// NBDTransportTCP uses TCP connections (required for remote/DPU scenarios)
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
