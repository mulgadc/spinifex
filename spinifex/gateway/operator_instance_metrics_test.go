package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	telemetryv1 "github.com/mulgadc/spinifex/contracts/telemetry/v1"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	streamAccount  = "000000000002"
	streamInstance = "i-0123456789abcdef0"
)

func instanceMetricsDocs(resource string) []iamdomain.PolicyDocument {
	return []iamdomain.PolicyDocument{{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"spinifex:GetInstanceMetrics"}, Resource: iamdomain.StringOrArr{resource}},
		},
	}}
}

// answerWatches replies to the instance's watch with one sample, as the
// owning node's collector would.
func answerWatches(t *testing.T, nc *nats.Conn) {
	t.Helper()
	ring, err := json.Marshal(telemetryv1.LiveRing{Samples: []telemetryv1.LiveSample{{TimeMillis: 1000, CPUPercent: 7}}})
	require.NoError(t, err)
	sub, err := nc.Subscribe(telemetryv1.LiveWatchSubject(streamAccount, streamInstance), func(msg *nats.Msg) {
		if msg.Reply != "" {
			_ = msg.Respond(ring)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// postSignedMetrics sends a signed GetInstanceMetrics over real HTTP, so the
// response can be read while the stream is still open.
func postSignedMetrics(t *testing.T, srv *httptest.Server) *http.Response {
	t.Helper()
	body := []byte(url.Values{"Action": {"GetInstanceMetrics"}, "InstanceId": {streamInstance}}.Encode())
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/", bytes.NewReader(body))
	require.NoError(t, err)
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	gwfix.SignRequestAs(t, req, body, gwfix.AccessKey, gwfix.SecretKey, "spinifex", time.Now().UTC())
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// Through the signed pipeline, a tenant's policy naming the instance's ARN is
// enough, and the first sample arrives while the stream is still open.
func TestOperatorQuery_GetInstanceMetrics_StreamsThroughThePipeline(t *testing.T) {
	instanceARN := "arn:aws:ec2:" + gwfix.Region + ":" + streamAccount + ":instance/" + streamInstance
	gw, app := operatorPipelineApp(t, streamAccount, instanceMetricsDocs(instanceARN), nil)
	answerWatches(t, gw.NATSConn)
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)

	resp := postSignedMetrics(t, srv)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	lines := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
				lines <- data
				return
			}
		}
	}()
	select {
	case data := <-lines:
		var s telemetryv1.LiveSample
		require.NoError(t, json.Unmarshal([]byte(data), &s))
		assert.Equal(t, int64(1000), s.TimeMillis)
	case <-time.After(5 * time.Second):
		t.Fatal("no sample was flushed through the pipeline")
	}
}

func TestOperatorQuery_GetInstanceMetrics_PolicyOnAnotherInstanceIsDenied(t *testing.T) {
	otherARN := "arn:aws:ec2:" + gwfix.Region + ":" + streamAccount + ":instance/i-0fffffffffffffff0"
	gw, app := operatorPipelineApp(t, streamAccount, instanceMetricsDocs(otherARN), nil)
	answerWatches(t, gw.NATSConn)
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)

	resp := postSignedMetrics(t, srv)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, awserrors.ErrorAccessDenied, ec2ErrorCode(t, resp))
}
