package handlers_acm

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
)

// caKeyBits and leafKeyBits are RSA key sizes, kept as package-level vars so
// tests can lower them for faster key generation, mirroring
// admin.certKeyBits. Production keeps the 4096/2048 defaults.
var (
	caKeyBits   = 4096
	leafKeyBits = 2048
)

const (
	// tenantCAValidity is long because the root is a one-time trust-store
	// install on every client device: rotating it invalidates all of them at
	// once, so it should outlive many leaf-renewal cycles.
	tenantCAValidity = 10 * 365 * 24 * time.Hour

	// leafValidity is short because issuance and renewal are instant, offline
	// operations for this CA — unlike the public ACME path, there is no
	// rate-limit reason to prefer a long-lived leaf, so a short lifetime is
	// pure upside: it bounds the exposure window of the fresh key IssueLeaf
	// generates on every call.
	leafValidity = 90 * 24 * time.Hour

	// leafClockSkewBackdate absorbs a small amount of clock drift between
	// this node and whatever client validates the issued leaf.
	leafClockSkewBackdate = 5 * time.Minute
)

// TenantCA signs leaf certificates for tenant domains from an independent
// root, generated specifically for this purpose.
//
// It is deliberately not the platform CA (admin.GenerateCACert /
// admin.GenerateCertificatesIfNeeded). That CA is trusted by every node, the
// ipsec charon store, EKS and ECS guests, ALB microvms, and EKS addon pods,
// and is baked into catalog images — signing tenant leaves from it would turn
// any tenant certificate request into a control-plane-wide MITM primitive.
// Nor is it a subordinate beneath the platform CA: that CA is pathlen=0,
// which forbids a subordinate outright, and even if it were not, a shared
// hierarchy would still let a tenant leaf request touch platform trust.
//
// The root's x509 name constraints (see createTenantCA) are the actual
// security boundary, not the AccountID/domain checks in the API layer above
// this file: a client that enforces RFC 5280 constraints rejects an
// out-of-scope leaf even if every server-side check here is bypassed or
// buggy. Go, OpenSSL and browsers all honour constraints; some embedded TLS
// stacks do not, so treat this as defense in depth, not a guarantee.
type TenantCA struct {
	root    *x509.Certificate
	key     *rsa.PrivateKey
	rootPEM string // cached PEM encoding of root, returned as the chain for every issued leaf.
}

var _ CertAuthority = (*TenantCA)(nil)

// tenantCACreateCommandHint names the admin command that creates the tenant
// CA, quoted into every "no tenant CA here" error so an operator lands on the
// fix without having to go spelunking through docs.
const tenantCACreateCommandHint = "spx admin cert create-tenant-ca --domain <domain>"

// LoadOrCreateTenantCA loads the tenant root from certPath/keyPath, creating
// it on first use with permittedDomains baked in as x509 name constraints.
//
// Constraints are frozen at creation. If a root already exists at these
// paths, its PermittedDNSDomains must cover every entry in permittedDomains
// or this returns an error rather than regenerating: rotating the root
// invalidates every device's trust store at once, so that can only ever be a
// deliberate, operator-driven action.
func LoadOrCreateTenantCA(certPath, keyPath string, permittedDomains []string) (*TenantCA, error) {
	normalized, err := normalizeDomains(permittedDomains)
	if err != nil {
		return nil, fmt.Errorf("privateca: %w", err)
	}

	certExists := fileExists(certPath)
	keyExists := fileExists(keyPath)

	switch {
	case certExists && keyExists:
		ca, err := readTenantCAFiles(certPath, keyPath)
		if err != nil {
			return nil, err
		}
		if uncovered := uncoveredDomains(normalized, ca.root.PermittedDNSDomains); len(uncovered) > 0 {
			return nil, fmt.Errorf(
				"privateca: tenant CA at %s does not cover domain(s) %s; name constraints are frozen at creation — regenerate the tenant CA (delete %s and %s) and redistribute the new root to every client trust store",
				certPath, strings.Join(uncovered, ", "), certPath, keyPath,
			)
		}
		return ca, nil
	case certExists != keyExists:
		// One file present without its counterpart is an ambiguous, corrupt
		// state. Guessing wrong here (e.g. regenerating over a surviving key)
		// is worse than refusing, so this is never auto-repaired.
		return nil, fmt.Errorf(
			"privateca: tenant CA at %s is incomplete (cert present=%v, key present=%v); restore the missing file from backup or remove both to regenerate",
			certPath, certExists, keyExists,
		)
	default:
		return createTenantCA(certPath, keyPath, normalized)
	}
}

