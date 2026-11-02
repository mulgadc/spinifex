package cli_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/operator/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sqsGateway is a TLS gateway stand-in that records each request and answers
// with a fixed status and body.
func sqsGateway(t *testing.T, status int, reply string) (*httptest.Server, *atomic.Int32, *url.Values, *http.Header) {
	t.Helper()
	var calls atomic.Int32
	form := &url.Values{}
	header := &http.Header{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		parsed, _ := url.ParseQuery(string(b))
		*form, *header = parsed, r.Header.Clone()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, form, header
}

func sqsCreds(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
}

func TestSQSEnsureWorker_SendsSignedActionAndReportsBoundVM(t *testing.T) {
	sqsCreds(t)
	srv, calls, form, header := sqsGateway(t, http.StatusOK,
		`{"NodeId":"node2","Region":"ap-southeast-2","InstanceId":"i-0123456789abcdef0","Generation":3,"InstanceType":"t3.small","State":"bound"}`)

	var buf bytes.Buffer
	err := cli.EnsureSQSWorkerRemote(t.Context(), cli.AdminTarget(srv.URL, "ap-southeast-2", writeServerCA(t, srv)), "node2", "t3.small", 20, &buf)
	require.NoError(t, err)

	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, "EnsureSQSWorker", form.Get("Action"))
	assert.Equal(t, "node2", form.Get("NodeId"))
	assert.Equal(t, "t3.small", form.Get("InstanceType"))
	assert.Equal(t, "20", form.Get("VolumeSizeGiB"))
	assert.Contains(t, header.Get("Authorization"), "/ap-southeast-2/spinifex/aws4_request")

	out := buf.String()
	assert.Contains(t, out, "i-0123456789abcdef0")
	assert.Contains(t, out, "Generation:     3")
	assert.Contains(t, out, "Instance type:  t3.small")
	assert.Contains(t, out, "not ready SQS")
	assert.NotContains(t, out, "is ready")
}

// A reply that is not a bound VM on the requested node and type is not
// reported as one.
func TestSQSEnsure_RefusesUnexpectedReplies(t *testing.T) {
	sqsCreds(t)
	worker := map[string]string{
		"not bound":   `{"NodeId":"node2","InstanceId":"i-0123456789abcdef0","InstanceType":"t3.small","State":"launching"}`,
		"other node":  `{"NodeId":"node3","InstanceId":"i-0123456789abcdef0","InstanceType":"t3.small","State":"bound"}`,
		"no instance": `{"NodeId":"node2","InstanceType":"t3.small","State":"bound"}`,
		"other type":  `{"NodeId":"node2","InstanceId":"i-0123456789abcdef0","InstanceType":"t3.large","State":"bound"}`,
		"no type":     `{"NodeId":"node2","InstanceId":"i-0123456789abcdef0","State":"bound"}`,
		"not json":    `<html/>`,
	}
	for name, reply := range worker {
		t.Run("worker/"+name, func(t *testing.T) {
			srv, _, _, _ := sqsGateway(t, http.StatusOK, reply)
			var buf bytes.Buffer
			err := cli.EnsureSQSWorkerRemote(t.Context(), cli.AdminTarget(srv.URL, "r", writeServerCA(t, srv)), "node2", "t3.small", 20, &buf)
			require.Error(t, err)
			assert.Empty(t, buf.String())
		})
	}
}

// Gateway refusals surface with their code, so an operator can tell a pending
// launch from one that needs reconciliation.
func TestSQSEnsure_SurfacesGatewayErrorCodes(t *testing.T) {
	sqsCreds(t)
	for _, code := range []string{"ServiceUnavailable", "IncorrectState", "AccessDenied"} {
		t.Run(code, func(t *testing.T) {
			srv, _, _, _ := sqsGateway(t, http.StatusBadRequest,
				`<Response><Errors><Error><Code>`+code+`</Code><Message>m</Message></Error></Errors><RequestID>r1</RequestID></Response>`)
			err := cli.EnsureSQSWorkerRemote(t.Context(), cli.AdminTarget(srv.URL, "r", writeServerCA(t, srv)), "node2", "t3.small", 20, io.Discard)
			require.Error(t, err)
			assert.Contains(t, err.Error(), code)
		})
	}
}

// Missing or invalid inputs are refused before any request is sent. Every flag
// is passed explicitly because cobra keeps flag values between runs.
func TestSQSEnsure_ValidatesInputsBeforeCallingTheGateway(t *testing.T) {
	sqsCreds(t)
	srv, calls, _, _ := sqsGateway(t, http.StatusOK, `{}`)
	ca := writeServerCA(t, srv)
	common := []string{"--endpoint", srv.URL, "--region", "r", "--ca-bundle", ca}

	cases := map[string][]string{
		"without node":   {"admin", "sqs", "ensure-worker", "--node=", "--instance-type=t3.small", "--volume-size-gib=20"},
		"without type":   {"admin", "sqs", "ensure-worker", "--node=node2", "--instance-type= ", "--volume-size-gib=20"},
		"without size":   {"admin", "sqs", "ensure-worker", "--node=node2", "--instance-type=t3.small", "--volume-size-gib=0"},
		"negative size":  {"admin", "sqs", "ensure-worker", "--node=node2", "--instance-type=t3.small", "--volume-size-gib=-5"},
		"size not a num": {"admin", "sqs", "ensure-worker", "--node=node2", "--instance-type=t3.small", "--volume-size-gib=big"},
		"enable no size": {"admin", "sqs", "enable", "--node=node2", "--instance-type=t3.small", "--volume-size-gib=0"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			err := cli.ExecuteRoot(append(args, common...), io.Discard)
			require.Error(t, err)
		})
	}
	assert.Equal(t, int32(0), calls.Load())
}

// Enable sends the instance type and size, renders requested apart from
// ready, and refuses a reply that does not show the service enabled on node.
func TestSQSEnable_SendsSignedActionAndSeparatesRequestedFromReady(t *testing.T) {
	sqsCreds(t)
	srv, calls, form, _ := sqsGateway(t, http.StatusOK,
		`{"Region":"ap-southeast-2","Desired":"enabled","NodeId":"node1","Generation":1,"Instance":"bound","InstanceId":"i-0123456789abcdef0","ReceiptKey":"pending","WorkerPolling":false,"TenantAPI":"not-registered","Ready":false}`)
	var buf bytes.Buffer
	target := cli.AdminTarget(srv.URL, "ap-southeast-2", writeServerCA(t, srv))
	require.NoError(t, cli.EnableSQSRemote(t.Context(), target, "node1", "t3.small", 8, &buf))
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, "EnableSQS", form.Get("Action"))
	assert.Equal(t, "t3.small", form.Get("InstanceType"))
	assert.Equal(t, "8", form.Get("VolumeSizeGiB"))
	assert.Contains(t, buf.String(), "Requested:      enabled")
	assert.Contains(t, buf.String(), "Receipt key:    pending")
	assert.Contains(t, buf.String(), "Tenant API:     not-registered")
	assert.Contains(t, buf.String(), "Ready:          false")

	srv, _, _, _ = sqsGateway(t, http.StatusOK, `{"Region":"ap-southeast-2","Desired":"disabled","NodeId":"node1"}`)
	target = cli.AdminTarget(srv.URL, "ap-southeast-2", writeServerCA(t, srv))
	require.Error(t, cli.EnableSQSRemote(t.Context(), target, "node1", "t3.small", 8, &buf))
}
