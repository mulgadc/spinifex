// Package ochre owns placement and lifecycle of self-hosted vLLM
// serving VMs: it decides where a model's endpoint runs, launches and probes
// the VM, and persists the result as the single source of truth other
// components (the gateway's inference path) read over NATS.
package ochre

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"time"
)

// EndpointState is the lifecycle state of one model's serving endpoint.
type EndpointState string

const (
	// StateAbsent is the zero value: no record exists (or it was deleted). Not
	// itself stored — its "record" is simply a missing KV key.
	StateAbsent EndpointState = "ABSENT"
	// StateStarting covers admission through VM launch and the readiness probe.
	StateStarting EndpointState = "STARTING"
	// StateReady means the readiness probe observed a 200 from /v1/models;
	// BaseURL is safe to route inference traffic to.
	StateReady EndpointState = "READY"
	// StateDraining means a delete was requested; the VM is being torn down.
	StateDraining EndpointState = "DRAINING"
)

// ErrIllegalTransition is returned by validateTransition for any state change
// not in the table below.
var ErrIllegalTransition = errors.New("bedrock: illegal endpoint state transition")

// legalTransitions enumerates every allowed from->to move. A STARTING that
// fails (launch error or readiness timeout) goes back to ABSENT — the record
// is deleted rather than parked in a terminal FAILED state — so a retried
// Ensure gets a clean claim instead of piling up dead records.
var legalTransitions = map[EndpointState][]EndpointState{
	StateAbsent:   {StateStarting},
	StateStarting: {StateReady, StateAbsent},
	StateReady:    {StateDraining},
	StateDraining: {StateAbsent},
}

// validateTransition reports whether moving from -> to is legal.
func validateTransition(from, to EndpointState) error {
	if slices.Contains(legalTransitions[from], to) {
		return nil
	}
	return fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, from, to)
}

// EndpointRecord is the KV-persisted state of one model's serving endpoint.
// JSON tags are explicit as this struct crosses NATS subjects (DescribeEndpoint).
type EndpointRecord struct {
	AccountID string        `json:"account_id"`
	ModelID   string        `json:"model_id"`
	State     EndpointState `json:"state"`

	InstanceID string `json:"instance_id,omitempty"`
	// NodeID is the cluster node the VM landed on, so a caller can route a
	// node-scoped follow-up (e.g. a terminate) without a fleet-wide fan-out.
	NodeID string `json:"node_id,omitempty"`
	// BaseURL is the vLLM OpenAI-compatible server's base address
	// ("http://{eniIP}:{port}"), set once STARTING reaches READY.
	BaseURL string `json:"base_url,omitempty"`

	// ENIID and WeightsVolumeID are not in the bead's minimum field list but
	// are required to tear the VM down cleanly on DRAINING->ABSENT: without
	// them a delete has no way to find and release these resources again.
	// For a bundle, WeightsVolumeID mirrors the generative (vLLM) member's own
	// volume — see Members for every member's.
	ENIID           string `json:"eni_id,omitempty"`
	WeightsVolumeID string `json:"weights_volume_id,omitempty"`

	// PrivateIP is the bundle's shared serving VM's own system-VPC address.
	// Every member listens here, at its own Members[modelID].Port.
	PrivateIP string `json:"private_ip,omitempty"`

	// Members maps every co-served model this bundle's VM carries to its own
	// port and weights volume. A standalone model is a bundle of one: a
	// single entry keyed by its own ModelID. BaseURL/WeightsVolumeID above
	// mirror the generative member for callers (the reaper's Prometheus
	// scrape, the CLI summary) that only care about the bundle's primary model.
	Members map[string]MemberEndpoint `json:"members,omitempty"`

	CreatedAt time.Time `json:"created_at,omitzero"`
	ReadyAt   time.Time `json:"ready_at,omitzero"`

	// ParkedReason and ParkedAt mark a STARTING launch whose readiness wait
	// ended without READY. Its realization is retained; it is not serving.
	ParkedReason ParkReason `json:"parked_reason,omitempty"`
	ParkedAt     time.Time  `json:"parked_at,omitzero"`

	// HealthHold, when set, keeps automatic reclaim and capacity eviction off
	// this READY endpoint. An explicit delete still proceeds.
	HealthHold *HealthHold `json:"health_hold,omitempty"`

	// Pinned exempts this endpoint from the idle-reclaim sweep and from eviction.
	Pinned bool `json:"pinned,omitempty"`
	// OperatorReserved marks a pin an operator made explicitly, as opposed to
	// one a provisioned-throughput commitment made on the same key.
	OperatorReserved bool `json:"operator_reserved,omitempty"`

	// The reaper's last observation of this endpoint. Persisted rather than kept
	// in the reaper's memory because an eviction decision runs on whichever
	// replica the queue group picked, not necessarily the reaper's leader, and
	// because a leader change must not reset the idle clock or the failure count.
	//
	// LastActiveAt is the last sweep at which the endpoint was observed serving,
	// seeded from ReadyAt; it is the LRU ordering key. InFlight and SuccessTotal
	// are the last successful scrape's queue depth and completed-request count,
	// the latter being idleness's "unchanged since the previous tick" comparison
	// point. ScrapeFailures is a count older reapers wrote; a healthy probe
	// resets it, and a failed one now holds the endpoint instead.
	LastActiveAt   time.Time `json:"last_active_at,omitzero"`
	InFlight       int       `json:"in_flight,omitempty"`
	SuccessTotal   float64   `json:"success_total,omitempty"`
	ScrapeFailures int       `json:"scrape_failures,omitempty"`

	// LaunchOwner names the launch worker that claimed this record; NodeID is
	// its node. Records written before it carry none.
	LaunchOwner OwnerField `json:"launch_owner,omitzero"`

	// Generation increments on every write and is the CAS single-flight token:
	// a concurrent Ensure that lost the local-mutex race re-reads the record and
	// checks Generation before deciding whether a launch is still needed.
	Generation uint64 `json:"generation"`
}

