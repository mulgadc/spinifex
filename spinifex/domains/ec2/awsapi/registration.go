package awsapi

import (
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

// NewRegistration returns the EC2 AWS Query control-plane registration. Errors
// render in the EC2 <Response><Errors> body.
func NewRegistration(deps Deps) dispatch.Registration {
	return dispatch.Registration{
		Service:       ServiceName,
		ResolveAction: resolveAction,
		Dispatch:      NewHandler(deps).Dispatch,
		Errors:        dispatch.ErrorEnvelopeXML,
		Traits:        dispatch.ServiceTraits{XMLErrorShape: dispatch.XMLErrorShapeEC2},
		Inventory:     OperationInventory(),
	}
}

// OperationInventory classifies the actions the registration serves. Every EC2
// action in the table is implemented, so none is Stubbed or Unsupported.
func OperationInventory() dispatch.Inventory {
	return dispatch.Inventory{Registered: ActionNames()}
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
