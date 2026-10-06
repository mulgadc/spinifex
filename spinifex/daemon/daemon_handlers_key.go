package daemon

import (
	ec2key "github.com/mulgadc/spinifex/spinifex/domains/ec2/key"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"github.com/nats-io/nats.go"
)

// handleIMDSGetPublicKey answers the IMDS material-path RPC. The responder lives
// here (not in vpcd) because the daemon holds the key service and its Predastore
// credentials; vpcd carries only a nats.Conn.
func (d *Daemon) handleIMDSGetPublicKey(msg *nats.Msg) string {
	return outcomeFor(natsmsg.ServeNATSRequest(msg, func(req *ec2key.GetPublicKeyRequest) (*ec2key.GetPublicKeyResponse, error) {
		material, err := d.keyService.GetPublicKeyMaterial(req.AccountID, req.KeyName)
		if err != nil {
			return nil, err
		}
		return &ec2key.GetPublicKeyResponse{OpenSSHKey: material}, nil
	}))
}
