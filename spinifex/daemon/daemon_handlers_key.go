package daemon

import (
	handlers_ec2_key "github.com/mulgadc/spinifex/spinifex/handlers/ec2/key"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

func (d *Daemon) handleEC2CreateKeyPair(msg *nats.Msg) {
	handleNATSRequest(msg, d.keyService.CreateKeyPair)
}

func (d *Daemon) handleEC2DeleteKeyPair(msg *nats.Msg) {
	handleNATSRequest(msg, d.keyService.DeleteKeyPair)
}

func (d *Daemon) handleEC2DescribeKeyPairs(msg *nats.Msg) {
	handleNATSRequest(msg, d.keyService.DescribeKeyPairs)
}

func (d *Daemon) handleEC2ImportKeyPair(msg *nats.Msg) {
	handleNATSRequest(msg, d.keyService.ImportKeyPair)
}

// handleIMDSGetPublicKey answers the IMDS material-path RPC: it fetches an
// instance's launch SSH public key from the key store and replies with the
// trimmed OpenSSH line. The daemon holds the key service (and its Predastore
// credentials), so the responder lives here rather than in vpcd's IMDS handler,
// which carries only a nats.Conn.
func (d *Daemon) handleIMDSGetPublicKey(msg *nats.Msg) {
	utils.ServeNATSRequest(msg, func(req *handlers_ec2_key.GetPublicKeyRequest) (*handlers_ec2_key.GetPublicKeyResponse, error) {
		material, err := d.keyService.GetPublicKeyMaterial(req.AccountID, req.KeyName)
		if err != nil {
			return nil, err
		}
		return &handlers_ec2_key.GetPublicKeyResponse{OpenSSHKey: material}, nil
	})
}
