package clusterv1

import "regexp"

const (
	// NodeFactsSubject fans out a request for each daemon's hardware and
	// cabling description. Its reply payload is NodeFacts.
	NodeFactsSubject = "spinifex.node.facts"

	nodeNetTestPrefix = "spinifex.node.nettest."
)

var nodeIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidNodeID reports whether a node name is safe to place in a subject: no
// NATS token separator or wildcard can get through.
func ValidNodeID(node string) bool {
	return nodeIDPattern.MatchString(node)
}

// NetTestServeSubject asks one node to start a one-shot throughput server.
func NetTestServeSubject(node string) string {
	return nodeNetTestPrefix + node + ".serve"
}

// NetTestRunSubject asks one node to run the client side of a throughput test.
func NetTestRunSubject(node string) string {
	return nodeNetTestPrefix + node + ".run"
}

// Planes a node's NICs can carry, named as the installer names them.
const (
	PlaneWAN = "wan"
	PlaneLAN = "lan"
	PlaneVPC = "vpc"
)

// NetTestPort is the TCP port reserved for node-to-node throughput tests.
const NetTestPort = 5001

// NodeFacts describes one node's hardware, OS and cabling. Fields a host
// cannot report are left empty, and Sources says why.
type NodeFacts struct {
	Node        string            `json:"node"`
	CollectedAt string            `json:"collected_at,omitempty"`
	FormFactor  string            `json:"form_factor"`
	Virt        string            `json:"virt,omitempty"`
	System      SystemFacts       `json:"system"`
	OS          OSFacts           `json:"os"`
	CPU         CPUFacts          `json:"cpu"`
	Memory      MemoryFacts       `json:"memory"`
	NICs        []NICFacts        `json:"nics"`
	Planes      []PlaneFacts      `json:"planes"`
	Sources     map[string]string `json:"sources"`
}

// SystemFacts is the machine's identity from DMI.
type SystemFacts struct {
	Vendor       string `json:"vendor,omitempty"`
	Product      string `json:"product,omitempty"`
	Family       string `json:"family,omitempty"`
	Version      string `json:"version,omitempty"`
	Serial       string `json:"serial,omitempty"`
	UUID         string `json:"uuid,omitempty"`
	ChassisType  string `json:"chassis_type,omitempty"`
	ChassisAsset string `json:"chassis_asset,omitempty"`
	BoardVendor  string `json:"board_vendor,omitempty"`
	BoardProduct string `json:"board_product,omitempty"`
	BoardSerial  string `json:"board_serial,omitempty"`
	BIOSVendor   string `json:"bios_vendor,omitempty"`
	BIOSVersion  string `json:"bios_version,omitempty"`
	BIOSDate     string `json:"bios_date,omitempty"`
	DeviceModel  string `json:"device_model,omitempty"`
}

// OSFacts names the running OS and Spinifex build.
type OSFacts struct {
	Name          string `json:"name,omitempty"`
	Kernel        string `json:"kernel,omitempty"`
	Arch          string `json:"arch,omitempty"`
	Version       string `json:"spx_version,omitempty"`
	Commit        string `json:"spx_commit,omitempty"`
	UptimeSeconds int64  `json:"uptime_seconds,omitempty"`
	Hostname      string `json:"hostname,omitempty"`
}

// CPUFacts summarises the sockets. Threads and Cores are totals.
type CPUFacts struct {
	Model          string   `json:"model,omitempty"`
	Vendor         string   `json:"vendor,omitempty"`
	Sockets        int      `json:"sockets"`
	CoresPerSocket int      `json:"cores_per_socket,omitempty"`
	Cores          int      `json:"cores"`
	Threads        int      `json:"threads"`
	MaxMHz         int      `json:"max_mhz,omitempty"`
	CurrentMHz     int      `json:"current_mhz,omitempty"`
	Virtualization string   `json:"virtualization,omitempty"`
	Features       []string `json:"features,omitempty"`
}

// MemoryFacts groups identical DIMMs so a 32-slot board reads as one line.
type MemoryFacts struct {
	TotalBytes    uint64        `json:"total_bytes"`
	SlotsUsed     int           `json:"slots_used"`
	SlotsTotal    int           `json:"slots_total"`
	MaxCapacity   string        `json:"max_capacity,omitempty"`
	ECC           string        `json:"ecc,omitempty"`
	Groups        []MemoryGroup `json:"groups"`
	OSTotalBytes  uint64        `json:"os_total_bytes,omitempty"`
	OSAvailBytes  uint64        `json:"os_available_bytes,omitempty"`
	HugePageBytes uint64        `json:"huge_page_bytes,omitempty"`
}

