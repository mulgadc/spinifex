// Package handlers_bedrock owns placement and lifecycle of self-hosted vLLM
// serving VMs: it decides where a model's endpoint runs, launches and probes
// the VM, and persists the result as the single source of truth other
// components (the gateway's inference path) read over NATS.
package handlers_bedrock

import (
	"errors"
	"fmt"
	"slices"
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
	ENIID           string `json:"eni_id,omitempty"`
	WeightsVolumeID string `json:"weights_volume_id,omitempty"`

	CreatedAt time.Time `json:"created_at,omitzero"`
	ReadyAt   time.Time `json:"ready_at,omitzero"`

	// Pinned exempts this endpoint from the idle-reclaim sweep and from eviction.
	Pinned bool `json:"pinned,omitempty"`

	// The reaper's last observation of this endpoint. Persisted rather than kept
	// in the reaper's memory because an eviction decision runs on whichever
	// replica the queue group picked, not necessarily the reaper's leader, and
	// because a leader change must not reset the idle clock or the failure count.
	//
	// LastActiveAt is the last sweep at which the endpoint was observed serving,
	// seeded from ReadyAt; it is the LRU ordering key. InFlight and SuccessTotal
	// are the last successful scrape's queue depth and completed-request count,
	// the latter being idleness's "unchanged since the previous tick" comparison
	// point. ScrapeFailures counts consecutive failed scrapes.
	LastActiveAt   time.Time `json:"last_active_at,omitzero"`
	InFlight       int       `json:"in_flight,omitempty"`
	SuccessTotal   float64   `json:"success_total,omitempty"`
	ScrapeFailures int       `json:"scrape_failures,omitempty"`

	// Generation increments on every write and is the CAS single-flight token:
	// a concurrent Ensure that lost the local-mutex race re-reads the record and
	// checks Generation before deciding whether a launch is still needed.
	Generation uint64 `json:"generation"`
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
