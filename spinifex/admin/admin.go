package admin

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	toml "github.com/pelletier/go-toml/v2"
	"gopkg.in/ini.v1"
)

// RemoteNode holds basic info about a remote cluster node for config generation.
type RemoteNode struct {
	Name     string
	Host     string
	Region   string
	AZ       string
	Services []string
}

type ConfigSettings struct {
	AccessKey string
	SecretKey string
	AccountID string
	Region    string
	NatsToken string
	DataDir   string
	LogDir    string
	ConfigDir string

	Node   string
	Az     string
	Port   string
	BindIP string
	// AdvertiseIP is the off-host dial target rendered into [nodes.X].advertise.
	// Empty → callers fall back to BindIP.
	AdvertiseIP string

	// Cluster settings
	ClusterBindIP string
	ClusterRoutes []string
	ClusterName   string

	// Predastore multi-node
	PredastoreNodeID int

	// Node capabilities
	Services []string

	// OVN Northbound DB address — "tcp:127.0.0.1:6641" on the primary node,
	// "tcp:<primary-mgmt-ip>:6641" on joining nodes.
	OVNNBAddr string
	OVNSBAddr string

	// External networking for public subnets
	ExternalMode   string   // "pool" or "" (disabled)
	ExternalIface  string   // WAN NIC name (e.g., "eth0", "eth1")
	PoolName       string   // External pool name (e.g., "wan")
	PoolSource     string   // IP source: "static" or "dhcp"
	PoolBindBridge string   // Linux bridge for upstream DORA (source=dhcp only)
	PoolStart      string   // First IP in external pool range (static only)
	PoolEnd        string   // Last IP in external pool range (static only)
	PoolGateway    string   // WAN gateway IP
	PoolGatewayIP  string   // Explicit SNAT IP (overrides default of first IP in range)
	PoolPrefixLen  int      // Subnet prefix length (default 24)
	PoolDNSServers []string // DNS servers for VM DHCP (auto-detected from host)

	// OperatorEmail is the address collected at install time. Written under [operator]
	// in spinifex.toml so it survives wipes. Empty means no identity was supplied.
	OperatorEmail string

	// Other nodes in the cluster (for config source of truth)
	RemoteNodes []RemoteNode

	// Bootstrap: pre-generated default VPC IDs for vpcd reconciliation.
	// Written by admin init so [bootstrap] exists before services start.
	BootstrapAccountId  string
	BootstrapVpcId      string
	BootstrapSubnetId   string
	BootstrapIgwId      string
	BootstrapCidr       string
	BootstrapSubnetCidr string

	// GPUPassthrough enables VFIO GPU passthrough in the daemon config.
	// Sets gpu_passthrough = true under [nodes.<node>.daemon].
	GPUPassthrough bool

	// IPSecEnabled toggles cluster-wide OVN native IPsec on intra-AZ Geneve.
	// Written under [network] in spinifex.toml; daemon reads it via cluster config.
	IPSecEnabled bool

	// EncryptionKeyFile is the path to the cluster-wide viperblock at-rest
	// encryption key, rendered into [nodes.X.viperblock].encryption_key_file.
	// Empty means no key was provisioned and volumes are written cleartext
	// (legacy mode); the template omits the field entirely in that case.
	EncryptionKeyFile string

	// Northstar (DNS) settings. The northstar service reads zones from a
	// dedicated, read-only S3 bucket using bucket-scoped credentials rendered
	// into predastore.toml ([[auth]]) and northstar.toml ([s3]).
	NorthstarAccessKey     string
	NorthstarSecretKey     string
	NorthstarBucket        string // S3 bucket holding zone files (default "northstar")
	NorthstarDefaultDomain string // authoritative base domain (default "spinifex.local")
	NorthstarConfigPath    string // path to northstar.toml, rendered into spinifex.toml
}

// PredastoreNodeConfig describes a single Predastore node for multi-node config generation.
type PredastoreNodeConfig struct {
	ID   int
	Host string
}

type ConfigFile struct {
	Name     string
	Path     string
	Template string
}