// LoadTenantCA loads an existing tenant root from certPath/keyPath. Unlike
// LoadOrCreateTenantCA, it never creates one — there is no permitted-domains
// argument to bake in, because the daemon (this function's only caller) has
// paths but not a domain list to create with. The permitted domains come from
// the certificate itself once loaded (see PermittedDomains), not from a
// caller-supplied list, so there is nothing to drift-check here.
//
// Absent or half-present files return a clearly-worded, actionable error
// naming the admin command that creates the CA, rather than a bare "file not
// found" — this is the first thing an operator sees when PRIVATE_CA issuance
// is unreachable.
func LoadTenantCA(certPath, keyPath string) (*TenantCA, error) {
	certExists := fileExists(certPath)
	keyExists := fileExists(keyPath)

	switch {
	case certExists && keyExists:
		return readTenantCAFiles(certPath, keyPath)
	case certExists != keyExists:
		return nil, fmt.Errorf(
			"privateca: tenant CA at %s is incomplete (cert present=%v, key present=%v); restore the missing file from backup or remove both and recreate with %q",
			certPath, certExists, keyExists, tenantCACreateCommandHint,
		)
	default:
		return nil, fmt.Errorf(
			"privateca: no tenant CA found at %s; PRIVATE_CA certificate issuance is unavailable until one is created — run %q",
			certPath, tenantCACreateCommandHint,
		)
	}
}

// readTenantCAFiles reads and parses an existing root and key pair from disk,
// verifying the key matches the certificate's public key. It performs no
// domain-coverage check; callers that need one (LoadOrCreateTenantCA) apply it
// themselves against the freshly loaded root's own PermittedDNSDomains.
func readTenantCAFiles(certPath, keyPath string) (*TenantCA, error) {
	certPEMBytes, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("privateca: read tenant CA cert: %w", err)
	}
	certBlock, _ := pem.Decode(certPEMBytes)
	if certBlock == nil {
		return nil, fmt.Errorf("privateca: no PEM certificate block in %s", certPath)
	}
	root, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("privateca: parse tenant CA cert: %w", err)
	}

	keyPEMBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("privateca: read tenant CA key: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEMBytes)
	if keyBlock == nil {
		return nil, fmt.Errorf("privateca: no PEM key block in %s", keyPath)
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		// Never fold key bytes into an error string — this only reports the
		// parse failure, not the key material.
		return nil, fmt.Errorf("privateca: parse tenant CA key: %w", err)
	}
	key, ok := parsedKey.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("privateca: tenant CA key at %s is not RSA", keyPath)
	}
	if !key.PublicKey.Equal(root.PublicKey) {
		return nil, fmt.Errorf("privateca: tenant CA key at %s does not match the public key in cert at %s", keyPath, certPath)
	}

	return &TenantCA{root: root, key: key, rootPEM: string(certPEMBytes)}, nil
}

