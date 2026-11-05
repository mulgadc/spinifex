//go:build lambdatestlink && lambdatestlinkseam

//test:in-package — drives the unexported test-link executor and seam fixtures
package awsgw

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/agents/lambda/runtimeapi"
	lambdaawsapi "github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/testlink"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
)

// jsonString returns a JSON string literal of exactly n bytes.
func jsonString(n int) []byte {
	return []byte(`"` + strings.Repeat("a", n-2) + `"`)
}

// invokeComposedBody is invokeComposedIn with a signed Invoke carrying body.
func invokeComposedBody(t *testing.T, body []byte, codeURL string, client *http.Client, guest guestFunc) seamRun {
	t.Helper()
	env := newSeamEnv(t)
	exec, daemon, grants, presigns := composedExecutor(t, env, codeURL, client, guest)
	reg := lambdaawsapi.NewRegistration(lambdaawsapi.Deps{Meta: seamFunction{timeout: 30, role: env.roleARN}, Exec: exec})
	req := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/f/invocations", bytes.NewReader(body))
	req.Host = "localhost:9999"
	gwfix.SignRequestAs(t, req, body, gwfix.AccessKey, gwfix.SecretKey, lambdaawsapi.ServiceName, time.Now().UTC())
	w := httptest.NewRecorder()
	defaultGateway(t, lambdaTestRoutes(reg)...).ServeHTTP(w, req)
	resp := w.Result()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	reqs, replies := daemon.recorded()
	return seamRun{env: env, resp: resp, body: out, requests: reqs, replies: replies, presigns: presigns.Load(), grants: grants.issued.Load()}
}

// echoGuest is a function runtime that returns its event unchanged.
func echoGuest(base string) {
	resp, err := http.Get(base + "invocation/next")
	if err != nil {
		return
	}
	event, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	id := resp.Header.Get("Lambda-Runtime-Aws-Request-Id")
	if r, err := http.Post(base+"invocation/"+id+"/response", "application/json", bytes.NewReader(event)); err == nil {
		_ = r.Body.Close()
	}
}

// A 6 MiB event crosses the real gateway, control socket and guest link,
// and the 6 MiB response returns byte-identical.
func TestLambdaTestLinkSeam_SixMiBRequestAndResponseRoundTrip(t *testing.T) {
	require.Equal(t, testlink.MaxPayloadBytes, runtimeapi.MaxResponseBytes)
	event := jsonString(testlink.MaxPayloadBytes)
	srv := codeServer(t)
	run := invokeComposedBody(t, event, srv.URL+"/code", srv.Client(), agentGuest(0, echoGuest))
	require.Equal(t, http.StatusOK, run.resp.StatusCode, string(run.body[:min(len(run.body), 300)]))
	require.Empty(t, run.resp.Header.Get("X-Amz-Function-Error"))
	require.Len(t, run.body, testlink.MaxPayloadBytes)
	require.True(t, bytes.Equal(event, run.body), "response differs from the event")
	require.Len(t, run.replies, 1)
}

// A 6 MiB + 1 event is refused with 413 before anything is presigned,
// granted or sent to the daemon.
func TestLambdaTestLinkSeam_OverSixMiBRequestRefusedBeforeDispatch(t *testing.T) {
	srv := codeServer(t)
	run := invokeComposedBody(t, jsonString(testlink.MaxPayloadBytes+1), srv.URL+"/code", srv.Client(), agentGuest(0, echoGuest))
	require.Equal(t, http.StatusRequestEntityTooLarge, run.resp.StatusCode)
	require.Contains(t, run.resp.Header.Get("X-Amzn-Errortype"), "RequestEntityTooLargeException")
	require.Zero(t, run.presigns)
	require.Zero(t, run.grants)
	require.Empty(t, run.requests)
	require.Zero(t, run.env.sessionCount(t))
}