func GenerateConfigFiles(configs []ConfigFile, configSettings ConfigSettings) error {
	for _, cfg := range configs {
		if err := GenerateConfigFile(cfg.Path, cfg.Template, configSettings); err != nil {
			return fmt.Errorf("error creating %s: %v", cfg.Name, err)
		}
		fmt.Printf("✅ Created: %s\n", cfg.Name)
	}

	return nil
}

// GenerateConfigFile creates a configuration file from a template.
func GenerateConfigFile(configPath string, configTemplate string, configSettings ConfigSettings) error {
	tmpl, err := template.New("config").Parse(configTemplate)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	f, err := os.OpenFile(configPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to create config file: %w", err)
	}
	defer f.Close()

	if err := tmpl.Execute(f, configSettings); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	return nil
}

func GenerateCertificatesIfNeeded(configDir string, force bool, bindIP string) (caCertPath string) {
	caCertPath = filepath.Join(configDir, "ca.pem")
	caKeyPath := filepath.Join(configDir, "ca.key")
	serverCertPath := filepath.Join(configDir, "server.pem")
	serverKeyPath := filepath.Join(configDir, "server.key")

	// Check if we need to generate certificates
	needsGeneration := force ||
		!FileExists(caCertPath) || !FileExists(caKeyPath) ||
		!FileExists(serverCertPath) || !FileExists(serverKeyPath)

	if needsGeneration {
		fmt.Println("\n🔐 Generating Certificate Authority and SSL certificates...")

		if err := GenerateCACert(caCertPath, caKeyPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error generating CA certificate: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ CA certificate generated:\n")
		fmt.Printf("   CA Certificate: %s\n", caCertPath)
		fmt.Printf("   CA Key: %s\n", caKeyPath)

		if err := GenerateSignedCert(serverCertPath, serverKeyPath, caCertPath, caKeyPath, bindIP); err != nil {
			fmt.Fprintf(os.Stderr, "Error generating server certificate: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Server certificate generated (signed by CA):\n")
		fmt.Printf("   Certificate: %s\n", serverCertPath)
		fmt.Printf("   Key: %s\n", serverKeyPath)

		// Print manual instructions only when not root (root gets auto-install)
		if os.Getuid() != 0 {
			fmt.Println("\n📋 To trust the Spinifex CA system-wide (recommended):")
			fmt.Printf("   sudo cp %s /usr/local/share/ca-certificates/spinifex-ca.crt\n", caCertPath)
			fmt.Println("   sudo update-ca-certificates")
			fmt.Println("\n   This allows AWS CLI and other tools to trust Spinifex services automatically.")
		}
	} else {
		fmt.Println("\n✅ CA and SSL certificates already exist")
	}

	return caCertPath
}

// GenerateServerCertOnly generates a server certificate signed by an existing CA.
// Used by joining nodes that receive the CA from the leader.
func GenerateServerCertOnly(configDir string, bindIP string) error {
	caCertPath := filepath.Join(configDir, "ca.pem")
	caKeyPath := filepath.Join(configDir, "ca.key")
	serverCertPath := filepath.Join(configDir, "server.pem")
	serverKeyPath := filepath.Join(configDir, "server.key")

	if !FileExists(caCertPath) || !FileExists(caKeyPath) {
		return fmt.Errorf("CA files not found in %s", configDir)
	}

	return GenerateSignedCert(serverCertPath, serverKeyPath, caCertPath, caKeyPath, bindIP)
}

func CreateServiceDirectories(spxRoot string) {
	dirs := []string{
		filepath.Join(spxRoot, "images"),
		filepath.Join(spxRoot, "amis"),
		filepath.Join(spxRoot, "volumes"),
		filepath.Join(spxRoot, "state"),
		filepath.Join(spxRoot, "logs"),
		filepath.Join(spxRoot, "nats"),
		filepath.Join(spxRoot, "predastore"),
		filepath.Join(spxRoot, "viperblock"),
		filepath.Join(spxRoot, "vpcd"),
		filepath.Join(spxRoot, "spinifex"),
		filepath.Join(spxRoot, "awsgw"),
	}

	fmt.Println("\n📁 Creating directory structure...")
	for _, dir := range dirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			if err := os.MkdirAll(dir, 0750); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: Could not create %s: %v\n", dir, err)
			}
		}
	}
	fmt.Printf("✅ Directory structure created in %s\n", spxRoot)
}

