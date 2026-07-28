// Package gateway_rds implements the RDS surface on awsgw. RDS speaks the AWS
// Query protocol, so this package mirrors the ELBv2 gateway shape: an action
// table, a typed-input adapter, and one function per action.
//
// The table carries the whole namespace from day one, registering not-yet-built
// actions as explicit stubs, so an unimplemented action stays distinguishable
// from an unknown one.
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

// principalTypeAssumedRole is the ctxPrincipalType value for an assumed-role
// session. Only such a session can be an in-guest agent.
const principalTypeAssumedRole = "assumed-role"

// Caller is the authenticated identity behind one RDS request. Customer actions
// need only AccountID; the internal agent actions have to tell an instance role
// apart from a user, which the account alone cannot do.
type Caller struct {
	AccountID     string
	PrincipalType string
	RoleName      string
	// SessionName is the RoleSessionName of an assumed-role session. For IMDS
	// instance-role credentials it is the internal EC2 instance ID.
	SessionName string
}

// Handler processes parsed query args for one action and returns XML response
// bytes. It takes the NATS connection rather than the gateway config, which
// lives in the parent package and offers nothing else a handler needs.
type Handler func(ctx context.Context, action string, q map[string]string, nc *nats.Conn, caller Caller) ([]byte, error)

// typed builds a Handler from a typed per-action function: it allocates the
// input struct, parses the query params into it, calls the function and marshals
// the output into the IAM-style <ActionResponse><ActionResult> envelope.
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

// pending registers a namespace action whose body lands in a later phase. It
// fails with NotImplemented, so a caller learns the action is recognised and
// simply not ready — never a silent success.
func pending() Handler {
	return rejectWith(awserrors.ErrorNotImplemented)
}

// unsupported registers an action that is recognised but deliberately outside
// v1. These fail loudly, so a client sees "not offered" rather than "you typo'd
// the action name".
func unsupported() Handler {
	return rejectWith(awserrors.ErrorOperationNotSupported)
}

// rejectWith returns a Handler that rejects every call with the given awserror
// code, without parsing the request body.
func rejectWith(code string) Handler {
	return func(ctx context.Context, action string, _ map[string]string, _ *nats.Conn, _ Caller) ([]byte, error) {
		slog.DebugContext(ctx, "RDS: action not available", "action", action, "code", code)
		return nil, errors.New(code)
	}
}

// actions is the RDS v1 action namespace. DescribeDBInstances and the internal
// agent actions are live; everything else is a stub.
var actions = map[string]Handler{
	// Instance lifecycle.
	"CreateDBInstance":    pending(),
	"DescribeDBInstances": typed(DescribeDBInstances),
	"ModifyDBInstance":    pending(),
	"DeleteDBInstance":    pending(),
	"RebootDBInstance":    pending(),
	"StartDBInstance":     pending(),
	"StopDBInstance":      pending(),

	// Snapshots.
	"CreateDBSnapshot":                pending(),
	"DescribeDBSnapshots":             pending(),
	"DeleteDBSnapshot":                pending(),
	"RestoreDBInstanceFromDBSnapshot": pending(),

	// Automated backups.
	"DescribeDBInstanceAutomatedBackups": pending(),

	// Subnet groups.
	"CreateDBSubnetGroup":    pending(),
	"DescribeDBSubnetGroups": pending(),
	"DeleteDBSubnetGroup":    pending(),

	// Parameter groups.
	"CreateDBParameterGroup":    pending(),
	"DescribeDBParameterGroups": pending(),
	"ModifyDBParameterGroup":    pending(),
	"DescribeDBParameters":      pending(),
	"DeleteDBParameterGroup":    pending(),

	// Tags.
	"AddTagsToResource":      pending(),
	"RemoveTagsFromResource": pending(),
	"ListTagsForResource":    pending(),

	// Events.
	"DescribeEvents": pending(),

	// Internal agent actions, callable only by the in-guest agent's system role.
	// They share the namespace because the agent reaches the control plane over
	// SigV4-on-awsgw like every other in-guest agent.
	"RegisterDBInstance":   typed(RegisterDBInstance),
	"SubmitDBStateChange":  typed(SubmitDBStateChange),
	"PollDBCommands":       typed(PollDBCommands),
	"GetDBBootstrapConfig": typed(GetDBBootstrapConfig),

	// Recognised but out of scope. Read replicas, Aurora clusters and option
	// groups are not offered at all; point-in-time restore waits on WAL
	// archiving.
	"CreateDBInstanceReadReplica":    unsupported(),
	"PromoteReadReplica":             unsupported(),
	"CreateDBCluster":                unsupported(),
	"ModifyDBCluster":                unsupported(),
	"DeleteDBCluster":                unsupported(),
	"DescribeDBClusters":             unsupported(),
	"FailoverDBCluster":              unsupported(),
	"CreateOptionGroup":              unsupported(),
	"ModifyOptionGroup":              unsupported(),
	"DeleteOptionGroup":              unsupported(),
	"DescribeOptionGroups":           unsupported(),
	"RestoreDBInstanceToPointInTime": unsupported(),
}

// HasAction reports whether action is part of the RDS namespace this gateway
// serves. The dispatcher checks it before the IAM policy check, so an unknown
// action is rejected as InvalidAction rather than logged as a denial.
func HasAction(action string) bool {
	_, ok := actions[action]
	return ok
}

// Dispatch runs the handler for action and returns its XML response body.
// Callers are expected to have authorized the action already; the unknown-action
// check here is a backstop, not the enforcement point.
func Dispatch(ctx context.Context, action string, q map[string]string, nc *nats.Conn, caller Caller) ([]byte, error) {
	handler, ok := actions[action]
	if !ok {
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}
	return handler(ctx, action, q, nc, caller)
}
