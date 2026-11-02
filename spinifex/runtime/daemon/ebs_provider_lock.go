package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/nats-io/nats.go/jetstream"
)

// EBSProviderKey is the ClusterConfigBucket key recording the one EBS
// provider the cluster runs.
const EBSProviderKey = "ebs-provider"

// EBSProviderRecord is the value at EBSProviderKey.
type EBSProviderRecord struct {
	Provider string `json:"provider"`
	// RecordedBy names the node whose Create won, for the refusal message.
	RecordedBy string `json:"recorded_by"`
}

// EBSProviderMismatchError is the refusal a daemon returns when its configured
// provider is not the one the cluster recorded. Retrying cannot clear it.
type EBSProviderMismatchError struct {
	Node       string
	Configured string
	Recorded   EBSProviderRecord
}

func (e *EBSProviderMismatchError) Error() string {
	return fmt.Sprintf("EBS provider mismatch: node %s is configured with provider %q, but this cluster runs %q (recorded by node %s in KV bucket %s, key %s); "+
		"set [nodes.%s.ebs] provider = %q to join it, or change the cluster's provider by wiping and reforming the cluster, since volumes record no provider to migrate by",
		e.Node, e.Configured, e.Recorded.Provider, e.Recorded.RecordedBy, ClusterConfigBucket, EBSProviderKey, e.Node, e.Recorded.Provider)
}

func (e *EBSProviderMismatchError) Unwrap() error { return errStartupPermanent }

// EBSProviderRecordInvalidError reports a recorded value that does not decode
// or names no supported provider, so there is nothing to compare against.
type EBSProviderRecordInvalidError struct {
	Raw    []byte
	Reason string
}

func (e *EBSProviderRecordInvalidError) Error() string {
	return fmt.Sprintf("EBS provider lock unreadable: KV bucket %s, key %s holds %q (%s); "+
		"correct the value to {\"provider\":\"<provider>\"} naming the provider the cluster's volumes were created on, or delete the key so the next daemon records its own",
		ClusterConfigBucket, EBSProviderKey, e.Raw, e.Reason)
}

func (e *EBSProviderRecordInvalidError) Unwrap() error { return errStartupPermanent }

// ClaimEBSProvider records provider as the cluster's EBS provider if none is
// recorded, and otherwise checks it against the recorded one. Create decides
// a race, so one provider is ever recorded; it returns that record's revision.
func (m *JetStreamManager) ClaimEBSProvider(ctx context.Context, node, provider string) (uint64, error) {
	if m.clusterConfig == nil {
		return 0, errors.New("cluster config KV not initialized")
	}
	claim := &EBSProviderRecord{Provider: provider, RecordedBy: node}
	rev, err := kvstore.On[EBSProviderRecord](m.clusterConfig).Create(ctx, EBSProviderKey, claim)
	if err == nil {
		return rev, nil
	}
	if !errors.Is(err, kvstore.ErrExists) {
		return 0, fmt.Errorf("record EBS provider: %w", err)
	}
	recorded, rev, err := m.readEBSProvider(ctx)
	if err != nil {
		return 0, err
	}
	if recorded.Provider != provider {
		return 0, &EBSProviderMismatchError{Node: node, Configured: provider, Recorded: *recorded}
	}
	return rev, nil
}

// readEBSProvider reads the raw value rather than through the store's codec,
// so a malformed one can be quoted in the refusal.
func (m *JetStreamManager) readEBSProvider(ctx context.Context) (*EBSProviderRecord, uint64, error) {
	kv, err := m.clusterConfig.KV(ctx)
	if err != nil {
		return nil, 0, err
	}
	entry, err := kv.Get(ctx, EBSProviderKey)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, 0, fmt.Errorf("recorded EBS provider vanished after Create lost: %w", err)
		}
		return nil, 0, fmt.Errorf("read recorded EBS provider: %w", err)
	}
	var rec EBSProviderRecord
	if err := json.Unmarshal(entry.Value(), &rec); err != nil {
		return nil, 0, &EBSProviderRecordInvalidError{Raw: entry.Value(), Reason: "not a JSON record: " + err.Error()}
	}
	if !config.SupportedEBSProvider(rec.Provider) {
		return nil, 0, &EBSProviderRecordInvalidError{Raw: entry.Value(), Reason: fmt.Sprintf("provider %q is not a supported provider", rec.Provider)}
	}
	return &rec, entry.Revision(), nil
}
