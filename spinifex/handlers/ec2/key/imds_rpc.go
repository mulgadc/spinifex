package handlers_ec2_key

// GetPublicKeyRequest is the GetPublicKeyMaterial payload. The account ID is
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
