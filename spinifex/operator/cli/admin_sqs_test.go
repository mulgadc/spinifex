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
		`{"NodeId":"node2","Region":"ap-southeast-2","InstanceId":"i-0123456789abcdef0","Generation":3,"State":"bound"}`)

	var buf bytes.Buffer
	err := cli.EnsureSQSWorkerRemote(t.Context(), cli.AdminTarget(srv.URL, "ap-southeast-2", writeServerCA(t, srv)), "node2", "t3.small", &buf)
	require.NoError(t, err)

	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, "EnsureSQSWorker", form.Get("Action"))
	assert.Equal(t, "node2", form.Get("NodeId"))
	assert.Equal(t, "t3.small", form.Get("InstanceType"))
	assert.Contains(t, header.Get("Authorization"), "/ap-southeast-2/spinifex/aws4_request")

	out := buf.String()
	assert.Contains(t, out, "i-0123456789abcdef0")
	assert.Contains(t, out, "Generation:  3")
	assert.Contains(t, out, "not a ready worker")
	assert.NotContains(t, out, "is ready")
}

func TestSQSEnsureStore_SendsSizeAndReportsBoundVM(t *testing.T) {
	sqsCreds(t)
	srv, _, form, header := sqsGateway(t, http.StatusOK,
		`{"NodeId":"node2","Region":"ap-southeast-2","InstanceId":"i-0fedcba9876543210","Address":"10.240.0.12","InstanceType":"t3.small","State":"bound"}`)

	var buf bytes.Buffer
	err := cli.EnsureSQSStoreRemote(t.Context(), cli.AdminTarget(srv.URL, "ap-southeast-2", writeServerCA(t, srv)), "node2", "t3.small", 20, &buf)
	require.NoError(t, err)

	assert.Equal(t, "EnsureSQSStore", form.Get("Action"))
	assert.Equal(t, "node2", form.Get("NodeId"))
	assert.Equal(t, "t3.small", form.Get("InstanceType"))
	assert.Equal(t, "20", form.Get("VolumeSizeGiB"))
	assert.Contains(t, header.Get("Authorization"), "/spinifex/aws4_request")

	out := buf.String()
	assert.Contains(t, out, "10.240.0.12")
	assert.Contains(t, out, "not a ready store")
}

// A reply that is not a bound VM on the requested node and type is not
// reported as one.
func TestSQSEnsure_RefusesUnexpectedReplies(t *testing.T) {
	sqsCreds(t)
	worker := map[string]string{
		"not bound":   `{"NodeId":"node2","InstanceId":"i-0123456789abcdef0","State":"launching"}`,
		"other node":  `{"NodeId":"node3","InstanceId":"i-0123456789abcdef0","State":"bound"}`,
		"no instance": `{"NodeId":"node2","State":"bound"}`,
		"not json":    `<html/>`,
	}
	for name, reply := range worker {
		t.Run("worker/"+name, func(t *testing.T) {
			srv, _, _, _ := sqsGateway(t, http.StatusOK, reply)
			var buf bytes.Buffer
			err := cli.EnsureSQSWorkerRemote(t.Context(), cli.AdminTarget(srv.URL, "r", writeServerCA(t, srv)), "node2", "t3.small", &buf)
			require.Error(t, err)
			assert.Empty(t, buf.String())
		})
	}
	store := map[string]string{
		"other type": `{"NodeId":"node2","InstanceId":"i-0fedcba9876543210","InstanceType":"t3.large","State":"bound"}`,
		"other node": `{"NodeId":"node3","InstanceId":"i-0fedcba9876543210","InstanceType":"t3.small","State":"bound"}`,
		"not bound":  `{"NodeId":"node2","InstanceId":"i-0fedcba9876543210","InstanceType":"t3.small","State":""}`,
	}
	for name, reply := range store {
		t.Run("store/"+name, func(t *testing.T) {
			srv, _, _, _ := sqsGateway(t, http.StatusOK, reply)
			var buf bytes.Buffer
			err := cli.EnsureSQSStoreRemote(t.Context(), cli.AdminTarget(srv.URL, "r", writeServerCA(t, srv)), "node2", "t3.small", 20, &buf)
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
			err := cli.EnsureSQSWorkerRemote(t.Context(), cli.AdminTarget(srv.URL, "r", writeServerCA(t, srv)), "node2", "t3.small", io.Discard)
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
		"worker without node":  {"admin", "sqs", "ensure-worker", "--node=", "--instance-type=t3.small"},
		"worker without type":  {"admin", "sqs", "ensure-worker", "--node=node2", "--instance-type= "},
		"store without size":   {"admin", "sqs", "ensure-store", "--node=node2", "--instance-type=t3.small", "--volume-size-gib=0"},
		"store negative size":  {"admin", "sqs", "ensure-store", "--node=node2", "--instance-type=t3.small", "--volume-size-gib=-5"},
		"store without node":   {"admin", "sqs", "ensure-store", "--node=", "--instance-type=t3.small", "--volume-size-gib=20"},
		"store size not a num": {"admin", "sqs", "ensure-store", "--node=node2", "--instance-type=t3.small", "--volume-size-gib=big"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			err := cli.ExecuteRoot(append(args, common...), io.Discard)
			require.Error(t, err)
		})
	}
	assert.Equal(t, int32(0), calls.Load())
}
