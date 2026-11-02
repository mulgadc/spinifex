package gwfix

import (
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
)

var _ credential.Verifier = (*Verifier)(nil)

// Verifier authenticates a long-lived access key as the IAM user the key names.
// It decides nothing else: no status, session or account check, so a test that
// asserts a credential verdict must use the real verifier instead.
type Verifier struct {
	IAM iamdomain.IAMService
}

func (v *Verifier) Authenticate(req credential.Request, verifySig func(secret string) error) (credential.Authenticated, *credential.Failure) {
	ak, err := v.IAM.LookupAccessKey(req.AccessKeyID)
	if err != nil {
		return credential.Authenticated{}, &credential.Failure{Code: awserrors.ErrorInvalidClientTokenId}
	}
	secret, err := v.IAM.DecryptSecret(ak.SecretAccessKey)
	if err != nil {
		return credential.Authenticated{}, &credential.Failure{Code: awserrors.ErrorInternalError}
	}
	if err := verifySig(secret); err != nil {
		return credential.Authenticated{}, &credential.Failure{Code: awserrors.ErrorSignatureDoesNotMatch, SignatureMismatch: true}
	}
	return credential.Authenticated{AccountID: ak.AccountID, Handle: ak}, nil
}

func (v *Verifier) Resolve(_ credential.Request, auth credential.Authenticated) (credential.Principal, *credential.Failure) {
	ak, ok := auth.Handle.(*iamdomain.AccessKey)
	if !ok {
		return credential.Principal{}, &credential.Failure{Code: awserrors.ErrorInternalError}
	}
	return credential.Principal{
		Identity:      ak.UserName,
		AccountID:     ak.AccountID,
		PrincipalType: PrincipalTypeUser,
		CallerARN:     "arn:aws:iam::" + ak.AccountID + ":user/" + ak.UserName,
	}, nil
}
