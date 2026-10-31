package gateway

import (
	"net/http"

	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
)

// callerOf returns the verified caller, or the zero value when the request was
// not authenticated.
func callerOf(r *http.Request) credential.Principal {
	p, _ := reqctx.Principal(r.Context())
	return p
}
