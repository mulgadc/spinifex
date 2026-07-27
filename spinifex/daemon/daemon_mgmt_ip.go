package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go/jetstream"
)

// mgmtIPAMKeyPrefix namespaces mgmt-IP allocation records within the shared
// spinifex-cluster-state KV bucket (see JetStreamManager.UpdateMgmtIPAM). One
// record per management subnet, so clusters running distinct br-mgmt CIDRs
// never contend on the same key.
const mgmtIPAMKeyPrefix = "mgmt-ipam."

// mgmtCASOuterRetries bounds the outer retry loop Allocate/Release/Rebuild
// wrap around UpdateMgmtIPAM. casUpdate already retries a revision conflict
// internally (maxCASRetries), but that window only absorbs one overlapping
// writer per call; a burst of many nodes or VMs allocating on the same
// subnet at once (e.g. several system instances launched together at
// cluster bootstrap) can exhaust it. Retrying the whole read-mutate-write
// cycle from a fresh Get here converges on success under that contention
// instead of surfacing a transient conflict as a hard allocation failure.
const mgmtCASOuterRetries = 20

// updateMgmtIPAMWithRetry wraps jsManager.UpdateMgmtIPAM with the outer
// retry described above. Only a revision-conflict exhaustion is retried —
// any other error (KV not initialized, context errors, etc.) is returned
// immediately.
func updateMgmtIPAMWithRetry(jsManager *JetStreamManager, subnet string, mutate func(*MgmtIPRecord), createIfAbsent bool) (*MgmtIPRecord, error) {
	var lastErr error
	for range mgmtCASOuterRetries {
		record, err := jsManager.UpdateMgmtIPAM(subnet, mutate, createIfAbsent)
		if err == nil {
			return record, nil
		}
		if !errors.Is(err, jetstream.ErrKeyExists) {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("mgmt IPAM update for subnet %s exhausted %d outer retries under contention: %w",
		subnet, mgmtCASOuterRetries, lastErr)
}

// MgmtIPEntry records one management IP allocation: the address, the
// instance it belongs to, and the node that made the allocation. Node is
// carried for audit and for a future reclaim sweep against dead nodes'
// heartbeats — it plays no part in allocation itself, which must consider
// every entry regardless of which node owns it.
type MgmtIPEntry struct {
	IP         string `json:"ip"`
	InstanceID string `json:"instance_id"`
	Node       string `json:"node"`
}

// MgmtIPRecord is the KV-CAS record for one management subnet. br-mgmt is
// trunked onto a shared physical NIC on the tofu/ansible/baremetal cluster
// paths, so the /24 it lives on is one flat L2 segment across every host —
// every node allocating on that subnet reads and writes this same record.
type MgmtIPRecord struct {
	Subnet    string        `json:"subnet"` // e.g. "10.28.8.0/24"
	Allocated []MgmtIPEntry `json:"allocated"`
}

// MgmtIPAllocator manages static IP allocation for management NICs on
// system instances. IPs are derived from the host's management bridge
// subnet (.10-.249 range).
//
// Allocation is backed by a cluster-wide NATS KV compare-and-swap record
// once BindKV has been called, so nodes sharing the same br-mgmt L2 segment
// never hand out the same address (mulga-f3j2x): every node independently
// scanning from .10 was the bug, since br-mgmt is one flat subnet across the
// whole cluster, not a per-host range.
//
// allocated is a write-through local cache mirroring this node's view of the
// KV record, so AllocatedCount and lookups for an already-known instance ID
// keep working even when KV is unreachable. Allocate refuses to mint a NEW
// address in that case — this node's view of the cluster-wide record may be
// stale — but a cached instance ID and Release both still work locally.
type MgmtIPAllocator struct {
	mu        sync.Mutex
	allocated map[string]string // instanceID -> IP (write-through cache)
	baseIP    net.IP            // network base (e.g. 10.15.8.0)
	subnet    string            // "10.15.8.0/24", the CAS record's key suffix
	rangeMin  byte              // first host byte (10)
	rangeMax  byte              // last host byte (249)

	// jsManager / node are bound by BindKV once the daemon reaches
	// startCluster. Both stay nil/empty through startLocal — the DDIL
	// boundary there (daemon.go's assertNoClusterServicesInitialised) means
	// startLocal must never touch JetStream KV, so the allocator constructed
	// there is local-cache-only until BindKV is called.
	jsManager *JetStreamManager
	node      string
}

// NewMgmtIPAllocator creates an allocator for the /24 subnet of bridgeIP.
// Allocates from .10 to .249 (240 addresses). The allocator is KV-less until
// BindKV is called; see the DDIL boundary note above.
func NewMgmtIPAllocator(bridgeIP string) (*MgmtIPAllocator, error) {
	ip := net.ParseIP(bridgeIP)
	if ip == nil {
		return nil, fmt.Errorf("invalid bridge IP: %s", bridgeIP)
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("not an IPv4 address: %s", bridgeIP)
	}

	// Base is the /24 network: zero out the last octet
	base := make(net.IP, 4)
	copy(base, ip4)
	base[3] = 0

	return &MgmtIPAllocator{
		allocated: make(map[string]string),
		baseIP:    base,
		subnet:    fmt.Sprintf("%d.%d.%d.0/24", base[0], base[1], base[2]),
		rangeMin:  10,
		rangeMax:  249,
	}, nil
}

// BindKV attaches the cluster-wide KV backing store. Called once from
// daemon.startCluster after JetStream is up — never from startLocal (DDIL
// §1e-audit). node identifies this node's entries in the shared record for
// audit; it plays no part in allocation, which considers every entry in the
// record regardless of which node owns it.
func (a *MgmtIPAllocator) BindKV(jsManager *JetStreamManager, node string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.jsManager = jsManager
	a.node = node
}

// Allocate assigns a management IP to the given instance.
// Returns the allocated IP string (e.g. "10.15.8.10").
//
// A known instance ID is served from the local cache without touching KV, so
// a node that already owns an address can always answer for it. A brand new
// allocation requires a healthy cluster KV — this node's cache cannot prove
// an address is free cluster-wide on its own — and is refused otherwise.
func (a *MgmtIPAllocator) Allocate(instanceID string) (string, error) {
	a.mu.Lock()
	if ip, ok := a.allocated[instanceID]; ok {
		a.mu.Unlock()
		return ip, nil
	}
	jsManager, node, subnet := a.jsManager, a.node, a.subnet
	a.mu.Unlock()

	if jsManager == nil || !jsManager.KVHealthy() {
		return "", fmt.Errorf("mgmt IP allocation unavailable: cluster state KV unreachable for subnet %s", subnet)
	}

	// mutate has no error return (casUpdate's contract), so exhaustion and
	// idempotent hits are threaded back out through these closed-over
	// locals. On either path mutate leaves the record unchanged, so the
	// CAS write that follows is a harmless no-op against the same revision.
	var allocatedIP string
	var mutateErr error
	_, err := updateMgmtIPAMWithRetry(jsManager, subnet, func(r *MgmtIPRecord) {
		if r.Subnet == "" {
			r.Subnet = subnet
		}

		// Idempotent: an instance that already holds an address (allocated
		// by this node earlier, or by another node before a restart lost
		// the local cache) keeps it rather than taking a second one.
		for _, e := range r.Allocated {
			if e.InstanceID == instanceID {
				allocatedIP = e.IP
				return
			}
		}

		ip, findErr := nextFreeIP(a.baseIP, a.rangeMin, a.rangeMax, r.Allocated)
		if findErr != nil {
			mutateErr = findErr
			return
		}
		r.Allocated = append(r.Allocated, MgmtIPEntry{IP: ip, InstanceID: instanceID, Node: node})
		allocatedIP = ip
	}, true)
	if err != nil {
		return "", fmt.Errorf("mgmt IP CAS update for subnet %s: %w", subnet, err)
	}
	if mutateErr != nil {
		return "", mutateErr
	}

	a.mu.Lock()
	a.allocated[instanceID] = allocatedIP
	a.mu.Unlock()

	slog.Debug("Management IP allocated", "instance", instanceID, "ip", allocatedIP, "node", node)
	return allocatedIP, nil
}

// nextFreeIP scans rangeMin-rangeMax on the /24 rooted at baseIP, skipping
// every IP already present in allocated regardless of which entry's Node it
// belongs to — that is the whole fix for mulga-f3j2x: the range is shared
// across the cluster, not partitioned per host.
func nextFreeIP(baseIP net.IP, rangeMin, rangeMax byte, allocated []MgmtIPEntry) (string, error) {
	used := make(map[string]struct{}, len(allocated))
	for _, e := range allocated {
		used[e.IP] = struct{}{}
	}

	for i := rangeMin; i <= rangeMax; i++ {
		candidate := fmt.Sprintf("%d.%d.%d.%d", baseIP[0], baseIP[1], baseIP[2], i)
		if _, taken := used[candidate]; !taken {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("management IP range exhausted across the cluster (%d.%d.%d.%d-%d.%d.%d.%d, %d addresses)",
		baseIP[0], baseIP[1], baseIP[2], rangeMin,
		baseIP[0], baseIP[1], baseIP[2], rangeMax,
		int(rangeMax-rangeMin)+1)
}

// Release frees the management IP for the given instance, cluster-wide.
// Best-effort against KV: the local cache is always updated so this node
// never reoffers the address, but a KV write failure is logged rather than
// returned — callers (LaunchSystemInstance rollback, CleanupMgmtNetwork)
// cannot act on a Release error, matching the pre-existing signature.
func (a *MgmtIPAllocator) Release(instanceID string) {
	a.mu.Lock()
	ip, hadLocal := a.allocated[instanceID]
	delete(a.allocated, instanceID)
	jsManager, subnet := a.jsManager, a.subnet
	a.mu.Unlock()

	if hadLocal {
		slog.Debug("Management IP released locally", "instance", instanceID, "ip", ip)
	}

	if jsManager == nil {
		return
	}

	// createIfAbsent=false: releasing an instance this node never allocated
	// (or that no node has a record for) must not conjure an empty record
	// into existence — jetstream.ErrKeyNotFound just means there was nothing to
	// release.
	_, err := updateMgmtIPAMWithRetry(jsManager, subnet, func(r *MgmtIPRecord) {
		for i, e := range r.Allocated {
			if e.InstanceID == instanceID {
				r.Allocated = append(r.Allocated[:i], r.Allocated[i+1:]...)
				return
			}
		}
	}, false)
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		slog.Warn("Failed to release mgmt IP in cluster KV; address remains reserved until reconciled",
			"instance", instanceID, "err", err)
	}
}

// Rebuild reconciles the allocator against vms (this node's own VM
// snapshot). It always refreshes the local write-through cache first, then
// — once KV is bound (see BindKV) and healthy — CAS-inserts an entry for
// every local VM carrying a MgmtIP, idempotent by instance ID. This never
// removes another node's entries: reclaiming addresses of a dead node is a
// separate concern (tracked apart from mulga-f3j2x).
//
// Called on daemon startup (startLocal, cache-only since KV isn't bound yet)
// and again after cluster bootstrap (startCluster, once BindKV has run) so
// already-allocated IPs are never handed out a second time.
func (a *MgmtIPAllocator) Rebuild(vms map[string]*vm.VM) {
	a.mu.Lock()
	for id, v := range vms {
		if v.MgmtIP != "" {
			a.allocated[id] = v.MgmtIP
		}
	}
	count := len(a.allocated)
	jsManager, node, subnet := a.jsManager, a.node, a.subnet
	a.mu.Unlock()

	slog.Info("Management IP allocator rebuilt", "count", count)

	if jsManager == nil {
		// startLocal: no cluster KV yet, cache-only rebuild.
		return
	}
	if !jsManager.KVHealthy() {
		slog.Warn("Skipping mgmt IP KV reconcile: cluster state KV unreachable", "subnet", subnet)
		return
	}

	for id, v := range vms {
		if v.MgmtIP == "" {
			continue
		}
		mgmtIP := v.MgmtIP
		_, err := updateMgmtIPAMWithRetry(jsManager, subnet, func(r *MgmtIPRecord) {
			if r.Subnet == "" {
				r.Subnet = subnet
			}
			for _, e := range r.Allocated {
				if e.InstanceID == id {
					return // already reconciled
				}
			}
			r.Allocated = append(r.Allocated, MgmtIPEntry{IP: mgmtIP, InstanceID: id, Node: node})
		}, true)
		if err != nil {
			slog.Warn("Failed to reconcile mgmt IP into cluster KV", "instance", id, "ip", mgmtIP, "err", err)
		}
	}
}

// AllocatedCount returns the number of currently allocated IPs known to this
// node's local cache.
func (a *MgmtIPAllocator) AllocatedCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.allocated)
}
