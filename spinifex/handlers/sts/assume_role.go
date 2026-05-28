package handlers_sts

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/nats-io/nats.go"
)

const (
	stsActionAssumeRole = "sts:AssumeRole"
	stsActionWildcard   = "sts:*"
	globalWildcard      = "*"

	minDurationSeconds     int64 = 900
	maxDurationSeconds     int64 = 43200
	defaultDurationSeconds int64 = 3600

	// sessionAKIDRandomBytes hex-encodes to 16 chars; with the ASIA prefix the
	// AKID is 20 chars total, matching the AWS public format.
	sessionAKIDRandomBytes = 8
	sessionSecretBytes     = 30 // base64 → 40 chars
	sessionTokenBytes      = 32 // base64 → 44 chars

	mintMaxAttempts = 3
)

// roleSessionNameRegex enforces AWS's documented session-name character set
// expressed as a literal ASCII range: `\w` would silently widen to Unicode
// under a future build tag and is avoided. `:` and `/` are excluded so the
// synthesised assumed-role ARN and AssumedRoleId stay unambiguously parseable.
var roleSessionNameRegex = regexp.MustCompile(`^[A-Za-z0-9_+=,.@-]{2,64}$`)

// AssumeRole mints temporary credentials after evaluating the target role's
// trust policy against the caller.
func (s *STSServiceImpl) AssumeRole(callerAccountID, callerARN, callerIdentity string, input *sts.AssumeRoleInput) (*sts.AssumeRoleOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if aws.StringValue(input.RoleArn) == "" || aws.StringValue(input.RoleSessionName) == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	sessionName := *input.RoleSessionName
	if !roleSessionNameRegex.MatchString(sessionName) {
		return nil, errors.New(awserrors.ErrorValidationError)
	}

	if aws.StringValue(input.Policy) != "" || len(input.PolicyArns) > 0 {
		return nil, errors.New(awserrors.ErrorPackedPolicyTooLarge)
	}
	if len(input.Tags) > 0 || len(input.TransitiveTagKeys) > 0 {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if aws.StringValue(input.SerialNumber) != "" || aws.StringValue(input.TokenCode) != "" {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}

	roleAccountID, roleName, err := parseRoleARN(*input.RoleArn)
	if err != nil {
		return nil, errors.New(awserrors.ErrorValidationError)
	}

	roleOut, err := s.iamSvc.GetRole(roleAccountID, &iam.GetRoleInput{RoleName: aws.String(roleName)})
	if err != nil {
		// Same-account miss leaks no information the caller couldn't get via
		// ListRoles; cross-account miss is masked to AccessDenied to prevent
		// enumeration. Comparing against the bare error code matches how
		// IAMService surfaces NoSuchEntity (errors.New(awserrors.ErrorIAMNoSuchEntity)).
		if err.Error() == awserrors.ErrorIAMNoSuchEntity && callerAccountID != roleAccountID {
			return nil, errors.New(awserrors.ErrorAccessDenied)
		}
		return nil, err
	}
	role := roleOut.Role

	duration := defaultDurationSeconds
	if input.DurationSeconds != nil {
		duration = *input.DurationSeconds
	}
	effectiveMax := aws.Int64Value(role.MaxSessionDuration)
	if effectiveMax == 0 {
		effectiveMax = defaultDurationSeconds
	}
	if effectiveMax > maxDurationSeconds {
		effectiveMax = maxDurationSeconds
	}
	if duration < minDurationSeconds || duration > effectiveMax {
		return nil, errors.New(awserrors.ErrorValidationError)
	}

	if err := evalTrustPolicy(aws.StringValue(role.AssumeRolePolicyDocument), callerARN); err != nil {
		return nil, err
	}

	cred, plainSecret, plainToken, err := s.mintSessionCredential(role, roleAccountID, sessionName, aws.StringValue(input.SourceIdentity), duration)
	if err != nil {
		return nil, err
	}

	slog.Info("AssumeRole success",
		"caller_arn", callerARN,
		"caller_identity", callerIdentity,
		"role_arn", aws.StringValue(role.Arn),
		"session_name", sessionName,
		"akid", cred.AccessKeyID,
		"expires_at", cred.ExpiresAt,
		"source_identity", aws.StringValue(input.SourceIdentity),
		"external_id", aws.StringValue(input.ExternalId),
	)

	return &sts.AssumeRoleOutput{
		Credentials: &sts.Credentials{
			AccessKeyId:     aws.String(cred.AccessKeyID),
			SecretAccessKey: aws.String(plainSecret),
			SessionToken:    aws.String(plainToken),
			Expiration:      aws.Time(cred.ExpiresAt),
		},
		AssumedRoleUser: &sts.AssumedRoleUser{
			AssumedRoleId: aws.String(cred.AssumedRoleID),
			Arn:           aws.String(cred.AssumedRoleARN),
		},
		PackedPolicySize: aws.Int64(0),
	}, nil
}

// parseRoleARN extracts the account ID and role name from an IAM role ARN of
// the form arn:aws:iam::<accountID>:role/<path>/<name> (path optional).
func parseRoleARN(arnStr string) (accountID, name string, err error) {
	parts := strings.SplitN(arnStr, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" || parts[2] != "iam" || parts[3] != "" {
		return "", "", errors.New("not an IAM ARN")
	}
	resource := parts[5]
	const prefix = "role/"
	if !strings.HasPrefix(resource, prefix) {
		return "", "", errors.New("ARN resource is not a role")
	}
	pathAndName := resource[len(prefix):]
	if pathAndName == "" {
		return "", "", errors.New("role name is empty")
	}
	if slash := strings.LastIndex(pathAndName, "/"); slash >= 0 {
		name = pathAndName[slash+1:]
	} else {
		name = pathAndName
	}
	if name == "" {
		return "", "", errors.New("role name is empty")
	}
	return parts[4], name, nil
}

// evalTrustPolicy implements AWS's explicit-deny-wins semantics: a first pass
// scans every Deny statement (returning AccessDenied on any match), a second
// pass scans every Allow statement (returning nil on the first match). A
// single-pass "return on first Allow" loop would silently skip a later Deny
// and is a real authorisation bug.
func evalTrustPolicy(docJSON, callerARN string) error {
	doc, err := handlers_iam.ValidateTrustPolicyDocument(docJSON)
	if err != nil {
		// Stored docs were validated at CreateRole / UpdateAssumeRolePolicy
		// time, so reaching here implies on-disk corruption — fail closed.
		return fmt.Errorf("stored trust policy invalid: %w", err)
	}

	for _, stmt := range doc.Statement {
		if stmt.Effect != handlers_iam.PolicyEffectDeny {
			continue
		}
		if !matchTrustAction(stmt.Action) {
			continue
		}
		match, err := matchTrustPrincipal(stmt.Principal, callerARN)
		if err != nil {
			return err
		}
		if match {
			return errors.New(awserrors.ErrorAccessDenied)
		}
	}

	for _, stmt := range doc.Statement {
		if stmt.Effect != handlers_iam.PolicyEffectAllow {
			continue
		}
		if !matchTrustAction(stmt.Action) {
			continue
		}
		match, err := matchTrustPrincipal(stmt.Principal, callerARN)
		if err != nil {
			return err
		}
		if match {
			return nil
		}
	}

	return errors.New(awserrors.ErrorAccessDenied)
}

func matchTrustAction(actions []string) bool {
	for _, a := range actions {
		if a == stsActionAssumeRole || a == stsActionWildcard || a == globalWildcard {
			return true
		}
	}
	return false
}

// matchTrustPrincipal evaluates each top-level Principal key independently —
// AWS semantics treat the map as an OR across keys, and unsupported keys
// (Service, Federated) skip at the entry level rather than failing the whole
// statement.
func matchTrustPrincipal(raw json.RawMessage, callerARN string) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	// Principal: "*" form (a bare string, not a map).
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString == globalWildcard, nil
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false, fmt.Errorf("unmarshal Principal: %w", err)
	}
	awsRaw, ok := m["AWS"]
	if !ok {
		return false, nil
	}
	return matchAWSPrincipal(awsRaw, callerARN)
}

