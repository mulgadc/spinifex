package dhcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// KVBucketPrefix is the lease bucket's name prefix. The full bucket name
// is BucketName(az), per-AZ to keep chassis-failover within the AZ scope
// (D7 in the design doc).
const KVBucketPrefix = "spinifex-dhcp-leases"

// BucketName returns the per-AZ lease bucket name. Empty az is allowed
// (single-AZ deployments collapse to "spinifex-dhcp-leases").
func BucketName(az string) string {
	if az == "" {
		return KVBucketPrefix
	}
	return KVBucketPrefix + "-" + az
}

// Entry is one persisted lease: the wire Lease plus the allocator-side
// bookkeeping (purpose, pool name, vpc id) the manager needs to reconcile
// state after a restart.
type Entry struct {
	Purpose  string // "eip" | "eni-public" | "natgw-external" | "gw-lrp"
	PoolName string
	VPCID    string // populated for purpose=gw-lrp
	Lease    *Lease
}

// Store is the persistence surface for active DHCP leases. Backed by
// NATS JetStream KV in production; tests can substitute via the
// NewStoreWithKV constructor.
type Store struct {
	kv nats.KeyValue
	az string
}

// NewStore creates/opens the per-AZ lease bucket and returns a Store.
func NewStore(js nats.JetStreamContext, az string) (*Store, error) {
	kv, err := utils.GetOrCreateKVBucket(js, BucketName(az), 5)
	if err != nil {
		return nil, fmt.Errorf("create dhcp lease KV bucket: %w", err)
	}
	return &Store{kv: kv, az: az}, nil
}

// NewStoreWithKV is the test constructor — caller owns the bucket.
func NewStoreWithKV(kv nats.KeyValue, az string) *Store {
	return &Store{kv: kv, az: az}
}

// AZ returns the AZ this store is scoped to.
func (s *Store) AZ() string { return s.az }

// KV exposes the underlying bucket. Used by tests that share buckets.
func (s *Store) KV() nats.KeyValue { return s.kv }

// Put persists an entry keyed by Lease.ClientID. Overwrites any existing
// entry for the same client-id.
func (s *Store) Put(e Entry) error {
	if e.Lease == nil {
		return errors.New("dhcp store put: lease is nil")
	}
	if e.Lease.ClientID == "" {
		return errors.New("dhcp store put: client_id is empty")
	}
	data, err := json.Marshal(toWireEntry(e))
	if err != nil {
		return fmt.Errorf("marshal lease entry: %w", err)
	}
	if _, err := s.kv.Put(e.Lease.ClientID, data); err != nil {
		return fmt.Errorf("put lease entry: %w", err)
	}
	return nil
}

// Get returns the entry for clientID. Returns (nil, nats.ErrKeyNotFound)
// when absent.
func (s *Store) Get(clientID string) (*Entry, error) {
	raw, err := s.kv.Get(clientID)
	if err != nil {
		return nil, err
	}
	var w wireEntry
	if err := json.Unmarshal(raw.Value(), &w); err != nil {
		return nil, fmt.Errorf("unmarshal lease entry %q: %w", clientID, err)
	}
	e, err := fromWireEntry(w)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// Delete removes the entry. Idempotent: missing keys return nil.
func (s *Store) Delete(clientID string) error {
	if err := s.kv.Delete(clientID); err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return nil
		}
		return fmt.Errorf("delete lease entry %q: %w", clientID, err)
	}
	return nil
}

// LookupByIP scans the bucket for an entry whose lease IP matches ip (and
// pool name when non-empty). O(N) is fine — only called on release.
func (s *Store) LookupByIP(poolName, ip string) (*Entry, error) {
	if ip == "" {
		return nil, errors.New("dhcp store lookup: ip required")
	}
	entries, err := s.List()
	if err != nil {
		return nil, err
	}
	for i := range entries {
		e := &entries[i]
		if e.Lease == nil || e.Lease.IP == nil {
			continue
		}
		if poolName != "" && e.PoolName != poolName {
			continue
		}
		if e.Lease.IP.String() == ip {
			return e, nil
		}
	}
	return nil, nats.ErrKeyNotFound
}