func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ChownRecursive changes ownership of path and its contents to username.
// Best-effort: errors are logged but do not halt the operation.
func ChownRecursive(path, username string) {
	u, err := user.Lookup(username)
	if err != nil {
		slog.Warn("ChownRecursive: user lookup failed, skipping", "user", username, "path", path, "err", err)
		return
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		slog.Warn("ChownRecursive: invalid UID, skipping", "user", username, "uid", u.Uid, "err", err)
		return
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		slog.Warn("ChownRecursive: invalid GID, skipping", "user", username, "gid", u.Gid, "err", err)
		return
	}

	// Use os.Root to scope filesystem operations and avoid symlink TOCTOU races.
	// Falls back to direct chown if os.Root is not available.
	root, rootErr := os.OpenRoot(path)
	if rootErr != nil {
		// Fallback: direct chown on the top-level path only — subdirectory contents
		// will retain their original ownership.
		slog.Warn("ChownRecursive: OpenRoot not available, only top-level directory ownership changed",
			"path", path, "err", rootErr)
		if chownErr := os.Lchown(path, uid, gid); chownErr != nil {
			slog.Warn("chown failed", "path", path, "err", chownErr)
		}
		return
	}
	defer root.Close()

	_ = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			slog.Debug("chown walk: skipping inaccessible entry", "path", p, "err", err)
			return nil // best-effort: continue past inaccessible entries
		}
		fullPath := filepath.Join(path, p)
		if chownErr := os.Lchown(fullPath, uid, gid); chownErr != nil {
			slog.Debug("chown failed", "path", fullPath, "err", chownErr)
		}
		return nil
	})
}

// SetServiceOwnership sets per-service ownership on data/config directories
// and shared config files to root:spinifex with correct modes.
func SetServiceOwnership() {
	grp, err := user.LookupGroup("spinifex")
	if err != nil {
		slog.Error("SetServiceOwnership: spinifex group not found, skipping all ownership changes", "err", err)
		fmt.Fprintln(os.Stderr, "WARNING: spinifex group not found — service ownership not set. Run setup.sh first or create the group manually.")
		return
	}
	gid, err := strconv.Atoi(grp.Gid)
	if err != nil {
		slog.Error("SetServiceOwnership: invalid spinifex group GID", "gid", grp.Gid, "err", err)
		fmt.Fprintln(os.Stderr, "WARNING: invalid spinifex group GID — service ownership not set.")
		return
	}

	// Per-service directory trees
	for path, u := range map[string]string{
		"/etc/spinifex/nats":           "spinifex-nats",
		"/var/lib/spinifex/nats":       "spinifex-nats",
		"/etc/spinifex/predastore":     "spinifex-storage",
		"/var/lib/spinifex/predastore": "spinifex-storage",
		"/etc/spinifex/viperblock":     "spinifex-viperblock",
		"/var/lib/spinifex/spinifex":   "spinifex-daemon",
		"/var/lib/spinifex/viperblock": "spinifex-viperblock",
		"/var/lib/spinifex/vpcd":       "spinifex-vpcd",
		"/var/lib/spinifex/awsgw":      "spinifex-gw",
	} {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		ChownRecursive(path, u)
	}

	// Shared data directories — root:spinifex 0770 so daemon + admin CLI can write
	for _, dir := range []string{
		"/var/lib/spinifex/images",
		"/var/lib/spinifex/amis",
		"/var/lib/spinifex/volumes",
		"/var/lib/spinifex/state",
		"/run/spinifex/nbd",
	} {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		if err := os.Lchown(dir, 0, gid); err != nil {
			slog.Warn("SetServiceOwnership: chown failed", "path", dir, "err", err)
		}
		if err := os.Chmod(dir, 0770); err != nil { //nolint:gosec // directories need group-write for daemon + admin CLI
			slog.Warn("SetServiceOwnership: chmod failed", "path", dir, "err", err)
		}
	}

	// Shared config files — root:spinifex, ca.key stays root:root 0600
	// bootstrap.json lives in the awsgw data dir (not /etc/spinifex),
	// so /etc/spinifex stays at 0750 (no group-write needed).
	for path, mode := range map[string]os.FileMode{
		"/etc/spinifex/spinifex.toml":             0640,
		"/etc/spinifex/master.key":                0640,
		"/etc/spinifex/viperblock/encryption.key": 0640,
		"/etc/spinifex/server.pem":                0644,
		"/etc/spinifex/server.key":                0640,
		"/etc/spinifex/ca.pem":                    0644,
	} {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := os.Lchown(path, 0, gid); err != nil {
			slog.Warn("SetServiceOwnership: chown failed", "path", path, "err", err)
		}
		if err := os.Chmod(path, mode); err != nil {
			slog.Warn("SetServiceOwnership: chmod failed", "path", path, "err", err)
		}
	}
}

