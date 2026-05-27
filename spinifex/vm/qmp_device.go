package vm

import (
	"fmt"
	"maps"
	"sync"

	"github.com/mulgadc/spinifex/spinifex/qmp"
)

// DeviceController abstracts the QEMU Machine Protocol surface the ENI
// hot-plug pipeline uses: device_add / device_del / netdev_add / netdev_del /
// query-pci. Sprint 3a lands the interface and the in-memory stub so the
// downstream pipeline (Sprint 3b) can be unit-tested without a live QEMU.
//
// The production backend (QMPDeviceController) wraps the existing
// sendQMPCommand helper so wire-level serialization continues to live on the
// underlying *qmp.QMPClient mutex. The stub backend (StubDeviceController)
// keeps an in-memory map of attached devices/netdevs for assertions and lets
// tests pre-program failure responses per execute name.
type DeviceController interface {
	DeviceAdd(args map[string]any) error
	DeviceDel(deviceID string) error
	NetdevAdd(args map[string]any) error
	NetdevDel(netdevID string) error
	QueryPCI() ([]PCIDevice, error)
}

// PCIDevice is the trimmed projection of QEMU's query-pci response that the
// hot-plug pipeline consumes. Full response parsing lands in Sprint 3b; the
// stub backend populates QDevID only, which is the field the pipeline checks
// to confirm device materialization.
type PCIDevice struct {
	Bus    int    `json:"bus"`
	Slot   int    `json:"slot"`
	QDevID string `json:"qdev_id,omitempty"`
}

// QMPDeviceController is the production DeviceController bound to a live
// QEMU instance. Construct one per VM via NewQMPDeviceController and reuse
// it across the hot-plug / hot-unplug pipeline for the same VM.
type QMPDeviceController struct {
	client     *qmp.QMPClient
	instanceID string
}

var _ DeviceController = (*QMPDeviceController)(nil)

func NewQMPDeviceController(client *qmp.QMPClient, instanceID string) *QMPDeviceController {
	return &QMPDeviceController{client: client, instanceID: instanceID}
}

func (c *QMPDeviceController) DeviceAdd(args map[string]any) error {
	_, err := sendQMPCommand(c.client, qmp.QMPCommand{Execute: "device_add", Arguments: args}, c.instanceID)
	return err
}

func (c *QMPDeviceController) DeviceDel(deviceID string) error {
	_, err := sendQMPCommand(c.client, qmp.QMPCommand{
		Execute:   "device_del",
		Arguments: map[string]any{"id": deviceID},
	}, c.instanceID)
	return err
}

func (c *QMPDeviceController) NetdevAdd(args map[string]any) error {
	_, err := sendQMPCommand(c.client, qmp.QMPCommand{Execute: "netdev_add", Arguments: args}, c.instanceID)
	return err
}

func (c *QMPDeviceController) NetdevDel(netdevID string) error {
	_, err := sendQMPCommand(c.client, qmp.QMPCommand{
		Execute:   "netdev_del",
		Arguments: map[string]any{"id": netdevID},
	}, c.instanceID)
	return err
}

// QueryPCI is unwired in Sprint 3a; the live query-pci response parser
// lands alongside the hot-plug pipeline in Sprint 3b. Callers (the stub
// backend below + future eni_hotplug.go) get the contract today.
func (c *QMPDeviceController) QueryPCI() ([]PCIDevice, error) {
	return nil, fmt.Errorf("QueryPCI: live query-pci wiring lands in Sprint 3b (enihotplug)")
}

// StubDeviceController is an in-memory DeviceController for unit tests. It
// records every call in order, enforces duplicate-id and unknown-id errors
// the way real QEMU does, and lets tests inject a one-shot failure per
// execute name via FailNext.
type StubDeviceController struct {
	mu       sync.Mutex
	devices  map[string]map[string]any
	netdevs  map[string]map[string]any
	calls    []StubCall
	failNext map[string]error
}