func matchAWSPrincipal(raw json.RawMessage, callerARN string) (bool, error) {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return matchAWSPrincipalEntry(single, callerARN), nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil {
		return false, fmt.Errorf("Principal.AWS must be string or array: %w", err)
	}
	for _, entry := range arr {
		if matchAWSPrincipalEntry(entry, callerARN) {
			return true, nil
		}
	}
	return false, nil
}

// matchAWSPrincipalEntry resolves one principal clause against the caller's
// ARN, handling four forms: the wildcard, a bare 12-digit account ID (treated
// as :root shorthand), exact ARN match, :root in the same account, and the
// chained-assume auto-expansion where a role ARN matches any session of that
// role.
func matchAWSPrincipalEntry(clause, callerARN string) bool {
	if clause == globalWildcard {
		return true
	}
	if isTwelveDigits(clause) {
		clause = fmt.Sprintf("arn:aws:iam::%s:root", clause)
	}
	if clause == callerARN {
		return true
	}

	clauseARN, ok := parsePrincipalARN(clause)
	if !ok {
		return false
	}
	callerParts, ok := parsePrincipalARN(callerARN)
	if !ok {
		return false
	}

	// :root → any principal in the same account.
	if clauseARN.resource == "root" && clauseARN.account == callerParts.account {
		return true
	}

	// Role-ARN auto-expansion: `arn:aws:iam::A:role/.../X` matches
	// `arn:aws:sts::A:assumed-role/X/<session>` for any session.
	if clauseARN.service == "iam" && strings.HasPrefix(clauseARN.resource, "role/") &&
		callerParts.service == "sts" && strings.HasPrefix(callerParts.resource, "assumed-role/") &&
		clauseARN.account == callerParts.account {
		clauseRoleName := lastPathSegment(clauseARN.resource[len("role/"):])
		sessionTail := callerParts.resource[len("assumed-role/"):]
		callerRoleName, _, ok := strings.Cut(sessionTail, "/")
		if !ok {
			return false
		}
		if clauseRoleName != "" && clauseRoleName == callerRoleName {
			return true
		}
	}

	return false
}