// UpdateAWSINIFile updates or creates an AWS INI file section with the given key-value pairs.
func UpdateAWSINIFile(path, section string, values map[string]string) error {
	var cfg *ini.File
	var err error

	if FileExists(path) {
		cfg, err = ini.Load(path)
		if err != nil {
			return fmt.Errorf("failed to load INI file: %w", err)
		}
	} else {
		cfg = ini.Empty()
	}

	sec, err := cfg.NewSection(section)
	if err != nil {
		// Section already exists, get it.
		sec, err = cfg.GetSection(section)
		if err != nil {
			return fmt.Errorf("failed to get section: %w", err)
		}
	}

	for key, value := range values {
		sec.Key(key).SetValue(value)
	}

	// Write atomically: ini.SaveTo uses os.Create (world-readable); render to a
	// sibling temp file (0600) and rename to avoid briefly exposing secrets.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".aws-ini-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp INI file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := cfg.WriteTo(tmp); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to write INI file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close INI file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("failed to rename INI file into place: %w", err)
	}
	return nil
}

// GenerateAWSAccessKey generates an AWS-style access key (AKIA + 16 random alphanumeric chars).
func GenerateAWSAccessKey() (string, error) {
	const prefix = "AKIA"
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const length = 16

	result := make([]byte, length)
	for i := range result {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", fmt.Errorf("crypto/rand failure: %w", err)
		}
		result[i] = charset[num.Int64()]
	}

	return prefix + string(result), nil
}

// GenerateAWSSecretKey generates a 40-character base64-encoded AWS-style secret key.
func GenerateAWSSecretKey() (string, error) {
	bytes := make([]byte, 30) // 30 bytes = 40 chars in base64
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("crypto/rand failure: %w", err)
	}
	return base64.StdEncoding.EncodeToString(bytes), nil
}

// Northstar DNS defaults baked into config files at init time.
const (
	// NorthstarBucketName is the S3 bucket holding DNS zone files.
	NorthstarBucketName = "northstar"
	// NorthstarDefaultDomain is the authoritative base domain for internal names.
	NorthstarDefaultDomain = "spinifex.local"
)

// SystemAccountID returns the system/root account ID (000000000000).
// Used for service-to-service auth credentials baked into config files.
func SystemAccountID() string {
	return "000000000000"
}

// DefaultAccountID returns the default admin account ID (000000000001).
// This is the first human-facing account created during bootstrap.
func DefaultAccountID() string {
	return "000000000001"
}

// DefaultAccountName returns the default admin account name ("spinifex").
func DefaultAccountName() string {
	return "spinifex"
}

// GenerateNATSToken generates a secure random token for NATS
func GenerateNATSToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("crypto/rand failure: %w", err)
	}
	return "nats_" + base64.URLEncoding.EncodeToString(bytes)[:32], nil
}

