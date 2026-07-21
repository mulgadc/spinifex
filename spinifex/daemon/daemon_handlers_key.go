package daemon

import (
	handlers_ec2_key "github.com/mulgadc/spinifex/spinifex/handlers/ec2/key"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// handleIMDSGetPublicKey answers the IMDS material-path RPC. The responder lives
// here (not in vpcd) because the daemon holds the key service and its Predastore
// credentials; vpcd carries only a nats.Conn.
func (d *Daemon) handleIMDSGetPublicKey(msg *nats.Msg) {
	utils.ServeNATSRequest(msg, func(req *handlers_ec2_key.GetPublicKeyRequest) (*handlers_ec2_key.GetPublicKeyResponse, error) {
		material, err := d.keyService.GetPublicKeyMaterial(req.AccountID, req.KeyName)
		if err != nil {
			return nil, err
		}
		return &handlers_ec2_key.GetPublicKeyResponse{OpenSSHKey: material}, nil
	})
}
