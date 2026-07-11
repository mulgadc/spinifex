package qmp

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"
)

type unmarshalTarget any

var CommandResponseTypes = map[string]unmarshalTarget{
	"stop":             &json.RawMessage{},
	"cont":             &json.RawMessage{},
	"system_powerdown": &json.RawMessage{},
	"system_reset":     &json.RawMessage{},
	"system_wakeup":    &json.RawMessage{},

	"query-block": &[]BlockDevice{},

	"query-status": &Status{},

	"query-cpus-fast":  &[]CPUInfoFast{},
	"query-blockstats": &[]BlockStats{},
	"query-balloon":    &BalloonInfo{},
}

// CPUInfoFast is one vCPU entry from query-cpus-fast. ThreadID is the host
// thread backing the vCPU; its /proc/<tid>/stat utime+stime deltas yield
// guest CPU utilization.
type CPUInfoFast struct {
	CPUIndex int `json:"cpu-index"`
	ThreadID int `json:"thread-id"`
}

// BlockStats is one device entry from query-blockstats.
type BlockStats struct {
	Device string           `json:"device,omitempty"`
	Stats  BlockDeviceStats `json:"stats"`
}

// BlockDeviceStats holds the cumulative I/O counters of a block device.
type BlockDeviceStats struct {
	RdBytes      int64 `json:"rd_bytes"`
	WrBytes      int64 `json:"wr_bytes"`
	RdOperations int64 `json:"rd_operations"`
	WrOperations int64 `json:"wr_operations"`
}

// BalloonInfo is the query-balloon response; Actual is the guest's current
// memory allocation in bytes.
type BalloonInfo struct {
	Actual int64 `json:"actual"`
}

type Status struct {
	Status     string `json:"status"`
	Singlestep bool   `json:"singlestep"`
	Running    bool   `json:"running"`
}

type QMPQueryBlockResponse struct {
	Return []BlockDevice `json:"return"`
	Error  *QMPError     `json:"error,omitempty"`
}

type BlockDevice struct {
	IOStatus  string         `json:"io-status,omitempty"`
	Device    string         `json:"device"`
	Locked    bool           `json:"locked"`
	Removable bool           `json:"removable"`
	TrayOpen  *bool          `json:"tray_open,omitempty"`
	Inserted  *BlockInserted `json:"inserted,omitempty"`
	QDev      string         `json:"qdev,omitempty"`
	Type      string         `json:"type"`
}

type BlockInserted struct {
	IOPSRead       int        `json:"iops_rd"`
	IOPSWrite      int        `json:"iops_wr"`
	IOPS           int        `json:"iops"`
	BPSRead        int        `json:"bps_rd"`
	BPSWrite       int        `json:"bps_wr"`
	BPS            int        `json:"bps"`
	WriteThreshold int        `json:"write_threshold"`
	DetectZeroes   string     `json:"detect_zeroes"`
	RO             bool       `json:"ro"`
	NodeName       string     `json:"node-name"`
	BackingDepth   int        `json:"backing_file_depth"`
	Encrypted      bool       `json:"encrypted"`
	Driver         string     `json:"drv"`
	Image          BlockImage `json:"image"`
	File           string     `json:"file"`
	Cache          BlockCache `json:"cache"`
}

type BlockImage struct {
	VirtualSize int64  `json:"virtual-size"`
	Filename    string `json:"filename"`
	Format      string `json:"format"`
}

type BlockCache struct {
	NoFlush   bool `json:"no-flush"`
	Direct    bool `json:"direct"`
	Writeback bool `json:"writeback"`
}

type QMPError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

// QMP greeting on connect
type QMPGreeting struct {
	QMP struct {
		Version struct {
			QEMU struct {
				Major int `json:"major"`
				Minor int `json:"minor"`
			} `json:"qemu"`
		} `json:"version"`
		Capabilities []string `json:"capabilities"`
	} `json:"QMP"`
}

type QMPCommand struct {
	Execute   string         `json:"execute"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type QMPResponse struct {
	Return json.RawMessage `json:"return"`
	Error  *QMPError       `json:"error,omitempty"`
}

type QMPClient struct {
	Conn    net.Conn
	Decoder *json.Decoder
	Encoder *json.Encoder
	Mu      sync.Mutex
	// Path is the QMP unix socket, retained so a wedged client can redial the
	// same QEMU without the caller re-plumbing it.
	Path string
	// Dead marks the stream as unusable after a failed read: a timed-out decode
	// leaves the shared Decoder mid-message, so every later command would fail
	// until the connection is re-established. The next send reconnects.
	Dead bool
}

// DefaultGreetingTimeout bounds the wait for QEMU's QMP greeting on a plain VM.
// A VFIO/GPU guest must lock+DMA-map its entire RAM before the monitor answers,
// which exceeds this — such callers pass a larger value via
// NewQMPClientWithGreetingTimeout.
const DefaultGreetingTimeout = 30 * time.Second

func NewQMPClient(path string) (*QMPClient, error) {
	return NewQMPClientWithGreetingTimeout(path, DefaultGreetingTimeout)
}

// NewQMPClientWithGreetingTimeout is NewQMPClient with an explicit deadline for
// the QMP greeting. VFIO passthrough guests pin all guest RAM before the monitor
// responds, so the greeting can take far longer than DefaultGreetingTimeout.
func NewQMPClientWithGreetingTimeout(path string, greetingTimeout time.Duration) (*QMPClient, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}

	// Deadline prevents a hung QEMU from blocking forever.
	if err := conn.SetReadDeadline(time.Now().Add(greetingTimeout)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("set read deadline: %w", err)
	}

	client := &QMPClient{
		Conn:    conn,
		Decoder: json.NewDecoder(conn),
		Encoder: json.NewEncoder(conn),
		Path:    path,
	}

	// wait for greeting
	var greeting QMPGreeting
	if err := client.Decoder.Decode(&greeting); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("waiting for QMP greeting: %w", err)
	}

	// Clear deadline for normal operations
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clear read deadline: %w", err)
	}

	return client, nil
}

// Close releases the underlying socket. Safe on a nil client.
func (c *QMPClient) Close() error {
	if c == nil || c.Conn == nil {
		return nil
	}
	return c.Conn.Close()
}

// Execute sends one command and decodes its response into out (a pointer,
// typically from CommandResponseTypes shapes). Async QMP events interleaved on
// the stream are skipped. Intended for short-lived, single-owner connections
// (e.g. the telemetry collector); the manager path in vm uses its own
// reconnecting wrapper. The caller must hold no other reader on the stream.
func (c *QMPClient) Execute(cmd QMPCommand, out any) error {
	c.Mu.Lock()
	defer c.Mu.Unlock()

	if err := c.Conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return fmt.Errorf("set read deadline: %w", err)
	}
	defer func() { _ = c.Conn.SetReadDeadline(time.Time{}) }()

	if err := c.Encoder.Encode(cmd); err != nil {
		return fmt.Errorf("encode %s: %w", cmd.Execute, err)
	}
	for {
		var resp struct {
			Return json.RawMessage `json:"return"`
			Error  *QMPError       `json:"error"`
			Event  string          `json:"event"`
		}
		if err := c.Decoder.Decode(&resp); err != nil {
			return fmt.Errorf("decode %s: %w", cmd.Execute, err)
		}
		if resp.Event != "" {
			continue
		}
		if resp.Error != nil {
			return fmt.Errorf("QMP %s: %s: %s", cmd.Execute, resp.Error.Class, resp.Error.Desc)
		}
		if resp.Return == nil {
			continue
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(resp.Return, out)
	}
}
