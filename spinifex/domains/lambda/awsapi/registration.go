// Package awsapi serves the Lambda REST-JSON control plane (API version
// 2015-03-31) for the actions this build supports.
package awsapi

import (
	"context"
	"crypto/cipher"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/private/protocol/json/jsonutil"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/codestore"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/passrole"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/envelope"
)

// ServiceName is the SigV4 credential-scope service Lambda clients sign for.
const ServiceName = "lambda"

// servicePrincipal is the service a function execution role is passed to.
const servicePrincipal = "lambda.amazonaws.com"

// CodeStore is what the handlers need of the Regional code store.
type CodeStore interface {
	Put(ctx context.Context, ref codestore.Ref, code []byte) (string, error)
	PresignGet(ctx context.Context, ref codestore.Ref, sha256Hex string, ttl time.Duration) (string, error)
}

var _ CodeStore = (*codestore.Store)(nil)

// CodeStoreFunc opens the code store on first use; it fails while the store is
// not ready.
type CodeStoreFunc func(ctx context.Context) (CodeStore, error)

// Deps are the composed capabilities the Lambda control plane dispatches to.
type Deps struct {
	Meta  function.Client
	Codes CodeStoreFunc
	Roles passrole.Resolver
	// Trusts reports whether the role's trust policy lets Lambda assume it for
	// the account; false is a refusal, an error an outage.
	Trusts func(accountID, roleARN string) (bool, error)
	// AEAD seals function environment variables; it is the master key's cipher.
	AEAD cipher.AEAD
}

// NewRegistration returns the Lambda REST-JSON registration.
func NewRegistration(deps Deps) dispatch.Registration {
	return dispatch.Registration{
		Service:       ServiceName,
		ResolveAction: resolveAction,
		Dispatch:      deps.dispatch,
		Errors:        dispatch.ErrorEnvelopeRESTJSON,
		Inventory:     dispatch.Inventory{Registered: actionNames()},
	}
}

func resolveAction(r *http.Request) string {
	action, _, _, _ := router.Lookup(r.Method, r.URL.EscapedPath())
	return action
}

func readBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValueException)
	}
	if len(body) > maxBodyBytes {
		return nil, errors.New(awserrors.ErrorRequestEntityTooLarge)
	}
	return body, nil
}

// functionARN names the function the request is about, or "*" when the request
// does not yet name a legal one, so a malformed request stays the handler's
// validation fault rather than becoming an authorization failure.
func functionARN(region, accountID, name string) string {
	if region == "" || accountID == "" || !nameRE.MatchString(name) {
		return "*"
	}
	return arn.FormatLambdaFunction(region, accountID, name)
}

// serve resolves the route, authorizes the request, then runs the action.
func (d Deps) serve(w http.ResponseWriter, inv dispatch.Invocation) error {
	r := inv.Request
	action, params, handler, ok := router.Lookup(r.Method, r.URL.EscapedPath())
	if !ok {
		return errors.New(awserrors.ErrorInvalidAction)
	}
	if inv.AccountID == "" {
		slog.ErrorContext(r.Context(), "Lambda dispatch: no account ID in auth context")
		return errors.New(awserrors.ErrorInternalError)
	}
	body, err := readBody(r)
	if err != nil {
		return err
	}
	c := &call{AccountID: inv.AccountID, Region: inv.Region, Params: params, Query: r.URL.Query(), Body: body}

	// The region is the gateway's own, never the caller-supplied credential
	// scope, so a caller cannot sign for another region to slide out from
	// under a Deny.
	resource := "*"
	var decodeErr error
	switch action {
	case "CreateFunction":
		c.Create, decodeErr = decodeCreate(body)
		if decodeErr == nil {
			resource = functionARN(inv.Region, inv.AccountID, aws.StringValue(c.Create.FunctionName))
		}
	case "GetFunction", "GetFunctionConfiguration":
		if name, _, err := resolveFunctionName(inv.Region, inv.AccountID, params[0]); err == nil {
			resource = functionARN(inv.Region, inv.AccountID, name)
		}
	}
	if err := inv.Authorize(ServiceName, action, []string{resource}, nil); err != nil {
		return err
	}
	if decodeErr != nil {
		return decodeErr
	}
	if action == "CreateFunction" {
		if err := d.checkPassRole(inv, aws.StringValue(c.Create.Role)); err != nil {
			return err
		}
	}
	// A configuration read is metadata only, so it does not need the code store.
	if d.Meta == nil || (d.Codes == nil && action != "GetFunctionConfiguration") {
		return errors.New(awserrors.ErrorServiceException)
	}

	out, err := handler(r.Context(), d, c)
	if err != nil {
		return err
	}
	writeJSON(w, out)
	return nil
}

func (d Deps) dispatch(w http.ResponseWriter, inv dispatch.Invocation) error {
	return d.serve(w, inv)
}

// checkPassRole enforces iam:PassRole on the execution role and that its trust
// policy admits Lambda. Only the exact stored ARN resolves, and a missing
// resolver fails closed.
func (d Deps) checkPassRole(inv dispatch.Invocation, roleARN string) error {
	if roleARN == "" {
		return nil
	}
	if d.Roles == nil || d.Trusts == nil {
		return errors.New(awserrors.ErrorServiceException)
	}
	role, err := passrole.Check(d.Roles, passrole.AuthorizeFunc(inv.Authorize), inv.AccountID, roleARN, servicePrincipal)
	switch {
	case err == nil:
		trusted, terr := d.Trusts(inv.AccountID, role.ARN)
		if terr != nil {
			return terr
		}
		if !trusted {
			return invalid("The role defined for the function cannot be assumed by Lambda.")
		}
		return nil
	case errors.Is(err, passrole.ErrMalformed), errors.Is(err, passrole.ErrForeignAccount):
		return invalid("The role defined for the function cannot be assumed by Lambda.")
	case errors.Is(err, passrole.ErrNotFound):
		return invalid("The role defined for the function cannot be assumed by Lambda.")
	default:
		return err
	}
}

func writeJSON(w http.ResponseWriter, out reply) {
	body, err := jsonutil.BuildJSON(out.Body)
	if err != nil {
		slog.Error("Failed to marshal Lambda response", "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(envelope.JSONBody(awserrors.ErrorServiceException, "failed to marshal response"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(out.Status)
	if _, err := w.Write(body); err != nil {
		slog.Error("Failed to write Lambda response", "err", err)
	}
}
