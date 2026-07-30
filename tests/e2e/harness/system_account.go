//go:build e2e

package harness

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
)

// systemCredentialsFile holds the gateway credentials for the system account,
// written root-owned 0600 at bootstrap.
const systemCredentialsFile = "system-credentials.json"

// The on-disk shape the cluster bootstrap writes.
type systemCredentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

// SystemAWSClient builds an AWSClient authenticated as the system account.
//
// A VM a service owns on a customer's behalf — an RDS DB instance's engine VM,
// an ELBv2 load balancer — is hidden from every caller but the system account,
// so a suite's own tenant credentials describe nothing about it. Every
// assertion about the resources behind a managed service needs this client.
//
// Reading the credential file requires passwordless sudo, and its absence is a
// hard failure rather than a skip: a suite that silently loses its system-side
// assertions still reports green.
func SystemAWSClient(t *testing.T, env *Env) *AWSClient {
	t.Helper()
	if env.ConfigDir == "" {
		t.Fatalf("SystemAWSClient: no config dir located; set SPINIFEX_CONFIG_DIR")
	}
	path := filepath.Join(env.ConfigDir, systemCredentialsFile)
	out, err := exec.Command("sudo", "-n", "cat", path).Output() //nolint:gosec // path is env-derived, not caller input
	if err != nil {
		t.Fatalf("SystemAWSClient: sudo -n cat %s: %v", path, err)
	}
	var creds systemCredentials
	if err := json.Unmarshal(out, &creds); err != nil {
		t.Fatalf("SystemAWSClient: parse %s: %v", path, err)
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		t.Fatalf("SystemAWSClient: %s carries no credentials", path)
	}
	return NewAWSClientWithCreds(t, env, creds.AccessKeyID, creds.SecretAccessKey)
}
