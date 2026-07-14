package daemon

import (
	"github.com/aws/aws-sdk-go/aws"
	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// dnsDesiredSet builds the full desired managed-record set for the reconcile
// backstop. It spans all tenants: every running instance on this
// node plus every active load balancer and EKS cluster across all accounts.
// Prune authority is granted per record class only when that class enumerated
// completely, so a transient store error can never delete another tenant's live
// records — the reconcile only ever repairs, never over-prunes, on a partial view.
func (d *Daemon) dnsDesiredSet() handlers_dns.DesiredSet {
	ds := handlers_dns.DesiredSet{}
	ds.Changes = append(ds.Changes, d.desiredEC2DNSChanges()...)

	if d.elbv2Service != nil {
		if ch, ok := d.elbv2Service.DesiredDNSChanges(); ok {
			ds.Changes = append(ds.Changes, ch...)
			ds.Prunable.ELB = true
		}
	}
	if d.eksService != nil {
		if ch, ok := d.eksService.DesiredDNSChanges(); ok {
			ds.Changes = append(ds.Changes, ch...)
			ds.Prunable.EKS = true
		}
	}
	return ds
}

// desiredEC2DNSChanges returns UPSERTs for this node's running instances. EC2
// records are node-local — vmMgr holds only this node's VMs — so they are never
// pruned by the reconcile; the terminate hook owns EC2 record removal. The
// domains mirror the lifecycle publish so re-asserting is a no-op when in sync.
func (d *Daemon) desiredEC2DNSChanges() []handlers_dns.Change {
	var changes []handlers_dns.Change
	d.vmMgr.View(func(vms map[string]*vm.VM) {
		for _, v := range vms {
			if v == nil || v.Status != vm.StateRunning {
				continue
			}
			privateIP := ""
			if v.Instance != nil {
				privateIP = aws.StringValue(v.Instance.PrivateIpAddress)
			}
			if v.PublicIP == "" && privateIP == "" {
				continue
			}
			changes = append(changes, handlers_dns.EC2Changes(
				handlers_dns.ActionUpsert, d.config.Region,
				d.dnsBaseDomain, d.dnsInternalDomain, v.PublicIP, privateIP,
			)...)
		}
	})
	return changes
}
