// Package gateway_rds implements the RDS surface on awsgw. RDS speaks the AWS
// Query protocol with XML responses (rds-v1.md D2), so this package mirrors the
// ELBv2 gateway shape: an action table, a typed-input adapter that parses query
// params and marshals handler output into the IAM-style XML envelope, and one
// function per action.
//
// The table carries the whole v1 namespace from day one, with the actions whose
// bodies land in a later phase registered as explicit stubs. That keeps an
// unimplemented action distinguishable from an unknown one, and it means a
// client driving the service sees a stable namespace rather than one that grows
// action by action.
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

// Handler processes parsed query args for one action and returns XML response
// bytes. It takes the NATS connection rather than the gateway config because the
// gateway config lives in the parent package; every RDS handler reaches the
// control plane over NATS and needs nothing else from it.
type Handler func(ctx context.Context, action string, q map[string]string, nc *nats.Conn, accountID string) ([]byte, error)

// typed builds a Handler from a typed per-action function: it allocates the
// input struct, parses the query params into it, calls the function and marshals
// the output into the IAM-style XML envelope RDS shares with ELBv2 —
// <ActionResponse><ActionResult>...</ActionResult></ActionResponse>.
func typed[In any](handler func(context.Context, *In, *nats.Conn, string) (any, error)) Handler {
	return func(ctx context.Context, action string, q map[string]string, nc *nats.Conn, accountID string) ([]byte, error) {
		input := new(In)
		if err := awsec2query.QueryParamsToStruct(q, input); err != nil {
			// An over-long indexed list is a client-side malformation, not an
			// internal failure, so it keeps its own error code.
			if errors.Is(err, awsec2query.ErrSliceTooLarge) {
				return nil, errors.New(awserrors.ErrorMalformedQueryString)
			}
			return nil, err
		}
		output, err := handler(ctx, input, nc, accountID)
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

// pending registers an action that belongs to the v1 namespace but whose body
// lands in a later phase. It fails with NotImplemented so a caller learns the
// action is recognised and simply not ready yet — never a silent success.
func pending() Handler {
	return rejectWith(awserrors.ErrorNotImplemented)
}

// unsupported registers an action that is recognised but deliberately outside
// v1 — read replicas, Aurora clusters, option groups, point-in-time restore.
// These fail loudly rather than being left unknown, so a client sees "not
// offered" instead of "you typo'd the action name".
func unsupported() Handler {
	return rejectWith(awserrors.ErrorOperationNotSupported)
}

// rejectWith returns a Handler that rejects every call with the given awserror
// code, without parsing the request body.
func rejectWith(code string) Handler {
	return func(ctx context.Context, action string, _ map[string]string, _ *nats.Conn, _ string) ([]byte, error) {
		slog.DebugContext(ctx, "RDS: action not available", "action", action, "code", code)
		return nil, errors.New(code)
	}
}

// actions is the RDS v1 action namespace (rds-v1.md §1). DescribeDBInstances is
// the one live action in this phase; everything else is a stub.
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
	// They are part of the namespace rather than a private channel because the
	// agent reaches the control plane over SigV4-on-awsgw like every other
	// in-guest agent in the platform.
	"RegisterDBInstance":   pending(),
	"SubmitDBStateChange":  pending(),
	"PollDBCommands":       pending(),
	"GetDBBootstrapConfig": pending(),

	// Recognised but out of v1 scope. Read replicas, Aurora clusters and option
	// groups are not offered at all; point-in-time restore waits on WAL
	// archiving in v1.1.
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
// serves. The dispatcher checks it before the IAM policy check so an unknown
// action is rejected as InvalidAction rather than evaluated — and logged — as a
// denied rds:<garbage> permission.
func HasAction(action string) bool {
	_, ok := actions[action]
	return ok
}

// Dispatch runs the handler for action and returns its XML response body.
// Callers are expected to have authorized the action already; the unknown-action
// check here is a backstop, not the enforcement point.
func Dispatch(ctx context.Context, action string, q map[string]string, nc *nats.Conn, accountID string) ([]byte, error) {
	handler, ok := actions[action]
	if !ok {
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}
	return handler(ctx, action, q, nc, accountID)
}
