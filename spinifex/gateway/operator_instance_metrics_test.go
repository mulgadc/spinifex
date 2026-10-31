package gateway

import (
	"bufio"
	"encoding/json"
	telemetryv1 "github.com/mulgadc/spinifex/contracts/telemetry/v1"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