// GenerateCACert generates a Certificate Authority certificate and key.
func GenerateCACert(caCertPath, caKeyPath string) error {
	caPrivateKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return fmt.Errorf("failed to generate CA private key: %w", err)
	}

	notBefore := time.Now()
	notAfter := notBefore.Add(3650 * 24 * time.Hour) // 10 years

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("failed to generate serial number: %w", err)
	}

	caTemplate := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   "Spinifex Local CA",
			Organization: []string{"Spinifex Platform"},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}

	caDerBytes, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caPrivateKey.PublicKey, caPrivateKey)
	if err != nil {
		return fmt.Errorf("failed to create CA certificate: %w", err)
	}

	caCertOut, err := os.Create(caCertPath)
	if err != nil {
		return fmt.Errorf("failed to create CA cert file: %w", err)
	}
	defer caCertOut.Close()

	if err := pem.Encode(caCertOut, &pem.Block{Type: "CERTIFICATE", Bytes: caDerBytes}); err != nil {
		return fmt.Errorf("failed to write CA cert: %w", err)
	}

	caKeyOut, err := os.OpenFile(caKeyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to create CA key file: %w", err)
	}
	defer caKeyOut.Close()

	caPrivBytes, err := x509.MarshalPKCS8PrivateKey(caPrivateKey)
	if err != nil {
		return fmt.Errorf("failed to marshal CA private key: %w", err)
	}

	if err := pem.Encode(caKeyOut, &pem.Block{Type: "PRIVATE KEY", Bytes: caPrivBytes}); err != nil {
		return fmt.Errorf("failed to write CA key: %w", err)
	}

	return nil
}

// DiscoverLocalIPs enumerates all non-loopback network interface addresses on
// this machine and returns them as strings. Link-local addresses are excluded.
func DiscoverLocalIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		slog.Warn("failed to enumerate network interfaces", "error", err)
		return nil
	}

	seen := make(map[string]struct{})
	var ips []string

	for _, iface := range ifaces {
		// Skip loopback and down interfaces.
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			slog.Debug("failed to get addresses for interface", "iface", iface.Name, "error", err)
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}
			s := ip.String()
			if _, ok := seen[s]; !ok {
				seen[s] = struct{}{}
				ips = append(ips, s)
			}
		}
	}
	return ips
}

// DiscoverHostname returns the system hostname suitable for use as a DNS SAN.
// Returns an empty string if the hostname cannot be determined or is "localhost".
func DiscoverHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		slog.Warn("failed to determine system hostname", "error", err)
		return ""
	}
	if hostname == "" || hostname == "localhost" {
		return ""
	}
	return hostname
}

// GenerateSignedCert generates a server certificate signed by the CA.
// extraIPs are additional IP addresses to include in the certificate's SANs.
// All non-loopback interface IPs on the local machine are automatically included.
func GenerateSignedCert(certPath, keyPath, caCertPath, caKeyPath string, extraIPs ...string) error {
	return GenerateSignedCertWithDNS(certPath, keyPath, caCertPath, caKeyPath, extraIPs, nil)
}

