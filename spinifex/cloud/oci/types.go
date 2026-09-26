// Package oci wraps the subset of the Oracle Cloud API that Spinifex needs to
// hand out publicly reachable addresses on an OCI instance.
//
// OCI enforces the source IP per VNIC: a packet leaving the instance with a
// source address that is not a registered private IP object on that VNIC is
// dropped by the hypervisor, whatever the MAC. Measured on mulga-poc — from one
// NIC with one MAC, an unregistered address gets 100% loss past the subnet
// while registered ones get none. So an external address is not ours to invent
// from range math; it has to be created through this API first.
//
// The unit of a publicly reachable address here is a pair: a secondary private
// IP on the VNIC, which is what travels on the wire and what the host
// configures on the interface, and a RESERVED public IP attached to it, which
// is what the internet sees and what AWS semantics call the Elastic IP. OCI
// 1:1 NATs between them. RESERVED rather than EPHEMERAL because an AWS EIP
// outlives its association — an ephemeral public IP dies with the private IP
// it was created against, which would change the customer's address on every
// re-association.
package oci

import (
	"errors"
	"fmt"
	"net/netip"
)

// PrivateIP is a VCN private IP object on a VNIC. The OCID, not the address,
// is the handle every other call takes.
type PrivateIP struct {
	ID          string
	Address     netip.Addr
	VNICID      string
	SubnetID    string
	DisplayName string
	IsPrimary   bool
}

// PublicIP is an OCI public IP object. PrivateIPID is empty when the address
// is reserved but not currently attached to anything — the state an allocated
// but unassociated AWS EIP maps onto.
type PublicIP struct {
	ID             string
	Address        netip.Addr
	PrivateIPID    string
	DisplayName    string
	Lifetime       string
	LifecycleState string
}

// IsAssigned reports whether the public IP has finished provisioning and is
// attached. OCI assignment is asynchronous, so a create returns before this is
// true and callers poll.
func (p PublicIP) IsAssigned() bool { return p.LifecycleState == LifecycleStateAssigned }

// Public IP lifecycle states, as returned by the API.
const (
	LifecycleStateProvisioning = "PROVISIONING"
	LifecycleStateAvailable    = "AVAILABLE"
	LifecycleStateAssigning    = "ASSIGNING"
	LifecycleStateAssigned     = "ASSIGNED"
	LifecycleStateUnassigning  = "UNASSIGNING"
	LifecycleStateUnassigned   = "UNASSIGNED"
	LifecycleStateTerminating  = "TERMINATING"
	LifecycleStateTerminated   = "TERMINATED"
)

// Public IP lifetimes.
const (
	LifetimeReserved  = "RESERVED"
	LifetimeEphemeral = "EPHEMERAL"
)

// The three failures that need distinct handling upstream. Everything else is
// returned wrapped and untyped, because a caller that cannot act differently on
// an error does not benefit from being able to name it.
var (
	// ErrLimitExceeded is a quota refusal: 64 private IPs per VNIC, 50 public
	// IPs per region. The allocator maps this to InsufficientAddressCapacity so
	// it reaches the customer as the AWS error that means the same thing.
	ErrLimitExceeded = errors.New("oci: service limit exceeded")

	// ErrNotFound is a 404. Release paths treat it as success — an address that
	// is already gone is the state the caller wanted.
	ErrNotFound = errors.New("oci: resource not found")

	// ErrConflict is a 409 or a 412 etag mismatch: another node changed the
	// resource first. The caller re-reads rather than retrying blind.
	ErrConflict = errors.New("oci: conflicting state")
)

// APIError carries the OCI service error alongside one of the sentinels above,
// so errors.Is works for control flow while the message keeps the request ID
// that Oracle support asks for.
type APIError struct {
	Op         string
	Code       string
	Message    string
	StatusCode int
	RequestID  string
	kind       error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("oci %s: %s (%s, http %d, opc-request-id %s)",
		e.Op, e.Message, e.Code, e.StatusCode, e.RequestID)
}

// Unwrap returns the sentinel so errors.Is(err, ErrLimitExceeded) matches.
func (e *APIError) Unwrap() error { return e.kind }
