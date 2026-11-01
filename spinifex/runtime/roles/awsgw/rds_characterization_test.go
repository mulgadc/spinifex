// Characterization tests for RDS Query-protocol dispatch as it exists in
// rds.go today, pinned ahead of moving dispatch out of gateway into a
// registration seam. These assert current behaviour, not AWS parity.

package awsgw_test

import (
	"bytes"
	"encoding/xml"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mulgadc/bluebottle/pkg/sigv4"
	"github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	rdsinstance "github.com/mulgadc/spinifex/spinifex/domains/rds/instance"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/audit"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rdsPipelineApp wires SigV4AuthMiddleware -> Request as SetupRoutes does, on
// live test NATS, with docs as the caller's policy.
func rdsPipelineApp(t *testing.T, docs []iamdomain.PolicyDocument) (*gateway.GatewayConfig, http.Handler) {
	t.Helper()
	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	require.NoError(t, err)
	_, nc := testutil.StartTestNATS(t)

	gw := withVerifier(gwfix.WithDeps(&gateway.GatewayConfig{
		DisableLogging: true,
		Region:         gwfix.Region,
	}, gwfix.Deps{NATS: nc, Nodes: 1, IAM: &gwfix.PolicyIAM{
		IAM: gwfix.IAM{
			MasterKey: gwfix.MasterKey,
			AccessKeys: map[string]*iamdomain.AccessKey{
				gwfix.AccessKey: {
					AccessKeyID:     gwfix.AccessKey,
					SecretAccessKey: encryptedSecret,
					UserName:        "alice",
					AccountID:       authzAccountID,
					Status:          "Active",
				},
			},
		},
		GetUserPoliciesFn: func(_, _ string) ([]iamdomain.PolicyDocument, error) { return docs, nil },
	}}))
	withRDS(gw)

	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)
	return gw, r
}

func signRDSRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	b := []byte(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	gwfix.SignRequestAs(t, req, b, gwfix.AccessKey, gwfix.SecretKey, "rds", time.Now().UTC())
	return req
}

func respondRDS(t *testing.T, nc *nats.Conn, subject, payload string) *int {
	t.Helper()
	calls := 0
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		calls++
		_ = msg.Respond([]byte(payload))
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return &calls
}

func rdsInstanceARN(id string) string {
	return "arn:aws:rds:" + gwfix.Region + ":" + authzAccountID + ":db:" + id
}

func TestRDSPipeline_ErrorEnvelopes_GoldenXML(t *testing.T) {
	cases := []struct {
		name, body, code, message string
		status                    int
	}{
		{"missing action", "", "MissingAction", "The request is missing an action or a required parameter.", http.StatusBadRequest},
		{"unknown action", "Action=Bogus", "InvalidAction", "The action or operation requested is not valid. Verify that the action is typed correctly.", http.StatusBadRequest},
		{"malformed query string", "Action=%zz", "MalformedQueryString", "The query string contains a syntax error.", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, app := rdsPipelineApp(t, allowAllTaggingDocs())
			resp := doRequest(app, signRDSRequest(t, tc.body))

			require.Equal(t, tc.status, resp.StatusCode)
			assert.Equal(t, "application/xml", resp.Header.Get("Content-Type"))
			assert.Equal(t, `<?xml version="1.0" encoding="UTF-8"?>
<ErrorResponse>
  <Error>
    <Type>Sender</Type>
    <Code>`+tc.code+`</Code>
    <Message>`+tc.message+`</Message>
  </Error>
  <RequestId>ID</RequestId>
</ErrorResponse>`, normalizeRequestID(readBody(t, resp)))
		})
	}
}

// An action RDS recognises but does not implement keeps its own wording.
func TestRDSPipeline_UnsupportedAction_Envelope(t *testing.T) {
	_, app := rdsPipelineApp(t, allowAllTaggingDocs())
	resp := doRequest(app, signRDSRequest(t, "Action=CreateDBCluster"))

	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "application/xml", resp.Header.Get("Content-Type"))
	assert.Equal(t, `<?xml version="1.0" encoding="UTF-8"?>
<ErrorResponse>
  <Error>
    <Type>Sender</Type>
    <Code>OperationNotSupportedException</Code>
    <Message>The specified RDS action is not supported in the RDS v1 API.</Message>
  </Error>
  <RequestId>ID</RequestId>
</ErrorResponse>`, normalizeRequestID(readBody(t, resp)))
}

func TestRDSPipeline_NoResponder_500InternalError(t *testing.T) {
	_, app := rdsPipelineApp(t, allowAllTaggingDocs())
	resp := doRequest(app, signRDSRequest(t, "Action=DescribeDBInstances"))

	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, "application/xml", resp.Header.Get("Content-Type"))
	assert.Contains(t, readBody(t, resp), "<Code>InternalError</Code>")
}

