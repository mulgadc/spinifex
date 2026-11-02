package awsapi

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"slices"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	ec2instanceapi "github.com/mulgadc/spinifex/spinifex/domains/ec2/awsapi/instance"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/query"
)

const (
	// ServiceName is the SigV4 credential-scope service EC2 clients sign for.
	ServiceName = "ec2"

	ec2ServicePrincipal = "ec2.amazonaws.com"
)

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

// Dispatch parses, authorizes and runs one EC2 request, then writes the XML
// response. A returned error is an awserrors code the gateway renders.
func (h *Handler) Dispatch(w http.ResponseWriter, inv dispatch.Invocation) error {
	r := inv.Request
	queryArgs, err := readQueryArgs(r)
	if err != nil {
		slog.Debug("EC2: malformed query string", "err", err)
		return errors.New(awserrors.ErrorMalformedQueryString)
	}

	action := queryArgs["Action"]
	if action == "" {
		return errors.New(awserrors.ErrorMissingAction)
	}
	handler, ok := ec2Actions[action]
	if !ok {
		return errors.New(awserrors.ErrorInvalidAction)
	}

	// Hoisted above the policy check because the resolver builds ARNs from it.
	// inv.Region is the gateway's own region, never the caller-supplied
	// credential-scope region, which would let a caller sign for another region
	// and slide out from under a Deny.
	accountID := inv.AccountID
	if accountID == "" {
		slog.Error("EC2 dispatch: no account ID in auth context")
		// InternalError, not ServerInternal: the policy gate used to reach this
		// case first and that is the code the caller has always seen.
		return errors.New(awserrors.ErrorInternalError)
	}

	input, err := handler.parse(queryArgs)
	if err != nil {
		if errors.Is(err, query.ErrSliceTooLarge) {
			return errors.New(awserrors.ErrorMalformedQueryString)
		}
		return err
	}

	// Launch templates supply effective RunInstances resources. Resolve them
	// before authorization, and dispatch the same expanded input afterwards.
	if runInput, ok := input.(*ec2.RunInstancesInput); ok && action == "RunInstances" {
		if err := ec2instanceapi.ExpandLaunchTemplate(r.Context(), h.NATS, runInput, accountID); err != nil {
			return err
		}
	}

	resources, err := ResourceARNs(action, inv.Region, accountID, input)
	if err != nil {
		return err
	}
	if err := inv.Authorize(ServiceName, action, resources, nil); err != nil {
		// EC2 answers a policy denial with UnauthorizedOperation, not AccessDenied.
		if code, _ := awserrors.ResolveErrorCode(err); code == awserrors.ErrorAccessDenied {
			return errors.New(awserrors.ErrorUnauthorizedOperation)
		}
		return err
	}
	// AWS refuses a bad tag resource type even on a DryRun request.
	if err := validateTagSpecificationTypes(action, input); err != nil {
		return err
	}
	if dryRunRequested(input) {
		return errors.New(awserrors.ErrorDryRunOperation)
	}
	if err := validateMaxResults(action, input); err != nil {
		return err
	}

	if h.NATS == nil && !ec2LocalActions[action] {
		return errors.New(awserrors.ErrorServerInternal)
	}

	c := &call{
		Handler: h,
		Region:  inv.Region,
		passRole: func(roleARN string) error {
			return inv.Authorize("iam", "PassRole", []string{roleARN},
				iampolicy.ConditionKeys{iampolicy.KeyPassedToService: ec2ServicePrincipal})
		},
	}
	xmlOutput, err := handler.dispatch(requestContext(r), action, input, c, accountID)
	if err != nil {
		// A gateway iam:PassRole denial is EC2's UnauthorizedOperation too; an
		// AccessDenied a daemon returns is left as it is.
		if _, denied := errors.AsType[*dispatch.PolicyDenialError](err); denied {
			return errors.New(awserrors.ErrorUnauthorizedOperation)
		}
		return err
	}

	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(xmlOutput); err != nil {
		slog.Error("Failed to write EC2 response", "err", err)
	}
	return nil
}

// ActionNames lists every action the handler dispatches, sorted.
func ActionNames() []string {
	return slices.Sorted(maps.Keys(ec2Actions))
}

// InputHasField reports whether the SDK input type an action parses into has
// the named field, so callers can ask what the model declares for an action
// without reaching the route table.
func InputHasField(action, field string) bool {
	entry, ok := ec2Actions[action]
	if !ok {
		return false
	}
	input, err := entry.parse(map[string]string{"Action": action})
	if err != nil {
		return false
	}
	_, has := reflect.TypeOf(input).Elem().FieldByName(field)
	return has
}
