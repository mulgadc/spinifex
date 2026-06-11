package handlers_ec2_key

// The IMDS public-key RPC rides the internal, ACL-gated subject
// "imds.ec2.get_public_key" — the daemon-side key service (which holds the
// Predastore credentials) answers it; vpcd's IMDS handler calls it to fetch an
// instance's launch SSH public key. The subject is hardcoded at both ends
// (daemon subscription table + nats_clients.go), never reachable by a guest.

// GetPublicKeyRequest is the imds.ec2.get_public_key payload. The account ID is
// data here (resolved from the requesting ENI), not an authenticated caller
// identity.
type GetPublicKeyRequest struct {
	AccountID string `json:"account_id"`
	KeyName   string `json:"key_name"`
}

// GetPublicKeyResponse carries the trimmed OpenSSH public key line
// ("ssh-… AAAA… <comment>"); the comment is freeform and may be empty.
type GetPublicKeyResponse struct {
	OpenSSHKey string `json:"openssh_key"`
}
