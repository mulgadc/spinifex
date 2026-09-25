package daemon

import (
	"encoding/json"
	"log/slog"

	"github.com/nats-io/nats.go"

	handlers_ec2_eip "github.com/mulgadc/spinifex/spinifex/handlers/ec2/eip"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// handleENIPublicIPChanged applies an EIP association to the instance record on
// whichever node is running it. AssociateAddress is served by a queue-group
// worker that is rarely that node, so without this the address was written to
// the EIP and ENI records while the instance kept reporting whatever launch
// assigned it — the stale address survived the association and every describe.
//
// Every node receives this and all but one ignore it: UpdateState reports the
// instance absent, which is the normal case, not a failure.
func (d *Daemon) handleENIPublicIPChanged(msg *nats.Msg) string {
	var evt handlers_ec2_eip.ENIPublicIPChanged
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		slog.Error("public-IP change: bad event", "err", err)
		return outcomeError
	}
	if evt.InstanceID == "" {
		return outcomeSuccess
	}

	found, err := d.vmMgr.UpdateAndPersist(evt.InstanceID, func(v *vm.VM) bool {
		if v.PublicIP == evt.PublicIP {
			return false
		}
		v.PublicIP = evt.PublicIP
		v.PublicIPPool = evt.PoolName
		return true
	})
	if err != nil {
		slog.Error("public-IP change: failed to persist the new address",
			"instanceId", evt.InstanceID, "publicIp", evt.PublicIP, "err", err)
		return outcomeError
	}
	if found {
		slog.Info("public-IP change applied to a local instance",
			"instanceId", evt.InstanceID, "eniId", evt.ENIID, "publicIp", evt.PublicIP)
	}
	return outcomeSuccess
}
