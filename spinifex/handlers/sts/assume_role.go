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
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const (
	stsActionAssumeRole = "sts:AssumeRole"
	stsActionWildcard   = "sts:*"
	globalWildcard      = "*"

	// ec2ServicePrincipal is the synthetic caller used by AssumeRoleForInstance.
	// It is the only service principal that matches a trust policy in v1; the
	// HTTPS AssumeRole path always supplies an empty principalSource, so service
	// principals never match there.
	ec2ServicePrincipal = "ec2.amazonaws.com"

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

	duration := int64(0)
	if input.DurationSeconds != nil {
		duration = *input.DurationSeconds
	}

	out, err := s.assumeRoleForCaller(callerAccountID, callerARN, "", *input.RoleArn, sessionName, aws.StringValue(input.SourceIdentity), duration)
	if err != nil {
		return nil, err
	}

	slog.Info("AssumeRole success",
		"caller_arn", callerARN,
		"caller_identity", callerIdentity,
		"role_arn", aws.StringValue(input.RoleArn),
		"session_name", sessionName,
		"akid", aws.StringValue(out.Credentials.AccessKeyId),
		"expires_at", aws.TimeValue(out.Credentials.Expiration),
		"source_identity", aws.StringValue(input.SourceIdentity),
		"external_id", aws.StringValue(input.ExternalId),
	)

	return out, nil
}

// AssumeRoleForInstance is the EC2-instance-metadata internal entry point. It is
// not reachable over HTTPS: the IMDS handler calls it in-process after resolving
// an instance's IamInstanceProfileArn to a role. The caller is synthesised as
// the EC2 service principal (principalSource = ec2.amazonaws.com), which is the
// only way a Principal: {"Service": "ec2.amazonaws.com"} trust statement matches.
// The assumed-role session name is the instance ID, so the resulting ARN is
// arn:aws:sts::<accountID>:assumed-role/<roleName>/<instanceID> — matching AWS.
func (s *STSServiceImpl) AssumeRoleForInstance(accountID, roleARN, instanceID string, durationSeconds int64) (*sts.AssumeRoleOutput, error) {
	if accountID == "" || roleARN == "" || instanceID == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}
	if !roleSessionNameRegex.MatchString(instanceID) {
		return nil, errors.New(awserrors.ErrorValidationError)
	}

	out, err := s.assumeRoleForCaller(accountID, "", ec2ServicePrincipal, roleARN, instanceID, "", durationSeconds)
	if err != nil {
		return nil, err
	}

	slog.Info("AssumeRoleForInstance success",
		"account_id", accountID,
		"instance_id", instanceID,
		"role_arn", roleARN,
		"akid", aws.StringValue(out.Credentials.AccessKeyId),
		"expires_at", aws.TimeValue(out.Credentials.Expiration),
	)

	return out, nil
}

// assumeRoleForCaller is the shared core of AssumeRole (HTTPS) and
// AssumeRoleForInstance (in-process IMDS): it resolves the role, validates the
// requested duration (0 → default), evaluates the trust policy against the
// supplied caller, and mints session credentials. principalSource is "" on the
// HTTPS path — where the caller is an IAM user or assumed-role session named by
// callerARN — and "ec2.amazonaws.com" on the instance path, where callerARN is
// empty and only a service-principal statement can match.
func (s *STSServiceImpl) assumeRoleForCaller(callerAccountID, callerARN, principalSource, roleARN, sessionName, sourceIdentity string, requestedDuration int64) (*sts.AssumeRoleOutput, error) {
	roleAccountID, roleName, err := parseRoleARN(roleARN)
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

	duration := requestedDuration
	if duration == 0 {
		duration = defaultDurationSeconds
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

	if err := evalTrustPolicy(aws.StringValue(role.AssumeRolePolicyDocument), callerARN, principalSource); err != nil {
		return nil, err
	}

	cred, plainSecret, plainToken, err := s.mintSessionCredential(role, roleAccountID, sessionName, sourceIdentity, duration)
	if err != nil {
		return nil, err
	}

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
func evalTrustPolicy(docJSON, callerARN, principalSource string) error {
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
		match, err := matchTrustPrincipal(stmt.Principal, callerARN, principalSource)
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
		match, err := matchTrustPrincipal(stmt.Principal, callerARN, principalSource)
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
// AWS semantics treat the map as an OR across keys. The AWS key matches against
// callerARN; the Service key matches against principalSource (only ever set by
// the in-process AssumeRoleForInstance path). Unsupported keys (Federated) skip
// at the entry level rather than failing the whole statement.
func matchTrustPrincipal(raw json.RawMessage, callerARN, principalSource string) (bool, error) {
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
	if awsRaw, ok := m["AWS"]; ok {
		match, err := matchAWSPrincipal(awsRaw, callerARN)
		if err != nil {
			return false, err
		}
		if match {
			return true, nil
		}
	}
	if svcRaw, ok := m["Service"]; ok {
		match, err := matchServicePrincipal(svcRaw, principalSource)
		if err != nil {
			return false, err
		}
		if match {
			return true, nil
		}
	}
	return false, nil
}

// allowedServicePrincipals is the v1 whitelist of service principals that may
// match a Principal: {"Service": ...} trust statement. Expanding this (e.g.
// ecs-tasks.amazonaws.com, lambda.amazonaws.com) is a one-line change per
// follow-on plan.
var allowedServicePrincipals = map[string]bool{
	ec2ServicePrincipal: true,
}

// matchServicePrincipal matches a Service principal against the synthesised
// caller. An empty principalSource (the HTTPS AssumeRole path) never matches, so
// service-principal trust statements are unreachable over HTTPS. The clause must
// also be in the v1 whitelist, so an unsupported service principal is denied
// even if a future caller synthesised it.
func matchServicePrincipal(raw json.RawMessage, principalSource string) (bool, error) {
	if principalSource == "" {
		return false, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return matchServiceEntry(single, principalSource), nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil {
		return false, fmt.Errorf("Principal.Service must be string or array: %w", err)
	}
	for _, entry := range arr {
		if matchServiceEntry(entry, principalSource) {
			return true, nil
		}
	}
	return false, nil
}

func matchServiceEntry(clause, principalSource string) bool {
	return allowedServicePrincipals[clause] && clause == principalSource
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
	if utils.IsAccountID(clause) {
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

	// :root → any principal in the same account. Scoped to the IAM service
	// segment; arn:aws:s3::A:root (or any other service) must not match —
	// validators upstream do not check service-segment shape, so a malformed
	// ARN pasted into a trust policy must fail closed here.
	if clauseARN.service == "iam" && clauseARN.resource == "root" &&
		clauseARN.account == callerParts.account {
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
