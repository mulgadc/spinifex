package gateway_eks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"log/slog"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
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
// the ones naming the customer account in the path or body, which no customer
// grant may reach. Derived from eksScopes so a new such route cannot ship
// ungated — that table is exhaustive by contract against the dispatch table.
func IsInternalAction(action string) bool {
	return slices.ContainsFunc(eksScopes[action], func(source resourceSource) bool {
		return source == sourceInternalCluster || source == sourceInternalBodyCluster
	})
}

// AuthorizeInternal is the principal gate for the internal CP-VM routes, run
// ahead of the policy check. params are the route captures: cluster name,
// account ID, and for GetRecoveryDirective the member's instance ID. body is
// the request body the handler will read; PublishInternal and WebhookTokenReview
// name their account there.
func AuthorizeInternal(ctx context.Context, natsConn *nats.Conn, action string, caller Caller, params []string, body []byte) error {
	if !IsInternalAction(action) {
		return nil
	}
	if err := requireCPAgent(ctx, action, caller); err != nil {
		return err
	}

	clusterName, accountID, err := internalTarget(action, params, body)
	if err != nil {
		return err
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

// internalTarget reads the cluster and owning account an internal route acts
// on, from the same place its handler reads them, so the binding checks the
// account the handler then uses.
func internalTarget(action string, params []string, body []byte) (clusterName, accountID string, err error) {
	clusterName, accountID = param(params, 0), param(params, 1)
	if slices.Contains(eksScopes[action], sourceInternalBodyCluster) {
		readAccount, ok := internalBodyAccounts[action]
		if !ok {
			slog.Error("EKS: internal route has no body account reader", "action", action)
			return "", "", errors.New(awserrors.ErrorServerInternal)
		}
		bodyAccount, decodeErr := readAccount(body)
		if decodeErr != nil {
			return "", "", errors.New(awserrors.ErrorInvalidParameterValue)
		}
		accountID = bodyAccount
	}
	if clusterName == "" || accountID == "" {
		return "", "", errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return clusterName, accountID, nil
}

// internalBodyAccounts reads the owning account of each body-scoped internal
// route through the same decoder its handler uses.
var internalBodyAccounts = map[string]func(body []byte) (string, error){
	"PublishInternal": func(body []byte) (string, error) {
		req, err := decodeInternalPublish(body)
		return req.AccountID, err
	},
	"WebhookTokenReview": func(body []byte) (string, error) {
		req, err := decodeWebhookTokenReview(body)
		return req.AccountID, err
	},
}

// The class check on its own: a session assumed from the CP VM's instance role
// in the system account. It says the caller is a control-plane VM, not which
// one — the binding to a cluster is AuthorizeInternal's, and needs NATS.
func requireCPAgent(ctx context.Context, action string, caller Caller) error {
	if caller.PrincipalType != principalTypeAssumedRole ||
		caller.AccountID != awsidentifiers.GlobalAccountID ||
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
// for the bucket handle. A nil meta is returned for an account with no clusters,
// for a cluster that is absent, and for a name no bucket or key can hold; all
// are denials rather than failures.
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
		if errors.Is(err, jetstream.ErrBucketNotFound) || errors.Is(err, jetstream.ErrInvalidBucketName) {
			return nil, nil
		}
		return nil, err
	}
	entry, err := kv.Get(ctx, handlers_eks.ClusterMetaKey(clusterName))
	if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrInvalidKey) {
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