// createTenantCA generates a fresh, self-signed tenant root with
// permittedDomains baked in as x509 name constraints, and persists it to
// certPath/keyPath.
func createTenantCA(certPath, keyPath string, permittedDomains []string) (*TenantCA, error) {
	if len(permittedDomains) == 0 {
		// An empty PermittedDNSDomains list is not "constrained to nothing" —
		// Go's constraint matcher (crypto/x509/constraints.go) treats a nil
		// permitted-DNS constraint as "no restriction", so this would silently
		// create an unconstrained root. Refuse instead of building something
		// that looks constrained but is not.
		return nil, fmt.Errorf("privateca: refusing to create a tenant CA with no permitted domains — it would be unconstrained")
	}

	key, err := rsa.GenerateKey(rand.Reader, caKeyBits)
	if err != nil {
		return nil, fmt.Errorf("privateca: generate tenant CA key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, fmt.Errorf("privateca: generate tenant CA serial: %w", err)
	}

	// 0.0.0.0/0 and ::/0 as excluded ranges, with no permitted IP ranges,
	// make every possible IP address excluded — this root can never validate
	// a leaf carrying an IP SAN.
	_, allV4, err := net.ParseCIDR("0.0.0.0/0")
	if err != nil {
		return nil, fmt.Errorf("privateca: parse IPv4 exclusion range: %w", err)
	}
	_, allV6, err := net.ParseCIDR("::/0")
	if err != nil {
		return nil, fmt.Errorf("privateca: parse IPv6 exclusion range: %w", err)
	}

	notBefore := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Spinifex Tenant CA",
			Organization: []string{"Spinifex Tenant"},
		},
		NotBefore: notBefore,
		NotAfter:  notBefore.Add(tenantCAValidity),

		// This root only ever signs and revokes tenant leaves — it never
		// authenticates a TLS connection itself, so KeyUsage is limited to
		// certificate and CRL signing.
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// MaxPathLen 0 with MaxPathLenZero true (Go treats a bare zero value
		// as "unset" otherwise) forbids any subordinate CA beneath this root:
		// it may only ever sign end-entity leaves.
		MaxPathLen:     0,
		MaxPathLenZero: true,

		// Name constraints are the cryptographic security boundary described
		// on TenantCA: PermittedDNSDomains in apex form covers every
		// subdomain, so adding a service never touches the CA.
		PermittedDNSDomains: permittedDomains,
		// RFC 5280 4.2.1.10: "conforming CAs MUST mark this extension as
		// critical." A client that does not understand name constraints must
		// then reject the certificate outright rather than silently ignore
		// the restriction.
		PermittedDNSDomainsCritical: true,
		ExcludedIPRanges:            []*net.IPNet{allV4, allV6},

		// Go's constraint matcher (crypto/x509/constraints.go,
		// newDNSConstraints) treats a single zero-length entry in a
		// domain-shaped constraint list as "match every name" — that
		// function backs DNS, email and URI constraints alike, keyed on the
		// domain portion of the address/URI. RFC 5280 defines no dedicated
		// "exclude everything" wildcard for rfc822Name/URI; this relies
		// specifically on Go's interpretation (consistent with errata 5997)
		// of an empty constraint value, verified against the go1.26.5
		// stdlib. A single "" entry in each list therefore excludes every
		// possible email address and URI outright — this root never expects
		// to sign either.
		//
		// This CA is installed in device trust stores, so it is validated by
		// non-Go verifiers too. Checked against OpenSSL 3.5.6: `openssl x509
		// -text` parses a root built this way without error, printing empty
		// "email:"/"URI:" excluded-subtree lines rather than rejecting the
		// extension, and `openssl verify -CAfile` both accepts a valid
		// in-scope leaf and rejects a leaf forged outside
		// PermittedDNSDomains with "error 47: permitted subtree violation" —
		// so the zero-length exclusion neither breaks parsing nor changes
		// the DNS-constraint outcome on a non-Go client. If a future
		// verifier turns out to disagree, drop these two fields rather than
		// fight it: PermittedDNSDomains plus ExcludedIPRanges already carry
		// the security argument, and IssueLeaf never emits an email or URI
		// SAN, so the exclusions are redundant defense in depth, not load
		// bearing.
		ExcludedEmailAddresses: []string{""},
		ExcludedURIDomains:     []string{""},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("privateca: create tenant CA cert: %w", err)
	}
	root, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("privateca: parse newly created tenant CA cert: %w", err)
	}

	certPEMBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	// The cert is public material (distributed to every client trust store),
	// so it is written with the default create mode, matching
	// admin.GenerateCACert's cert file.
	certOut, err := os.Create(certPath)
	if err != nil {
		return nil, fmt.Errorf("privateca: create tenant CA cert file: %w", err)
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return nil, fmt.Errorf("privateca: write tenant CA cert: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("privateca: marshal tenant CA key: %w", err)
	}
	// 0600: this key signs every tenant certificate in the deployment —
	// getting this wrong compromises every tenant certificate at once.
	keyOut, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("privateca: create tenant CA key file: %w", err)
	}
	defer keyOut.Close()
	if err := pem.Encode(keyOut, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}); err != nil {
		return nil, fmt.Errorf("privateca: write tenant CA key: %w", err)
	}

	return &TenantCA{root: root, key: key, rootPEM: string(certPEMBytes)}, nil
}

