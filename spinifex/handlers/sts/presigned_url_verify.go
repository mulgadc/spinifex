package handlers_sts

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

const (
	presignedAlgorithm    = "AWS4-HMAC-SHA256"
	presignedTerminator   = "aws4_request"
	presignedService      = "sts"
	presignedAmzDateFmt   = "20060102T150405Z"
	presignedDateFmt      = "20060102"
	presignedSignedHeader = "x-k8s-aws-id"
	presignedHostHeader   = "host"

	// presignedMaxExpiresSeconds is the AWS-imposed ceiling on X-Amz-Expires.
	// Rejecting beyond this ceiling catches forged URLs that lie about their TTL.
	presignedMaxExpiresSeconds = 7 * 24 * 60 * 60

	// presignedClockSkew is the SigV4 wire-protocol clock-skew allowance.
	presignedClockSkew = 5 * time.Minute
)

// PresignedCallerIdentity is the result of validating a SigV4-presigned GetCallerIdentity
// URL — the token shape produced by `aws eks get-token`.
type PresignedCallerIdentity struct {
	AccountID     string
	ARN           string
	UserID        string
	XK8sAwsID     string // value the caller signed under (= expectedClusterName on success)
	PrincipalType string
}

// presignedTimeNow is the time-source seam; tests override it to pin the clock.
var presignedTimeNow = func() time.Time { return time.Now().UTC() }

// VerifyPresignedGetCallerIdentity validates a SigV4-presigned GetCallerIdentity URL
// and resolves the calling principal. Called by the EKS token webhook over NATS.
// Reconstructs the canonical request with expectedClusterName to prevent cross-cluster replay.
func (s *STSServiceImpl) VerifyPresignedGetCallerIdentity(presignedURL, expectedClusterName string) (*PresignedCallerIdentity, error) {
	if presignedURL == "" || expectedClusterName == "" {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	u, err := url.Parse(presignedURL)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidIdentityToken)
	}
	if u.Scheme != "https" || u.Host == "" {
		return nil, errors.New(awserrors.ErrorInvalidIdentityToken)
	}

	q := u.Query()
	env, err := parsePresignedEnvelope(q)
	if err != nil {
		slog.Warn("VerifyPresignedGetCallerIdentity: envelope parse failed", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidIdentityToken)
	}

	if !env.signedHeaders[presignedSignedHeader] {
		slog.Warn("VerifyPresignedGetCallerIdentity: x-k8s-aws-id not in SignedHeaders",
			"signed_headers", env.rawSignedHeaders)
		return nil, errors.New(awserrors.ErrorInvalidIdentityToken)
	}
	if !env.signedHeaders[presignedHostHeader] {
		return nil, errors.New(awserrors.ErrorInvalidIdentityToken)
	}

	if env.credential.service != presignedService {
		return nil, errors.New(awserrors.ErrorInvalidIdentityToken)
	}
	if env.credential.terminator != presignedTerminator {
		return nil, errors.New(awserrors.ErrorInvalidIdentityToken)
	}

	now := presignedTimeNow()
	if now.Sub(env.signedTime).Abs() > presignedClockSkew+time.Duration(env.expiresSeconds)*time.Second {
		// Guards a nonsense X-Amz-Date (months out); exact expiry is checked below.
		return nil, errors.New(awserrors.ErrorExpiredToken)
	}
	if now.After(env.signedTime.Add(time.Duration(env.expiresSeconds) * time.Second)) {
		return nil, errors.New(awserrors.ErrorExpiredToken)
	}
	if env.signedTime.UTC().Format(presignedDateFmt) != env.credential.date {
		// Credential-scope date must match the X-Amz-Date date portion.
		return nil, errors.New(awserrors.ErrorInvalidIdentityToken)
	}

	principal, secret, err := s.resolvePrincipalForVerify(env.credential.accessKeyID)
	if err != nil {
		return nil, err
	}

	canonicalQuery := canonicalQueryStringExcludingSignature(q)
	canonicalHeaders, signedHeadersList := buildCanonicalHeaders(u.Host, expectedClusterName)
	canonicalRequest := strings.Join([]string{
		"GET",
		canonicalURI(u),
		canonicalQuery,
		canonicalHeaders,
		signedHeadersList,
		// Non-S3 presigned requests use SHA256("") as the payload hash; "UNSIGNED-PAYLOAD" is S3-only.
		emptyStringSHA256,
	}, "\n")

	stringToSign := strings.Join([]string{
		presignedAlgorithm,
		env.amzDate,
		env.credential.scope(),
		hexSHA256(canonicalRequest),
	}, "\n")

	signingKey := deriveSigningKey(secret, env.credential.date, env.credential.region, env.credential.service)
	expectedSig := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	if subtle.ConstantTimeCompare([]byte(expectedSig), []byte(env.signature)) != 1 {
		slog.Warn("VerifyPresignedGetCallerIdentity: signature mismatch (cluster name replay or tampered URL)",
			"akid", env.credential.accessKeyID,
			"expected_cluster", expectedClusterName)
		return nil, errors.New(awserrors.ErrorInvalidIdentityToken)
	}

	principal.XK8sAwsID = expectedClusterName
	return principal, nil
}

