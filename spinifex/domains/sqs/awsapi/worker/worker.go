// Package worker is the SQS worker's private control-plane surface. It is
// served under its own SigV4 signing service, apart from the public AmazonSQS
// action inventory, and admits only the Region's current SQS worker VM.
package worker

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/firststore"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerrole"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/query"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
)

// The contract lives in workerrole so the role grant can read it without
// importing this adapter.
const (
	ServiceName                 = workerrole.ServiceName
	WorkerRoleName              = workerrole.RoleName
	ActionClaimFirstStore       = workerrole.ActionClaimFirstStore
	ActionRecordWorkerPublicKey = workerrole.ActionRecordWorkerPublicKey
)

const principalTypeAssumedRole = "assumed-role"

// params lists the only parameters each action accepts. Identity is never a
// parameter: it is derived from the session and the current worker record.
var params = map[string][]string{
	ActionClaimFirstStore:       {},
	ActionRecordWorkerPublicKey: {"PublicKey"},
}

// ActionNames lists every action the registration dispatches, sorted.
func ActionNames() []string {
	return slices.Sorted(maps.Keys(params))
}

// Identities resolves a worker session to its current authoritative identity.
type Identities interface {
	Authorize(ctx context.Context, instanceID string) (workerident.Identity, error)
}

// FirstStore is the first-store decision the worker's key handling consults.
type FirstStore interface {
	Claim(ctx context.Context, region string, id firststore.Identity) (firststore.Decision, error)
	RecordPublicKey(ctx context.Context, region string, id firststore.Identity, pub string) error
}

var (
	_ Identities = workerident.Store{}
	_ FirstStore = firststore.Store{}
)

// Deps are the capabilities the actions run against. Either one nil answers
// every authorized request with ServiceUnavailable, never a decision.
type Deps struct {
	Identities Identities
	FirstStore FirstStore
}

// NewRegistration returns the internal SQS worker Query registration.
func NewRegistration(deps Deps) dispatch.Registration {
	return dispatch.Registration{
		Service:       ServiceName,
		ResolveAction: resolveAction,
		Dispatch:      deps.dispatch,
		Errors:        dispatch.ErrorEnvelopeXML,
		Inventory:     dispatch.Inventory{Registered: ActionNames()},
	}
}

func resolveAction(r *http.Request) string {
	args, err := readQueryArgs(r)
	if err != nil {
		return ""
	}
	return args["Action"]
}

// readQueryArgs parses the Query body and rewinds it for a second read.
func readQueryArgs(r *http.Request) (map[string]string, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, sigv4.MaxPayloadLen+1))
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if int64(len(body)) > sigv4.MaxPayloadLen {
		return nil, errors.New(awserrors.ErrorRequestEntityTooLarge)
	}
	return query.ParseArgs(string(body))
}

// dispatch gates the caller as a worker session before anything about the
// request is evaluated, then the role policy, then resolves the session to
// the Region's current worker identity. Errors are awserrors codes.
func (d Deps) dispatch(w http.ResponseWriter, inv dispatch.Invocation) error {
	r := inv.Request
	ctx := r.Context()
	args, err := readQueryArgs(r)
	if err != nil {
		return errors.New(awserrors.ErrorMalformedQueryString)
	}
	action := args["Action"]
	if action == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}
	allowed, ok := params[action]
	if !ok {
		return errors.New(awserrors.ErrorInvalidAction)
	}

	actor, _ := inv.Actor()
	session, err := requireWorkerPrincipal(ctx, inv.AccountID, actor)
	if err != nil {
		return err
	}
	if err := inv.Authorize(ServiceName, action, []string{"*"}, nil); err != nil {
		return err
	}
	for name := range args {
		if name != "Action" && name != "Version" && !slices.Contains(allowed, name) {
			// The name is caller-chosen and may carry a secret, so it is not logged.
			slog.DebugContext(ctx, "SQS internal: unexpected parameter refused", "action", action)
			return errors.New(awserrors.ErrorInvalidParameterValue)
		}
	}

	if d.Identities == nil || d.FirstStore == nil {
		return errors.New(awserrors.ErrorServiceUnavailable)
	}
	ident, err := d.Identities.Authorize(ctx, session)
	if err != nil {
		return mapError(ctx, action, err)
	}
	// inv.Region is the gateway's own Region; a worker bound elsewhere is not
	// this Region's worker, whatever region it signed for.
	if ident.Region != inv.Region {
		slog.DebugContext(ctx, "SQS internal: worker bound to another Region", "instanceID", session)
		return errors.New(awserrors.ErrorAccessDenied)
	}
	id := firststore.Identity{InstanceID: ident.InstanceID, ClaimID: ident.ClaimID, Generation: ident.Generation}

	switch action {
	case ActionClaimFirstStore:
		dec, err := d.FirstStore.Claim(ctx, ident.Region, id)
		if err != nil {
			return mapError(ctx, action, err)
		}
		return writeXML(ctx, w, claimResponse{Result: claimResult{CreateKey: dec.CreateKey, PublicKey: dec.PublicKey}})
	default:
		pub := args["PublicKey"]
		if pub == "" {
			return errors.New(awserrors.ErrorMissingParameter)
		}
		// A seed or any other value is refused without echoing it anywhere.
		if !nkeys.IsValidPublicUserKey(pub) {
			return errors.New(awserrors.ErrorInvalidParameterValue)
		}
		if err := d.FirstStore.RecordPublicKey(ctx, ident.Region, id, pub); err != nil {
			return mapError(ctx, action, err)
		}
		return writeXML(ctx, w, recordResponse{})
	}
}

