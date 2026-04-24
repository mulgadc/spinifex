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

	// Add more fields as needed
	Node   string
	Az     string
	Port   string
	BindIP string
	// AdvertiseIP is the off-host dial target rendered into the local node's
	// [nodes.X].advertise field. Empty → callers fall back to BindIP.
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
	DhcpBindBridge string   // Bridge where the DHCP AF_PACKET socket binds (Linux bridge in veth mode, OVS bridge in direct mode; never "br-ext")
	ExternalDHCP   bool     // Obtain gateway IP via DHCP on macvlan/bridge
	PoolName       string   // External pool name (e.g., "wan")
	PoolSource     string   // IP source: "static" (default) or "dhcp" (from router DHCP)
	PoolStart      string   // First IP in external pool range (static source only)
	PoolEnd        string   // Last IP in external pool range (static source only)
	PoolGateway    string   // WAN gateway IP
	PoolGatewayIP  string   // Explicit SNAT IP (for nat mode without DHCP)
	PoolPrefixLen  int      // Subnet prefix length (default 24)
	PoolDNSServers []string // DNS servers for VM DHCP (auto-detected from host)

	// OperatorEmail is the address collected at install time (TUI, SPINIFEX_EMAIL,
	// or `spx admin init --email`). Written under [operator] in spinifex.toml so
	// reset-dev-env.sh can preserve it across wipes. Empty means no operator
	// identity was supplied.
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

// generateConfigFile creates a configuration file from a template
func GenerateConfigFile(configPath string, configTemplate string, configSettings ConfigSettings) error {
	// Parse the embedded template
	tmpl, err := template.New("config").Parse(configTemplate)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	// Create file with secure permissions
	f, err := os.OpenFile(configPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to create config file: %w", err)
	}
	defer f.Close()

	// Execute template
	if err := tmpl.Execute(f, configSettings); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	return nil
}

func GenerateCertificatesIfNeeded(configDir string, force bool, bindIP string) (caCertPath string) {
	// Certificate paths
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

		// Step 1: Generate CA certificate
		if err := GenerateCACert(caCertPath, caKeyPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error generating CA certificate: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ CA certificate generated:\n")
		fmt.Printf("   CA Certificate: %s\n", caCertPath)
		fmt.Printf("   CA Key: %s\n", caKeyPath)

		// Step 2: Generate server certificate signed by CA (with bind IP in SANs)
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

	// Verify CA files exist
	if !FileExists(caCertPath) || !FileExists(caKeyPath) {
		return fmt.Errorf("CA files not found in %s", configDir)
	}

	// Generate server cert signed by CA with this node's bind IP
	return GenerateSignedCert(serverCertPath, serverKeyPath, caCertPath, caKeyPath, bindIP)
}

func CreateServiceDirectories(spxRoot string) {
	// Create additional directories
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
		// Check if directory exists
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			if err := os.MkdirAll(dir, 0750); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: Could not create %s: %v\n", dir, err)
			}
		}
	}
	fmt.Printf("✅ Directory structure created in %s\n", spxRoot)
}

// Helper functions

func FileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ChownRecursive changes ownership of a path and all its contents to the
// specified username. Used after init to hand production directories back
// to the service user when init runs as root via sudo.
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
// Keep in sync with setup.sh create_directories() and plan doc section 2.
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
		"/etc/spinifex/spinifex.toml": 0640,
		"/etc/spinifex/master.key":    0640,
		"/etc/spinifex/server.pem":    0644,
		"/etc/spinifex/server.key":    0640,
		"/etc/spinifex/ca.pem":        0644,
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

// updateAWSINIFile updates or creates an AWS INI file section with given key-value pairs
func UpdateAWSINIFile(path, section string, values map[string]string) error {
	var cfg *ini.File
	var err error

	// Load existing file or create new one
	if FileExists(path) {
		cfg, err = ini.Load(path)
		if err != nil {
			return fmt.Errorf("failed to load INI file: %w", err)
		}
	} else {
		cfg = ini.Empty()
	}

	// Get or create section
	sec, err := cfg.NewSection(section)
	if err != nil {
		// Section already exists, get it
		sec, err = cfg.GetSection(section)
		if err != nil {
			return fmt.Errorf("failed to get section: %w", err)
		}
	}

	// Set key-value pairs
	for key, value := range values {
		sec.Key(key).SetValue(value)
	}

	// Save with proper permissions
	return cfg.SaveTo(path)
}