// GenerateSignedCertWithDNS generates a server certificate signed by the CA.
// All non-loopback interface IPs and the machine hostname are automatically
// included. extraIPs and extraDNS allow adding additional SANs.
func GenerateSignedCertWithDNS(certPath, keyPath, caCertPath, caKeyPath string, extraIPs, extraDNS []string) error {
	caCertPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return fmt.Errorf("failed to read CA cert: %w", err)
	}
	caCertBlock, _ := pem.Decode(caCertPEM)
	if caCertBlock == nil {
		return fmt.Errorf("failed to decode CA cert PEM")
	}
	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse CA cert: %w", err)
	}

	caKeyPEM, err := os.ReadFile(caKeyPath)
	if err != nil {
		return fmt.Errorf("failed to read CA key: %w", err)
	}
	caKeyBlock, _ := pem.Decode(caKeyPEM)
	if caKeyBlock == nil {
		return fmt.Errorf("failed to decode CA key PEM")
	}
	caPrivateKey, err := x509.ParsePKCS8PrivateKey(caKeyBlock.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse CA private key: %w", err)
	}
	caRSAKey, ok := caPrivateKey.(*rsa.PrivateKey)
	if !ok {
		return fmt.Errorf("CA key is not RSA")
	}

	serverPrivateKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return fmt.Errorf("failed to generate server private key: %w", err)
	}

	notBefore := time.Now()
	notAfter := notBefore.Add(365 * 24 * time.Hour) // 1 year for server certs

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("failed to generate serial number: %w", err)
	}

	// Build IP list: localhost IPs + auto-discovered interface IPs + explicit extras.
	seen := map[string]struct{}{"127.0.0.1": {}, "::1": {}}
	ipAddresses := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	addIP := func(s string) {
		if s == "" || s == "0.0.0.0" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		if parsed := net.ParseIP(s); parsed != nil {
			seen[s] = struct{}{}
			ipAddresses = append(ipAddresses, parsed)
		}
	}
	for _, ip := range DiscoverLocalIPs() {
		addIP(ip)
	}
	for _, ip := range extraIPs {
		addIP(ip)
	}

	// Build DNS names: always include localhost, plus hostname if available.
	dnsSeen := map[string]struct{}{"localhost": {}}
	dnsNames := []string{"localhost"}
	if hostname := DiscoverHostname(); hostname != "" {
		if _, ok := dnsSeen[hostname]; !ok {
			dnsSeen[hostname] = struct{}{}
			dnsNames = append(dnsNames, hostname)
		}
	}
	for _, dns := range extraDNS {
		if dns == "" {
			continue
		}
		if _, ok := dnsSeen[dns]; !ok {
			dnsSeen[dns] = struct{}{}
			dnsNames = append(dnsNames, dns)
		}
	}

	serverTemplate := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   "Spinifex Server",
			Organization: []string{"Spinifex Platform"},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ipAddresses,
	}

	serverDerBytes, err := x509.CreateCertificate(rand.Reader, &serverTemplate, caCert, &serverPrivateKey.PublicKey, caRSAKey)
	if err != nil {
		return fmt.Errorf("failed to create server certificate: %w", err)
	}

	certOut, err := os.Create(certPath)
	if err != nil {
		return fmt.Errorf("failed to create cert file: %w", err)
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: serverDerBytes}); err != nil {
		return fmt.Errorf("failed to write cert: %w", err)
	}

	keyOut, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to create key file: %w", err)
	}
	defer keyOut.Close()
	privBytes, err := x509.MarshalPKCS8PrivateKey(serverPrivateKey)
	if err != nil {
		return fmt.Errorf("failed to marshal private key: %w", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "PRIVATE KEY", Bytes: privBytes}); err != nil {
		return fmt.Errorf("failed to write key: %w", err)
	}

	return nil
}

// GenerateSelfSignedCert generates a self-signed SSL certificate (legacy, kept for compatibility).
func GenerateSelfSignedCert(certPath, keyPath string) error {
	privateKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return fmt.Errorf("failed to generate private key: %w", err)
	}

	notBefore := time.Now()
	notAfter := notBefore.Add(3650 * 24 * time.Hour) // 10 years

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("failed to generate serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   "localhost",
			Organization: []string{"Spinifex Platform"},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return fmt.Errorf("failed to create certificate: %w", err)
	}

	certOut, err := os.Create(certPath)
	if err != nil {
		return fmt.Errorf("failed to create cert file: %w", err)
	}
	defer certOut.Close()

	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return fmt.Errorf("failed to write cert: %w", err)
	}

	keyOut, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to create key file: %w", err)
	}
	defer keyOut.Close()

	privBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("failed to marshal private key: %w", err)
	}

	if err := pem.Encode(keyOut, &pem.Block{Type: "PRIVATE KEY", Bytes: privBytes}); err != nil {
		return fmt.Errorf("failed to write key: %w", err)
	}

	return nil
}