// LaunchOwner is the launch worker that claimed a record: its per-launch
// nonce and the durable readiness token and epoch the launch ran under.
type LaunchOwner struct {
	WorkerNonce string `json:"worker_nonce"`
	TokenID     string `json:"token_id"`
	Epoch       uint64 `json:"epoch"`
}

// ErrLaunchOwnerInvalid is a launch owner that is present but null, partial or
// malformed, or that has no NodeID. It is never read as a legacy record.
var ErrLaunchOwnerInvalid = errors.New("bedrock: endpoint launch owner is invalid")

func (o LaunchOwner) validate() error {
	if o.WorkerNonce == "" || o.TokenID == "" || o.Epoch == 0 {
		return fmt.Errorf("%w: worker nonce, token ID and epoch are all required", ErrLaunchOwnerInvalid)
	}
	return nil
}

// OwnerField is an optional LaunchOwner that remembers whether the stored
// record carried one, so a damaged owner stays distinct from no owner. A
// damaged owner fails to encode, so it is never rewritten.
type OwnerField struct {
	owner   LaunchOwner
	present bool
	invalid error
}

// Owned returns a field carrying o.
func Owned(o LaunchOwner) OwnerField { return OwnerField{owner: o, present: true} }

// IsZero reports a record with no owner, which omits the field.
func (f OwnerField) IsZero() bool { return !f.present }

// MarshalJSON encodes a complete owner and refuses a damaged or partial one.
func (f OwnerField) MarshalJSON() ([]byte, error) {
	if !f.present {
		return []byte("null"), nil
	}
	if f.invalid != nil {
		return nil, f.invalid
	}
	if err := f.owner.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(f.owner)
}

// UnmarshalJSON records any stored owner, including null; a malformed one is
// kept as invalid rather than failing the whole record.
func (f *OwnerField) UnmarshalJSON(data []byte) error {
	*f = OwnerField{present: true}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		f.invalid = fmt.Errorf("%w: null", ErrLaunchOwnerInvalid)
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f.owner); err != nil {
		f.owner = LaunchOwner{}
		f.invalid = fmt.Errorf("%w: %w", ErrLaunchOwnerInvalid, err)
		return nil
	}
	f.invalid = f.owner.validate()
	return nil
}

// Owner returns the record's launch owner. ok is false only for a record with
// no owner; a present owner that is invalid, or one on a record with no
// NodeID, returns ok true and ErrLaunchOwnerInvalid.
func (r EndpointRecord) Owner() (LaunchOwner, bool, error) {
	f := r.LaunchOwner
	switch {
	case !f.present:
		return LaunchOwner{}, false, nil
	case f.invalid != nil:
		return LaunchOwner{}, true, f.invalid
	}
	if err := f.owner.validate(); err != nil {
		return LaunchOwner{}, true, err
	}
	if r.NodeID == "" {
		return LaunchOwner{}, true, fmt.Errorf("%w: no node", ErrLaunchOwnerInvalid)
	}
	return f.owner, true, nil
}

// ParkReason is the fixed reason a launch's readiness wait ended unready.
type ParkReason string

// The reasons a launch is parked: a member answered its readiness route with
// a redirect, or not every member became ready before the startup timeout.
const (
	ParkReadinessRedirected ParkReason = "readiness-redirected"
	ParkReadinessTimedOut   ParkReason = "readiness-timed-out"
)

// Parked reports a STARTING record whose launch has stopped waiting, so no
// READY will follow without recovery.
func (r EndpointRecord) Parked() bool {
	return r.State == StateStarting && r.ParkedReason != ""
}

// LastActive is the instant this endpoint was last known to be doing work: the
// last sweep that saw it serving, or when it became READY if no sweep ever
// has. Both the idle clock and the LRU eviction order are measured from here,
// so they cannot drift apart. The ReadyAt fallback is what stops an endpoint
// that has been quiet since launch reading as idle since the zero time.
func (r EndpointRecord) LastActive() time.Time {
	if !r.LastActiveAt.IsZero() {
		return r.LastActiveAt
	}
	return r.ReadyAt
}

// MemberEndpoint is one co-served model's own address within a bundle's
// shared VM: the port it listens on at the record's PrivateIP, the weights
// volume cloned for it, and the family (engine) it serves under. Field order
// mirrors LaunchMemberOutput so runLaunch's MemberEndpoint(m) conversion
// between the two stays a plain type conversion.
type MemberEndpoint struct {
	Port            int    `json:"port"`
	WeightsVolumeID string `json:"weights_volume_id,omitempty"`
	Family          string `json:"family,omitempty"`
}

// MemberBaseURL returns modelID's own base address within this bundle:
// "http://{PrivateIP}:{port}". Falls back to the legacy BaseURL field when
// Members/PrivateIP are unpopulated for modelID (a record predating this
// field, or a synthetic test fixture), so a bundle-of-one's resolution is
// unaffected.
func (r EndpointRecord) MemberBaseURL(modelID string) string {
	if member, ok := r.Members[modelID]; ok && r.PrivateIP != "" {
		return "http://" + net.JoinHostPort(r.PrivateIP, strconv.Itoa(member.Port))
	}
	return r.BaseURL
}
