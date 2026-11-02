package pipeline

import (
	"errors"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// ActorFor projects a verified caller onto the actor a service receives. The
// actor is fully populated even when err is set: err reports that no caller ARN
// could be composed, so a reader that needs no ARN may ignore it.
func ActorFor(p credential.Principal, accessKeyID string) (dispatch.Actor, error) {
	callerARN := p.CallerARN
	var err error
	if callerARN == "" {
		slog.Error("caller ARN could not be composed", "principalType", p.PrincipalType)
		err = errors.New(awserrors.ErrorInternalError)
	}
	actor := dispatch.Actor{
		AccountID:     p.AccountID,
		CallerARN:     callerARN,
		PrincipalType: p.PrincipalType,
		AccessKeyID:   accessKeyID,
		UserID:        p.UserID,
	}
	if p.PrincipalType == dispatch.PrincipalTypeAssumedRole {
		actor.PrincipalARN = p.UnderlyingRoleARN
		actor.SessionName = p.Identity
	} else {
		actor.PrincipalARN = callerARN
		actor.UserName = p.Identity
	}
	return actor, err
}