// SetupAWSCredentials updates ~/.aws/credentials and ~/.aws/config.
// When running under sudo, writes to SUDO_USER's home instead of root's.
func SetupAWSCredentials(accessKey, secretKey, region, certPath, bindIP, wanIP string) error {
	_ = wanIP
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	sudoUser := os.Getenv("SUDO_USER")
	if os.Getuid() == 0 && sudoUser != "" {
		if u, err := user.Lookup(sudoUser); err == nil {
			homeDir = u.HomeDir
		}
	}

	awsDir := filepath.Join(homeDir, ".aws")
	if err := os.MkdirAll(awsDir, 0700); err != nil {
		return err
	}

	credPath := filepath.Join(awsDir, "credentials")
	configPath := filepath.Join(awsDir, "config")

	profileName := "spinifex"

	if err := UpdateAWSINIFile(credPath, profileName, map[string]string{
		"aws_access_key_id":     accessKey,
		"aws_secret_access_key": secretKey,
	}); err != nil {
		return err
	}

	configSection := profileName
	if profileName != "default" {
		configSection = "profile " + profileName
	}

	endpointHost := bindIP
	if endpointHost == "" || endpointHost == "0.0.0.0" {
		endpointHost = "localhost"
	}

	if err := UpdateAWSINIFile(configPath, configSection, map[string]string{
		"region":       region,
		"endpoint_url": "https://" + net.JoinHostPort(endpointHost, "9999"),
		"ca_bundle":    certPath,
		"output":       "json",
	}); err != nil {
		return err
	}

	// Fix ownership so the sudo invoking user can read the files
	if os.Getuid() == 0 && sudoUser != "" {
		ChownRecursive(awsDir, sudoUser)
	}

	fmt.Printf("   Profile: %s\n", profileName)
	if profileName != "default" {
		fmt.Printf("   Use: export AWS_PROFILE=%s\n", profileName)
	}

	return nil
}

// GenerateMultiNodePredastoreConfig produces a complete predastore.toml for a
// multi-node Predastore cluster. Each node gets its own DB entry (port 6660)
// and shard entry (port 9991) on a distinct IP. Node ID 1 is the bootstrap leader.
func GenerateMultiNodePredastoreConfig(templateStr string, nodes []PredastoreNodeConfig, accessKey, secretKey, region, natsToken, configDir, bindIP string) (string, error) {
	if len(nodes) < 2 {
		return "", fmt.Errorf("multi-node predastore requires at least 2 nodes, got %d", len(nodes))
	}

	data := struct {
		Nodes     []PredastoreNodeConfig
		AccessKey string
		SecretKey string
		Region    string
		NatsToken string
		ConfigDir string
		BindIP    string
		// Northstar provisioning is single-node only for V1; these stay empty
		// in the multi-node path so the template omits the northstar stanzas.
		NorthstarAccessKey string
		NorthstarSecretKey string
		NorthstarBucket    string
	}{Nodes: nodes, AccessKey: accessKey, SecretKey: secretKey, Region: region, NatsToken: natsToken, ConfigDir: configDir, BindIP: bindIP}

	tmpl, err := template.New("predastore-multinode").Parse(templateStr)
	if err != nil {
		return "", fmt.Errorf("failed to parse predastore template: %w", err)
	}

	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("failed to execute predastore template: %w", err)
	}

	return b.String(), nil
}

// FindNodeIDByIP returns the node ID for the given IP in the node list,
// or 0 if the IP is not found.
func FindNodeIDByIP(nodes []PredastoreNodeConfig, ip string) int {
	for _, n := range nodes {
		if n.Host == ip {
			return n.ID
		}
	}
	return 0
}

// ParsePredastoreNodeIDFromConfig parses a predastore.toml string and returns
// the node ID whose host matches the given IP, or 0 if not found.
func ParsePredastoreNodeIDFromConfig(tomlContent string, ip string) int {
	var cfg struct {
		DB []PredastoreNodeConfig `toml:"db"`
	}
	if err := toml.Unmarshal([]byte(tomlContent), &cfg); err != nil {
		slog.Warn("Failed to parse predastore.toml content", "error", err)
		return 0
	}
	return FindNodeIDByIP(cfg.DB, ip)
}

