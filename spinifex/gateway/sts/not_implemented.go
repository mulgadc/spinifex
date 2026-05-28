// Package gateway_sts wires the STS action handlers into the AWS gateway.
// v1 ships AssumeRole + GetCallerIdentity as live actions and registers 501
// stubs for every other STS action so follow-on work can slot in handler
// bodies without touching the dispatch map.
package gateway_sts

import (
	"errors"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// NotImplemented is the body for STS actions that are registered in the
// dispatch map but not yet implemented (GetSessionToken,
// AssumeRoleWithWebIdentity, AssumeRoleWithSAML, GetAccessKeyInfo,
// GetFederationToken, DecodeAuthorizationMessage). Returns the
// NotImplementedException code which the error-response builder turns into a
// 501 — matches AWS' behaviour for unimplemented actions on an endpoint that
// accepts the action verb at the dispatch layer.
//
// Action policy gating happens at the dispatcher before this handler runs, so
// a denied principal sees AccessDenied rather than NotImplementedException —
// matching AWS where authorization always precedes implementation lookup.
func NotImplemented() (any, error) {
	return nil, errors.New(awserrors.ErrorSTSNotImplemented)
}
