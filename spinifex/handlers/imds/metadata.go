package handlers_imds

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

const (
	pathToken            = "/latest/api/token" //nolint:gosec // URL path, not a credential
	prefixMetaData       = "/latest/meta-data/"
	pathMetaDataRoot     = "/latest/meta-data"
	pathUserData         = "/latest/user-data"
	prefixSecurityCreds  = "/latest/meta-data/iam/security-credentials/" //nolint:gosec // URL path, not a credential
	pathSecurityCredsDir = "/latest/meta-data/iam/security-credentials"  //nolint:gosec // URL path, not a credential
	prefixPublicKeys     = "/latest/meta-data/public-keys/"
	pathPublicKeysDir    = "/latest/meta-data/public-keys"
	prefixNetworkMacs    = "/latest/meta-data/network/interfaces/macs/"

	prefixDynamic        = "/latest/dynamic"
	pathIdentityDir      = "/latest/dynamic/instance-identity"
	pathIdentityDocument = "/latest/dynamic/instance-identity/document"

	// identityDocSchemaVersion is the identity-document schema version, distinct
	// from the IMDS API version (pinnedVersion).
	identityDocSchemaVersion = "2017-09-30"

	hdrToken    = "X-Aws-Ec2-Metadata-Token"             //nolint:gosec // HTTP header name, not a credential
	hdrTokenTTL = "X-Aws-Ec2-Metadata-Token-Ttl-Seconds" //nolint:gosec // HTTP header name, not a credential

	hdrForwardedFor = "X-Forwarded-For"
)

// rejectForwarded enforces the IMDS SSRF defence: requests with X-Forwarded-For are 403'd.
func rejectForwarded(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(hdrForwardedFor) != "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// dateVersion matches a dated IMDS API-version segment, e.g. 2021-03-23.
var dateVersion = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// normalizeVersion rewrites any dated API-version prefix to the canonical /latest
// tree so one dispatch table serves every version a client probes — cloud-init
// probes its own hardcoded dated versions, not the GET / listing. latest is left
// untouched; a bare GET / is not date-shaped, so it falls through to the listing.
func normalizeVersion(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seg, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if dateVersion.MatchString(seg) {
			r.URL.Path = "/latest"
			if rest != "" {
				r.URL.Path += "/" + rest
			}
		}
		next.ServeHTTP(w, r)
	})
}

// handleToken serves PUT /latest/api/token, issuing a fresh ENI-bound IMDSv2 token.
func (s *IMDSServiceImpl) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	ttl, ok := parseTokenTTL(r.Header.Get(hdrTokenTTL))
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	eni := s.resolveCaller(r)
	if eni == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	token, err := s.tokens.issue(eni.eniID, ttl, s.now())
	if err != nil {
		slog.Error("IMDS: token issuance failed", "eni_id", eni.eniID, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// First-contact log: a guest reaching this proves its packets traverse the
	// per-tap datapath; its absence (with the responder bound) points at the datapath.
	slog.Info("IMDS: issued IMDSv2 token", "instance_id", eni.instanceID, "private_ip", eni.privateIP, "public_ip", eni.publicIP)

	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set(hdrTokenTTL, strconv.Itoa(int(ttl.Seconds())))
	_, _ = w.Write([]byte(token))
}

// handleMetadata serves token-gated GET requests, resolving the ENI and dispatching.
func (s *IMDSServiceImpl) handleMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	eni := s.resolveCaller(r)
	if eni == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// IMDSv2: valid ENI-bound token required; any failure is 401 to prevent probing.
	if !s.tokens.validate(r.Header.Get(hdrToken), eni.eniID, s.now()) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	s.dispatch(w, r, eni)
}

// resolveCaller returns the request's owning ENI, threaded in via ctxKeyENI by the
// per-tap responder (resolved once from the tap's bound device). The tap is unique,
// so source IP is never consulted. Returns nil on miss, producing a 404 not a 500.
func (s *IMDSServiceImpl) resolveCaller(r *http.Request) *eniFacts {
	eni, _ := r.Context().Value(ctxKeyENI).(*eniFacts)
	return eni
}