func TestRDSPipeline_DescribeDBEngineVersions_Golden(t *testing.T) {
	_, app := rdsPipelineApp(t, allowAllTaggingDocs())
	resp := doRequest(app, signRDSRequest(t, "Action=DescribeDBEngineVersions"))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/xml", resp.Header.Get("Content-Type"))
	var out struct {
		Versions []struct {
			Engine  string `xml:"Engine"`
			Version string `xml:"EngineVersion"`
		} `xml:"DescribeDBEngineVersionsResult>DBEngineVersions>DBEngineVersion"`
	}
	body := readBody(t, resp)
	require.NoError(t, xml.Unmarshal([]byte(body), &out))
	got := map[string]string{}
	for _, v := range out.Versions {
		got[v.Engine] = v.Version
	}
	assert.Equal(t, map[string]string{"mariadb": "11.8", "postgres": "18"}, got)
}

func TestRDSPipeline_DescribeDBInstances_Golden(t *testing.T) {
	gw, app := rdsPipelineApp(t, allowAllTaggingDocs())
	calls := respondRDS(t, gwfix.DepsOf(gw).NATS, "rds.DescribeDBInstances", `{"DBInstances":[]}`)
	resp := doRequest(app, signRDSRequest(t, "Action=DescribeDBInstances"))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/xml", resp.Header.Get("Content-Type"))
	assert.Equal(t, 1, *calls)
	assert.Equal(t, `<DescribeDBInstancesResponse><DescribeDBInstancesResult><DBInstances></DBInstances></DescribeDBInstancesResult></DescribeDBInstancesResponse>`, readBody(t, resp))
}

func TestRDSPipeline_Authz_DeniedStopsBeforeHandler(t *testing.T) {
	docs := scopedTaggingDocs("Allow", "rds:DeleteDBInstance", rdsInstanceARN("other"))
	gw, app := rdsPipelineApp(t, docs)
	calls := respondRDS(t, gwfix.DepsOf(gw).NATS, "rds.DeleteDBInstance", `{}`)

	resp := doRequest(app, signRDSRequest(t, "Action=DeleteDBInstance&DBInstanceIdentifier=prod"))

	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, readBody(t, resp), "<Code>AccessDenied</Code>")
	assert.Zero(t, *calls)
}

// rds:* is the natural admin grant and must not reach an internal agent action.
func TestRDSPipeline_Authz_InternalActionDeniedToAdmin(t *testing.T) {
	gw, app := rdsPipelineApp(t, allowAllTaggingDocs())
	for _, action := range rdsinstance.InternalAgentActions {
		t.Run(action, func(t *testing.T) {
			calls := respondRDS(t, gwfix.DepsOf(gw).NATS, "rds."+action, `{}`)
			resp := doRequest(app, signRDSRequest(t, "Action="+action))

			require.Equal(t, http.StatusForbidden, resp.StatusCode)
			assert.Contains(t, readBody(t, resp), "<Code>AccessDenied</Code>")
			assert.Zero(t, *calls)
		})
	}
}

func TestRDSPipeline_Authz_UnrelatedGrantDeniedUnknownActionNot(t *testing.T) {
	_, app := rdsPipelineApp(t, scopedTaggingDocs("Allow", "ec2:DescribeTags", "*"))

	resp := doRequest(app, signRDSRequest(t, "Action=DescribeDBInstances"))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	resp = doRequest(app, signRDSRequest(t, "Action=Bogus"))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, readBody(t, resp), "<Code>InvalidAction</Code>")
}

func TestRDSPipeline_NilNATS_503ServiceUnavailable(t *testing.T) {
	gw := withVerifier(gwfix.WithDeps(&gateway.GatewayConfig{DisableLogging: true}, gwfix.Deps{IAM: allowAllIAMService()}))
	withRDS(gw)

	w := httptest.NewRecorder()
	gw.Request(w, setupRDSRequest("Action=DescribeDBInstances"))

	resp := w.Result()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, "application/xml", resp.Header.Get("Content-Type"))
	assert.Contains(t, readBody(t, resp), "<Code>ServiceUnavailable</Code>")
}

func TestRDSPipeline_OversizedBody_413OverHTTP(t *testing.T) {
	_, app := rdsPipelineApp(t, allowAllTaggingDocs())

	oversized := bytes.Repeat([]byte("x"), int(sigv4.MaxPayloadLen)+1)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(oversized))
	req.Host = "localhost:9999"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	gwfix.SignRequestAs(t, req, oversized, gwfix.AccessKey, gwfix.SecretKey, "rds", time.Now().UTC())
	resp := doRequest(app, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	assert.Contains(t, readBody(t, resp), "<Code>RequestEntityTooLarge</Code>")
}

func TestRDSPipeline_RequestAudit_RecordsResolvedAction(t *testing.T) {
	gw, _ := rdsPipelineApp(t, allowAllTaggingDocs())

	var got *audit.Record
	r := chi.NewRouter()
	r.Use(audit.Middleware)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
			next.ServeHTTP(w, rq)
			got = audit.From(rq.Context())
		})
	})
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", gw.Request)

	doRequest(r, signRDSRequest(t, "Action=DescribeDBEngineVersions"))

	require.NotNil(t, got)
	assert.Equal(t, "DescribeDBEngineVersions", auditField(got, "action"))
	assert.Equal(t, "rds", auditField(got, "service"))
}
