package awsapi_test

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/elbv2"
	iampolicy "github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/internal/testkit"
	elbv2awsapi "github.com/mulgadc/spinifex/spinifex/domains/elbv2/awsapi"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentForms are the two actions the AWS SDK has no builder for, so their
// request bodies are written by hand in the shape the LB agent sends.
var agentForms = map[string]string{
	"LBAgentHeartbeat": "Action=LBAgentHeartbeat&LBID=lb-1" +
		"&Servers.member.1.Backend=be1&Servers.member.1.Server=srv1&Servers.member.1.Status=UP",
	"GetLBConfig": "Action=GetLBConfig&LBID=lb-1",
}

// queryBody encodes input the way a real client does, using the SDK's own
// Query builder, so the registration parses what clients actually send.
func queryBody(t *testing.T, action string, input any) string {
	t.Helper()
	if form, ok := agentForms[action]; ok {
		return form
	}
	svc := elbv2.New(session.Must(session.NewSession(&aws.Config{
		Region:      aws.String("us-east-1"),
		Credentials: credentials.NewStaticCredentials("AKIATEST", "secret", ""),
	})))
	method := reflect.ValueOf(svc).MethodByName(action + "Request")
	require.True(t, method.IsValid(), "SDK has no %sRequest", action)
	out := method.Call([]reflect.Value{reflect.ValueOf(input)})
	req, ok := reflect.TypeAssert[*request.Request](out[0])
	require.True(t, ok)
	require.NoError(t, req.Build())
	body, err := io.ReadAll(req.HTTPRequest.Body)
	require.NoError(t, err)
	return string(body)
}

type authCall struct {
	service, action string
	resources       []string
}

func invoke(t *testing.T, deps elbv2awsapi.Deps, body string, deny error) (*httptest.ResponseRecorder, []authCall, error) {
	t.Helper()
	var calls []authCall
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	inv := dispatch.Invocation{
		Request:   req,
		AccountID: testAccountID,
		Region:    "us-east-1",
		Authorize: func(service, action string, resources []string, _ iampolicy.ConditionKeys) error {
			calls = append(calls, authCall{service, action, resources})
			return deny
		},
	}
	w := httptest.NewRecorder()
	err := elbv2awsapi.NewRegistration(deps).Dispatch(w, inv)
	return w, calls, err
}

// jsonStrings collects every string leaf of v, which is what the XML response
// must carry for the reply to have survived the round trip.
func jsonStrings(t *testing.T, v any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	var tree any
	require.NoError(t, json.Unmarshal(raw, &tree))
	var out []string
	var walk func(any)
	walk = func(n any) {
		switch x := n.(type) {
		case string:
			out = append(out, x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(tree)
	return out
}

func rootElement(t *testing.T, body []byte) string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	for {
		tok, err := dec.Token()
		require.NoError(t, err)
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Local
		}
	}
}

// Every NATS-backed action: the request body reaches the right subject with the
// caller's values, the account rides in the header, authorization sees the
// action first, and the daemon's reply comes back as that action's XML.
func TestRegistrationDispatch_EveryActionRoundTrips(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	for _, tc := range dispatchCases() {
		t.Run(tc.name, func(t *testing.T) {
			replyBody, err := json.Marshal(tc.reply)
			require.NoError(t, err)
			got := serveOnce(t, nc, tc.subject, replyBody)

			w, auths, err := invoke(t, elbv2awsapi.Deps{NATS: nc}, queryBody(t, tc.name, tc.input), nil)
			require.NoError(t, err)

			require.Len(t, auths, 1)
			assert.Equal(t, elbv2awsapi.ServiceName, auths[0].service)
			assert.Equal(t, tc.name, auths[0].action)

			var msg *nats.Msg
			select {
			case msg = <-got:
			case <-time.After(2 * time.Second):
				t.Fatalf("no request observed on %s", tc.subject)
			}
			wantBody, err := json.Marshal(tc.input)
			require.NoError(t, err)
			assert.JSONEq(t, string(wantBody), string(msg.Data), "the daemon must receive the request values")

			assert.Equal(t, http.StatusOK, w.Code)
			assert.Equal(t, "text/xml", w.Header().Get("Content-Type"))
			assert.Equal(t, tc.name+"Response", rootElement(t, w.Body.Bytes()))
			for _, s := range jsonStrings(t, tc.want) {
				assert.Contains(t, w.Body.String(), xmlText(s), "reply value missing from XML")
			}
		})
	}
}

// A daemon error reply is surfaced as that awserrors code with nothing written.
func TestRegistrationDispatch_EveryActionSurfacesDaemonError(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)

	for _, tc := range dispatchCases() {
		t.Run(tc.name, func(t *testing.T) {
			serveOnce(t, nc, tc.subject, awserrors.GenerateErrorPayload(awserrors.ErrorELBv2LoadBalancerNotFound))

			w, _, err := invoke(t, elbv2awsapi.Deps{NATS: nc}, queryBody(t, tc.name, tc.input), nil)
			require.EqualError(t, err, awserrors.ErrorELBv2LoadBalancerNotFound)
			assert.Zero(t, w.Body.Len())
		})
	}
}

