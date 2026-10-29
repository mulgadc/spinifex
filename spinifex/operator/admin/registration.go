package admin

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"

	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/spinifex/domains/ochre"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/query"
	"github.com/nats-io/nats.go"
)

// ServiceName is the SigV4 credential-scope service the operator tooling signs for.
const ServiceName = "spinifex"

// ModelAccess is the writable grant store the model-access actions drive.
type ModelAccess interface {
	Grant(ctx context.Context, accountID, modelID string) error
	Revoke(ctx context.Context, accountID, modelID string) error
	List(ctx context.Context, accountID string) ([]string, error)
}

var _ ModelAccess = (*ochre.ModelAccessStore)(nil)

// Deps are the capabilities the spinifex Query actions run against. A nil NATS
// connection or nil ModelAccess answers the actions that need it with
// ServerInternal; nil Credentials and Access resolve nothing and grant nothing.
type Deps struct {
	Version     string
	Commit      string
	NATS        *nats.Conn
	RootCAs     *x509.CertPool
	ActiveNodes func(ctx context.Context) int
	ModelAccess ModelAccess
	Credentials ochre.CredentialResolver
	Access      ochre.AccessResolver
}

// adminActions are the actions only the super-admin account may call.
var adminActions = map[string]bool{
	"GetVersion":        true,
	"GetNodes":          true,
	"GetVMs":            true,
	"GetStorageStatus":  true,
	"PromoteImage":      true,
	"GrantModelAccess":  true,
	"RevokeModelAccess": true,
	"ListModelAccess":   true,
	"ListOchreCatalog":  true,
}

// ActionNames lists every action the registration dispatches, sorted.
func ActionNames() []string {
	return slices.Sorted(maps.Keys(adminActions))
}

// NewRegistration returns the spinifex Query control-plane registration.
func NewRegistration(deps Deps) dispatch.Registration {
	return dispatch.Registration{
		Service:       ServiceName,
		ResolveAction: resolveAction,
		Dispatch:      deps.dispatch,
		Errors:        dispatch.ErrorEnvelopeXML,
		Inventory:     dispatch.Inventory{Registered: ActionNames()},
	}
}

// resolveAction is the one Query Action parser telemetry and dispatch share.
// It leaves the body readable for the dispatcher and answers "" on any failure.
func resolveAction(r *http.Request) string {
	args, err := readQueryArgs(r)
	if err != nil {
		return ""
	}
	return args["Action"]
}

// readQueryArgs parses the Query body and rewinds it, so the same bytes can be
// read again. Any read or parse failure is a malformed query string.
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

// dispatch parses, authorizes and runs one request. The policy gate runs
// before the super-admin account gate, and both before an unknown action is
// refused, so a denied caller learns nothing about which actions exist.
func (d Deps) dispatch(w http.ResponseWriter, inv dispatch.Invocation) error {
	r := inv.Request
	args, err := readQueryArgs(r)
	if err != nil {
		slog.DebugContext(r.Context(), "Spinifex: malformed query string", "err", err)
		return errors.New(awserrors.ErrorMalformedQueryString)
	}

	action := args["Action"]
	if action == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}

	if err := inv.Authorize(ServiceName, action, []string{"*"}, nil); err != nil {
		return err
	}

	accountID := inv.AccountID
	if accountID == "" {
		slog.ErrorContext(r.Context(), "Spinifex dispatch: no account ID in auth context")
		return errors.New(awserrors.ErrorServerInternal)
	}

	if adminActions[action] && accountID != awsidentifiers.DefaultAccountID() {
		slog.InfoContext(r.Context(), "Spinifex dispatch: non-admin access denied", "action", action, "accountID", accountID)
		return errors.New(awserrors.ErrorAccessDenied)
	}

	output, err := d.run(r.Context(), action, args, accountID)
	if err != nil {
		return err
	}

	jsonOutput, err := json.Marshal(output)
	if err != nil {
		slog.ErrorContext(r.Context(), "Failed to marshal spinifex response", "action", action, "err", err)
		return errors.New(awserrors.ErrorInternalError)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(jsonOutput); err != nil {
		slog.ErrorContext(r.Context(), "Failed to write spinifex response", "err", err)
	}
	return nil
}

// run executes one authorized action.
func (d Deps) run(ctx context.Context, action string, args map[string]string, accountID string) (any, error) {
	switch action {
	case "GetVersion":
		return GetVersion(d.Version, d.Commit)
	case "GetNodes":
		if d.NATS == nil {
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		return GetNodes(ctx, d.NATS, d.activeNodes(ctx))
	case "GetVMs":
		if d.NATS == nil {
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		return GetVMs(ctx, d.NATS, d.activeNodes(ctx))
	case "GetStorageStatus":
		if d.NATS == nil {
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		return GetStorageStatus(d.NATS, d.RootCAs)
	case "PromoteImage":
		if d.NATS == nil {
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		imageID := args["ImageId"]
		if imageID == "" {
			return nil, errors.New(awserrors.ErrorMissingParameter)
		}
		return PromoteImage(ctx, d.NATS, imageID, accountID)
	case "GrantModelAccess", "RevokeModelAccess":
		// Grants are gateway-owned KV state, like the bedrock credential store,
		// so these need no NATS hop: the gateway writes the bucket it reads.
		if d.ModelAccess == nil {
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		targetAccount, modelID := args["AccountId"], args["ModelId"]
		if targetAccount == "" || modelID == "" {
			return nil, errors.New(awserrors.ErrorMissingParameter)
		}
		var err error
		if action == "GrantModelAccess" {
			err = d.ModelAccess.Grant(ctx, targetAccount, modelID)
		} else {
			err = d.ModelAccess.Revoke(ctx, targetAccount, modelID)
		}
		if err != nil {
			return nil, err
		}
		return ochre.ModelAccessChange{AccountID: targetAccount, ModelID: modelID}, nil
	case "ListModelAccess":
		if d.ModelAccess == nil {
			return nil, errors.New(awserrors.ErrorServerInternal)
		}
		targetAccount := args["AccountId"]
		if targetAccount == "" {
			return nil, errors.New(awserrors.ErrorMissingParameter)
		}
		models, err := d.ModelAccess.List(ctx, targetAccount)
		if err != nil {
			return nil, err
		}
		return ochre.ModelAccessList{AccountID: targetAccount, ModelIDs: models}, nil
	case "ListOchreCatalog":
		entries, err := ochre.AdminCatalog(ctx, accountID, d.credentials(), d.access())
		if err != nil {
			return nil, err
		}
		return ochre.AdminCatalogList{Entries: entries}, nil
	default:
		return nil, errors.New(awserrors.ErrorInvalidAction)
	}
}

// activeNodes is the discovered node count, or zero when no discovery is wired.
func (d Deps) activeNodes(ctx context.Context) int {
	if d.ActiveNodes == nil {
		return 0
	}
	return d.ActiveNodes(ctx)
}

func (d Deps) credentials() ochre.CredentialResolver {
	if d.Credentials != nil {
		return d.Credentials
	}
	return ochre.NoopCredentialResolver
}

// access stages open access over the grant store, as the bedrock routes do.
func (d Deps) access() ochre.AccessResolver {
	if d.Access != nil {
		return ochre.NewStagedOpenAccessResolver(d.Access)
	}
	return ochre.DenyAllAccessResolver
}