// List returns every entry currently in the bucket. Skips the internal
// version key written by utils.GetOrCreateKVBucket.
func (s *Store) List() ([]Entry, error) {
	keys, err := s.kv.Keys()
	if err != nil {
		if errors.Is(err, nats.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("list lease keys: %w", err)
	}
	var out []Entry
	for _, k := range keys {
		if k == utils.VersionKey {
			continue
		}
		e, err := s.Get(k)
		if err != nil {
			if errors.Is(err, nats.ErrKeyNotFound) {
				continue
			}
			slog.Warn("dhcp store list: skipping unreadable entry", "key", k, "err", err)
			continue
		}
		out = append(out, *e)
	}
	return out, nil
}

// wireEntry is the on-disk JSON shape. Timestamps are absolute so
// reloading after a restart yields the same wall-clock state without
// re-running the AcquiredAt + duration arithmetic.
type wireEntry struct {
	ClientID    string    `json:"client_id"`
	Purpose     string    `json:"purpose,omitempty"`
	PoolName    string    `json:"pool_name,omitempty"`
	VPCID       string    `json:"vpc_id,omitempty"`
	IP          string    `json:"ip,omitempty"`
	SubnetMask  string    `json:"subnet_mask,omitempty"`
	Routers     []string  `json:"routers,omitempty"`
	DNS         []string  `json:"dns,omitempty"`
	ServerID    string    `json:"server_id,omitempty"`
	HWAddr      string    `json:"hwaddr,omitempty"`
	Bridge      string    `json:"bridge,omitempty"`
	Hostname    string    `json:"hostname,omitempty"`
	VendorClass string    `json:"vendor_class,omitempty"`
	Acquired    time.Time `json:"acquired"`
	T1          time.Time `json:"t1,omitzero"`
	T2          time.Time `json:"t2,omitzero"`
	Expiry      time.Time `json:"expiry,omitzero"`
	RawOffer    []byte    `json:"raw_offer,omitempty"`
	RawACK      []byte    `json:"raw_ack,omitempty"`
}

func toWireEntry(e Entry) wireEntry {
	l := e.Lease
	w := wireEntry{
		ClientID:    l.ClientID,
		Purpose:     e.Purpose,
		PoolName:    e.PoolName,
		VPCID:       e.VPCID,
		IP:          ipToString(l.IP),
		SubnetMask:  maskToString(l.SubnetMask),
		Routers:     ipsToStrings(l.Routers),
		DNS:         ipsToStrings(l.DNS),
		ServerID:    ipToString(l.ServerID),
		HWAddr:      hwToString(l.HWAddr),
		Bridge:      l.Bridge,
		Hostname:    l.Hostname,
		VendorClass: l.VendorClass,
		Acquired:    l.AcquiredAt,
		T1:          l.AcquiredAt.Add(l.T1),
		T2:          l.AcquiredAt.Add(l.T2),
		Expiry:      l.AcquiredAt.Add(l.LeaseDuration),
		RawOffer:    l.RawOffer,
		RawACK:      l.RawACK,
	}
	return w
}

func fromWireEntry(w wireEntry) (Entry, error) {
	lease := &Lease{
		ClientID:    w.ClientID,
		Bridge:      w.Bridge,
		Hostname:    w.Hostname,
		VendorClass: w.VendorClass,
		AcquiredAt:  w.Acquired,
		RawOffer:    w.RawOffer,
		RawACK:      w.RawACK,
	}
	if w.IP != "" {
		lease.IP = net.ParseIP(w.IP)
	}
	if w.SubnetMask != "" {
		mask, err := parseMask(w.SubnetMask)
		if err != nil {
			return Entry{}, fmt.Errorf("parse subnet_mask %q: %w", w.SubnetMask, err)
		}
		lease.SubnetMask = mask
	}
	for _, s := range w.Routers {
		if ip := net.ParseIP(s); ip != nil {
			lease.Routers = append(lease.Routers, ip)
		}
	}
	for _, s := range w.DNS {
		if ip := net.ParseIP(s); ip != nil {
			lease.DNS = append(lease.DNS, ip)
		}
	}
	if w.ServerID != "" {
		lease.ServerID = net.ParseIP(w.ServerID)
	}
	if w.HWAddr != "" {
		hw, err := net.ParseMAC(w.HWAddr)
		if err != nil {
			return Entry{}, fmt.Errorf("parse hwaddr %q: %w", w.HWAddr, err)
		}
		lease.HWAddr = hw
	}
	if !w.Expiry.IsZero() {
		lease.LeaseDuration = w.Expiry.Sub(w.Acquired)
	}
	if !w.T1.IsZero() {
		lease.T1 = w.T1.Sub(w.Acquired)
	}
	if !w.T2.IsZero() {
		lease.T2 = w.T2.Sub(w.Acquired)
	}
	return Entry{
		Purpose:  w.Purpose,
		PoolName: w.PoolName,
		VPCID:    w.VPCID,
		Lease:    lease,
	}, nil
}

func ipToString(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}

func ipsToStrings(ips []net.IP) []string {
	if len(ips) == 0 {
		return nil
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		if ip != nil {
			out = append(out, ip.String())
		}
	}
	return out
}

func hwToString(hw net.HardwareAddr) string {
	if len(hw) == 0 {
		return ""
	}
	return hw.String()
}

func maskToString(m net.IPMask) string {
	if len(m) == 0 {
		return ""
	}
	ip := net.IP(m).To4()
	if ip == nil {
		return ""
	}
	return ip.String()
}

func parseMask(s string) (net.IPMask, error) {
	ip := net.ParseIP(strings.TrimSpace(s)).To4()
	if ip == nil {
		return nil, errors.New("not a dotted-quad mask")
	}
	return net.IPv4Mask(ip[0], ip[1], ip[2], ip[3]), nil
}
