package acm

import "path/filepath"

// tenantCACertFilename and tenantCAKeyFilename name the ACM tenant private
// CA's files on disk. Colocated with the platform CA (ca.pem/ca.key) and
// master.key under the same config directory, but a distinct "tenant-ca"
// basename keeps the two roots from ever being confused: they are
// independent, and the platform CA (trusted by every node, ipsec, EKS/ECS
// guests, ALB microvms and baked catalog images) must never be read, written
// or reused as the tenant CA, or vice versa.
const (
	tenantCACertFilename = "tenant-ca.pem"
	tenantCAKeyFilename  = "tenant-ca.key"
)

// TenantCACertPath and TenantCAKeyPath return the on-disk paths for the ACM
// tenant private CA under configDir, mirroring how admin.GenerateCertificatesIfNeeded
// derives the platform CA's paths (filepath.Join(configDir, "ca.pem"/"ca.key"))
// rather than threading them through ConfigSettings: like the platform CA,
// these paths are computed on demand by whoever needs them (the tenant-CA admin
// command, the daemon at startup), not rendered into a config file.
func TenantCACertPath(configDir string) string {
	return filepath.Join(configDir, tenantCACertFilename)
}

// TenantCAKeyPath returns the tenant private CA key path under configDir; see
// TenantCACertPath.
func TenantCAKeyPath(configDir string) string {
	return filepath.Join(configDir, tenantCAKeyFilename)
}