type principalARN struct {
	service  string
	account  string
	resource string
}

func parsePrincipalARN(arnStr string) (principalARN, bool) {
	parts := strings.SplitN(arnStr, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[1] != "aws" || parts[3] != "" {
		return principalARN{}, false
	}
	return principalARN{service: parts[2], account: parts[4], resource: parts[5]}, true
}

func lastPathSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func isTwelveDigits(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// mintSessionCredential generates a fresh ASIA AKID, secret, and session
// token, encrypts the secret with the IAM master key, HMACs the session
// token, and persists the record. Retries on AKID collision; the birthday
// bound for 64-bit AKID entropy makes that practically unreachable.
func (s *STSServiceImpl) mintSessionCredential(role *iam.Role, roleAccountID, sessionName, sourceIdentity string, duration int64) (*SessionCredential, string, string, error) {
	plainSecret, err := generateRandomBase64(sessionSecretBytes)
	if err != nil {
		return nil, "", "", err
	}
	secretEncrypted, err := handlers_iam.EncryptSecret(plainSecret, s.masterKey)
	if err != nil {
		return nil, "", "", fmt.Errorf("encrypt session secret: %w", err)
	}

	plainToken, err := generateRandomBase64(sessionTokenBytes)
	if err != nil {
		return nil, "", "", err
	}
	tokenHMAC := computeTokenHMAC(s.masterKey, plainToken)

	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(duration) * time.Second)

	roleID := aws.StringValue(role.RoleId)
	roleName := aws.StringValue(role.RoleName)
	underlyingRoleARN := aws.StringValue(role.Arn)

	for range mintMaxAttempts {
		akid, err := generateSessionAKID()
		if err != nil {
			return nil, "", "", err
		}
		cred := &SessionCredential{
			AccessKeyID:       akid,
			SecretEncrypted:   secretEncrypted,
			SessionTokenHMAC:  tokenHMAC,
			AccountID:         roleAccountID,
			AssumedRoleARN:    fmt.Sprintf("arn:aws:sts::%s:assumed-role/%s/%s", roleAccountID, roleName, sessionName),
			UnderlyingRoleARN: underlyingRoleARN,
			RoleID:            roleID,
			AssumedRoleID:     fmt.Sprintf("%s:%s", roleID, sessionName),
			SessionName:       sessionName,
			SourceIdentity:    sourceIdentity,
			ExpiresAt:         expiresAt,
			CreatedAt:         now,
		}
		if err := putSessionCredential(s.sessionsBucket, cred); err != nil {
			if errors.Is(err, nats.ErrKeyExists) {
				continue
			}
			return nil, "", "", fmt.Errorf("persist session credential: %w", err)
		}
		return cred, plainSecret, plainToken, nil
	}
	return nil, "", "", errors.New(awserrors.ErrorInternalError)
}

func generateSessionAKID() (string, error) {
	b := make([]byte, sessionAKIDRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand failure: %w", err)
	}
	return SessionAccessKeyIDPrefix + strings.ToUpper(hex.EncodeToString(b)), nil
}

func generateRandomBase64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand failure: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// computeTokenHMAC HMACs the wire-form (base64) session token with the IAM
// master key. The SigV4 verifier recomputes this over the
// X-Amz-Security-Token header and constant-time-compares against the stored
// value — a bucket read by any process that lacks the master key cannot
// recover a usable token.
func computeTokenHMAC(key []byte, token string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(token))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
