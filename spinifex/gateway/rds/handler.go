// Package gateway_rds implements the RDS surface on awsgw. RDS speaks the AWS
// Query protocol, so this package mirrors the ELBv2 gateway shape: an action
// table, a typed-input adapter, and one function per action.
package gateway_rds

import (
	"context"
	"errors"
	"log/slog"
	"sort"

	"github.com/mulgadc/spinifex/spinifex/awsec2query"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// Only an assumed-role session can be an in-guest agent.
const principalTypeAssumedRole = "assumed-role"

// Customer actions need only AccountID; the internal agent actions have to tell
// an instance role apart from a user, which the account alone cannot do.
type Caller struct {
	AccountID     string
	PrincipalType string
	RoleName      string
	// The RoleSessionName of an assumed-role session. For IMDS instance-role
	// credentials it is the internal EC2 instance ID.
	SessionName string
}

// Cluster facts a handler needs that the request cannot carry. It holds the
// ingredient rather than a ready-made probe, so the next action needing a
// fan-out gets it without another parameter.
type Env struct {
	// How many nodes a fan-out waits for before it stops waiting. Without it a
	// gather burns its full timeout on every call instead of early-exiting.
	ExpectedNodes int

	// QuotaCheck gates a create on the account's DB-instance cap. It is a
	// function so this package stays free of the quota dependency; nil skips
	// the check, which is what a gateway with quotas disabled supplies.
	QuotaCheck func(ctx context.Context, accountID string, want int) error
}

type Handler func(ctx context.Context, action string, q map[string]string, nc *nats.Conn, caller Caller, env Env) ([]byte, error)

// Allocates the input struct, parses the query params into it, calls handler and
// marshals the output into the IAM-style <ActionResponse><ActionResult> envelope.
func typedEnv[In any](handler func(context.Context, *In, *nats.Conn, Caller, Env) (any, error)) Handler {
	return func(ctx context.Context, action string, q map[string]string, nc *nats.Conn, caller Caller, env Env) ([]byte, error) {
		input := new(In)
		if err := awsec2query.QueryParamsToStruct(q, input); err != nil {
			// An over-long indexed list is a client-side malformation, not an
			// internal failure, so it keeps its own error code.
			if errors.Is(err, awsec2query.ErrSliceTooLarge) {
				return nil, errors.New(awserrors.ErrorMalformedQueryString)
			}
			return nil, errors.New(awserrors.ErrorInvalidParameterValue)
		}
		output, err := handler(ctx, input, nc, caller, env)
		if err != nil {
			return nil, err
		}
		payload := utils.GenerateIAMXMLPayload(action, output)
		xmlOutput, err := utils.MarshalToXML(payload)
		if err != nil {
			return nil, errors.New("failed to marshal response to XML")
		}
		return xmlOutput, nil
	}
}

// The same adapter for the majority of actions, which address one account's own
// state and need nothing from the cluster beyond the NATS connection.
func typed[In any](handler func(context.Context, *In, *nats.Conn, Caller) (any, error)) Handler {
	return typedEnv(func(ctx context.Context, input *In, nc *nats.Conn, caller Caller, _ Env) (any, error) {
		return handler(ctx, input, nc, caller)
	})
}

// One entry per action: what serves it, whether it is agent-only, and which
// resource its policy check evaluates against. Both authorization facts live on
// the table so a new action cannot be added without deciding either.
type actionDef struct {
	handler Handler
	// Agent-only: refused to every customer principal by class, before any
	// policy is evaluated.
	internal bool
	// Recognised but deliberately outside v1, so a client sees "not offered"
	// rather than "you typo'd the action name". Carries no handler.
	unsupported bool
	// nil evaluates against "*", which is right for creates and for describes
	// that filter rather than address one resource.
	scope *resourceScope
}

// The whole namespace is registered from day one, so an action outside v1 stays
// distinct from an unknown one.
var actions = map[string]actionDef{
	// Instance lifecycle.
	"CreateDBInstance":    {handler: typedEnv(CreateDBInstance)},
	"DescribeDBInstances": {handler: typed(DescribeDBInstances)},
	"ModifyDBInstance":    {handler: typed(ModifyDBInstance), scope: dbInstanceScope},
	"DeleteDBInstance":    {handler: typed(DeleteDBInstance), scope: dbInstanceScope},
	"RebootDBInstance":    {handler: typed(RebootDBInstance), scope: dbInstanceScope},
	"StartDBInstance":     {handler: typed(StartDBInstance), scope: dbInstanceScope},
	"StopDBInstance":      {handler: typed(StopDBInstance), scope: dbInstanceScope},

	// Snapshots. A create and a restore each name two resources, and both are
	// scoped to the source: a deny written on an instance has to stop it being
	// snapshotted, and a deny on a snapshot has to stop it being restored.
	"CreateDBSnapshot":                {handler: typed(CreateDBSnapshot), scope: dbInstanceScope},
	"DescribeDBSnapshots":             {handler: typed(DescribeDBSnapshots)},
	"DeleteDBSnapshot":                {handler: typed(DeleteDBSnapshot), scope: dbSnapshotScope},
	"RestoreDBInstanceFromDBSnapshot": {handler: typed(RestoreDBInstanceFromDBSnapshot), scope: dbSnapshotScope},

	// Automated backups.
	"DescribeDBInstanceAutomatedBackups": {handler: typed(DescribeDBInstanceAutomatedBackups)},

	// Subnet groups.
	"CreateDBSubnetGroup":    {handler: typed(CreateDBSubnetGroup)},
	"DescribeDBSubnetGroups": {handler: typed(DescribeDBSubnetGroups)},
	"DeleteDBSubnetGroup":    {handler: typed(DeleteDBSubnetGroup), scope: dbSubnetGroupScope},

	// Parameter groups. DescribeDBParameters is scoped despite being a describe:
	// its parameter group is required and singular, so it addresses one resource.
	"CreateDBParameterGroup":    {handler: typed(CreateDBParameterGroup)},
	"DescribeDBParameterGroups": {handler: typed(DescribeDBParameterGroups)},
	"ModifyDBParameterGroup":    {handler: typed(ModifyDBParameterGroup), scope: dbParameterGroupScope},
	"DescribeDBParameters":      {handler: typed(DescribeDBParameters), scope: dbParameterGroupScope},
	"DeleteDBParameterGroup":    {handler: typed(DeleteDBParameterGroup), scope: dbParameterGroupScope},

	// Tags. The request names its resource by ARN, so the scope validates one
	// rather than building it.
	"AddTagsToResource":      {handler: typed(AddTagsToResource), scope: taggedResourceScope},
	"RemoveTagsFromResource": {handler: typed(RemoveTagsFromResource), scope: taggedResourceScope},
	"ListTagsForResource":    {handler: typed(ListTagsForResource), scope: taggedResourceScope},

	// Events. The ring is per-account and a filter names no single resource.
	"DescribeEvents": {handler: typed(DescribeEvents)},

	// Catalogs. Neither reads per-account state, so both evaluate against "*";
	// the orderable one reads cluster capability, which is not tenant data either.
	"DescribeDBEngineVersions":           {handler: typed(DescribeDBEngineVersions)},
	"DescribeOrderableDBInstanceOptions": {handler: typedEnv(DescribeOrderableDBInstanceOptions)},

	// Internal agent actions, callable only by the in-guest agent's system role.
	// They share the namespace because the agent reaches the control plane over
	// SigV4-on-awsgw like every other in-guest agent.
	"RegisterDBInstance":     {handler: typed(RegisterDBInstance), internal: true},
	"SubmitDBStateChange":    {handler: typed(SubmitDBStateChange), internal: true},
	"PollDBCommands":         {handler: typed(PollDBCommands), internal: true},
	"GetDBBootstrapConfig":   {handler: typed(GetDBBootstrapConfig), internal: true},
	"AcknowledgeDBBootstrap": {handler: typed(AcknowledgeDBBootstrap), internal: true},

	// Recognised but out of scope. Read replicas, Aurora clusters and option
	// groups are not offered at all; point-in-time restore waits on WAL
	// archiving.
	"CreateDBInstanceReadReplica":    {unsupported: true},
	"PromoteReadReplica":             {unsupported: true},
	"CreateDBCluster":                {unsupported: true},
	"ModifyDBCluster":                {unsupported: true},
	"DeleteDBCluster":                {unsupported: true},
	"DescribeDBClusters":             {unsupported: true},
	"FailoverDBCluster":              {unsupported: true},
	"CreateOptionGroup":              {unsupported: true},
	"ModifyOptionGroup":              {unsupported: true},
	"DeleteOptionGroup":              {unsupported: true},
	"DescribeOptionGroups":           {unsupported: true},
	"RestoreDBInstanceToPointInTime": {unsupported: true},
}

// Checked before the IAM policy check, so an unknown action is rejected as
// InvalidAction rather than logged as a denial.
func HasAction(action string) bool {
	_, ok := actions[action]
	return ok
}

// Callers are expected to have authorized the action already; the unknown-action
// check here is a backstop, not the enforcement point. The internal actions
// re-run their own gate, so skipping AuthorizeCaller still cannot reach one.
func Dispatch(ctx context.Context, action string, q map[string]string, nc *nats.Conn, caller Caller, env Env) ([]byte, error) {
	def, ok := actions[action]
	if !ok {
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}
	if def.unsupported {
		slog.DebugContext(ctx, "RDS: action not available", "action", action)
		return nil, errors.New(awserrors.ErrorOperationNotSupported)
	}
	return def.handler(ctx, action, q, nc, caller, env)
}

// ActionNames returns every action the gateway recognises, in stable order.
func ActionNames() []string {
	return actionNames(func(actionDef) bool { return true })
}

// UnsupportedActionNames returns the recognised actions that deliberately
// report the feature as unavailable rather than serving it.
func UnsupportedActionNames() []string {
	return actionNames(func(def actionDef) bool { return def.unsupported })
}

func actionNames(match func(actionDef) bool) []string {
	names := make([]string, 0, len(actions))
	for name, def := range actions {
		if match(def) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