// MemoryGroup is a set of DIMMs alike in every field shown. ConfiguredMTs
// below SpeedMTs means the board runs them slower than rated.
type MemoryGroup struct {
	Count         int      `json:"count"`
	SizeBytes     uint64   `json:"size_bytes"`
	Type          string   `json:"type,omitempty"`
	TypeDetail    string   `json:"type_detail,omitempty"`
	FormFactor    string   `json:"form_factor,omitempty"`
	SpeedMTs      int      `json:"speed_mts,omitempty"`
	ConfiguredMTs int      `json:"configured_mts,omitempty"`
	Manufacturer  string   `json:"manufacturer,omitempty"`
	PartNumber    string   `json:"part_number,omitempty"`
	Rank          string   `json:"rank,omitempty"`
	Serials       []string `json:"serials,omitempty"`
}

// NICFacts is one physical network interface and, where LLDP reports it, the
// switch port it is cabled to.
type NICFacts struct {
	Name      string        `json:"name"`
	MAC       string        `json:"mac,omitempty"`
	Driver    string        `json:"driver,omitempty"`
	Firmware  string        `json:"firmware,omitempty"`
	BusInfo   string        `json:"bus_info,omitempty"`
	Model     string        `json:"model,omitempty"`
	SpeedMbps int           `json:"speed_mbps,omitempty"`
	MaxMbps   int           `json:"max_mbps,omitempty"`
	Duplex    string        `json:"duplex,omitempty"`
	Port      string        `json:"port,omitempty"`
	MTU       int           `json:"mtu,omitempty"`
	State     string        `json:"state"`
	Master    string        `json:"master,omitempty"`
	Planes    []string      `json:"planes,omitempty"`
	Neighbor  *LLDPNeighbor `json:"neighbor,omitempty"`
}

// LLDPNeighbor is what the switch on the other end of a NIC advertises.
type LLDPNeighbor struct {
	ChassisID         string   `json:"chassis_id"`
	SystemName        string   `json:"system_name,omitempty"`
	SystemDescription string   `json:"system_description,omitempty"`
	PortID            string   `json:"port_id,omitempty"`
	PortDescription   string   `json:"port_description,omitempty"`
	Capabilities      []string `json:"capabilities,omitempty"`
}

// PlaneFacts is the address a plane uses on this node and the NICs under it.
type PlaneFacts struct {
	Plane   string   `json:"plane"`
	Bridge  string   `json:"bridge,omitempty"`
	Address string   `json:"address,omitempty"`
	Prefix  int      `json:"prefix,omitempty"`
	NICs    []string `json:"nics,omitempty"`
	MTU     int      `json:"mtu,omitempty"`
}

// NetTestServeRequest starts a server bound to the node's address on Plane.
type NetTestServeRequest struct {
	Plane           string `json:"plane"`
	DurationSeconds int    `json:"duration_seconds"`
}

// NetTestServeResponse names the address the server listens on.
type NetTestServeResponse struct {
	Address string `json:"address,omitempty"`
	Port    int    `json:"port,omitempty"`
	Error   string `json:"error,omitempty"`
}

// NetTestRunRequest runs the client towards Address from the node's own
// address on Plane.
type NetTestRunRequest struct {
	Plane           string `json:"plane"`
	Address         string `json:"address"`
	Port            int    `json:"port"`
	DurationSeconds int    `json:"duration_seconds"`
	Streams         int    `json:"streams"`
}

// NetTestRunResponse is one direction of a pair's measurement.
type NetTestRunResponse struct {
	SourceAddress string    `json:"source_address,omitempty"`
	BitsPerSecond float64   `json:"bits_per_second"`
	Bytes         int64     `json:"bytes"`
	Seconds       float64   `json:"seconds"`
	Streams       int       `json:"streams"`
	StreamBPS     []float64 `json:"stream_bps,omitempty"`
	RTTMinMillis  float64   `json:"rtt_min_ms,omitempty"`
	RTTAvgMillis  float64   `json:"rtt_avg_ms,omitempty"`
	RTTMaxMillis  float64   `json:"rtt_max_ms,omitempty"`
	PacketLoss    float64   `json:"packet_loss_percent,omitempty"`
	LinkMbps      int       `json:"link_mbps,omitempty"`
	MTU           int       `json:"mtu,omitempty"`
	Tool          string    `json:"tool,omitempty"`
	Error         string    `json:"error,omitempty"`
}