// StubCall records one DeviceController invocation. Args is a shallow copy
// of the args map passed in (or {"id": …} for the *Del methods) so tests
// can assert on payload without worrying about caller mutation.
type StubCall struct {
	Execute string
	Args    map[string]any
}

func NewStubDeviceController() *StubDeviceController {
	return &StubDeviceController{
		devices:  make(map[string]map[string]any),
		netdevs:  make(map[string]map[string]any),
		failNext: make(map[string]error),
	}
}

var _ DeviceController = (*StubDeviceController)(nil)

// SetFailNext primes the stub to return err on the next call matching
// execute (one of "device_add", "device_del", "netdev_add", "netdev_del",
// "query-pci"). Cleared after the call fires.
func (s *StubDeviceController) SetFailNext(execute string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[execute] = err
}

// Calls returns a snapshot of recorded calls in invocation order. Safe for
// concurrent use.
func (s *StubDeviceController) Calls() []StubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StubCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// HasDevice reports whether deviceID is currently attached in the stub's
// in-memory state.
func (s *StubDeviceController) HasDevice(deviceID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.devices[deviceID]
	return ok
}

// HasNetdev reports whether netdevID is currently attached in the stub's
// in-memory state.
func (s *StubDeviceController) HasNetdev(netdevID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.netdevs[netdevID]
	return ok
}

func (s *StubDeviceController) consumeFailure(execute string) error {
	if err, ok := s.failNext[execute]; ok {
		delete(s.failNext, execute)
		return err
	}
	return nil
}

func cloneArgs(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	return out
}

func (s *StubDeviceController) DeviceAdd(args map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, StubCall{Execute: "device_add", Args: cloneArgs(args)})
	if err := s.consumeFailure("device_add"); err != nil {
		return err
	}
	id, _ := args["id"].(string)
	if id == "" {
		return fmt.Errorf("stub device_add: missing id")
	}
	if _, exists := s.devices[id]; exists {
		return fmt.Errorf("stub device_add: duplicate id %q", id)
	}
	s.devices[id] = cloneArgs(args)
	return nil
}

func (s *StubDeviceController) DeviceDel(deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, StubCall{Execute: "device_del", Args: map[string]any{"id": deviceID}})
	if err := s.consumeFailure("device_del"); err != nil {
		return err
	}
	if _, ok := s.devices[deviceID]; !ok {
		return fmt.Errorf("stub device_del: device %q not found", deviceID)
	}
	delete(s.devices, deviceID)
	return nil
}

func (s *StubDeviceController) NetdevAdd(args map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, StubCall{Execute: "netdev_add", Args: cloneArgs(args)})
	if err := s.consumeFailure("netdev_add"); err != nil {
		return err
	}
	id, _ := args["id"].(string)
	if id == "" {
		return fmt.Errorf("stub netdev_add: missing id")
	}
	if _, exists := s.netdevs[id]; exists {
		return fmt.Errorf("stub netdev_add: duplicate id %q", id)
	}
	s.netdevs[id] = cloneArgs(args)
	return nil
}

func (s *StubDeviceController) NetdevDel(netdevID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, StubCall{Execute: "netdev_del", Args: map[string]any{"id": netdevID}})
	if err := s.consumeFailure("netdev_del"); err != nil {
		return err
	}
	if _, ok := s.netdevs[netdevID]; !ok {
		return fmt.Errorf("stub netdev_del: netdev %q not found", netdevID)
	}
	delete(s.netdevs, netdevID)
	return nil
}

func (s *StubDeviceController) QueryPCI() ([]PCIDevice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, StubCall{Execute: "query-pci"})
	if err := s.consumeFailure("query-pci"); err != nil {
		return nil, err
	}
	out := make([]PCIDevice, 0, len(s.devices))
	for id := range s.devices {
		out = append(out, PCIDevice{QDevID: id})
	}
	return out, nil
}
