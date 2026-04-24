package dhcp

// NATS subjects that spinifex-vpcd's DHCP manager subscribes to and
// publishes on. The daemon-side handlers in handlers/ec2/vpc send
// requests against the request/reply subjects.
const (
	// TopicAcquire is the request/reply subject for obtaining a lease.
	// Request: AcquireRequestMsg. Reply: AcquireReplyMsg.
	TopicAcquire = "vpc.dhcp.acquire"
	// TopicRelease is the request/reply subject for releasing a lease.
	// Request: ReleaseRequestMsg. Reply: ReleaseReplyMsg.
	TopicRelease = "vpc.dhcp.release"
	// TopicLeaseExpired is a fan-out event published by vpcd when a
	// renewal permanently fails and the lease has passed its expiry.
	// Consumers (EIP / instance / VPC reconcilers) decide whether to
	// re-request a lease, reschedule, or fail the pending API call.
	TopicLeaseExpired = "vpc.dhcp.lease-expired"
)

// KV bucket for persisted lease state. Keyed by ClientID. Separate from
// the external IPAM allocation ledger by design: IPAM records who owns
// which IP, this bucket records when a lease expires and how to renew it.
const (
	KVBucketDHCPLeases        = "spinifex-dhcp-leases"
	KVBucketDHCPLeasesVersion = 1
)

// AcquireRequestMsg is the NATS payload for TopicAcquire.
type AcquireRequestMsg struct {
	Bridge      string `json:"bridge"`
	ClientID    string `json:"client_id"`
	Hostname    string `json:"hostname"`
	VendorClass string `json:"vendor_class"`
	// HWAddr is the MAC to put in the DHCP payload chaddr, formatted as
	// 02:aa:bb:cc:dd:ee. Empty means the vpcd Manager derives a synthetic
	// MAC from ClientID.
	HWAddr   string `json:"hw_addr,omitempty"`
	PoolName string `json:"pool_name,omitempty"`
}

// AcquireReplyMsg is the NATS reply for TopicAcquire. Error is the only
// field set on failure.
type AcquireReplyMsg struct {
	IP          string   `json:"ip,omitempty"`
	SubnetMask  string   `json:"subnet_mask,omitempty"`
	Routers     []string `json:"routers,omitempty"`
	DNS         []string `json:"dns,omitempty"`
	ServerID    string   `json:"server_id,omitempty"`
	HWAddr      string   `json:"hw_addr,omitempty"`
	ExpiresUnix int64    `json:"expires_unix,omitempty"`
	Error       string   `json:"error,omitempty"`
}

// ReleaseRequestMsg is the NATS payload for TopicRelease. ClientID must
// match the identifier the lease was acquired with.
type ReleaseRequestMsg struct {
	ClientID string `json:"client_id"`
}

// ReleaseReplyMsg is the NATS reply for TopicRelease.
type ReleaseReplyMsg struct {
	Error string `json:"error,omitempty"`
}

// LeaseExpiredEvent is the payload fan-out on TopicLeaseExpired.
type LeaseExpiredEvent struct {
	ClientID string `json:"client_id"`
	PoolName string `json:"pool_name,omitempty"`
	IP       string `json:"ip,omitempty"`
	// Reason is a short machine-readable code (e.g. "renew_failed",
	// "nak", "server_unreachable").
	Reason string `json:"reason"`
}
