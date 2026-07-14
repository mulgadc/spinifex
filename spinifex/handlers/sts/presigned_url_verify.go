package handlers_sts

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/predastore/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

const (
	presignedService      = "sts"
	presignedSignedHeader = "x-k8s-aws-id"
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
// The X-K8s-Aws-Id header is reconstructed from expectedClusterName rather than the URL,
// so a URL presigned for one cluster produces a signature mismatch against another.
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

	// Reconstruct the signed request, binding X-K8s-Aws-Id to the expected cluster.
	// sigv4.Parse reads the header value off the request, so an attacker's URL signed
	// for another cluster reconstructs under a different value and fails the compare.
	r, err := http.NewRequest(http.MethodGet, presignedURL, nil)
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidIdentityToken)
	}
	// Canonical MIME key on Set; sigv4 lowercases header names for the SignedHeaders set.
	r.Header.Set("X-K8s-Aws-Id", expectedClusterName)

	req, err := sigv4.Parse(r, sigv4.WithTime(presignedTimeNow()))
	if err != nil {
		slog.Warn("VerifyPresignedGetCallerIdentity: envelope parse failed", "err", err)
		return nil, mapSigv4Err(err)
	}

	// sigv4 enforces host is signed but not X-K8s-Aws-Id; without it the reconstructed
	// canonical request drops the cluster binding, so verify would self-match any URL.
	if _, ok := req.Canonical.SignedHeaders[presignedSignedHeader]; !ok {
		slog.Warn("VerifyPresignedGetCallerIdentity: x-k8s-aws-id not in SignedHeaders")
		return nil, errors.New(awserrors.ErrorInvalidIdentityToken)
	}

	principal, secret, err := s.resolvePrincipalForVerify(req.Credential.AccessKeyID)
	if err != nil {
		return nil, err
	}

	// Pass the credential's own region back so Verify's region check is a no-op:
	// `aws eks get-token` signs against the caller's regional STS endpoint, which we
	// do not constrain. The service is pinned to sts.
	if _, err := req.Verify(secret, req.Credential.Region, presignedService); err != nil {
		slog.Warn("VerifyPresignedGetCallerIdentity: signature verify failed (cluster name replay or tampered URL)",
			"akid", req.Credential.AccessKeyID,
			"expected_cluster", expectedClusterName,
			"err", err)
		return nil, errors.New(awserrors.ErrorInvalidIdentityToken)
	}

	principal.XK8sAwsID = expectedClusterName
	return principal, nil
}

// mapSigv4Err maps sigv4 sentinel errors onto AWS error codes. Time-window failures
// surface as ExpiredToken; every other structural failure is an invalid token.
func mapSigv4Err(err error) error {
	switch {
	case errors.Is(err, sigv4.ErrPresignedURLExpired), errors.Is(err, sigv4.ErrRequestTimeTooSkewed):
		return errors.New(awserrors.ErrorExpiredToken)
	default:
		return errors.New(awserrors.ErrorInvalidIdentityToken)
	}
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
