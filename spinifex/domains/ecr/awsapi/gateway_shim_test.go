package awsapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go"
)

const (
	ecrTestRegion  = gwtest.Region
	ecrTestSuffix  = "spinifex.internal"
	ecrTestAccount = "000000000001"
)

// ecrGW is the ECR registration invoked as the pipeline would for one
// authenticated user of ecrTestAccount.
type ecrGW struct {
	h   *gwtest.Harness
	reg dispatch.Registration
}

// newECRGW registers ECR over deps, relaying onto nc unless deps names its own.
func newECRGW(nc *nats.Conn, deps awsapi.Deps) *ecrGW {
	if deps.NATS == nil {
		deps.NATS = nc
	}
	h := gwtest.AllowAll()
	h.AccountID = ecrTestAccount
	return &ecrGW{h: h, reg: awsapi.NewRegistration(deps)}
}

func (g *ecrGW) serveECR(w http.ResponseWriter, r *http.Request) error {
	return g.h.Dispatch(g.reg, awsapi.ServiceName, w, r)
}

// setupECRRequest builds an ECR control-plane request for target.
func setupECRRequest(target, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	return req
}
