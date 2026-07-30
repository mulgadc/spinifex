package handlers_ec2_vpc

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

// RebindENIPublicIP moves an auto-assigned public IP onto newIP after vpcd's
// lease for it was re-issued on a different address. The old address is already
// released upstream, so leaving the record alone would have
// DescribeInstances report an address nothing holds.
func (s *VPCServiceImpl) RebindENIPublicIP(ctx context.Context, eniID, oldIP, newIP string) error {
	if eniID == "" || newIP == "" {
		return errors.New("rebind ENI public IP: eniID and newIP required")
	}

	record, key, revision, err := s.findENIAnyAccount(ctx, eniID)
	if err != nil {
		return err
	}
	if record.PublicIpAddress == newIP {
		return nil
	}
	if oldIP != "" && record.PublicIpAddress != oldIP {
		return fmt.Errorf("rebind ENI %s: record holds %q, not the superseded %s", eniID, record.PublicIpAddress, oldIP)
	}

	// AddEIP scrubs the predecessor row keyed on this logical IP, so committing
	// the new external IP is enough. Request-reply, because a failure leaves the
	// instance with no working ingress.
	if record.PrivateIpAddress != "" {
		if err := utils.AddNAT(s.natsConn, record.VpcId, newIP, record.PrivateIpAddress,
			topology.Port(eniID), record.MacAddress); err != nil {
			return fmt.Errorf("rebind ENI %s: commit NAT for %s: %w", eniID, newIP, err)
		}
	}

	record.PublicIpAddress = newIP
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("rebind ENI %s: marshal record: %w", eniID, err)
	}
	if _, err := s.eniKV.Update(ctx, key, data, revision); err != nil {
		return fmt.Errorf("rebind ENI %s onto %s: %w", eniID, newIP, err)
	}

	slog.WarnContext(ctx, "ENI public address moved after its DHCP lease was re-issued",
		"eniId", eniID, "oldIp", oldIP, "newIp", newIP, "instanceId", record.InstanceId)
	return nil
}

// findENIAnyAccount locates an ENI without knowing its account. vpcd's lease
// store keys on the client-id alone, so the owning account is not available at
// the point the lease moves.
func (s *VPCServiceImpl) findENIAnyAccount(ctx context.Context, eniID string) (*ENIRecord, string, uint64, error) {
	keys, err := s.eniKV.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, "", 0, fmt.Errorf("list ENI records: %w", err)
	}

	suffix := "." + eniID
	for _, k := range keys {
		if k == utils.VersionKey || !strings.HasSuffix(k, suffix) {
			continue
		}
		entry, err := s.eniKV.Get(ctx, k)
		if err != nil {
			return nil, "", 0, fmt.Errorf("get ENI record %s: %w", k, err)
		}
		var record ENIRecord
		if err := json.Unmarshal(entry.Value(), &record); err != nil {
			return nil, "", 0, fmt.Errorf("unmarshal ENI record %s: %w", k, err)
		}
		return &record, k, entry.Revision(), nil
	}
	return nil, "", 0, fmt.Errorf("no ENI record for %s", eniID)
}
