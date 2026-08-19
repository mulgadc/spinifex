package gateway_rds

import (
	"context"
	"errors"
	"log/slog"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// The resource a policy check evaluates against when the request names nothing
// in particular: every create, and the describes that filter rather than
// address.
const anyResource = "*"

// Which query parameter names a resource an action acts on, and the ARN type it
// resolves to. Actions carry one scope for each resource IAM evaluates.
type resourceScope struct {
	param string
	// Empty when param already carries a full ARN — the tag actions — which is
	// validated rather than built.
	kind handlers_rds.ResourceKind
}

var (
	dbInstanceScope       = &resourceScope{param: "DBInstanceIdentifier", kind: handlers_rds.ResourceKindDBInstance}
	dbSnapshotScope       = &resourceScope{param: "DBSnapshotIdentifier", kind: handlers_rds.ResourceKindDBSnapshot}
	dbSubnetGroupScope    = &resourceScope{param: "DBSubnetGroupName", kind: handlers_rds.ResourceKindDBSubnetGroup}
	dbParameterGroupScope = &resourceScope{param: "DBParameterGroupName", kind: handlers_rds.ResourceKindDBParameterGroup}
	taggedResourceScope   = &resourceScope{param: "ResourceName"}
)

// AuthorizeCaller is the principal-class gate, run before the policy check.
// Internal actions are agent-only, and no customer grant can reach them: rds:* is
// a legitimate admin grant, and GetDBBootstrapConfig returns a master password.
func AuthorizeCaller(ctx context.Context, action string, caller Caller) error {
	def, ok := actions[action]
	if !ok {
		return errors.New(awserrors.ErrorInvalidAction)
	}
	if !def.internal {
		return nil
	}
	return requireAgentPrincipal(ctx, caller)
}

// The class check on its own: a session assumed from the DB VM's instance role
// in the system account. It says the caller is an RDS VM, not which one — the
// binding to a specific DB instance is authorizeAgent's, and needs NATS.
func requireAgentPrincipal(ctx context.Context, caller Caller) error {
	if caller.PrincipalType != principalTypeAssumedRole ||
		caller.AccountID != utils.GlobalAccountID ||
		caller.RoleName != handlers_rds.InstanceRoleName ||
		caller.SessionName == "" {
		slog.DebugContext(ctx, "RDS: internal action rejected for non-agent caller",
			"principalType", caller.PrincipalType, "accountID", caller.AccountID, "roleName", caller.RoleName)
		return errors.New(awserrors.ErrorAccessDenied)
	}
	return nil
}

// ResourceARN builds every resource the action's policy checks evaluate. A
// policy written for arn:aws:rds:*:*:db:prod-* but checked against "*" would
// permit every instance in the account, which is worse than no policy at all.
func ResourceARN(action, region, accountID string, q map[string]string) ([]string, error) {
	def, ok := actions[action]
	// Rejected here too, not just by the dispatcher: a caller that skipped the
	// action check must not get a resource back for rds:<garbage>.
	if !ok {
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}
	if len(def.scopes) == 0 {
		return []string{anyResource}, nil
	}

	resources := make([]string, 0, len(def.scopes))
	for _, scope := range def.scopes {
		identifier := q[scope.param]
		// A missing member contributes "*" independently. It remains the handler's
		// validation fault rather than becoming an ARN resolution failure here.
		if identifier == "" {
			resources = append(resources, anyResource)
			continue
		}
		if scope.kind == "" {
			// Parsed rather than passed through, so a foreign account or region is an
			// InvalidParameterValue before the evaluator sees it.
			if _, err := handlers_rds.ParseARN(identifier, region, accountID); err != nil {
				return nil, err
			}
			resources = append(resources, identifier)
			continue
		}
		resources = append(resources, handlers_rds.FormatARN(scope.kind, region, accountID, identifier))
	}
	return resources, nil
}
