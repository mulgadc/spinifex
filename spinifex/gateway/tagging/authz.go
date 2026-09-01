package gateway_tagging

import (
	"errors"
	"log/slog"
	"maps"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// The resource a policy check evaluates against when AWS documents no resource
// type for the action, or when the identifier cannot be resolved at gate time.
const anyResource = "*"

// Where an action's resource is named. Only one member today; the type exists
// so a scoped action reads the same way here as it does in the other services.
type resourceSource uint8

const (
	sourceAny resourceSource = iota
)

// Every action Tagging_Request serves, with the resource AWS evaluates it
// against. Exhaustive by contract: a completeness test compares this table with
// the dispatch table in both directions, so an action cannot be added with a
// silent account-wide grant.
var taggingScopes = map[string]resourceSource{
	// Account-level. The Resource Groups Tagging API defines no resource types,
	// so AWS evaluates every tag: action against "*". GetResources aggregates
	// the account's EC2 and ELBv2 resources but is not scoped to them.
	"GetResources": sourceAny,
}

// HasScope reports whether action has an explicit tagging scope-table entry.
// taggingActions lives in package gateway, so the completeness test calls this
// rather than reaching into the table.
func HasScope(action string) bool {
	_, ok := taggingScopes[action]
	return ok
}

// ScopedActions returns every action represented in the tagging scope table.
func ScopedActions() []string {
	return slices.Sorted(maps.Keys(taggingScopes))
}

// ResourceARNs resolves the resources a tagging request authorizes against.
// It takes no region, account or body because every action AWS defines for this
// service is account-level; adding a scoped one means widening this signature,
// which is a compile error at the dispatcher rather than a silent "*".
func ResourceARNs(action string) ([]string, error) {
	source, ok := taggingScopes[action]
	if !ok {
		slog.Error("tagging authz: action is served but absent from the scope table", "action", action)
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}

	switch source {
	case sourceAny:
		return []string{anyResource}, nil

	default:
		slog.Error("tagging authz: unhandled resource source, failing closed", "action", action, "source", source)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
}
