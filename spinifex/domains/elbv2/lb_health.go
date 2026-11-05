package elbv2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// KeyPrefixLBHealth holds the data-path evidence of one load balancer. It is
	// a separate key from the LB record so whole-record writes cannot drop it.
	KeyPrefixLBHealth = "lbhealth."

	// StateActiveImpaired is what DescribeLoadBalancers reports while a data NIC
	// of the LB's current VM is failing its gateway probe.
	StateActiveImpaired = "active_impaired"

	// lbSuspendBackstop is how long a suspension may outlive its mutation before
	// a heartbeat from the current VM lifts it.
	lbSuspendBackstop = 30 * time.Minute
)

// Outcomes of a recorded repair. Reserved is written before the repair is
// dispatched, so an entry whose outcome was never updated still counts.
const (
	RepairReserved    = "reserved"
	RepairDispatched  = "dispatched"
	RepairRefused     = "refused"
	RepairUnreachable = "unreachable"
)

// LBNICFailure is one data NIC that was failing its gateway probe when the
// transition was recorded.
type LBNICFailure struct {
	ENIID               string    `json:"eni_id,omitempty"`
	FailingSince        time.Time `json:"failing_since"`
	ConsecutiveFailures int64     `json:"consecutive_failures"`
	RxPackets           int64     `json:"rx_packets"`
	TxPackets           int64     `json:"tx_packets"`
	RxDropped           int64     `json:"rx_dropped"`
	TxDropped           int64     `json:"tx_dropped"`
}

