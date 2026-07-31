package handlers_ec2_eip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/network/topology"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

// RebindPublicIP moves an allocation onto newIP after vpcd's lease for it was
// re-issued on a different address. The old address is already released
// upstream by the time this runs, so refusing to move the record would leave
// DescribeAddresses advertising an address nothing holds.
func (s *EIPServiceImpl) RebindPublicIP(ctx context.Context, allocationID, oldIP, newIP string) error {
	if allocationID == "" || newIP == "" {
		return errors.New("rebind EIP: allocationID and newIP required")
	}

	record, key, revision, err := s.findByAllocationIDAnyAccount(ctx, allocationID)
	if err != nil {
		return err
	}
	// Already on newIP: a duplicate delivery. The NAT commit below is
	// idempotent but the revision check is not, so stop here.
	if record.PublicIp == newIP {
		return nil
	}
	// Moving a record naming neither address would overwrite an allocation
	// this lease does not own.
	if oldIP != "" && record.PublicIp != oldIP {
		return fmt.Errorf("rebind EIP %s: record holds %s, not the superseded %s", allocationID, record.PublicIp, oldIP)
	}

	// Only an associated EIP has a dnat_and_snat to move. AddEIP scrubs the
	// predecessor row keyed on this logical IP, so committing the new external
	// IP is sufficient. Request-reply: a failure here means the instance has no
	// working ingress, so it must not be swallowed.
	if record.ENIId != "" && record.PrivateIp != "" {
		if err := utils.AddNAT(s.natsConn, record.VpcId, newIP, record.PrivateIp,
			topology.Port(record.ENIId), record.MacAddress); err != nil {
			return fmt.Errorf("rebind EIP %s: commit NAT for %s: %w", allocationID, newIP, err)
		}
	}

	record.PublicIp = newIP
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("rebind EIP %s: marshal record: %w", allocationID, err)
	}
	if _, err := s.eipKV.Update(ctx, key, data, revision); err != nil {
		return fmt.Errorf("rebind EIP %s onto %s: %w", allocationID, newIP, err)
	}

	slog.WarnContext(ctx, "EIP public address moved after its DHCP lease was re-issued",
		"allocationId", allocationID, "oldIp", oldIP, "newIp", newIP, "eniId", record.ENIId)
	return nil
}

// AllocationExists reports whether an allocation record survives, without
// knowing its account. Used to decide whether a DHCP lease still has an owner,
// so a listing failure is an error rather than a false — releasing an address on
// a KV timeout would be worse than keeping an orphan.
func (s *EIPServiceImpl) AllocationExists(ctx context.Context, allocationID string) (bool, error) {
	if allocationID == "" {
		return false, errors.New("allocation exists: allocationID required")
	}
	keys, err := s.eipKV.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return false, nil
		}
		return false, fmt.Errorf("list EIP records: %w", err)
	}
	suffix := "." + allocationID
	for _, k := range keys {
		if k != utils.VersionKey && strings.HasSuffix(k, suffix) {
			return true, nil
		}
	}
	return false, nil
}

// findByAllocationIDAnyAccount locates an allocation without knowing its
// account. vpcd's lease store keys on the client-id alone, so the owning
// account is not available at the point the lease moves.
func (s *EIPServiceImpl) findByAllocationIDAnyAccount(ctx context.Context, allocationID string) (*EIPRecord, string, uint64, error) {
	keys, err := s.eipKV.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, "", 0, fmt.Errorf("list EIP records: %w", err)
	}

	suffix := "." + allocationID
	for _, k := range keys {
		if k == utils.VersionKey || !strings.HasSuffix(k, suffix) {
			continue
		}
		entry, err := s.eipKV.Get(ctx, k)
		if err != nil {
			return nil, "", 0, fmt.Errorf("get EIP record %s: %w", k, err)
		}
		var record EIPRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			return nil, "", 0, fmt.Errorf("unmarshal EIP record %s: %w", k, err)
		}
		return &record, k, entry.Revision(), nil
	}
	return nil, "", 0, fmt.Errorf("no EIP record for allocation %s", allocationID)
}
