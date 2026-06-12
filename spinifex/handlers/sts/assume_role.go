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
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/predastore/auth"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const (
	stsActionAssumeRole = "sts:AssumeRole"
	stsActionWildcard   = "sts:*"
	globalWildcard      = "*"

	// principalTypeAssumedRole is the SessionCredential.PrincipalType for a role session;
	// empty stored values are also treated as this type.
	principalTypeAssumedRole = "assumed-role"

	// principalTypeUser is the SessionCredential.PrincipalType for sessions minted by
	// GetSessionToken, which resolve back to the calling IAM user.
	principalTypeUser = "user"

	// ec2ServicePrincipal is the synthetic caller used by AssumeRoleForInstance.
	// The HTTPS AssumeRole path supplies an empty principalSource, so service principals never match there.
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

// roleSessionNameRegex enforces AWS's documented session-name character set as a literal
// ASCII range. `:` and `/` are excluded so the assumed-role ARN stays unambiguously parseable.
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

// AssumeRoleForInstance is the in-process IMDS entry point, not reachable over HTTPS.
// Caller is synthesised as ec2.amazonaws.com; the session name is the instanceID.
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

// assumeRoleForCaller is the shared core of AssumeRole and AssumeRoleForInstance:
// resolves the role, clamps the duration, evaluates the trust policy, and mints credentials.
// principalSource is "" for HTTPS, ec2.amazonaws.com for IMDS.
func (s *STSServiceImpl) assumeRoleForCaller(callerAccountID, callerARN, principalSource, roleARN, sessionName, sourceIdentity string, requestedDuration int64) (*sts.AssumeRoleOutput, error) {
	roleAccountID, roleName, err := auth.ParseRoleARN(roleARN)
	if err != nil {
		return nil, errors.New(awserrors.ErrorValidationError)
	}

	roleOut, err := s.iamSvc.GetRole(roleAccountID, &iam.GetRoleInput{RoleName: aws.String(roleName)})
	if err != nil {
		// Cross-account miss is masked to AccessDenied to prevent role enumeration.
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

	env := assumedRoleEnvelope(role, roleAccountID, sessionName, sourceIdentity)
	cred, plainSecret, plainToken, err := s.mintSession(env, duration)
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

// evalTrustPolicy implements AWS's explicit-deny-wins semantics: first pass scans all
// Deny statements, second pass scans all Allows. A single-pass loop would skip a later
// Deny and silently grant access.
func evalTrustPolicy(docJSON, callerARN, principalSource string) error {
	doc, err := handlers_iam.ValidateTrustPolicyDocument(docJSON)
	if err != nil {
		// Docs are validated at write time; reaching here implies on-disk corruption.
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

// matchTrustPrincipal evaluates each Principal key as an OR. AWS matches callerARN;
// Service matches principalSource (IMDS path only); unsupported keys (Federated) skip.
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

// matchServicePrincipal matches a Service principal against the synthesised caller.
// Only the EC2 service principal is trusted; any other principalSource is denied.
func matchServicePrincipal(raw json.RawMessage, principalSource string) (bool, error) {
	if principalSource != ec2ServicePrincipal {
		return false, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single == principalSource, nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil {
		return false, fmt.Errorf("Principal.Service must be string or array: %w", err)
	}
	return slices.Contains(arr, principalSource), nil
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

// matchAWSPrincipalEntry resolves one principal clause: wildcard, bare account ID
// (treated as :root), exact ARN, :root shorthand, and role-ARN session expansion.
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

	// :root matches any principal in the same account; scoped to the IAM service
	// segment so arn:aws:s3::A:root (or any other service) does not match.
	if clauseARN.service == "iam" && clauseARN.resource == "root" &&
		clauseARN.account == callerParts.account {
		return true
	}

	// Role-ARN auto-expansion: arn:aws:iam::A:role/X matches any assumed-role session of X.
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

// sessionEnvelope is the resolved identity a session credential is minted for.
// Role paths fill the assumed-role fields; GetSessionToken sets PrincipalType "user"
// and leaves the assumed-role fields empty.
type sessionEnvelope struct {
	PrincipalType  string
	AccountID      string
	SessionName    string
	SourceIdentity string

	// Assumed-role-only; empty for a user (GetSessionToken) envelope.
	AssumedRoleARN    string
	UnderlyingRoleARN string
	RoleID            string
	AssumedRoleID     string
}

// assumedRoleEnvelope derives the session envelope for a role session, shared by all
// AssumeRole variants. It centralises assumed-role ARN and AssumedRoleId construction.
func assumedRoleEnvelope(role *iam.Role, roleAccountID, sessionName, sourceIdentity string) sessionEnvelope {
	roleID := aws.StringValue(role.RoleId)
	roleName := aws.StringValue(role.RoleName)
	return sessionEnvelope{
		PrincipalType:     principalTypeAssumedRole,
		AccountID:         roleAccountID,
		SessionName:       sessionName,
		SourceIdentity:    sourceIdentity,
		AssumedRoleARN:    fmt.Sprintf("arn:aws:sts::%s:assumed-role/%s/%s", roleAccountID, roleName, sessionName),
		UnderlyingRoleARN: aws.StringValue(role.Arn),
		RoleID:            roleID,
		AssumedRoleID:     fmt.Sprintf("%s:%s", roleID, sessionName),
	}
}

// mintSession generates a fresh ASIA AKID, encrypts the secret, HMACs the token,
// and persists the credential. Retries up to mintMaxAttempts on AKID collision.
func (s *STSServiceImpl) mintSession(env sessionEnvelope, duration int64) (*SessionCredential, string, string, error) {
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

	for range mintMaxAttempts {
		akid, err := generateSessionAKID()
		if err != nil {
			return nil, "", "", err
		}
		cred := &SessionCredential{
			AccessKeyID:       akid,
			SecretEncrypted:   secretEncrypted,
			SessionTokenHMAC:  tokenHMAC,
			AccountID:         env.AccountID,
			PrincipalType:     env.PrincipalType,
			AssumedRoleARN:    env.AssumedRoleARN,
			UnderlyingRoleARN: env.UnderlyingRoleARN,
			RoleID:            env.RoleID,
			AssumedRoleID:     env.AssumedRoleID,
			SessionName:       env.SessionName,
			SourceIdentity:    env.SourceIdentity,
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

// computeTokenHMAC HMACs the wire-form session token with the master key.
// The SigV4 verifier recomputes and constant-time-compares; a bucket read without
// the master key cannot recover a valid token.
func computeTokenHMAC(key []byte, token string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(token))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