// LBRepair is one repair of an LB's VM and how it ended.
type LBRepair struct {
	At         time.Time `json:"at"`
	InstanceID string    `json:"instance_id"`
	ENIID      string    `json:"eni_id,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Outcome    string    `json:"outcome"`
}

// LBDataPathRecord is the data-path evidence of one load balancer VM. NICs holds
// only failing NICs, keyed by lower-case MAC; an absent NIC is healthy.
type LBDataPathRecord struct {
	InstanceID  string                   `json:"instance_id"`
	Suspended   bool                     `json:"suspended,omitempty"`
	SuspendedAt time.Time                `json:"suspended_at,omitzero"`
	BoundAt     time.Time                `json:"bound_at,omitzero"`
	NICs        map[string]*LBNICFailure `json:"nics,omitempty"`
	Repairs     []LBRepair               `json:"repairs,omitempty"`
}

// failing reports whether any NIC of the record is failing.
func (r *LBDataPathRecord) failing() bool {
	return r != nil && len(r.NICs) > 0
}

// GetLBHealth returns the evidence record of a load balancer, or nil when none
// has been written.
func (s *Store) GetLBHealth(ctx context.Context, lbID string) (*LBDataPathRecord, error) {
	entry, err := s.kv.Get(ctx, KeyPrefixLBHealth+lbID)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var rec LBDataPathRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return nil, fmt.Errorf("unmarshal lb health: %w", err)
	}
	return &rec, nil
}

// UpdateLBHealth applies mutate to the evidence record under optimistic
// concurrency, creating it when absent. mutate returns false to write nothing.
func (s *Store) UpdateLBHealth(ctx context.Context, lbID string, mutate func(*LBDataPathRecord) (bool, error)) (*LBDataPathRecord, error) {
	return kvutil.Update(ctx, s.kv, KeyPrefixLBHealth+lbID, kvutil.CASConfig{
		Attempts:       targetGroupCASAttempts,
		CreateIfAbsent: true,
		Exhausted: func(_ string, attempts int) error {
			return fmt.Errorf("elbv2: lb health %s contended, CAS exhausted after %d attempts", lbID, attempts)
		},
	}, mutate)
}

// DeleteLBHealth removes the evidence record of a load balancer.
func (s *Store) DeleteLBHealth(ctx context.Context, lbID string) error {
	err := s.kv.Delete(ctx, KeyPrefixLBHealth+lbID)
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return err
	}
	return nil
}

// SuspendLBHealth marks the load balancer as being torn down or reshaped, so
// no repair is admitted against its VM until the mutation finishes.
func (s *Store) SuspendLBHealth(ctx context.Context, lbID string) error {
	_, err := s.UpdateLBHealth(ctx, lbID, func(rec *LBDataPathRecord) (bool, error) {
		if rec.Suspended {
			return false, nil
		}
		rec.Suspended = true
		rec.SuspendedAt = time.Now().UTC()
		return true, nil
	})
	return err
}

// ResumeLBHealth lifts a suspension. A non-empty instanceID also binds the
// record to that VM and drops the old VM's failing NICs.
func (s *Store) ResumeLBHealth(ctx context.Context, lbID, instanceID string) error {
	_, err := s.UpdateLBHealth(ctx, lbID, func(rec *LBDataPathRecord) (bool, error) {
		changed := rec.Suspended
		rec.Suspended = false
		rec.SuspendedAt = time.Time{}
		if instanceID != "" && rec.InstanceID != instanceID {
			rec.InstanceID = instanceID
			rec.BoundAt = time.Now().UTC()
			rec.NICs = nil
			changed = true
		}
		return changed, nil
	})
	return err
}

// failingNICMACs returns the lower-case MACs that are failing. A probe that
// failed starts a failure. An established failure ends only when the latest
// probe succeeded, because an agent restarted by a repair reports healthy for
// its first misses.
func failingNICMACs(nics []*LBAgentNICStatus, established map[string]*LBNICFailure) map[string]*LBAgentNICStatus {
	out := make(map[string]*LBAgentNICStatus)
	for _, n := range nics {
		if n == nil || n.MAC == nil || n.ProbeOK == nil {
			continue
		}
		mac := strings.ToLower(*n.MAC)
		if !*n.ProbeOK || (established[mac] != nil && aws.Int64Value(n.ConsecutiveFailures) != 0) {
			out[mac] = n
		}
	}
	return out
}

// recordDataPathEvidence stores the data-path evidence of one heartbeat. It
// writes only when the set of failing NICs changes, and ignores evidence that
// does not come from the VM the LB record currently names.
func (s *ELBv2ServiceImpl) recordDataPathEvidence(ctx context.Context, lb *LoadBalancerRecord, input *LBAgentHeartbeatInput) {
	if lb.InstanceID == "" || input.InstanceID == nil || *input.InstanceID != lb.InstanceID || len(input.NICs) == 0 {
		return
	}

	rec, err := s.store.GetLBHealth(ctx, lb.LoadBalancerID)
	if err != nil {
		slog.WarnContext(ctx, "LBAgentHeartbeat: failed to read data-path evidence", "lbId", lb.LoadBalancerID, "err", err)
		return
	}
	converge := false
	if rec != nil && rec.Suspended {
		if !s.suspensionConverges(ctx, lb, rec, *input.InstanceID) {
			return
		}
		converge = true
	}
	var known map[string]*LBNICFailure
	if rec != nil && rec.InstanceID == lb.InstanceID {
		known = rec.NICs
	}
	failing := failingNICMACs(input.NICs, known)
	if !converge && sameMACs(known, failing) && (len(failing) == 0 || rec != nil) {
		return
	}

	eniByMAC := s.resolveLBENIs(ctx, lb, failing)
	now := time.Now().UTC()
	_, err = s.store.UpdateLBHealth(ctx, lb.LoadBalancerID, func(cur *LBDataPathRecord) (bool, error) {
		changed := false
		if cur.Suspended {
			if !converge || !suspensionLifts(cur, lb.InstanceID, now) {
				return false, nil
			}
			cur.Suspended = false
			cur.SuspendedAt = time.Time{}
			changed = true
			if cur.InstanceID != lb.InstanceID {
				cur.InstanceID = lb.InstanceID
				cur.BoundAt = now
				cur.NICs = nil
			}
		}
		if cur.InstanceID != "" && cur.InstanceID != lb.InstanceID {
			return changed, nil
		}
		prev := cur.NICs
		failing := failingNICMACs(input.NICs, prev)
		if cur.InstanceID == lb.InstanceID && sameMACs(prev, failing) {
			return changed, nil
		}
		next := make(map[string]*LBNICFailure, len(failing))
		for mac, n := range failing {
			f := &LBNICFailure{
				ENIID:               eniByMAC[mac],
				FailingSince:        now,
				ConsecutiveFailures: aws.Int64Value(n.ConsecutiveFailures),
				RxPackets:           aws.Int64Value(n.RxPackets),
				TxPackets:           aws.Int64Value(n.TxPackets),
				RxDropped:           aws.Int64Value(n.RxDropped),
				TxDropped:           aws.Int64Value(n.TxDropped),
			}
			if old, ok := prev[mac]; ok {
				f.FailingSince = old.FailingSince
			}
			next[mac] = f
		}
		cur.InstanceID = lb.InstanceID
		cur.NICs = next
		return true, nil
	})
	if err != nil {
		slog.WarnContext(ctx, "LBAgentHeartbeat: failed to store data-path evidence", "lbId", lb.LoadBalancerID, "err", err)
		return
	}
	for mac, n := range failing {
		if _, ok := known[mac]; !ok {
			slog.WarnContext(ctx, "ELBv2 data-path probe failing",
				"lbArn", lb.LoadBalancerArn, "lbId", lb.LoadBalancerID, "instanceId", lb.InstanceID,
				"eniId", eniByMAC[mac], "mac", mac, "consecutive_failures", aws.Int64Value(n.ConsecutiveFailures),
				"rx_packets", aws.Int64Value(n.RxPackets), "tx_packets", aws.Int64Value(n.TxPackets),
				"rx_dropped", aws.Int64Value(n.RxDropped), "tx_dropped", aws.Int64Value(n.TxDropped))
		}
	}
	for mac := range known {
		if _, ok := failing[mac]; !ok {
			slog.InfoContext(ctx, "ELBv2 data-path probe recovered",
				"lbArn", lb.LoadBalancerArn, "lbId", lb.LoadBalancerID, "instanceId", lb.InstanceID, "mac", mac)
		}
	}
}

// suspensionLifts reports whether a suspended record may be lifted by a
// heartbeat from instanceID: it is bound to an older VM, or it has outlived the
// backstop while bound to the current one.
func suspensionLifts(rec *LBDataPathRecord, instanceID string, now time.Time) bool {
	if rec.InstanceID != "" && rec.InstanceID != instanceID {
		return true
	}
	return !rec.SuspendedAt.IsZero() && now.Sub(rec.SuspendedAt) >= lbSuspendBackstop
}

// suspensionConverges confirms against a fresh read of the LB record that the
// VM the heartbeat reports is the one the LB currently names.
func (s *ELBv2ServiceImpl) suspensionConverges(ctx context.Context, lb *LoadBalancerRecord, rec *LBDataPathRecord, heartbeatInstance string) bool {
	if !suspensionLifts(rec, heartbeatInstance, time.Now().UTC()) {
		return false
	}
	fresh, err := s.store.GetLoadBalancer(ctx, lb.LoadBalancerID)
	return err == nil && fresh != nil && fresh.InstanceID == heartbeatInstance
}

// sameMACs reports whether two failing sets name the same NICs.
func sameMACs[A, B any](a map[string]A, b map[string]B) bool {
	if len(a) != len(b) {
		return false
	}
	for mac := range a {
		if _, ok := b[mac]; !ok {
			return false
		}
	}
	return true
}

// resolveLBENIs maps failing MACs to ENI IDs from the LB's cross-account ENIs
// and one describe of its own ENIs. A MAC it cannot resolve maps to nothing.
func (s *ELBv2ServiceImpl) resolveLBENIs(ctx context.Context, lb *LoadBalancerRecord, failing map[string]*LBAgentNICStatus) map[string]string {
	out := make(map[string]string, len(failing))
	for _, x := range lb.CrossAccountENIs {
		mac := strings.ToLower(x.ENIMac)
		if _, ok := failing[mac]; ok {
			out[mac] = x.ENIID
		}
	}
	if len(out) == len(failing) {
		return out
	}
	for id, eni := range s.describeENIs(ctx, lb.ENIs, lb.AccountID) {
		mac := strings.ToLower(aws.StringValue(eni.MacAddress))
		if _, ok := failing[mac]; ok && out[mac] == "" {
			out[mac] = id
		}
	}
	return out
}

// impairedByEvidence reports whether the LB's current VM has a failing data NIC.
func (s *ELBv2ServiceImpl) impairedByEvidence(ctx context.Context, r *LoadBalancerRecord) bool {
	if r.State != StateActive || r.InstanceID == "" {
		return false
	}
	rec, err := s.store.GetLBHealth(ctx, r.LoadBalancerID)
	if err != nil {
		slog.DebugContext(ctx, "lbRecordToSDK: failed to read data-path evidence", "lbId", r.LoadBalancerID, "err", err)
		return false
	}
	return rec != nil && !rec.Suspended && rec.InstanceID == r.InstanceID && rec.failing()
}

// resumeLBHealth lifts a suspension after a failure that left the VM untouched.
func (s *ELBv2ServiceImpl) resumeLBHealth(ctx context.Context, lbID string) {
	if err := s.store.ResumeLBHealth(ctx, lbID, ""); err != nil {
		slog.WarnContext(ctx, "failed to lift data-path suspension", "lbId", lbID, "err", err)
	}
}
