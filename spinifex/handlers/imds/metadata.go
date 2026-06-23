package handlers_imds

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
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

// resolveCaller maps (vpcID-from-context, source-IP) to the owning ENI.
// Returns nil on miss or backend error, producing a 404 instead of 500.
func (s *IMDSServiceImpl) resolveCaller(r *http.Request) *eniFacts {
	vpcID, _ := r.Context().Value(ctxKeyVPCID).(string)
	subnetID, _ := r.Context().Value(ctxKeySubnetID).(string)
	srcIP := utils.ClientIP(r.RemoteAddr)
	if vpcID == "" || srcIP == "" {
		slog.Warn("IMDS: request missing VPC context or source IP", "vpc_id", vpcID, "subnet_id", subnetID, "remote_addr", r.RemoteAddr)
		return nil
	}

	eni, err := s.resolver.resolveENI(vpcID, srcIP)
	if err != nil {
		slog.Error("IMDS: ENI resolution failed", "vpc_id", vpcID, "subnet_id", subnetID, "src_ip", srcIP, "err", err)
		return nil
	}
	if eni == nil {
		slog.Warn("IMDS: no ENI for source IP", "vpc_id", vpcID, "subnet_id", subnetID, "src_ip", srcIP)
		return nil
	}
	return eni
}

// dispatch routes a token-validated GET to the right metadata producer.
func (s *IMDSServiceImpl) dispatch(w http.ResponseWriter, r *http.Request, eni *eniFacts) {
	path := r.URL.Path

	if strings.HasPrefix(path, prefixSecurityCreds) && len(path) > len(prefixSecurityCreds) {
		s.serveRoleCredentials(w, eni, strings.TrimPrefix(path, prefixSecurityCreds))
		return
	}

	if sub, ok := strings.CutPrefix(path, prefixPublicKeys); ok {
		s.servePublicKeys(w, eni, sub)
		return
	}

	switch path {
	case pathMetaDataRoot, prefixMetaData:
		writeText(w, s.metadataListing(eni))
	case prefixMetaData + "instance-id":
		writeText(w, eni.instanceID)
	case prefixMetaData + "local-ipv4":
		writeText(w, eni.privateIP)
	case prefixMetaData + "public-ipv4":
		writeText(w, eni.publicIP)
	case prefixMetaData + "mac":
		writeText(w, eni.mac)
	case prefixMetaData + "security-groups":
		writeText(w, strings.Join(s.resolver.resolveSGNames(eni.accountID, eni.securityGroupIDs), "\n"))
	case prefixMetaData + "hostname", prefixMetaData + "local-hostname":
		writeText(w, synthHostname(eni.privateIP, regionFromAZ(eni.availabilityZone)))
	case prefixMetaData + "public-hostname":
		writeText(w, publicHostname(eni.publicIP, regionFromAZ(eni.availabilityZone), s.baseDomain))
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
	case prefixMetaData + "iam", prefixMetaData + "iam/":
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

// serveInstanceField resolves the instance record and writes one of its
// string fields, 404ing when the instance is no longer visible.
func (s *IMDSServiceImpl) serveInstanceField(w http.ResponseWriter, eni *eniFacts, field func(*instanceFacts) string) {
	inst := s.instanceFor(w, eni)
	if inst == nil {
		return
	}
	writeText(w, field(inst))
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
		writeText(w, material+"\n")
	default:
		w.WriteHeader(http.StatusNotFound)
	}
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

// publicHostname builds "ec2-1-2-3-4.<region>.compute.<baseDomain>" from a
// public IP, region and the cluster base domain. Empty when any input is
// missing (no public IP, or DNS not configured).
func publicHostname(ip, region, baseDomain string) string {
	if ip == "" || region == "" || baseDomain == "" {
		return ""
	}
	return fmt.Sprintf("ec2-%s.%s.compute.%s", strings.ReplaceAll(ip, ".", "-"), region, baseDomain)
}

// metadataListing returns the top-level metadata keys, including the public-*
// entries only when the instance has a public IP (AWS parity).
func (s *IMDSServiceImpl) metadataListing(eni *eniFacts) string {
	keys := []string{"ami-id", "hostname", "iam/", "instance-id", "instance-type",
		"local-hostname", "local-ipv4", "mac", "placement/"}
	if eni.publicIP != "" {
		if s.baseDomain != "" {
			keys = append(keys, "public-hostname")
		}
		keys = append(keys, "public-ipv4")
	}
	keys = append(keys, "public-keys/", "security-groups")
	return strings.Join(keys, "\n")
}

func writeText(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, body)
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

// ctxKey is the unexported context-key type used to thread subnet and VPC into each request.
type ctxKey int

const (
	ctxKeyVPCID ctxKey = iota
	ctxKeySubnetID
)