// SetMIGProfile idempotently writes mig_profile = "<profile>" for the given node
// into spinifex.toml. An empty profile clears the setting.
func SetMIGProfile(tomlPath, node, profile string) error {
	raw, err := os.ReadFile(tomlPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", tomlPath, err)
	}
	text := string(raw)
	quoted := `"` + profile + `"`

	sectionHeader := "[nodes." + node + ".daemon]"
	sectionStart := strings.Index(text, sectionHeader)
	if sectionStart < 0 {
		text = strings.TrimRight(text, "\n") +
			"\n\n[nodes." + node + ".daemon]\nmig_profile = " + quoted + "\n"
		return os.WriteFile(tomlPath, []byte(text), 0640) //nolint:gosec // spinifex.toml is root:spinifex 0640
	}

	bodyStart := sectionStart + len(sectionHeader)
	rest := text[bodyStart:]
	nextSection := strings.Index(rest, "\n[")
	var body, suffix string
	if nextSection < 0 {
		body = rest
	} else {
		body = rest[:nextSection]
		suffix = rest[nextSection:]
	}

	if regexp.MustCompile(`mig_profile\s*=\s*` + regexp.QuoteMeta(quoted)).MatchString(body) {
		return nil
	}

	flipRe := regexp.MustCompile(`mig_profile\s*=\s*"[^"]*"`)
	var newBody string
	if flipRe.MatchString(body) {
		newBody = flipRe.ReplaceAllString(body, "mig_profile = "+quoted)
	} else {
		newBody = "\nmig_profile = " + quoted + body
	}

	text = text[:bodyStart] + newBody + suffix
	return os.WriteFile(tomlPath, []byte(text), 0640) //nolint:gosec // spinifex.toml is root:spinifex 0640
}

// SetGPUPassthrough idempotently writes gpu_passthrough = <enabled> for the
// given node into spinifex.toml, preserving all other content and comments.
// Returns nil without touching the file if the setting is already correct.
func SetGPUPassthrough(tomlPath, node string, enabled bool) error {
	raw, err := os.ReadFile(tomlPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", tomlPath, err)
	}
	text := string(raw)
	value := "false"
	if enabled {
		value = "true"
	}

	sectionHeader := "[nodes." + node + ".daemon]"

	sectionStart := strings.Index(text, sectionHeader)
	if sectionStart < 0 {
		// No daemon section — append one.
		text = strings.TrimRight(text, "\n") +
			"\n\n[nodes." + node + ".daemon]\ngpu_passthrough = " + value + "\n"
		return os.WriteFile(tomlPath, []byte(text), 0640) //nolint:gosec // spinifex.toml is root:spinifex 0640 so the daemon can read it
	}

	// Extract body of just this section (stops at the next section header).
	bodyStart := sectionStart + len(sectionHeader)
	rest := text[bodyStart:]
	nextSection := strings.Index(rest, "\n[")
	var body, suffix string
	if nextSection < 0 {
		body = rest
	} else {
		body = rest[:nextSection]
		suffix = rest[nextSection:]
	}

	// Already correct — no-op.
	if regexp.MustCompile(`gpu_passthrough\s*=\s*` + value).MatchString(body) {
		return nil
	}

	// Key exists with wrong value — flip within this section only.
	flipRe := regexp.MustCompile(`gpu_passthrough\s*=\s*(?:true|false)`)
	var newBody string
	if flipRe.MatchString(body) {
		newBody = flipRe.ReplaceAllString(body, "gpu_passthrough = "+value)
	} else {
		// Key absent — insert right after section header.
		newBody = "\ngpu_passthrough = " + value + body
	}

	text = text[:bodyStart] + newBody + suffix
	return os.WriteFile(tomlPath, []byte(text), 0640) //nolint:gosec // spinifex.toml is root:spinifex 0640 so the daemon can read it
}