// dispatch routes a token-validated GET to the right metadata producer.
func (s *IMDSServiceImpl) dispatch(w http.ResponseWriter, r *http.Request, eni *eniFacts) {
	path := r.URL.Path

	// Boot-crawl access log: traces every metadata GET per ENI so a guest's
	// cloud-init crawl is observable end-to-end (e.g. private vs public subnet).
	slog.Info("IMDS: serving metadata request", "path", path,
		"instance_id", eni.instanceID, "private_ip", eni.privateIP, "public_ip", eni.publicIP)

	if strings.HasPrefix(path, prefixSecurityCreds) && len(path) > len(prefixSecurityCreds) {
		s.serveRoleCredentials(w, eni, strings.TrimPrefix(path, prefixSecurityCreds))
		return
	}

	if sub, ok := strings.CutPrefix(path, prefixPublicKeys); ok {
		s.servePublicKeys(w, eni, sub)
		return
	}

	if sub, ok := strings.CutPrefix(path, prefixNetworkMacs); ok {
		s.serveNetworkInterface(w, eni, sub) // sub: "", "<mac>", "<mac>/", "<mac>/<key>"
		return
	}

	switch path {
	case "/":
		writeText(w, strings.Join(supportedVersions, "\n"))
	case "/latest", "/latest/":
		writeText(w, "dynamic\nmeta-data\nuser-data")
	case prefixDynamic, prefixDynamic + "/":
		writeText(w, "instance-identity/")
	case pathIdentityDir, pathIdentityDir + "/":
		writeText(w, "document") // signed forms (pkcs7/rsa2048/signature) listed when the signing key lands
	case pathIdentityDocument:
		s.serveInstanceIdentityDocument(w, eni)
	case pathMetaDataRoot, prefixMetaData:
		s.serveMetaDataRoot(w, eni)
	case prefixMetaData + "instance-id":
		writeText(w, eni.instanceID)
	case prefixMetaData + "instance-life-cycle":
		writeText(w, "on-demand") // Spot is not modelled yet
	case prefixMetaData + "local-ipv4":
		writeText(w, eni.privateIP)
	case prefixMetaData + "public-ipv4":
		if eni.publicIP == "" {
			w.WriteHeader(http.StatusNotFound) // no public IP → 404, as on real EC2
			return
		}
		writeText(w, eni.publicIP)
	case prefixMetaData + "public-hostname":
		if eni.publicIP == "" {
			w.WriteHeader(http.StatusNotFound) // no public IP → no public hostname
			return
		}
		writeText(w, eni.publicIP) // mirror public-ipv4 until public DNS exists
	case prefixMetaData + "mac":
		writeText(w, eni.mac)
	case prefixMetaData + "network", prefixMetaData + "network/":
		writeText(w, "interfaces/")
	case prefixMetaData + "network/interfaces", prefixMetaData + "network/interfaces/":
		writeText(w, "macs/")
	case prefixMetaData + "network/interfaces/macs":
		writeText(w, eni.mac+"/")
	case prefixMetaData + "security-groups":
		writeText(w, strings.Join(s.resolver.resolveSGNames(eni.accountID, eni.securityGroupIDs), "\n"))
	case prefixMetaData + "hostname", prefixMetaData + "local-hostname":
		writeText(w, synthHostname(eni.privateIP, regionFromAZ(eni.availabilityZone)))
	case prefixMetaData + "placement", prefixMetaData + "placement/":
		writeText(w, "availability-zone\nregion")
	case prefixMetaData + "placement/availability-zone":
		writeText(w, eni.availabilityZone)
	case prefixMetaData + "placement/region":
		writeText(w, regionFromAZ(eni.availabilityZone))
	case prefixMetaData + "instance-type":
		s.serveInstanceField(w, eni, func(i *instanceFacts) string { return i.instanceType })
	case prefixMetaData + "ami-id":
		s.serveInstanceField(w, eni, func(i *instanceFacts) string { return i.imageID })
	case prefixMetaData + "ami-launch-index":
		s.serveInstanceField(w, eni, func(i *instanceFacts) string {
			return strconv.FormatInt(i.amiLaunchIndex, 10)
		})
	case prefixMetaData + "reservation-id":
		s.serveInstanceField(w, eni, func(i *instanceFacts) string { return i.reservationID })
	case prefixMetaData + "services", prefixMetaData + "services/":
		writeText(w, "domain\npartition")
	case prefixMetaData + "services/domain":
		writeText(w, "amazonaws.com")
	case prefixMetaData + "services/partition":
		writeText(w, "aws")
	case prefixMetaData + "iam", prefixMetaData + "iam/":
		if !s.hasInstanceProfile(eni) {
			w.WriteHeader(http.StatusNotFound) // no profile → no iam/ subtree, as on real EC2
			return
		}
		writeText(w, "info\nsecurity-credentials/")
	case prefixMetaData + "iam/info":
		s.serveIAMInfo(w, eni)
	case pathSecurityCredsDir, prefixSecurityCreds:
		s.serveSecurityCredentialsList(w, eni)
	case pathPublicKeysDir:
		s.servePublicKeys(w, eni, "")
	case pathUserData:
		s.serveUserData(w, eni)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// serveMetaDataRoot writes the meta-data/ index, listing only children whose leaf
// serves content for this instance so cloud-init's recursive crawl never 404s
// mid-listing and falls back to DataSourceNone: public-hostname/public-ipv4 only
// with a public IP, public-keys/ only with a key pair, iam/ only with an instance
// profile — each omission matching real EC2, like the macs/ subtree in macKeys.
func (s *IMDSServiceImpl) serveMetaDataRoot(w http.ResponseWriter, eni *eniFacts) {
	keys := []string{
		"ami-id", "ami-launch-index", "hostname", "instance-id",
		"instance-life-cycle", "instance-type", "local-hostname", "local-ipv4",
		"mac", "network/", "placement/", "reservation-id", "security-groups",
		"services/",
	}
	if eni.publicIP != "" {
		keys = append(keys, "public-hostname", "public-ipv4")
	}
	// Resolve the instance once: public-keys/ is listed only with a key pair, iam/
	// only with a resolvable instance profile. A backend error counts as absent so
	// the listing never advertises a child whose leaf would 404 and break the crawl.
	if inst, err := s.resolver.resolveInstance(eni); err == nil && inst != nil {
		if inst.keyName != "" {
			keys = append(keys, "public-keys/")
		}
		if inst.iamInstanceProfileArn != "" {
			if p, err := s.iam.ResolveInstanceProfile(eni.accountID, inst.iamInstanceProfileArn); err == nil && p != nil {
				keys = append(keys, "iam/")
			}
		}
	}
	sort.Strings(keys)
	writeText(w, strings.Join(keys, "\n"))
}

// serveInstanceField resolves the instance record and writes one of its
// string fields, 404ing when the instance is no longer visible.
func (s *IMDSServiceImpl) serveInstanceField(w http.ResponseWriter, eni *eniFacts, field func(*instanceFacts) string) {
	inst := s.instanceFor(w, eni)
	if inst == nil {
		return
	}
	writeText(w, field(inst))
}

// instanceIdentityDocument is the unsigned EC2 instance-identity document. nil
// slices and *string fields marshal to JSON null, matching AWS's billingProducts,
// kernelId, and related fields, which Spinifex does not model.
type instanceIdentityDocument struct {
	AccountID               string   `json:"accountId"`
	Architecture            string   `json:"architecture"`
	AvailabilityZone        string   `json:"availabilityZone"`
	BillingProducts         []string `json:"billingProducts"`
	DevpayProductCodes      []string `json:"devpayProductCodes"`
	MarketplaceProductCodes []string `json:"marketplaceProductCodes"`
	ImageID                 string   `json:"imageId"`
	InstanceID              string   `json:"instanceId"`
	InstanceType            string   `json:"instanceType"`
	KernelID                *string  `json:"kernelId"`
	PendingTime             string   `json:"pendingTime"`
	PrivateIP               string   `json:"privateIp"`
	RamdiskID               *string  `json:"ramdiskId"`
	Region                  string   `json:"region"`
	Version                 string   `json:"version"`
}

// serveInstanceIdentityDocument writes the unsigned instance-identity document.
// The signed forms (pkcs7/rsa2048/signature) need a per-cluster signing key and
// are deferred. 404s when the instance is no longer visible.
func (s *IMDSServiceImpl) serveInstanceIdentityDocument(w http.ResponseWriter, eni *eniFacts) {
	inst := s.instanceFor(w, eni)
	if inst == nil {
		return
	}
	doc := instanceIdentityDocument{
		AccountID:        eni.accountID,
		Architecture:     inst.architecture,
		AvailabilityZone: eni.availabilityZone,
		ImageID:          inst.imageID,
		InstanceID:       eni.instanceID,
		InstanceType:     inst.instanceType,
		PendingTime:      inst.pendingTime.UTC().Format("2006-01-02T15:04:05Z"),
		PrivateIP:        eni.privateIP,
		Region:           regionFromAZ(eni.availabilityZone),
		Version:          identityDocSchemaVersion,
	}
	writeIdentityDocument(w, doc)
}

// serveUserData writes the instance's user-data, or 404 if absent.
func (s *IMDSServiceImpl) serveUserData(w http.ResponseWriter, eni *eniFacts) {
	inst := s.instanceFor(w, eni)
	if inst == nil {
		return
	}
	if len(inst.userData) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write(inst.userData)
}

// serveIAMInfo writes the instance profile ARN and ID, or 404 if none is attached.
func (s *IMDSServiceImpl) serveIAMInfo(w http.ResponseWriter, eni *eniFacts) {
	profile, err := s.profileFor(eni)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if profile == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]string{
		"InstanceProfileArn": profile.ARN,
		"InstanceProfileId":  profile.InstanceProfileID,
	})
}

