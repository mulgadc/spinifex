package gateway_eks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Only an assumed-role session can be a control-plane VM.
const principalTypeAssumedRole = "assumed-role"

// Caller is the principal behind an EKS request. The internal routes have to
// tell the CP VM's instance role apart from a user, which the account alone
// cannot do, and then tell one VM from another.
type Caller struct {
	AccountID     string
	PrincipalType string
	RoleName      string
	// The RoleSessionName of an assumed-role session. For IMDS instance-role
	// credentials it is the internal EC2 instance ID.
	SessionName string
}

// IsInternalAction reports whether action is one of the internal CP-VM routes:
// the ones naming the customer account in the path, which no customer grant may
// reach. Derived from eksScopes so a new such route cannot ship ungated — that
// table is exhaustive by contract against the dispatch table.
func IsInternalAction(action string) bool {
	return slices.Contains(eksScopes[action], sourceInternalCluster)
}

// AuthorizeInternal is the principal gate for the internal CP-VM routes, run
// ahead of the policy check. params are the route captures: cluster name,
// account ID, and for GetRecoveryDirective the member's instance ID.
func AuthorizeInternal(ctx context.Context, natsConn *nats.Conn, action string, caller Caller, params []string) error {
	if !IsInternalAction(action) {
		return nil
	}
	if err := requireCPAgent(ctx, action, caller); err != nil {
		return err
	}

	clusterName, accountID := param(params, 0), param(params, 1)
	if clusterName == "" || accountID == "" {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}

	// A member reads its own directive and no other's: the path segment is only
	// ever used to reject a mismatch, since the caller's own instance ID is the
	// one the credentials prove.
	if action == "GetRecoveryDirective" && param(params, 2) != caller.SessionName {
		slog.WarnContext(ctx, "EKS: CP agent asked for another member's recovery directive",
			"callerInstanceID", caller.SessionName, "requested", param(params, 2))
		return errors.New(awserrors.ErrorAccessDenied)
	}

	meta, err := lookupClusterMeta(ctx, natsConn, accountID, clusterName)
	if err != nil {
		slog.ErrorContext(ctx, "EKS: internal route cluster-meta lookup failed",
			"action", action, "accountID", accountID, "cluster", clusterName, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	// An absent cluster and a cluster the caller does not serve are the same
	// denial: either way this caller is not a member of what it named.
	if !meta.IsControlPlaneMember(caller.SessionName) {
		slog.WarnContext(ctx, "EKS: internal route named a cluster the caller does not serve",
			"action", action, "callerInstanceID", caller.SessionName, "accountID", accountID, "cluster", clusterName)
		return errors.New(awserrors.ErrorAccessDenied)
	}
	return nil
}

// The class check on its own: a session assumed from the CP VM's instance role
// in the system account. It says the caller is a control-plane VM, not which
// one — the binding to a cluster is AuthorizeInternal's, and needs NATS.
func requireCPAgent(ctx context.Context, action string, caller Caller) error {
	if caller.PrincipalType != principalTypeAssumedRole ||
		caller.AccountID != utils.GlobalAccountID ||
		caller.RoleName != handlers_eks.CPInstanceRoleName ||
		caller.SessionName == "" {
		slog.WarnContext(ctx, "EKS: internal route rejected for non-CP-agent caller",
			"action", action, "principalType", caller.PrincipalType,
			"accountID", caller.AccountID, "roleName", caller.RoleName)
		return errors.New(awserrors.ErrorAccessDenied)
	}
	return nil
}

// Reads JetStream directly, matching the OIDC discovery path, rather than a
// service round trip through the handler. js.KeyValue still costs a STREAM.INFO
// for the bucket handle. A nil meta is returned for an account with no clusters
// and for a cluster that is absent; both are denials rather than failures.
func lookupClusterMeta(ctx context.Context, natsConn *nats.Conn, accountID, clusterName string) (*handlers_eks.ClusterMeta, error) {
	if natsConn == nil {
		return nil, errors.New("gateway NATS connection not initialised")
	}
	js, err := jetstream.New(natsConn)
	if err != nil {
		return nil, err
	}
	kv, err := js.KeyValue(ctx, handlers_eks.AccountBucketName(accountID))
	if err != nil {
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			return nil, nil
		}
		return nil, err
	}
	entry, err := kv.Get(ctx, handlers_eks.ClusterMetaKey(clusterName))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var meta handlers_eks.ClusterMeta
	if err := json.Unmarshal(entry.Value(), &meta); err != nil {
		return nil, fmt.Errorf("unmarshal cluster meta %s: %w", clusterName, err)
	}
	return &meta, nil
}
