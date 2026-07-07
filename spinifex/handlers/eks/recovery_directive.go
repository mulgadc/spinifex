package handlers_eks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/nats-io/nats.go"
)

// RecoveryAction is the directive a control-plane member applies on its next boot
// to recover a wedged embedded-etcd quorum. The on-VM k3s-recovery agent fetches
// it from the host via the internal-recovery gateway route before k3s starts.
type RecoveryAction string

const (
	// RecoveryActionNone is the steady state: the member boots k3s normally.
	RecoveryActionNone RecoveryAction = "none"
	// RecoveryActionClusterReset runs `k3s server --cluster-reset` on the seed
	// member, re-forming a single-member etcd from its intact local data (or from
	// Snapshot when set); the other members then rejoin it.
	RecoveryActionClusterReset RecoveryAction = "cluster-reset"
	// RecoveryActionWipeRejoin removes the member's stale etcd data so k3s rejoins
	// the reset seed as a fresh member.
	RecoveryActionWipeRejoin RecoveryAction = "wipe-rejoin"
)

// RecoveryDirective is the per-member recovery instruction. Epoch increases on
// each set so the guest applies a directive at most once: it records the
// last-applied epoch and ignores an equal-or-older one across reboots.
type RecoveryDirective struct {
	Epoch    int64          `json:"epoch"`
	Action   RecoveryAction `json:"action"`
	Snapshot string         `json:"snapshot,omitempty"`
}

// GetRecoveryDirectiveInput names the cluster + member whose directive to read.
type GetRecoveryDirectiveInput struct {
	ClusterName string `json:"clusterName"`
	InstanceID  string `json:"instanceId"`
}

// GetRecoveryDirectiveOutput carries the member's directive (none when unset).
type GetRecoveryDirectiveOutput struct {
	Directive RecoveryDirective `json:"directive"`
}

// SetRecoveryDirectiveInput sets a member's directive; Epoch is assigned by the
// store (previous+1), so callers supply only the action + optional snapshot key.
type SetRecoveryDirectiveInput struct {
	ClusterName string         `json:"clusterName"`
	InstanceID  string         `json:"instanceId"`
	Action      RecoveryAction `json:"action"`
	Snapshot    string         `json:"snapshot,omitempty"`
}

// SetRecoveryDirectiveOutput returns the stored directive incl. the assigned epoch.
type SetRecoveryDirectiveOutput struct {
	Directive RecoveryDirective `json:"directive"`
}

// LoadRecoveryDirective reads a member's directive from the account KV, returning
// a zero-epoch none directive when the key is absent.
func LoadRecoveryDirective(acctKV nats.KeyValue, cluster, instanceID string) (RecoveryDirective, error) {
	entry, err := acctKV.Get(RecoveryDirectiveKey(cluster, instanceID))
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return RecoveryDirective{Epoch: 0, Action: RecoveryActionNone}, nil
		}
		return RecoveryDirective{}, err
	}
	var d RecoveryDirective
	if err := json.Unmarshal(entry.Value(), &d); err != nil {
		return RecoveryDirective{}, fmt.Errorf("unmarshal recovery directive: %w", err)
	}
	if d.Action == "" {
		d.Action = RecoveryActionNone
	}
	return d, nil
}

// StoreRecoveryDirective writes a member's directive with epoch = previous+1 and
// returns the stored value. Used by the reconciler escalation and the operator CLI.
func StoreRecoveryDirective(acctKV nats.KeyValue, cluster, instanceID string, action RecoveryAction, snapshot string) (RecoveryDirective, error) {
	prev, err := LoadRecoveryDirective(acctKV, cluster, instanceID)
	if err != nil {
		return RecoveryDirective{}, err
	}
	next := RecoveryDirective{Epoch: prev.Epoch + 1, Action: action, Snapshot: snapshot}
	data, err := json.Marshal(&next)
	if err != nil {
		return RecoveryDirective{}, fmt.Errorf("marshal recovery directive: %w", err)
	}
	if _, err := acctKV.Put(RecoveryDirectiveKey(cluster, instanceID), data); err != nil {
		return RecoveryDirective{}, fmt.Errorf("kv put recovery directive: %w", err)
	}
	return next, nil
}

// GetRecoveryDirective serves a member's current recovery directive to the on-VM
// k3s-recovery agent via the internal-recovery gateway route.
func (s *EKSServiceImpl) GetRecoveryDirective(_ context.Context, input *GetRecoveryDirectiveInput, accountID string) (*GetRecoveryDirectiveOutput, error) {
	if input == nil || input.ClusterName == "" || input.InstanceID == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	acctKV, err := s.acctKVForCluster(accountID, input.ClusterName)
	if err != nil {
		return nil, err
	}
	d, err := LoadRecoveryDirective(acctKV, input.ClusterName, input.InstanceID)
	if err != nil {
		return nil, err
	}
	return &GetRecoveryDirectiveOutput{Directive: d}, nil
}

// SetRecoveryDirective records a member's recovery directive (operator CLI path).
func (s *EKSServiceImpl) SetRecoveryDirective(_ context.Context, input *SetRecoveryDirectiveInput, accountID string) (*SetRecoveryDirectiveOutput, error) {
	if input == nil || input.ClusterName == "" || input.InstanceID == "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	switch input.Action {
	case RecoveryActionNone, RecoveryActionClusterReset, RecoveryActionWipeRejoin:
	default:
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	acctKV, err := s.acctKVForCluster(accountID, input.ClusterName)
	if err != nil {
		return nil, err
	}
	d, err := StoreRecoveryDirective(acctKV, input.ClusterName, input.InstanceID, input.Action, input.Snapshot)
	if err != nil {
		return nil, err
	}
	return &SetRecoveryDirectiveOutput{Directive: d}, nil
}
