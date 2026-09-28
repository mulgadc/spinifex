//go:build integration

package integration

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	awscreds "github.com/aws/aws-sdk-go/aws/credentials"
	v4 "github.com/aws/aws-sdk-go/aws/signer/v4"
	"github.com/mulgadc/spinifex/internal/awsmodel"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	"github.com/stretchr/testify/require"
)

// requestTimeout bounds each generated request: a subject no daemon-lite
// answers can hold a request for the gateway's full NATS timeout.
const requestTimeout = 5 * time.Second

// slowRequest is the duration past which a request is logged, since a slow
// one usually waits on a NATS subject no daemon-lite answers.
const slowRequest = 500 * time.Millisecond

// TestRequestConformance sends every generated acceptance and rejection
// request for each implemented operation through the real gateway, with every
// daemon-lite wired, and records each verdict for the suite report.
func TestRequestConformance(t *testing.T) {
	inventory := gateway.AWSOperationInventory()
	for _, service := range awsmodel.Services() {
		// Predastore serves S3, not the gateway.
		if service == awsmodel.S3 {
			continue
		}
		t.Run(string(service), func(t *testing.T) {
			t.Parallel()
			dispatch := inventory[string(service)]
			coverage, err := awsmodel.CompareOperations(service, awsmodel.DispatchInventory{
				Registered:  dispatch.Registered,
				Stubbed:     dispatch.Stubbed,
				Unsupported: dispatch.Unsupported,
			})
			require.NoError(t, err)

			gw := startGateway(t, nil)
			startRequestConformanceBackends(t, gw)
			sender := newRequestSender(gw, service)
			for _, operation := range coverage.Implemented {
				plan, err := awsmodel.GenerateRequests(service, operation, awsmodel.RequestOptions{AccountID: gw.AccountID, Region: testRegion})
				require.NoError(t, err, operation)
				suiteRequestConformance.recordOperation(service, len(plan.Skipped))
				judge := awsmodel.NewRequestJudge()
				for _, request := range plan.Cases {
					started := time.Now()
					result, err := sender.send(request)
					require.NoError(t, err, "%s %s", operation, request.Path)
					if elapsed := time.Since(started); elapsed > slowRequest {
						t.Logf("slow request: %s %s %s took %s (%d %s)", operation, request.Constraint, request.Path, elapsed, result.Status, result.Code)
					}
					suiteRequestConformance.record(service, request, result, judge.Judge(request, result))
				}
			}
		})
	}
}

// startRequestConformanceBackends wires every daemon-lite, so validation that
// lives behind NATS runs as it would on a live daemon.
func startRequestConformanceBackends(t *testing.T, gw *Gateway) {
	t.Helper()
	StartDaemonLite(t, gw)
	StartImageDaemonLite(t, gw)
	StartLaunchTemplateDaemonLite(t, gw)
	StartPlacementGroupDaemonLite(t, gw)
	StartSpotDaemonLite(t, gw)
	StartECRDaemonLite(t, gw)
	StartServiceDaemonLite(t, gw)
}

type requestSender struct {
	endpoint string
	service  awsmodel.Service
	signing  string
	signer   *v4.Signer
	client   *http.Client
}

func newRequestSender(gw *Gateway, service awsmodel.Service) *requestSender {
	model, _ := awsmodel.Load(service)
	return &requestSender{
		endpoint: gw.Server.URL,
		service:  service,
		signing:  model.Metadata().SigningName,
		signer:   v4.NewSigner(awscreds.NewStaticCredentials(testAccessKeyID, testSecretAccessKey, "")),
		client:   &http.Client{Timeout: requestTimeout},
	}
}

// send encodes, signs and sends one request. A request that times out is
// recorded with status 0, which judges as inconclusive.
func (s *requestSender) send(request awsmodel.RequestCase) (awsmodel.RequestResult, error) {
	encoded, err := awsmodel.EncodeRequest(s.service, request.Operation, request.Input)
	if err != nil {
		return awsmodel.RequestResult{}, err
	}
	httpRequest, err := http.NewRequest(encoded.Method, encoded.URL(s.endpoint), bytes.NewReader(encoded.Body))
	if err != nil {
		return awsmodel.RequestResult{}, err
	}
	for name, values := range encoded.Header {
		httpRequest.Header[name] = values
	}
	if _, err := s.signer.Sign(httpRequest, bytes.NewReader(encoded.Body), s.signing, testRegion, time.Now()); err != nil {
		return awsmodel.RequestResult{}, err
	}
	response, err := s.client.Do(httpRequest)
	if err != nil {
		return awsmodel.RequestResult{Code: "Timeout"}, nil
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return awsmodel.RequestResult{}, err
	}
	return awsmodel.DecodeRequestResult(s.service, response.StatusCode, response.Header, body), nil
}