type refusingQuota struct{ limit int }

func (refusingQuota) EnforceLoadBalancers(context.Context, *nats.Conn, string, int) error {
	return errors.New(awserrors.ErrorELBv2TooManyLoadBalancers)
}
func (refusingQuota) Exempt(string) bool { return false }
func (q refusingQuota) LoadBalancerLimit(context.Context, string) (int, error) {
	return q.limit, nil
}

// The cap is checked before the daemon is asked to create anything.
func TestRegistrationDispatch_CreateLoadBalancerQuotaRefusalSkipsDaemon(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	got := serveOnce(t, nc, "elbv2.CreateLoadBalancer", []byte(`{}`))

	in := &elbv2.CreateLoadBalancerInput{Name: aws.String("my-alb"), Subnets: []*string{aws.String("subnet-1")}}
	_, _, err := invoke(t, elbv2awsapi.Deps{NATS: nc, Quota: refusingQuota{}}, queryBody(t, "CreateLoadBalancer", in), nil)
	require.EqualError(t, err, awserrors.ErrorELBv2TooManyLoadBalancers)

	select {
	case <-got:
		t.Fatal("the daemon was asked to create a load balancer past the cap")
	case <-time.After(200 * time.Millisecond):
	}
}

// An authorization denial is returned as is and the daemon is never reached.
func TestRegistrationDispatch_DenyBeforeDaemon(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	got := serveOnce(t, nc, "elbv2.DeleteLoadBalancer", []byte(`{}`))

	in := &elbv2.DeleteLoadBalancerInput{LoadBalancerArn: aws.String(testLBArn)}
	deny := errors.New(awserrors.ErrorUnauthorizedOperation)
	_, auths, err := invoke(t, elbv2awsapi.Deps{NATS: nc}, queryBody(t, "DeleteLoadBalancer", in), deny)

	require.ErrorIs(t, err, deny)
	require.Len(t, auths, 1)
	assert.Equal(t, []string{testLBArn}, auths[0].resources)
	select {
	case <-got:
		t.Fatal("the daemon was reached after a denial")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRegistrationDispatch_RequestShapeErrors(t *testing.T) {
	cases := []struct {
		name    string
		deps    elbv2awsapi.Deps
		body    string
		account string
		want    string
	}{
		{"missing Action", elbv2awsapi.Deps{}, "Version=2015-12-01", testAccountID, awserrors.ErrorMissingAction},
		{"unknown Action", elbv2awsapi.Deps{}, "Action=NoSuchThing", testAccountID, awserrors.ErrorInvalidAction},
		{"bad percent-encoding", elbv2awsapi.Deps{}, "Action=%zz", testAccountID, awserrors.ErrorMalformedQueryString},
		{"no account in auth context", elbv2awsapi.Deps{}, "Action=DescribeLoadBalancers", "", awserrors.ErrorInternalError},
		{"nil NATS", elbv2awsapi.Deps{}, "Action=DescribeLoadBalancers", testAccountID, awserrors.ErrorServerInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			inv := dispatch.Invocation{
				Request: req, AccountID: tc.account, Region: "us-east-1",
				Authorize: func(string, string, []string, iampolicy.ConditionKeys) error { return nil },
			}
			err := elbv2awsapi.NewRegistration(tc.deps).Dispatch(httptest.NewRecorder(), inv)
			require.EqualError(t, err, tc.want)
		})
	}
}

// The listener attribute actions are served in process, with no daemon.
func TestRegistrationDispatch_ListenerAttributesNeedNoDaemon(t *testing.T) {
	_, nc := testutil.StartTestNATS(t)
	for _, action := range []string{"DescribeListenerAttributes", "ModifyListenerAttributes"} {
		t.Run(action, func(t *testing.T) {
			form := url.Values{"Action": {action}, "ListenerArn": {testLsnrArn}}
			w, _, err := invoke(t, elbv2awsapi.Deps{NATS: nc}, form.Encode(), nil)
			require.NoError(t, err)
			assert.Equal(t, action+"Response", rootElement(t, w.Body.Bytes()))
		})
	}
}

// xmlText is s as the XML encoder writes it, so a value with a newline or an
// ampersand is looked up in its escaped form.
func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
