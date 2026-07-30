// Package gateway_rds implements the RDS surface on awsgw. RDS speaks the AWS
// Query protocol, so this package mirrors the ELBv2 gateway shape: an action
// table, a typed-input adapter, and one function per action.
package gateway_rds

import (
	"context"
	"errors"
	"log/slog"

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

type Handler func(ctx context.Context, action string, q map[string]string, nc *nats.Conn, caller Caller) ([]byte, error)

// Allocates the input struct, parses the query params into it, calls handler and
// marshals the output into the IAM-style <ActionResponse><ActionResult> envelope.
func typed[In any](handler func(context.Context, *In, *nats.Conn, Caller) (any, error)) Handler {
	return func(ctx context.Context, action string, q map[string]string, nc *nats.Conn, caller Caller) ([]byte, error) {
		input := new(In)
		if err := awsec2query.QueryParamsToStruct(q, input); err != nil {
			// An over-long indexed list is a client-side malformation, not an
			// internal failure, so it keeps its own error code.
			if errors.Is(err, awsec2query.ErrSliceTooLarge) {
				return nil, errors.New(awserrors.ErrorMalformedQueryString)
			}
			return nil, err
		}
		output, err := handler(ctx, input, nc, caller)
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

// Recognised but deliberately outside v1, so a client sees "not offered" rather
// than "you typo'd the action name".
func unsupported() Handler {
	return rejectWith(awserrors.ErrorOperationNotSupported)
}

func rejectWith(code string) Handler {
	return func(ctx context.Context, action string, _ map[string]string, _ *nats.Conn, _ Caller) ([]byte, error) {
		slog.DebugContext(ctx, "RDS: action not available", "action", action, "code", code)
		return nil, errors.New(code)
	}
}

// One entry per action: what serves it, whether it is agent-only, and which
// resource its policy check evaluates against. Both authorization facts live on
// the table so a new action cannot be added without deciding either.
type actionDef struct {
	handler Handler
	// Agent-only: refused to every customer principal by class, before any
	// policy is evaluated.
	internal bool
	// nil evaluates against "*", which is right for creates and for describes
	// that filter rather than address one resource.
	scope *resourceScope
}

// The whole namespace is registered from day one, so an action outside v1 stays
// distinct from an unknown one.
var actions = map[string]actionDef{
	// Instance lifecycle.
	"CreateDBInstance":    {handler: typed(CreateDBInstance)},
	"DescribeDBInstances": {handler: typed(DescribeDBInstances)},
	"ModifyDBInstance":    {handler: typed(ModifyDBInstance), scope: dbInstanceScope},
	"DeleteDBInstance":    {handler: typed(DeleteDBInstance), scope: dbInstanceScope},
	"RebootDBInstance":    {handler: typed(RebootDBInstance), scope: dbInstanceScope},
	"StartDBInstance":     {handler: typed(StartDBInstance), scope: dbInstanceScope},
	"StopDBInstance":      {handler: typed(StopDBInstance), scope: dbInstanceScope},

	// Snapshots. A create names two resources — the source instance and the new
	// snapshot — so it is not scoped to either.
	"CreateDBSnapshot":                {handler: typed(CreateDBSnapshot)},
	"DescribeDBSnapshots":             {handler: typed(DescribeDBSnapshots)},
	"DeleteDBSnapshot":                {handler: typed(DeleteDBSnapshot), scope: dbSnapshotScope},
	"RestoreDBInstanceFromDBSnapshot": {handler: typed(RestoreDBInstanceFromDBSnapshot)},

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

	// Internal agent actions, callable only by the in-guest agent's system role.
	// They share the namespace because the agent reaches the control plane over
	// SigV4-on-awsgw like every other in-guest agent.
	"RegisterDBInstance":   {handler: typed(RegisterDBInstance), internal: true},
	"SubmitDBStateChange":  {handler: typed(SubmitDBStateChange), internal: true},
	"PollDBCommands":       {handler: typed(PollDBCommands), internal: true},
	"GetDBBootstrapConfig": {handler: typed(GetDBBootstrapConfig), internal: true},

	// Recognised but out of scope. Read replicas, Aurora clusters and option
	// groups are not offered at all; point-in-time restore waits on WAL
	// archiving.
	"CreateDBInstanceReadReplica":    {handler: unsupported()},
	"PromoteReadReplica":             {handler: unsupported()},
	"CreateDBCluster":                {handler: unsupported()},
	"ModifyDBCluster":                {handler: unsupported()},
	"DeleteDBCluster":                {handler: unsupported()},
	"DescribeDBClusters":             {handler: unsupported()},
	"FailoverDBCluster":              {handler: unsupported()},
	"CreateOptionGroup":              {handler: unsupported()},
	"ModifyOptionGroup":              {handler: unsupported()},
	"DeleteOptionGroup":              {handler: unsupported()},
	"DescribeOptionGroups":           {handler: unsupported()},
	"RestoreDBInstanceToPointInTime": {handler: unsupported()},
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
func Dispatch(ctx context.Context, action string, q map[string]string, nc *nats.Conn, caller Caller) ([]byte, error) {
	def, ok := actions[action]
	if !ok {
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}
	return def.handler(ctx, action, q, nc, caller)
}