// requireWorkerPrincipal admits only a session of the worker instance role in
// the system account, and returns its session name: the instance ID STS set.
// It says the caller is an SQS worker VM, not that it is the current one.
func requireWorkerPrincipal(ctx context.Context, accountID string, actor dispatch.Actor) (string, error) {
	if actor.PrincipalType != principalTypeAssumedRole ||
		accountID != awsidentifiers.GlobalAccountID || actor.AccountID != awsidentifiers.GlobalAccountID ||
		actor.RoleName() != WorkerRoleName || actor.SessionName == "" {
		slog.DebugContext(ctx, "SQS internal: rejected non-worker caller",
			"principalType", actor.PrincipalType, "accountID", accountID)
		return "", errors.New(awserrors.ErrorAccessDenied)
	}
	return actor.SessionName, nil
}

// mapError turns a store refusal into the code the worker acts on: identity
// refusals are AccessDenied, contention is retryable, the rest are faults.
func mapError(ctx context.Context, action string, err error) error {
	switch {
	case errors.Is(err, workerident.ErrUnknownInstance), errors.Is(err, workerident.ErrNotCurrent),
		errors.Is(err, firststore.ErrClaimHeld), errors.Is(err, firststore.ErrStaleGeneration):
		return errors.New(awserrors.ErrorAccessDenied)
	case errors.Is(err, firststore.ErrKeyMismatch):
		return errors.New(awserrors.ErrorInvalidParameterValue)
	case errors.Is(err, firststore.ErrNotClaimed):
		return errors.New(awserrors.ErrorInvalidState)
	case errors.Is(err, workerident.ErrContended), errors.Is(err, firststore.ErrContended):
		return errors.New(awserrors.ErrorServiceUnavailable)
	case errors.Is(err, errUnavailable):
		slog.WarnContext(ctx, "SQS internal: worker stores unavailable", "action", action, "err", err)
		return errors.New(awserrors.ErrorServiceUnavailable)
	}
	slog.ErrorContext(ctx, "SQS internal: store failure", "action", action, "err", err)
	return errors.New(awserrors.ErrorServerInternal)
}

type claimResult struct {
	CreateKey bool   `xml:"CreateKey"`
	PublicKey string `xml:"PublicKey,omitempty"`
}

type claimResponse struct {
	XMLName xml.Name    `xml:"ClaimFirstStoreResponse"`
	Result  claimResult `xml:"ClaimFirstStoreResult"`
}

type recordResponse struct {
	XMLName xml.Name `xml:"RecordWorkerPublicKeyResponse"`
}

func writeXML(ctx context.Context, w http.ResponseWriter, v any) error {
	out, err := xml.Marshal(v)
	if err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(out); err != nil {
		slog.ErrorContext(ctx, "Failed to write SQS internal response", "err", err)
	}
	return nil
}

// JetStreamDeps binds the existing worker-identity and first-store buckets per
// request and never creates them, so a gateway provisions nothing and refuses
// every request until the worker launch path has created them durably.
func JetStreamDeps(js jetstream.JetStream) Deps {
	if js == nil {
		return Deps{}
	}
	return Deps{Identities: boundIdentities{js}, FirstStore: boundFirstStore{js}}
}

// errUnavailable marks a bucket that could not be bound: absent, unreachable
// or not durable. It decides nothing, so the worker retries later.
var errUnavailable = errors.New("SQS worker stores unavailable")

type boundIdentities struct{ js jetstream.JetStream }

func (b boundIdentities) Authorize(ctx context.Context, instanceID string) (workerident.Identity, error) {
	s, err := workerident.Bind(ctx, b.js)
	if err != nil {
		return workerident.Identity{}, fmt.Errorf("%w: %w", errUnavailable, err)
	}
	return s.Authorize(ctx, instanceID)
}

type boundFirstStore struct{ js jetstream.JetStream }

func (b boundFirstStore) Claim(ctx context.Context, region string, id firststore.Identity) (firststore.Decision, error) {
	s, err := firststore.Bind(ctx, b.js)
	if err != nil {
		return firststore.Decision{}, fmt.Errorf("%w: %w", errUnavailable, err)
	}
	return s.Claim(ctx, region, id)
}

func (b boundFirstStore) RecordPublicKey(ctx context.Context, region string, id firststore.Identity, pub string) error {
	s, err := firststore.Bind(ctx, b.js)
	if err != nil {
		return fmt.Errorf("%w: %w", errUnavailable, err)
	}
	return s.RecordPublicKey(ctx, region, id, pub)
}

var (
	_ Identities = boundIdentities{}
	_ FirstStore = boundFirstStore{}
)