// IssueLeaf signs a leaf certificate for domain plus sans, generating a
// fresh private key. Returns PEM-encoded leaf, chain and key.
//
// domain and every entry in sans must fall within the root's permitted
// domains (see Authorized). This is enforced here, independently of any
// authorization check the caller already performed, as a second line of
// defense — the same reasoning that motivates baking the constraints into
// the certificate at all.
func (c *TenantCA) IssueLeaf(domain string, sans []string) (certPEM, chainPEM, keyPEM string, err error) {
	if domain == "" {
		return "", "", "", fmt.Errorf("privateca: domain is required")
	}
	names := dedupeStrings(append([]string{domain}, sans...))
	for _, n := range names {
		if !c.Authorized(n) {
			return "", "", "", fmt.Errorf("privateca: domain %q is outside the tenant CA's permitted domains %v", n, c.root.PermittedDNSDomains)
		}
	}

	leafKey, err := rsa.GenerateKey(rand.Reader, leafKeyBits)
	if err != nil {
		return "", "", "", fmt.Errorf("privateca: generate leaf key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return "", "", "", fmt.Errorf("privateca: generate leaf serial: %w", err)
	}

	notBefore := time.Now().Add(-leafClockSkewBackdate)
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: domain},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(leafValidity + leafClockSkewBackdate),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              names,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, c.root, &leafKey.PublicKey, c.key)
	if err != nil {
		return "", "", "", fmt.Errorf("privateca: sign leaf: %w", err)
	}

	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		// As above: report the failure, never the key material.
		return "", "", "", fmt.Errorf("privateca: marshal leaf key: %w", err)
	}
	leafKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	return string(leafPEM), c.rootPEM, string(leafKeyPEM), nil
}

// PermittedDomains returns the name constraints baked into the loaded root,
// read from the certificate itself rather than a parallel list so the two
// cannot drift. The returned slice is a copy; mutating it has no effect on
// the CA.
func (c *TenantCA) PermittedDomains() []string {
	return slices.Clone(c.root.PermittedDNSDomains)
}

// Authorized reports whether domain falls within the root's permitted
// domains. domain may be an apex, subdomain, or wildcard name; comparison is
// case-insensitive and mirrors the RFC 5280 apex-domain match Go's own
// verifier applies at certificate-verification time, so a domain authorized
// here is guaranteed to actually chain once a leaf is issued for it.
func (c *TenantCA) Authorized(domain string) bool {
	return domainCovered(domain, c.root.PermittedDNSDomains)
}

// randomSerial returns a random 128-bit certificate serial number, matching
// admin.GenerateCACert's convention.
func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// fileExists reports whether path names a regular, readable file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// normalizeDomains lowercases, strips wildcard/leading-dot prefixes, dedupes
// and sorts domains into apex form suitable for PermittedDNSDomains. It
// rejects any entry that normalizes to the empty string.
func normalizeDomains(domains []string) ([]string, error) {
	seen := make(map[string]struct{}, len(domains))
	out := make([]string, 0, len(domains))
	for _, d := range domains {
		nd := normalizeDomainQuery(d)
		if nd == "" {
			return nil, fmt.Errorf("empty or invalid domain %q", d)
		}
		if _, ok := seen[nd]; ok {
			continue
		}
		seen[nd] = struct{}{}
		out = append(out, nd)
	}
	sort.Strings(out)
	return out, nil
}

// normalizeDomainQuery lowercases domain and strips a trailing dot and a
// leading "*." or "." so apex, subdomain and wildcard forms all compare
// against PermittedDNSDomains the same way.
func normalizeDomainQuery(domain string) string {
	d := strings.ToLower(strings.TrimSuffix(domain, "."))
	d = strings.TrimPrefix(d, "*.")
	d = strings.TrimPrefix(d, ".")
	return d
}

// domainCovered reports whether name (apex, subdomain, or wildcard) is
// exactly one of permitted or a subdomain of one of permitted, matching the
// RFC 5280 dNSName constraint semantics for an apex-form (no leading dot)
// constraint: the apex itself and every subdomain are covered.
func domainCovered(name string, permitted []string) bool {
	nd := normalizeDomainQuery(name)
	if nd == "" {
		return false
	}
	for _, p := range permitted {
		np := strings.ToLower(strings.TrimSuffix(p, "."))
		if np == "" {
			continue
		}
		if nd == np || strings.HasSuffix(nd, "."+np) {
			return true
		}
	}
	return false
}

// uncoveredDomains returns the entries of requested (already normalized) not
// covered by existing, preserving requested's order.
func uncoveredDomains(requested, existing []string) []string {
	var out []string
	for _, d := range requested {
		if !domainCovered(d, existing) {
			out = append(out, d)
		}
	}
	return out
}

// dedupeStrings returns names with duplicates removed, preserving the first
// occurrence's order.
func dedupeStrings(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}
