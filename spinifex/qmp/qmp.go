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
}

func NewQMPClient(path string) (*QMPClient, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}

	// Deadline prevents a hung QEMU from blocking forever.
	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("set read deadline: %w", err)
	}

	client := &QMPClient{
		Conn:    conn,
		Decoder: json.NewDecoder(conn),
		Encoder: json.NewEncoder(conn),
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