// serveSecurityCredentialsList writes the role name(s) under the profile.
// No profile/role returns an empty 200; a backend failure returns 500.
func (s *IMDSServiceImpl) serveSecurityCredentialsList(w http.ResponseWriter, eni *eniFacts) {
	profile, err := s.profileFor(eni)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if profile == nil || profile.RoleName == "" {
		writeText(w, "")
		return
	}
	writeText(w, profile.RoleName)
}

// serveRoleCredentials mints or returns cached credentials for the named role.
// A name mismatch is 404; a backend failure resolving the profile is 500.
func (s *IMDSServiceImpl) serveRoleCredentials(w http.ResponseWriter, eni *eniFacts, roleParam string) {
	profile, err := s.profileFor(eni)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if profile == nil || profile.RoleName == "" || roleParam != profile.RoleName {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	role, err := s.iam.GetRole(eni.accountID, &iam.GetRoleInput{RoleName: aws.String(profile.RoleName)})
	if err != nil || role == nil || role.Role == nil || role.Role.Arn == nil {
		slog.Error("IMDS: resolve role ARN failed", "account_id", eni.accountID, "role", profile.RoleName, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	body, err := s.creds.get(eni, profile.RoleName, *role.Role.Arn, s.now())
	if err != nil {
		// Mirror the AWS Code:"Failed" JSON body so SDKs report a recognisable error.
		slog.Warn("IMDS: AssumeRoleForInstance failed", "account_id", eni.accountID, "role", profile.RoleName, "instance_id", eni.instanceID, "err", err)
		writeJSON(w, map[string]string{
			"Code":        "Failed",
			"LastUpdated": s.now().UTC().Format(time.RFC3339),
			"Message":     "failed to assume instance role",
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// servePublicKeys serves the /public-keys subtree (index always 0).
// A deleted key is 404; any other backend fault is 500 to avoid cloud-init booting keyless.
func (s *IMDSServiceImpl) servePublicKeys(w http.ResponseWriter, eni *eniFacts, sub string) {
	inst := s.instanceFor(w, eni)
	if inst == nil {
		return
	}
	if inst.keyName == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	switch sub {
	case "":
		writeText(w, "0="+inst.keyName)
	case "0", "0/":
		writeText(w, "openssh-key")
	case "0/openssh-key":
		material, err := s.pubKeys.GetPublicKey(eni.accountID, inst.keyName)
		if err != nil {
			if err.Error() == awserrors.ErrorInvalidKeyPairNotFound {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			slog.Error("IMDS: public key material fetch failed", "account_id", eni.accountID, "key_name", inst.keyName, "instance_id", eni.instanceID, "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		slog.Info("IMDS: served SSH public key", "key_name", inst.keyName, "instance_id", eni.instanceID, "private_ip", eni.privateIP)
		writeText(w, material+"\n")
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// serveNetworkInterface serves the single primary ENI's subtree under
// /network/interfaces/macs/. sub is the path after the macs/ prefix. An empty sub
// is the macs/ directory listing; a MAC that is not the caller's is 404, since the
// per-tap responder only ever resolves its own ENI (single-NIC; multi-ENI deferred).
func (s *IMDSServiceImpl) serveNetworkInterface(w http.ResponseWriter, eni *eniFacts, sub string) {
	if sub == "" {
		writeText(w, eni.mac+"/")
		return
	}
	mac, key, _ := strings.Cut(sub, "/")
	if mac != eni.mac {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch key {
	case "": // "<mac>" or "<mac>/" — the per-interface key listing
		keys, err := s.macKeys(eni)
		if err != nil {
			slog.Error("IMDS: network-interface listing failed", "account_id", eni.accountID, "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeText(w, strings.Join(keys, "\n"))
	case "mac":
		writeText(w, eni.mac)
	case "device-number":
		writeText(w, "0") // primary ENI
	case "interface-id":
		writeText(w, eni.eniID)
	case "owner-id":
		writeText(w, eni.accountID)
	case "subnet-id":
		writeText(w, eni.subnetID)
	case "vpc-id":
		writeText(w, eni.vpcID)
	case "local-ipv4s":
		writeText(w, eni.privateIP)
	case "local-hostname":
		writeText(w, synthHostname(eni.privateIP, regionFromAZ(eni.availabilityZone)))
	case "security-group-ids":
		writeText(w, strings.Join(eni.securityGroupIDs, "\n"))
	case "security-groups":
		writeText(w, strings.Join(s.resolver.resolveSGNames(eni.accountID, eni.securityGroupIDs), "\n"))
	case "subnet-ipv4-cidr-block":
		s.serveCIDR(w, eni.accountID, eni.subnetID, s.resolver.resolveSubnetCIDR)
	case "vpc-ipv4-cidr-block", "vpc-ipv4-cidr-blocks":
		s.serveCIDR(w, eni.accountID, eni.vpcID, s.resolver.resolveVPCCIDR)
	case "public-ipv4s", "public-hostname":
		if eni.publicIP == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeText(w, eni.publicIP)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// macKeys lists exactly the leaf keys served under macs/<mac>/ so cloud-init's
// recursive crawl never lists a key that 404s: CIDR keys appear only when the CIDR
// resolves, public keys only with a public IP. A resolver fault is propagated so the
// listing 500s like the leaf, never silently dropping a key on a transient KV blip.
func (s *IMDSServiceImpl) macKeys(eni *eniFacts) ([]string, error) {
	keys := []string{
		"device-number", "interface-id", "local-hostname", "local-ipv4s",
		"mac", "owner-id", "security-group-ids", "security-groups",
		"subnet-id", "vpc-id",
	}
	subnetCIDR, err := s.resolver.resolveSubnetCIDR(eni.accountID, eni.subnetID)
	if err != nil {
		return nil, err
	}
	if subnetCIDR != "" {
		keys = append(keys, "subnet-ipv4-cidr-block")
	}
	vpcCIDR, err := s.resolver.resolveVPCCIDR(eni.accountID, eni.vpcID)
	if err != nil {
		return nil, err
	}
	if vpcCIDR != "" {
		keys = append(keys, "vpc-ipv4-cidr-block", "vpc-ipv4-cidr-blocks")
	}
	if eni.publicIP != "" {
		keys = append(keys, "public-hostname", "public-ipv4s")
	}
	sort.Strings(keys)
	return keys, nil
}

// serveCIDR resolves and writes a subnet/VPC CIDR: 404 on miss, 500 on a backend
// fault, so a guest never renders network config from an empty CIDR.
func (s *IMDSServiceImpl) serveCIDR(w http.ResponseWriter, accountID, id string, resolve func(string, string) (string, error)) {
	cidr, err := resolve(accountID, id)
	if err != nil {
		slog.Error("IMDS: network-interface CIDR resolution failed", "account_id", accountID, "id", id, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if cidr == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeText(w, cidr)
}

// instanceFor resolves the instance record for an ENI, writing the appropriate
// HTTP error and returning nil on miss/failure so callers can early-return.
func (s *IMDSServiceImpl) instanceFor(w http.ResponseWriter, eni *eniFacts) *instanceFacts {
	inst, err := s.resolver.resolveInstance(eni)
	if err != nil {
		slog.Error("IMDS: instance resolution failed", "instance_id", eni.instanceID, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return nil
	}
	if inst == nil {
		w.WriteHeader(http.StatusNotFound)
		return nil
	}
	return inst
}

// profileFor resolves the IAM instance profile for an ENI.
// Returns (nil, nil) when absent, (nil, err) on backend failure — never collapses errors to absent.
func (s *IMDSServiceImpl) profileFor(eni *eniFacts) (*resolvedProfile, error) {
	inst, err := s.resolver.resolveInstance(eni)
	if err != nil {
		slog.Error("IMDS: instance resolution failed", "instance_id", eni.instanceID, "err", err)
		return nil, err
	}
	if inst == nil || inst.iamInstanceProfileArn == "" {
		return nil, nil
	}
	profile, err := s.iam.ResolveInstanceProfile(eni.accountID, inst.iamInstanceProfileArn)
	if err != nil {
		if err.Error() == awserrors.ErrorIAMNoSuchEntity {
			return nil, nil // profile deleted; treat as no role
		}
		slog.Error("IMDS: resolve instance profile failed", "account_id", eni.accountID, "arn", inst.iamInstanceProfileArn, "err", err)
		return nil, err
	}
	if profile == nil {
		return nil, nil
	}
	return &resolvedProfile{ARN: profile.ARN, InstanceProfileID: profile.InstanceProfileID, RoleName: profile.RoleName}, nil
}

// hasInstanceProfile reports whether a resolvable instance profile is attached. A
// backend error counts as "no profile" so the iam/ subtree stays self-consistent —
// omitted from the meta-data/ listing and 404 at the directory — rather than
// advertised with 404ing leaves, which fails cloud-init's metadata crawl.
func (s *IMDSServiceImpl) hasInstanceProfile(eni *eniFacts) bool {
	profile, err := s.profileFor(eni)
	return err == nil && profile != nil
}

// resolvedProfile is the slice of an IAM instance profile served by the metadata surface.
type resolvedProfile struct {
	ARN               string
	InstanceProfileID string
	RoleName          string
}

// parseTokenTTL validates the X-aws-ec2-metadata-token-ttl-seconds header.
func parseTokenTTL(raw string) (time.Duration, bool) {
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < tokenTTLMin || n > tokenTTLMax {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

// regionFromAZ strips the trailing AZ letter from an AZ name to get the region.
func regionFromAZ(az string) string {
	if az == "" {
		return ""
	}
	last := az[len(az)-1]
	if last >= 'a' && last <= 'z' {
		return az[:len(az)-1]
	}
	return az
}

// synthHostname builds "ip-10-0-1-5.<region>.compute.internal" from a private IP and region.
func synthHostname(ip, region string) string {
	if ip == "" || region == "" {
		return ""
	}
	return fmt.Sprintf("ip-%s.%s.compute.internal", strings.ReplaceAll(ip, ".", "-"), region)
}

func writeText(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, body)
}

// writeIdentityDocument writes the identity document pretty-printed as text/plain,
// mirroring the AWS IMDS response (2-space indent, nil fields rendered as null).
func writeIdentityDocument(w http.ResponseWriter, doc instanceIdentityDocument) {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// ctxKey is the unexported context-key type used to thread the per-tap ENI
// identity into each request.
type ctxKey int

// ctxKeyENI carries a *eniFacts resolved once per per-tap responder — the
// authoritative caller identity for every request it serves.
const ctxKeyENI ctxKey = iota
