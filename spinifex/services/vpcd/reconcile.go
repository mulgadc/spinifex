package vpcd

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	handlers_ec2_eip "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eip"
	handlers_ec2_igw "github.com/mulgadc/spinifex/spinifex/handlers/ec2/igw"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/services/vpcd/nbdb"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// reconcileLeaderBucket holds a single CAS-elected leader key. MaxAge bounds
// recovery time when an elected vpcd crashes before releasing the lock.
const (
	reconcileLeaderBucket = "spinifex-vpcd-reconcile"
	reconcileLeaderKey    = "leader"
	reconcileLeaderTTL    = 60 * time.Second
)

// AcquireReconcileLeader returns release+true exactly once across all vpcds in
// a cluster, gating the startup Reconcile/ReconcileFromKV passes. Other vpcds
// get release=nil, elected=false and skip reconcile (runtime VPC events use the
// vpcd-workers queue group, so they remain handled cluster-wide).
//
// Returns elected=true on KV-bucket failure: first-boot has at most one vpcd
// up, so falling through is safe and avoids deadlocking the cluster if NATS
// KV isn't ready.
func AcquireReconcileLeader(nc *nats.Conn, holder string) (func(), bool) {
	js, _ := nc.JetStream()
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:  reconcileLeaderBucket,
		History: 1,
		TTL:     reconcileLeaderTTL,
	})
	if err != nil {
		slog.Warn("vpcd reconcile-leader: KV bucket unavailable, running reconcile unguarded", "err", err)
		return func() {}, true
	}

	if _, err := kv.Create(reconcileLeaderKey, []byte(holder)); err != nil {
		slog.Info("vpcd reconcile-leader: another vpcd is leader, skipping reconcile", "holder", holder, "err", err)
		return nil, false
	}

	slog.Info("vpcd reconcile-leader: elected", "holder", holder)
	return func() {
		if err := kv.Delete(reconcileLeaderKey); err != nil {
			slog.Warn("vpcd reconcile-leader: failed to release lock (TTL will reap)", "holder", holder, "err", err)
		}
	}, true
}

// ReconcileResult tracks what was created during reconciliation.
type ReconcileResult struct {
	RoutersCreated  int
	SwitchesCreated int
	IGWsCreated     int
	PortsCreated    int
	NATsReconciled  int
}

// Reconcile ensures OVN topology matches the expected state from the bootstrap config.
// This runs on vpcd startup before subscribing to NATS topics.
//
// Pass 1 (bootstrap): Uses [bootstrap] from spinifex.toml to create the default VPC
// topology. This covers first-install where admin init ran before services started.
//
// All operations are idempotent — safe to call on every startup.
func Reconcile(ctx context.Context, topo *TopologyHandler, bootstrap *BootstrapVPC) ReconcileResult {
	var result ReconcileResult

	if bootstrap == nil || bootstrap.VpcId == "" {
		slog.Debug("vpcd reconcile: no bootstrap config, skipping")
		return result
	}

	slog.Info("vpcd reconcile: checking bootstrap VPC topology",
		"vpc_id", bootstrap.VpcId,
		"subnet_id", bootstrap.SubnetId,
	)

	// 1. Ensure VPC router exists
	routerName := "vpc-" + bootstrap.VpcId
	if _, err := topo.ovn.GetLogicalRouter(ctx, routerName); err != nil {
		slog.Info("vpcd reconcile: creating VPC router", "router", routerName)
		if err := topo.reconcileVPC(ctx, bootstrap.VpcId, bootstrap.Cidr); err != nil {
			slog.Error("vpcd reconcile: failed to create VPC router", "err", err)
		} else {
			result.RoutersCreated++
		}
	} else {
		slog.Debug("vpcd reconcile: VPC router exists", "router", routerName)
	}

	// 2. Ensure subnet switch + router port + DHCP exists
	if bootstrap.SubnetId != "" {
		switchName := "subnet-" + bootstrap.SubnetId
		if _, err := topo.ovn.GetLogicalSwitch(ctx, switchName); err != nil {
			slog.Info("vpcd reconcile: creating subnet topology", "switch", switchName)
			if err := topo.reconcileSubnet(ctx, bootstrap.SubnetId, bootstrap.VpcId, bootstrap.SubnetCidr); err != nil {
				slog.Error("vpcd reconcile: failed to create subnet topology", "err", err)
			} else {
				result.SwitchesCreated++
			}
		} else {
			slog.Debug("vpcd reconcile: subnet switch exists", "switch", switchName)
		}
	}

	// 3. Ensure IGW topology exists (external switch, SNAT, gateway chassis)
	extSwitchName := "ext-" + bootstrap.VpcId
	if _, err := topo.ovn.GetLogicalSwitch(ctx, extSwitchName); err != nil {
		slog.Info("vpcd reconcile: creating IGW topology", "switch", extSwitchName)
		if err := topo.reconcileIGW(ctx, bootstrap.VpcId, bootstrap.IgwId); err != nil {
			slog.Error("vpcd reconcile: failed to create IGW topology", "err", err)
		} else {
			result.IGWsCreated++
		}
	} else {
		slog.Debug("vpcd reconcile: IGW topology exists", "switch", extSwitchName)
	}

	slog.Info("vpcd reconcile: complete",
		"routers_created", result.RoutersCreated,
		"switches_created", result.SwitchesCreated,
		"igws_created", result.IGWsCreated,
	)

	return result
}

