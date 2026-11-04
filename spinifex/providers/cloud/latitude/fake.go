package latitude

import (
	"context"
	"fmt"
	"sync"
)

// Fake is an in-memory stand-in for Client. It holds a fixed set of addresses
// and moves one to any known server at once. It mirrors the documented
// behaviour, not a measured one: a move to the current holder succeeds, and
// moving to an unknown server is refused.
type Fake struct {
	mu      sync.Mutex
	ips     map[string]ElasticIP
	servers map[string]struct{}

	// Fail, when set for an operation name ("get", "move"), makes that
	// operation return the error once.
	Fail map[string]error
	// Calls records operation names in order.
	Calls []string
}

// NewFake returns a fake whose addresses may move among servers.
func NewFake(servers ...string) *Fake {
	f := &Fake{ips: map[string]ElasticIP{}, servers: map[string]struct{}{}, Fail: map[string]error{}}
	for _, s := range servers {
		f.servers[s] = struct{}{}
	}
	return f
}

// Seed adds an address. An empty status means active.
func (f *Fake) Seed(ip ElasticIP) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ip.Status == "" {
		ip.Status = StatusActive
	}
	if ip.Mode == "" && ip.Family == "" && ip.PrefixLength == 0 {
		ip.Mode, ip.Family, ip.PrefixLength = "routed", "IPv4", 32
	}
	f.ips[ip.ID] = ip
}

// HolderOf reports which server holds id; empty when unassigned or missing.
func (f *Fake) HolderOf(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ips[id].ServerID
}

// fail must be called with f.mu held.
func (f *Fake) fail(op string) error {
	f.Calls = append(f.Calls, op)
	if err, ok := f.Fail[op]; ok {
		delete(f.Fail, op)
		return err
	}
	return nil
}

func (f *Fake) GetElasticIP(_ context.Context, id string) (ElasticIP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("get"); err != nil {
		return ElasticIP{}, err
	}
	ip, ok := f.ips[id]
	if !ok {
		return ElasticIP{}, &APIError{Status: 404, Message: "not found", kind: ErrNotFound}
	}
	return ip, nil
}

func (f *Fake) MoveElasticIP(_ context.Context, id, serverID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("move"); err != nil {
		return err
	}
	ip, ok := f.ips[id]
	if !ok {
		return &APIError{Status: 404, Message: "not found", kind: ErrNotFound}
	}
	if _, ok := f.servers[serverID]; !ok {
		return &APIError{Status: 422, Code: "SERVER_NOT_IN_PROJECT", Message: fmt.Sprintf("server %s", serverID), kind: ErrMoveRefused}
	}
	ip.ServerID = serverID
	f.ips[id] = ip
	return nil
}