// presignedEnvelope holds the parsed envelope facts from a SigV4-presigned URL query string.
type presignedEnvelope struct {
	credential       presignedCredential
	amzDate          string // raw X-Amz-Date value (used in StringToSign)
	signedTime       time.Time
	expiresSeconds   int64
	rawSignedHeaders string
	signedHeaders    map[string]bool
	signature        string
}

type presignedCredential struct {
	accessKeyID string
	date        string // YYYYMMDD
	region      string
	service     string
	terminator  string
}

func (c presignedCredential) scope() string {
	return strings.Join([]string{c.date, c.region, c.service, c.terminator}, "/")
}

func parsePresignedEnvelope(q url.Values) (presignedEnvelope, error) {
	algo := q.Get("X-Amz-Algorithm")
	if algo != presignedAlgorithm {
		return presignedEnvelope{}, fmt.Errorf("unsupported algorithm %q", algo)
	}
	credRaw := q.Get("X-Amz-Credential")
	if credRaw == "" {
		return presignedEnvelope{}, errors.New("missing X-Amz-Credential")
	}
	parts := strings.Split(credRaw, "/")
	if len(parts) != 5 {
		return presignedEnvelope{}, fmt.Errorf("X-Amz-Credential has %d parts, expected 5", len(parts))
	}
	cred := presignedCredential{
		accessKeyID: parts[0],
		date:        parts[1],
		region:      parts[2],
		service:     parts[3],
		terminator:  parts[4],
	}
	if cred.accessKeyID == "" {
		return presignedEnvelope{}, errors.New("X-Amz-Credential has empty access key id")
	}

	amzDate := q.Get("X-Amz-Date")
	if amzDate == "" {
		return presignedEnvelope{}, errors.New("missing X-Amz-Date")
	}
	signedTime, err := time.Parse(presignedAmzDateFmt, amzDate)
	if err != nil {
		return presignedEnvelope{}, fmt.Errorf("invalid X-Amz-Date: %w", err)
	}

	expiresRaw := q.Get("X-Amz-Expires")
	if expiresRaw == "" {
		return presignedEnvelope{}, errors.New("missing X-Amz-Expires")
	}
	expires, err := strconv.ParseInt(expiresRaw, 10, 64)
	if err != nil || expires <= 0 || expires > presignedMaxExpiresSeconds {
		return presignedEnvelope{}, fmt.Errorf("invalid X-Amz-Expires: %q", expiresRaw)
	}

	signedHeadersRaw := q.Get("X-Amz-SignedHeaders")
	if signedHeadersRaw == "" {
		return presignedEnvelope{}, errors.New("missing X-Amz-SignedHeaders")
	}
	signedSet := make(map[string]bool)
	for h := range strings.SplitSeq(signedHeadersRaw, ";") {
		signedSet[strings.ToLower(h)] = true
	}

	sig := q.Get("X-Amz-Signature")
	if sig == "" {
		return presignedEnvelope{}, errors.New("missing X-Amz-Signature")
	}

	return presignedEnvelope{
		credential:       cred,
		amzDate:          amzDate,
		signedTime:       signedTime.UTC(),
		expiresSeconds:   expires,
		rawSignedHeaders: signedHeadersRaw,
		signedHeaders:    signedSet,
		signature:        sig,
	}, nil
}

