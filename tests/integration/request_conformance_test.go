//go:build integration

package integration

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	awscreds "github.com/aws/aws-sdk-go/aws/credentials"
	v4 "github.com/aws/aws-sdk-go/aws/signer/v4"
	"github.com/mulgadc/spinifex/internal/awsmodel"
	acmawsapi "github.com/mulgadc/spinifex/spinifex/domains/acm/awsapi"
	awsapi "github.com/mulgadc/spinifex/spinifex/domains/ecr/awsapi"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	awsdispatch "github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
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
	inventory := gateway.AWSOperationInventory(map[string]awsdispatch.Inventory{
		awsapi.ServiceName:    awsapi.OperationInventory(),
		acmawsapi.ServiceName: acmawsapi.OperationInventory(),
	})
	for _, service := range awsmodel.Services() {
		// Predastore serves S3, not the gateway.
		if service == awsmodel.S3 {
			continue
		}
		t.Run(string(service), func(t *testing.T) {
			t.Parallel()
			served := inventory[string(service)]
			coverage, err := awsmodel.CompareOperations(service, awsmodel.DispatchInventory{
				Registered:  served.Registered,
				Stubbed:     served.Stubbed,
				Unsupported: served.Unsupported,
			})
			require.NoError(t, err)

			gw := startGateway(t, nil)
			startRequestConformanceBackends(t, gw)
			sender, err := newRequestSender(gw, service)
			require.NoError(t, err)
			options := awsmodel.RequestOptions{AccountID: gw.AccountID, Region: testRegion, AccessKeyID: testAccessKeyID}
			options.Fixtures = createRequestFixtures(t, sender, service, gw.AccountID)
			for _, operation := range coverage.Implemented {
				plan, err := awsmodel.GenerateRequests(service, operation, options)
				require.NoError(t, err, operation)
				suiteRequestConformance.recordOperation(service, operation, plan)
				judge := plan.NewJudge()
				for _, request := range plan.Cases {
					started := time.Now()
					result, err := sender.send(request)
					require.NoError(t, err, "%s %s", operation, request.Path)
					if elapsed := time.Since(started); elapsed > slowRequest {
						t.Logf("slow request: %s %s %s took %dms (%d %s)", operation, request.Constraint, request.Path, elapsed.Milliseconds(), result.Status, result.Code)
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

// requestFixture is the name every fixture resource takes.
const requestFixture = "conformance-fixture"

// createRequestFixtures creates one resource of each kind the service's
// generated requests name, so a case reaches its resource instead of ending
// in not-found. It returns the members that name them.
func createRequestFixtures(t *testing.T, sender *requestSender, service awsmodel.Service, accountID string) map[string]string {
	t.Helper()
	create := func(operation string, input map[string]any) {
		t.Helper()
		result, err := sender.send(awsmodel.RequestCase{Operation: operation, Input: input})
		require.NoError(t, err, operation)
		require.True(t, result.Status >= 200 && result.Status < 300, "create fixture: %s: %d %s: %s", operation, result.Status, result.Code, result.Message)
	}
	switch service {
	case awsmodel.IAM:
		const oidcHost = "fixture.example.com"
		create("CreateRole", map[string]any{"RoleName": requestFixture, "AssumeRolePolicyDocument": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`})
		create("CreatePolicy", map[string]any{"PolicyName": requestFixture, "PolicyDocument": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`})
		create("CreateOpenIDConnectProvider", map[string]any{"Url": "https://" + oidcHost, "ClientIDList": []any{"sts.amazonaws.com"}, "ThumbprintList": []any{strings.Repeat("a", 40)}})
		create("CreateInstanceProfile", map[string]any{"InstanceProfileName": requestFixture})
		create("CreateGroup", map[string]any{"GroupName": requestFixture})
		create("CreateUser", map[string]any{"UserName": requestFixture})
		return map[string]string{
			"RoleName":                 requestFixture,
			"PolicyArn":                "arn:aws:iam::" + accountID + ":policy/" + requestFixture,
			"OpenIDConnectProviderArn": "arn:aws:iam::" + accountID + ":oidc-provider/" + oidcHost,
			"InstanceProfileName":      requestFixture,
			"GroupName":                requestFixture,
			"UserName":                 requestFixture,
		}
	case awsmodel.ECS:
		create("CreateCluster", map[string]any{"clusterName": requestFixture})
		return map[string]string{
			"cluster":     requestFixture,
			"clusters":    requestFixture,
			"resourceArn": "arn:aws:ecs:" + testRegion + ":" + accountID + ":cluster/" + requestFixture,
		}
	case awsmodel.ECR:
		create("CreateRepository", map[string]any{"repositoryName": requestFixture})
		return map[string]string{
			"repositoryName":  requestFixture,
			"repositoryNames": requestFixture,
			"resourceArn":     "arn:aws:ecr:" + testRegion + ":" + accountID + ":repository/" + requestFixture,
		}
	default:
		return nil
	}
}

type requestSender struct {
	endpoint string
	service  awsmodel.Service
	signing  string
	signer   *v4.Signer
	client   *http.Client
}

func newRequestSender(gw *Gateway, service awsmodel.Service) (*requestSender, error) {
	model, err := awsmodel.Load(service)
	if err != nil {
		return nil, err
	}
	return &requestSender{
		endpoint: gw.Server.URL,
		service:  service,
		signing:  model.Metadata().SigningName,
		signer:   v4.NewSigner(awscreds.NewStaticCredentials(testAccessKeyID, testSecretAccessKey, "")),
		client:   &http.Client{Timeout: requestTimeout},
	}, nil
}

// send encodes, signs and sends one request. A request that times out is
// recorded with status 0, which judges as inconclusive; any other transport
// failure, such as a gateway panic closing the connection, is an error.
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
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return awsmodel.RequestResult{Code: "Timeout"}, nil
	}
	if err != nil {
		return awsmodel.RequestResult{}, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return awsmodel.RequestResult{}, err
	}
	return awsmodel.DecodeRequestResult(s.service, response.StatusCode, response.Header, body), nil
}
