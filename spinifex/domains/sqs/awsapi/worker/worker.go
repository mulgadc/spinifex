// Package worker is the SQS worker's private control-plane surface. It is
// served under its own SigV4 signing service, apart from the public AmazonSQS
// action inventory, and admits only the Region's current SQS worker VM.
package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"time"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/firststore"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/relay"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/storelaunch"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerrole"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
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
	ActionDescribeStore         = workerrole.ActionDescribeStore
	ActionRecordWorkerPublicKey = workerrole.ActionRecordWorkerPublicKey
	ActionPollWork              = workerrole.ActionPollWork
	ActionCompleteWork          = workerrole.ActionCompleteWork
)

// pollWait bounds one PollWork long poll; pollBatch and pollBytes bound the
// items one poll hands out.
const (
	pollWait  = 20 * time.Second
	pollBatch = 16
	pollBytes = 16 << 20
)

var workIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

const principalTypeAssumedRole = "assumed-role"

// params lists the only parameters each action accepts. Identity is never a
// parameter: it is derived from the session and the current worker record.
var params = map[string][]string{
	ActionClaimFirstStore:       {},
	ActionDescribeStore:         {},
	ActionRecordWorkerPublicKey: {"PublicKey"},
	ActionPollWork:              {},
	ActionCompleteWork:          {"WorkId"},
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

// Stores reads the Region's store launch record from the stream leader.
type Stores interface {
	Current(ctx context.Context, region string) (storelaunch.Record, error)
}

var (
	_ Stores     = storelaunch.Records{}
	_ Identities = workerident.Store{}
	_ FirstStore = firststore.Store{}
)

// Work is the relay the worker pulls owner calls from and posts results to.
type Work interface {
	Take(ctx context.Context, taker relay.Taker, limit, maxBytes int) []relay.Work
	Complete(taker relay.Taker, id string, res relay.Result) (relay.Outcome, error)
}

var _ Work = (*relay.Relay)(nil)

// Deps are the capabilities the actions run against. Any one nil answers
// every authorized request with ServiceUnavailable, never a decision; a nil
// Work does so for the work actions only.
type Deps struct {
	Identities Identities
	FirstStore FirstStore
	Stores     Stores
	Work       Work
	// HostNodes reports this gateway's declared cluster node count; nil is
	// clustersize.Nodes.
	HostNodes func() int
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
// CompleteWork carries its arguments in the signed query string instead,
// because its body is the result.
func readQueryArgs(r *http.Request) (map[string]string, error) {
	if r.URL.Query().Get("Action") == ActionCompleteWork {
		return query.ParseArgs(r.URL.RawQuery)
	}
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

	if d.Identities == nil || d.FirstStore == nil || d.Stores == nil {
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
	case ActionPollWork, ActionCompleteWork:
		if d.Work == nil {
			return errors.New(awserrors.ErrorServiceUnavailable)
		}
		taker := relay.Taker{InstanceID: ident.InstanceID, Generation: ident.Generation, ClaimID: ident.ClaimID}
		if action == ActionPollWork {
			return d.pollWork(ctx, w, taker)
		}
		return d.completeWork(ctx, w, r, taker, args["WorkId"])
	case ActionDescribeStore:
		return d.describeStore(ctx, w, ident.Region)
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

// describeStore reports the Region's store launch: pending until it is bound,
// then the address and the public key it was launched for. Bound is not
// ready; the worker checks the store itself with its own key.
func (d Deps) describeStore(ctx context.Context, w http.ResponseWriter, region string) error {
	rec, err := d.Stores.Current(ctx, region)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return writeXML(ctx, w, describeResponse{Result: describeResult{State: StorePending}})
	case errors.Is(err, storelaunch.ErrUnreadable), errors.Is(err, storelaunch.ErrInvalidRecording):
		return mapError(ctx, ActionDescribeStore, err)
	case err != nil:
		return mapError(ctx, ActionDescribeStore, fmt.Errorf("%w: %w", errUnavailable, err))
	case rec.State != storelaunch.StateBound:
		return writeXML(ctx, w, describeResponse{Result: describeResult{State: StorePending}})
	}
	return writeXML(ctx, w, describeResponse{Result: describeResult{
		State: StoreBound, Address: rec.Address, PublicKey: rec.PublicKey, HostNodes: d.hostNodes(),
	}})
}

func (d Deps) hostNodes() int {
	if d.HostNodes != nil {
		return d.HostNodes()
	}
	return clustersize.Nodes()
}

// pollWork hands this worker queued owner calls, waiting up to pollWait.
// Each item is the Work JSON, base64 encoded.
func (d Deps) pollWork(ctx context.Context, w http.ResponseWriter, taker relay.Taker) error {
	pollCtx, cancel := context.WithTimeout(ctx, pollWait)
	defer cancel()
	works := d.Work.Take(pollCtx, taker, pollBatch, pollBytes)
	res := pollResult{Items: make([]string, 0, len(works))}
	for _, wk := range works {
		b, err := json.Marshal(wk)
		if err != nil {
			return errors.New(awserrors.ErrorServerInternal)
		}
		res.Items = append(res.Items, base64.StdEncoding.EncodeToString(b))
	}
	return writeXML(ctx, w, pollResponse{Result: res})
}

// completeWork posts the result in the body for the item the same worker
// identity took. The outcome tells the worker whether delivery is disproved.
func (d Deps) completeWork(ctx context.Context, w http.ResponseWriter, r *http.Request, taker relay.Taker, id string) error {
	if !workIDPattern.MatchString(id) {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, sigv4.MaxPayloadLen+1))
	if err != nil {
		return errors.New(awserrors.ErrorMalformedQueryString)
	}
	if int64(len(body)) > sigv4.MaxPayloadLen {
		return errors.New(awserrors.ErrorRequestEntityTooLarge)
	}
	var res relay.Result
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil || (res.Error == nil) == (res.Output == nil) {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	outcome, err := d.Work.Complete(taker, id, res)
	if errors.Is(err, relay.ErrNotTaker) {
		return errors.New(awserrors.ErrorAccessDenied)
	}
	if err != nil {
		return errors.New(awserrors.ErrorServerInternal)
	}
	return writeXML(ctx, w, completeResponse{Result: completeResult{Outcome: OutcomeName(outcome)}})
}

// CompleteWork outcomes on the wire.
const (
	OutcomeDelivered  = "delivered"
	OutcomeCallerGone = "caller-gone"
	OutcomeUnknown    = "unknown"
)

// OutcomeName is the wire name of o.
func OutcomeName(o relay.Outcome) string {
	switch o {
	case relay.Delivered:
		return OutcomeDelivered
	case relay.CallerGone:
		return OutcomeCallerGone
	}
	return OutcomeUnknown
}

type pollResult struct {
	Items []string `xml:"Items>member"`
}

type pollResponse struct {
	XMLName xml.Name   `xml:"PollWorkResponse"`
	Result  pollResult `xml:"PollWorkResult"`
}

type completeResult struct {
	Outcome string `xml:"Outcome"`
}

type completeResponse struct {
	XMLName xml.Name       `xml:"CompleteWorkResponse"`
	Result  completeResult `xml:"CompleteWorkResult"`
}

// DescribeStore states.
const (
	StorePending = "pending"
	StoreBound   = "bound"
)

type describeResult struct {
	State     string `xml:"State"`
	Address   string `xml:"Address,omitempty"`
	PublicKey string `xml:"PublicKey,omitempty"`
	// HostNodes is this gateway's declared cluster node count, zero when
	// undeclared; the worker serves only when it is exactly one.
	HostNodes int `xml:"HostNodes"`
}

type describeResponse struct {
	XMLName xml.Name       `xml:"DescribeStoreResponse"`
	Result  describeResult `xml:"DescribeStoreResult"`
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
	return Deps{Identities: boundIdentities{js}, FirstStore: boundFirstStore{js}, Stores: boundStores{js}}
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

type boundStores struct{ js jetstream.JetStream }

func (b boundStores) Current(ctx context.Context, region string) (storelaunch.Record, error) {
	r, err := storelaunch.Bind(ctx, b.js)
	if err != nil {
		return storelaunch.Record{}, fmt.Errorf("%w: %w", errUnavailable, err)
	}
	return r.Current(ctx, region)
}

var (
	_ Identities = boundIdentities{}
	_ FirstStore = boundFirstStore{}
	_ Stores     = boundStores{}
)