// canonicalQueryStringExcludingSignature returns the SigV4 canonical query string
// with X-Amz-Signature dropped, per-RFC-3986 encoded, sorted by key then value.
func canonicalQueryStringExcludingSignature(q url.Values) string {
	type kv struct {
		k string
		v string
	}
	pairs := make([]kv, 0, len(q))
	for k, vs := range q {
		if k == "X-Amz-Signature" {
			continue
		}
		ek := awsQueryEscape(k)
		for _, v := range vs {
			pairs = append(pairs, kv{ek, awsQueryEscape(v)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k == pairs[j].k {
			return pairs[i].v < pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

// awsQueryEscape encodes per SigV4 (unreserved set, uppercase hex).
// net/url.QueryEscape encodes spaces as '+' and lower-cases hex, both diverging from AWS.
func awsQueryEscape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'),
			c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteString(strings.ToUpper(hexByte(c)))
		}
	}
	return b.String()
}

func hexByte(b byte) string {
	const hexDigits = "0123456789ABCDEF"
	return string([]byte{hexDigits[b>>4], hexDigits[b&0x0f]})
}

// canonicalURI returns the SigV4 canonical URI; empty path normalises to "/".
func canonicalURI(u *url.URL) string {
	if u.Path == "" {
		return "/"
	}
	return u.Path
}

// buildCanonicalHeaders reconstructs the canonical-headers/signed-headers pair for
// the presigned URL, using the URL host and expectedClusterName for X-K8s-Aws-Id.
func buildCanonicalHeaders(host, xK8sAwsID string) (canonicalHeaders, signedHeadersList string) {
	headers := map[string]string{
		presignedHostHeader:   host,
		presignedSignedHeader: xK8sAwsID,
	}
	names := slices.Sorted(maps.Keys(headers))
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(':')
		b.WriteString(strings.TrimSpace(headers[n]))
		b.WriteByte('\n')
	}
	return b.String(), strings.Join(names, ";")
}

func hexSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// emptyStringSHA256 is the SigV4 payload hash for an empty body.
var emptyStringSHA256 = hexSHA256("")

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func deriveSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, presignedTerminator)
}

// resolvePrincipalForVerify resolves an access key to its plaintext secret and a
// PresignedCallerIdentity skeleton. Branches on AKID prefix (session vs long-lived).
func (s *STSServiceImpl) resolvePrincipalForVerify(accessKeyID string) (*PresignedCallerIdentity, string, error) {
	switch {
	case strings.HasPrefix(accessKeyID, SessionAccessKeyIDPrefix):
		cred, err := s.LookupSessionCredential(accessKeyID)
		if err != nil {
			return nil, "", err
		}
		if cred == nil {
			return nil, "", errors.New(awserrors.ErrorInvalidIdentityToken)
		}
		if presignedTimeNow().After(cred.ExpiresAt) {
			return nil, "", errors.New(awserrors.ErrorExpiredToken)
		}
		secret, err := handlers_iam.DecryptSecret(cred.SecretEncrypted, s.masterKey)
		if err != nil {
			return nil, "", fmt.Errorf("decrypt session secret: %w", err)
		}
		return &PresignedCallerIdentity{
			AccountID:     cred.AccountID,
			ARN:           cred.AssumedRoleARN,
			UserID:        cred.AssumedRoleID,
			PrincipalType: principalTypeAssumedRolePresigned,
		}, secret, nil
	case strings.HasPrefix(accessKeyID, longLivedAccessKeyIDPrefix):
		ak, err := s.iamSvc.LookupAccessKey(accessKeyID)
		if err != nil {
			if strings.Contains(err.Error(), awserrors.ErrorIAMNoSuchEntity) {
				return nil, "", errors.New(awserrors.ErrorInvalidIdentityToken)
			}
			return nil, "", err
		}
		if ak.Status != handlers_iam.AccessKeyStatusActive {
			return nil, "", errors.New(awserrors.ErrorInvalidIdentityToken)
		}
		secret, err := s.iamSvc.DecryptSecret(ak.SecretAccessKey)
		if err != nil {
			return nil, "", fmt.Errorf("decrypt IAM secret: %w", err)
		}
		userOut, err := s.iamSvc.GetUser(ak.AccountID, &iam.GetUserInput{UserName: aws.String(ak.UserName)})
		if err != nil {
			return nil, "", err
		}
		userARN := aws.StringValue(userOut.User.Arn)
		userID := aws.StringValue(userOut.User.UserId)
		if userARN == "" {
			userARN = fmt.Sprintf("arn:aws:iam::%s:user/%s", ak.AccountID, ak.UserName)
		}
		return &PresignedCallerIdentity{
			AccountID:     ak.AccountID,
			ARN:           userARN,
			UserID:        userID,
			PrincipalType: principalTypeUserPresigned,
		}, secret, nil
	default:
		return nil, "", errors.New(awserrors.ErrorInvalidIdentityToken)
	}
}

const (
	longLivedAccessKeyIDPrefix        = "AKIA"
	principalTypeUserPresigned        = "User"
	principalTypeAssumedRolePresigned = "AssumedRole"
)