// ReconcileFromKV reads all VPCs, subnets, IGW attachments, and ENIs from NATS KV
// and ensures OVN topology matches. This is Pass 2 from the Phase 12 plan — it
// handles reboots, OVN DB loss, and any missed events. All operations are idempotent.
//
// chassisNames is the set of OVN chassis names discovered from the SBDB at
// startup; reconcileGatewayChassis uses it to delete stale Gateway_Chassis
// rows (e.g. left over from a chassis_name change across reboot) and rebind
// every gateway LRP against the live chassis (mulga-999).
func ReconcileFromKV(ctx context.Context, nc *nats.Conn, topo *TopologyHandler, chassisNames []string) ReconcileResult {
	var result ReconcileResult

	// 0. Reconcile gateway_chassis bindings before any per-VPC work. State
	// drift here (a gateway_chassis row pointing at a chassis that no longer
	// exists) leaves the cr-gw* port unbound — centralized NAT silently
	// breaks. Single SB-derived list, applied once.
	if err := topo.reconcileGatewayChassis(ctx, chassisNames); err != nil {
		slog.Warn("vpcd reconcile-kv: gateway_chassis reconcile failed", "err", err)
	}

	js, err := nc.JetStream()
	if err != nil {
		slog.Warn("vpcd reconcile-kv: failed to get JetStream context", "err", err)
		return result
	}

	// 1. Reconcile VPCs: ensure each VPC has a logical router
	vpcKV, err := js.KeyValue(handlers_ec2_vpc.KVBucketVPCs)
	if err != nil {
		slog.Debug("vpcd reconcile-kv: VPC KV bucket not available (first boot before daemon?)", "err", err)
		return result
	}

	// Build a map of VPC ID → CIDR for subnet/IGW reconciliation
	type vpcInfo struct {
		VpcId     string
		CidrBlock string
	}
	vpcMap := make(map[string]vpcInfo)

	vpcKeys, err := vpcKV.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		slog.Warn("vpcd reconcile-kv: failed to list VPC keys", "err", err)
	}
	for _, key := range vpcKeys {
		if key == utils.VersionKey {
			continue
		}
		entry, err := vpcKV.Get(key)
		if err != nil {
			continue
		}
		var rec handlers_ec2_vpc.VPCRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			slog.Warn("vpcd reconcile-kv: failed to unmarshal VPC record", "key", key, "err", err)
			continue
		}
		vpcMap[rec.VpcId] = vpcInfo{VpcId: rec.VpcId, CidrBlock: rec.CidrBlock}

		routerName := "vpc-" + rec.VpcId
		if _, err := topo.ovn.GetLogicalRouter(ctx, routerName); err != nil {
			slog.Info("vpcd reconcile-kv: creating VPC router", "router", routerName)
			if err := topo.reconcileVPC(ctx, rec.VpcId, rec.CidrBlock); err != nil {
				slog.Error("vpcd reconcile-kv: failed to create VPC router", "err", err)
			} else {
				result.RoutersCreated++
			}
		}
	}

	// 2. Reconcile subnets: ensure each subnet has a logical switch + router port + DHCP
	subnetKV, err := js.KeyValue(handlers_ec2_vpc.KVBucketSubnets)
	if err != nil {
		slog.Debug("vpcd reconcile-kv: subnet KV bucket not available", "err", err)
	} else {
		subnetKeys, err := subnetKV.Keys()
		if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
			slog.Warn("vpcd reconcile-kv: failed to list subnet keys", "err", err)
		}
		for _, key := range subnetKeys {
			if key == utils.VersionKey {
				continue
			}
			entry, err := subnetKV.Get(key)
			if err != nil {
				continue
			}
			var rec handlers_ec2_vpc.SubnetRecord
			if err := json.Unmarshal(entry.Value(), &rec); err != nil {
				slog.Warn("vpcd reconcile-kv: failed to unmarshal subnet record", "key", key, "err", err)
				continue
			}

			switchName := "subnet-" + rec.SubnetId
			if _, err := topo.ovn.GetLogicalSwitch(ctx, switchName); err != nil {
				slog.Info("vpcd reconcile-kv: creating subnet topology", "switch", switchName)
				if err := topo.reconcileSubnet(ctx, rec.SubnetId, rec.VpcId, rec.CidrBlock); err != nil {
					slog.Error("vpcd reconcile-kv: failed to create subnet topology", "err", err)
				} else {
					result.SwitchesCreated++
				}
			}
		}
	}

	// 3. Reconcile IGW attachments: ensure attached IGWs have external switch + SNAT
	igwKV, err := js.KeyValue(handlers_ec2_igw.KVBucketIGW)
	if err != nil {
		slog.Debug("vpcd reconcile-kv: IGW KV bucket not available", "err", err)
	} else {
		igwKeys, err := igwKV.Keys()
		if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
			slog.Warn("vpcd reconcile-kv: failed to list IGW keys", "err", err)
		}
		for _, key := range igwKeys {
			if key == utils.VersionKey {
				continue
			}
			entry, err := igwKV.Get(key)
			if err != nil {
				continue
			}
			var rec handlers_ec2_igw.IGWRecord
			if err := json.Unmarshal(entry.Value(), &rec); err != nil {
				slog.Warn("vpcd reconcile-kv: failed to unmarshal IGW record", "key", key, "err", err)
				continue
			}
			// Only reconcile attached IGWs
			if rec.VpcId == "" || rec.State != "attached" {
				continue
			}

			extSwitchName := "ext-" + rec.VpcId
			if _, err := topo.ovn.GetLogicalSwitch(ctx, extSwitchName); err != nil {
				slog.Info("vpcd reconcile-kv: creating IGW topology", "switch", extSwitchName, "igw_id", rec.InternetGatewayId)
				if err := topo.reconcileIGW(ctx, rec.VpcId, rec.InternetGatewayId); err != nil {
					slog.Error("vpcd reconcile-kv: failed to create IGW topology", "err", err)
				} else {
					result.IGWsCreated++
				}
			}
		}
	}

	// 4. Reconcile ENI ports: ensure each ENI has a logical switch port.
	// eniMAC is populated here and consumed by Step 5 to build distributed NAT
	// rules (direct bridge mode only; centralized mode doesn't need the MAC).
	eniMAC := make(map[string]string) // eniID → MAC
	eniKV, err := js.KeyValue(handlers_ec2_vpc.KVBucketENIs)
	if err != nil {
		slog.Debug("vpcd reconcile-kv: ENI KV bucket not available", "err", err)
	} else {
		eniKeys, err := eniKV.Keys()
		if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
			slog.Warn("vpcd reconcile-kv: failed to list ENI keys", "err", err)
		}
		for _, key := range eniKeys {
			if key == utils.VersionKey {
				continue
			}
			entry, err := eniKV.Get(key)
			if err != nil {
				continue
			}
			var rec handlers_ec2_vpc.ENIRecord
			if err := json.Unmarshal(entry.Value(), &rec); err != nil {
				slog.Warn("vpcd reconcile-kv: failed to unmarshal ENI record", "key", key, "err", err)
				continue
			}

			eniMAC[rec.NetworkInterfaceId] = rec.MacAddress

			portName := "port-" + rec.NetworkInterfaceId
			if _, err := topo.ovn.GetLogicalSwitchPort(ctx, portName); err != nil {
				switchName := "subnet-" + rec.SubnetId
				addrStr := rec.MacAddress + " " + rec.PrivateIpAddress
				lsp := &nbdb.LogicalSwitchPort{
					Name:         portName,
					Addresses:    []string{addrStr},
					PortSecurity: []string{addrStr},
					ExternalIDs: map[string]string{
						"spinifex:eni_id":    rec.NetworkInterfaceId,
						"spinifex:subnet_id": rec.SubnetId,
						"spinifex:vpc_id":    rec.VpcId,
					},
				}

				// Attach DHCP options if available
				dhcpOpts, dhcpErr := topo.ovn.FindDHCPOptionsByExternalID(ctx, "spinifex:subnet_id", rec.SubnetId)
				if dhcpErr == nil {
					lsp.DHCPv4Options = &dhcpOpts.UUID
				}

				slog.Info("vpcd reconcile-kv: creating ENI port", "port", portName, "switch", switchName)
				if err := topo.ovn.CreateLogicalSwitchPort(ctx, switchName, lsp); err != nil {
					slog.Error("vpcd reconcile-kv: failed to create ENI port", "port", portName, "err", err)
				} else {
					result.PortsCreated++
				}
			}
		}
	}

	// 5. Reconcile EIP NAT rules: ensure each associated EIP has a dnat_and_snat
	// rule in OVN NB DB. NAT rules are only created by AssociateAddress events
	// and are never otherwise reconciled — an OVN NB DB wipe or any state drift
	// silently breaks inbound EIP traffic. This step is the safety net.
	eipKV, err := js.KeyValue(handlers_ec2_eip.KVBucketEIPs)
	if err != nil {
		slog.Debug("vpcd reconcile-kv: EIP KV bucket not available", "err", err)
	} else {
		eipKeys, err := eipKV.Keys()
		if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
			slog.Warn("vpcd reconcile-kv: failed to list EIP keys", "err", err)
		}
		for _, key := range eipKeys {
			if key == utils.VersionKey {
				continue
			}
			entry, err := eipKV.Get(key)
			if err != nil {
				continue
			}
			var rec handlers_ec2_eip.EIPRecord
			if err := json.Unmarshal(entry.Value(), &rec); err != nil {
				slog.Warn("vpcd reconcile-kv: failed to unmarshal EIP record", "key", key, "err", err)
				continue
			}
			if rec.State != "associated" || rec.VpcId == "" || rec.PublicIp == "" || rec.PrivateIp == "" {
				continue
			}

			existing, err := topo.ovn.FindNATByExternalIP(ctx, "dnat_and_snat", rec.PublicIp)
			if err != nil {
				slog.Warn("vpcd reconcile-kv: failed to query NAT rule", "external_ip", rec.PublicIp, "err", err)
				continue
			}
			if existing != nil && existing.LogicalIP == rec.PrivateIp {
				slog.Debug("vpcd reconcile-kv: NAT rule correct, skipping", "external_ip", rec.PublicIp)
				continue
			}

			routerName := "vpc-" + rec.VpcId
			natRule := &nbdb.NAT{
				Type:       "dnat_and_snat",
				ExternalIP: rec.PublicIp,
				LogicalIP:  rec.PrivateIp,
				ExternalIDs: map[string]string{
					"spinifex:vpc_id":    rec.VpcId,
					"spinifex:public_ip": rec.PublicIp,
				},
			}
			if !topo.useCentralizedNAT() && rec.ENIId != "" {
				portName := "port-" + rec.ENIId
				natRule.LogicalPort = &portName
				if mac, ok := eniMAC[rec.ENIId]; ok && mac != "" {
					natRule.ExternalMAC = &mac
				}
			}

			if existing != nil {
				if removed, err := topo.ovn.DeleteAllNATsByExternalIP(ctx, "dnat_and_snat", rec.PublicIp); err != nil {
					slog.Warn("vpcd reconcile-kv: failed to remove stale NAT rule", "external_ip", rec.PublicIp, "err", err)
					continue
				} else if removed > 0 {
					slog.Info("vpcd reconcile-kv: removed stale NAT rule before re-add", "external_ip", rec.PublicIp, "removed", removed)
				}
			}

			if err := topo.ovn.AddNAT(ctx, routerName, natRule); err != nil {
				slog.Error("vpcd reconcile-kv: failed to add NAT rule", "external_ip", rec.PublicIp, "logical_ip", rec.PrivateIp, "err", err)
				continue
			}
			slog.Info("vpcd reconcile-kv: reconciled NAT rule",
				"external_ip", rec.PublicIp, "logical_ip", rec.PrivateIp, "router", routerName)
			result.NATsReconciled++
		}
	}

	slog.Info("vpcd reconcile-kv: complete",
		"routers_created", result.RoutersCreated,
		"switches_created", result.SwitchesCreated,
		"igws_created", result.IGWsCreated,
		"ports_created", result.PortsCreated,
		"nats_reconciled", result.NATsReconciled,
	)

	return result
}
