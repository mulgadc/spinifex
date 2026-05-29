package handlers_imds

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
)

// HTTP paths. IMDSv2 token issuance is a PUT; everything else is a token-gated
// GET under /latest/.
const (
	pathToken            = "/latest/api/token" //nolint:gosec // URL path, not a credential
	prefixMetaData       = "/latest/meta-data/"
	pathMetaDataRoot     = "/latest/meta-data"
	pathUserData         = "/latest/user-data"
	prefixSecurityCreds  = "/latest/meta-data/iam/security-credentials/" //nolint:gosec // URL path, not a credential
	pathSecurityCredsDir = "/latest/meta-data/iam/security-credentials"  //nolint:gosec // URL path, not a credential

	hdrToken    = "X-Aws-Ec2-Metadata-Token"             //nolint:gosec // HTTP header name, not a credential
	hdrTokenTTL = "X-Aws-Ec2-Metadata-Token-Ttl-Seconds" //nolint:gosec // HTTP header name, not a credential

	hdrForwardedFor = "X-Forwarded-For"
)

// rejectForwarded enforces AWS IMDS's SSRF defence: any request carrying an
// X-Forwarded-For header is refused with 403, before token or identity checks.
// A reverse proxy or request-forwarding app on the instance stamps that header,
// so rejecting it stops the classic "trick a server-side app into relaying a
// metadata request" attack. Applies to every path including the token PUT,
// matching AWS.
func rejectForwarded(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(hdrForwardedFor) != "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleToken serves PUT /latest/api/token. It binds a fresh token to the
// requesting ENI (resolved from the datapath-attested source IP) and returns it
// as a text/plain body, matching AWS IMDSv2.
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

// handleMetadata serves every token-gated read path. It resolves the caller's
// ENI, enforces the IMDSv2 token, then dispatches on the request path.
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

	// IMDSv2: a valid, ENI-bound token is mandatory on every read. A missing,
	// unknown, expired, or wrong-ENI token is an indistinguishable 401 so a
	// guest cannot probe which tokens exist.
	if !s.tokens.validate(r.Header.Get(hdrToken), eni.eniID, s.now()) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	s.dispatch(w, r, eni)
}

// resolveCaller maps the request's (vpcID-from-context, source-IP) to the
// owning ENI, or nil when no mapping exists (logged for operator triage). A
// backend error is also surfaced as nil → the caller 404s, never 500s, matching
// AWS's opaque "eventually available" boot posture.
func (s *IMDSServiceImpl) resolveCaller(r *http.Request) *eniFacts {
	vpcID, _ := r.Context().Value(ctxKeyVPCID).(string)
	srcIP := clientIP(r.RemoteAddr)
	if vpcID == "" || srcIP == "" {
		slog.Warn("IMDS: request missing VPC context or source IP", "vpc_id", vpcID, "remote_addr", r.RemoteAddr)
		return nil
	}

	eni, err := s.resolver.resolveENI(vpcID, srcIP)
	if err != nil {
		slog.Error("IMDS: ENI resolution failed", "vpc_id", vpcID, "src_ip", srcIP, "err", err)
		return nil
	}
	if eni == nil {
		slog.Warn("IMDS: no ENI for source IP", "vpc_id", vpcID, "src_ip", srcIP)
		return nil
	}
	return eni
}

// dispatch routes a token-validated GET to the right metadata producer.
func (s *IMDSServiceImpl) dispatch(w http.ResponseWriter, r *http.Request, eni *eniFacts) {
	path := r.URL.Path

	// Credential fetch for a specific role: /iam/security-credentials/<role>.
	if strings.HasPrefix(path, prefixSecurityCreds) && len(path) > len(prefixSecurityCreds) {
		s.serveRoleCredentials(w, eni, strings.TrimPrefix(path, prefixSecurityCreds))
		return
	}

	switch path {
	case pathMetaDataRoot, prefixMetaData:
		writeText(w, "ami-id\nhostname\niam/\ninstance-id\ninstance-type\nlocal-hostname\nlocal-ipv4\nmac\nplacement/\npublic-ipv4\nsecurity-groups")
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
	case pathUserData:
		s.serveUserData(w, eni)
	default:
		// /latest/dynamic/instance-identity/document and
		// /latest/meta-data/network/interfaces/... are out of scope in v1.
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

// serveUserData writes the instance's user-data, or 404 when there is none —
// matching AWS for instances launched without user-data.
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

// serveIAMInfo writes {InstanceProfileArn, InstanceProfileId}, 404 when the
// instance has no instance profile, or 500 when the profile can't be resolved.
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

// serveSecurityCredentialsList writes the role name(s) under the profile, one
// per line. An instance with genuinely no profile/role returns an empty 200
// body, exactly as AWS does (the SDK then concludes there is no instance role).
// A backend failure resolving the profile is a 500, never an empty 200 — the
// latter would make a transient hiccup indistinguishable from "no role" and
// silently strip the instance's credentials.
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

// serveRoleCredentials mints (or serves cached) credentials for the named role.
// AWS only accepts the actual role name as the path parameter, not the profile
// name, so a mismatch is 404; a backend failure resolving the profile is 500.
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
		// AWS surfaces a trust-policy failure as a JSON body with Code:"Failed";
		// the SDK reports it as "EC2RoleProvider failed". Mirror that rather than
		// a bare HTTP error so the SDK error message is AWS-accurate.
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

// profileFor resolves the instance's IAM instance profile. It returns:
//   - (profile, nil) when the instance has a resolvable profile,
//   - (nil, nil) when the instance genuinely has no profile (no attached
//     instance, no profile ARN, or the ARN dereferences to nothing),
//   - (nil, err) when a backend lookup fails.
//
// Callers must distinguish the last case (500) from the middle one (404 /
// empty body): collapsing a backend error into "no profile" would make a
// transient failure look like a roleless instance and silently drop creds.
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
		slog.Error("IMDS: resolve instance profile failed", "account_id", eni.accountID, "arn", inst.iamInstanceProfileArn, "err", err)
		return nil, err
	}
	if profile == nil {
		return nil, nil
	}
	return &resolvedProfile{ARN: profile.ARN, InstanceProfileID: profile.InstanceProfileID, RoleName: profile.RoleName}, nil
}

// resolvedProfile is the slice of an IAM instance profile the metadata surface
// serves, decoupled from the handlers_iam type so this package needn't import
// it for the field access.
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

// clientIP strips the port from a RemoteAddr ("10.0.1.5:54321" → "10.0.1.5").
func clientIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// regionFromAZ derives the region from an availability-zone name by stripping
// the trailing AZ letter ("ap-southeast-2a" → "ap-southeast-2").
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

// synthHostname builds the AWS-style internal hostname
// "ip-10-0-1-5.<region>.compute.internal" from a private IP and region.
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

func writeJSON(w http.ResponseWriter, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// ctxKey is the unexported context-key type for values the bind manager threads
// into each request (the VPC ID the listener's veth maps to).
type ctxKey int

const ctxKeyVPCID ctxKey = iota