// generateAWSAccessKey generates an AWS-style access key
// Format: AKIA + 16 random uppercase alphanumeric characters
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

// generateAWSSecretKey generates an AWS-style secret key
// 40 character base64-encoded string
func GenerateAWSSecretKey() (string, error) {
	bytes := make([]byte, 30) // 30 bytes = 40 chars in base64
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("crypto/rand failure: %w", err)
	}
	return base64.StdEncoding.EncodeToString(bytes), nil
}

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

// GenerateCACert generates a Certificate Authority certificate and key
func GenerateCACert(caCertPath, caKeyPath string) error {
	// Generate CA private key
	caPrivateKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return fmt.Errorf("failed to generate CA private key: %w", err)
	}

	// Create CA certificate template
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

	// Self-sign the CA certificate
	caDerBytes, err := x509.CreateCertificate(rand.Reader, &caTemplate, &caTemplate, &caPrivateKey.PublicKey, caPrivateKey)
	if err != nil {
		return fmt.Errorf("failed to create CA certificate: %w", err)
	}

	// Write CA certificate to file
	caCertOut, err := os.Create(caCertPath)
	if err != nil {
		return fmt.Errorf("failed to create CA cert file: %w", err)
	}
	defer caCertOut.Close()

	if err := pem.Encode(caCertOut, &pem.Block{Type: "CERTIFICATE", Bytes: caDerBytes}); err != nil {
		return fmt.Errorf("failed to write CA cert: %w", err)
	}

	// Write CA private key to file
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

// generateSelfSignedCert generates a self-signed SSL certificate (legacy, kept for compatibility)
func GenerateSelfSignedCert(certPath, keyPath string) error {
	// Generate private key
	privateKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return fmt.Errorf("failed to generate private key: %w", err)
	}

	// Create certificate template
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

	// Create certificate
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return fmt.Errorf("failed to create certificate: %w", err)
	}

	// Write certificate to file
	certOut, err := os.Create(certPath)
	if err != nil {
		return fmt.Errorf("failed to create cert file: %w", err)
	}
	defer certOut.Close()

	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return fmt.Errorf("failed to write cert: %w", err)
	}

	// Write private key to file
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
// bindIP is the IP the AWS gateway listens on. If empty or "0.0.0.0", the
// operator's local ~/.aws/config endpoint_url falls back to "localhost" (this
// runs on the same box as the gateway). wanIP is reserved for a future
// --operator-endpoint flag that will let remote operators point their CLI at
// the host's WAN IP; today it is accepted but unused.
// When running under sudo, writes to SUDO_USER's home instead of root's.
func SetupAWSCredentials(accessKey, secretKey, region, certPath, bindIP, wanIP string) error {
	_ = wanIP
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	// When running under sudo, write to the invoking user's home directory
	// so the operator can use AWS_PROFILE=spinifex without sudo.
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

	// Determine profile name
	//profileName := "default"

	// Use Spinifex as the default profile
	profileName := "spinifex"

	if FileExists(credPath) {
		// Check if default profile already exists
		cfg, err := ini.Load(credPath)
		if err == nil && cfg.HasSection("default") {
			profileName = "spinifex"
		}
	}

	// Update credentials file
	if err := UpdateAWSINIFile(credPath, profileName, map[string]string{
		"aws_access_key_id":     accessKey,
		"aws_secret_access_key": secretKey,
	}); err != nil {
		return err
	}

	// Update config file
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
	if len(nodes) < 3 {
		return "", fmt.Errorf("multi-node predastore requires at least 3 nodes, got %d", len(nodes))
	}

	data := struct {
		Nodes     []PredastoreNodeConfig
		AccessKey string
		SecretKey string
		Region    string
		NatsToken string
		ConfigDir string
		BindIP    string
	}{nodes, accessKey, secretKey, region, natsToken, configDir, bindIP}

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
