package rdsgw

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/gwsign"
	handlers_rds "github.com/mulgadc/spinifex/spinifex/handlers/rds"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// gatewayStub captures the request the client sent and replies with the body a
// handler would produce. It answers plain HTTP; TLS pinning is New's concern,
// not Call's.
type gatewayStub struct {
	srv *httptest.Server

	gotAuth   string
	gotSHA    string
	gotType   string
	gotParams url.Values
}

// newGatewayStub serves respond() for every request, recording what arrived.
func newGatewayStub(t *testing.T, status int, respond func(form url.Values) []byte) *gatewayStub {
	t.Helper()
	stub := &gatewayStub{}
	stub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, err := url.ParseQuery(string(body))
		if err != nil {
			t.Errorf("gateway got an unparseable form body %q: %v", body, err)
		}
		stub.gotAuth = r.Header.Get("Authorization")
		stub.gotSHA = r.Header.Get("X-Amz-Content-Sha256")
		stub.gotType = r.Header.Get("Content-Type")
		stub.gotParams = form

		w.Header().Set("Content-Type", "text/xml")
		w.WriteHeader(status)
		_, _ = w.Write(respond(form))
	}))
	t.Cleanup(stub.srv.Close)
	return stub
}

// newTestClient points a client at the stub with static signing credentials.
func newTestClient(t *testing.T, stub *gatewayStub) *Client {
	t.Helper()
	c, err := New(stub.srv.URL, "", gwsign.NewStatic("AKIATEST", "secret"), "ap-southeast-2", 5*time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// xmlResult renders payload the way the gateway's typed adapter does, so the
// test exercises the real envelope rather than a hand-written approximation.
func xmlResult(t *testing.T, action string, payload any) []byte {
	t.Helper()
	body, err := utils.MarshalToXML(utils.GenerateIAMXMLPayload(action, payload))
	if err != nil {
		t.Fatalf("marshal %s result: %v", action, err)
	}
	return body
}

func TestCall_SignsAndFormEncodes(t *testing.T) {
	stub := newGatewayStub(t, http.StatusOK, func(url.Values) []byte {
		return xmlResult(t, "RegisterDBInstance", &handlers_rds.RegisterDBInstanceOutput{
			DBInstanceIdentifier: "db-1", HeartbeatIntervalSeconds: 30,
		})
	})

	var out handlers_rds.RegisterDBInstanceOutput
	err := newTestClient(t, stub).Call(context.Background(), "RegisterDBInstance",
		url.Values{"DBInstanceIdentifier": {"db-1"}, "AgentVersion": {"test"}}, &out)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if got := stub.gotParams.Get("Action"); got != "RegisterDBInstance" {
		t.Errorf("Action = %q, want RegisterDBInstance", got)
	}
	if got := stub.gotParams.Get("Version"); got != apiVersion {
		t.Errorf("Version = %q, want %s", got, apiVersion)
	}
	if got := stub.gotParams.Get("DBInstanceIdentifier"); got != "db-1" {
		t.Errorf("DBInstanceIdentifier = %q, want db-1", got)
	}
	if !strings.HasPrefix(stub.gotType, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q, want form encoding", stub.gotType)
	}

	// The gateway recovers the payload hash from X-Amz-Content-Sha256 and
	// verifies the scope, so both have to be on the wire for the call to
	// authenticate at all.
	if !strings.Contains(stub.gotAuth, "AWS4-HMAC-SHA256") || !strings.Contains(stub.gotAuth, "/ap-southeast-2/rds/aws4_request") {
		t.Errorf("Authorization = %q, want a SigV4 header scoped to rds", stub.gotAuth)
	}
	if stub.gotSHA == "" {
		t.Error("X-Amz-Content-Sha256 is absent; the gateway cannot verify the body")
	}

	if out.DBInstanceIdentifier != "db-1" || out.HeartbeatIntervalSeconds != 30 {
		t.Errorf("decoded output = %+v, want db-1/30", out)
	}
}

func TestCall_DecodesNestedListsAndOptionalFields(t *testing.T) {
	password := "s3cret"
	want := &handlers_rds.GetDBBootstrapConfigOutput{
		Mode:                 handlers_rds.BootstrapModeInitialize,
		DBInstanceIdentifier: "db-1",
		Engine:               "postgres",
		MasterUsername:       "master",
		MasterUserPassword:   &password,
		Port:                 5432,
		Parameters: []handlers_rds.Parameter{
			{Name: "max_connections", Value: "100"},
			{Name: "work_mem", Value: "4MB"},
		},
		ServingCertificate: "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n",
	}
	stub := newGatewayStub(t, http.StatusOK, func(url.Values) []byte {
		return xmlResult(t, "GetDBBootstrapConfig", want)
	})

	var got handlers_rds.GetDBBootstrapConfigOutput
	if err := newTestClient(t, stub).Call(context.Background(), "GetDBBootstrapConfig", nil, &got); err != nil {
		t.Fatalf("Call: %v", err)
	}

	if got.Mode != handlers_rds.BootstrapModeInitialize || got.Port != 5432 {
		t.Errorf("mode/port = %q/%d, want initialize/5432", got.Mode, got.Port)
	}
	if got.MasterUserPassword == nil || *got.MasterUserPassword != password {
		t.Errorf("MasterUserPassword = %v, want %q", got.MasterUserPassword, password)
	}
	if len(got.Parameters) != 2 || got.Parameters[1].Name != "work_mem" || got.Parameters[1].Value != "4MB" {
		t.Errorf("Parameters = %+v, want the two seeded members", got.Parameters)
	}
	if got.ServingCertificate != want.ServingCertificate {
		t.Errorf("ServingCertificate = %q, want the seeded PEM", got.ServingCertificate)
	}
}

// An absent optional field must decode as nil, not as an empty value: attach
// mode is defined by the password element being missing entirely.
func TestCall_AbsentOptionalStaysNil(t *testing.T) {
	stub := newGatewayStub(t, http.StatusOK, func(url.Values) []byte {
		return xmlResult(t, "GetDBBootstrapConfig", &handlers_rds.GetDBBootstrapConfigOutput{
			Mode:           handlers_rds.BootstrapModeAttach,
			MasterUsername: "master",
			Port:           5432,
		})
	})

	var got handlers_rds.GetDBBootstrapConfigOutput
	if err := newTestClient(t, stub).Call(context.Background(), "GetDBBootstrapConfig", nil, &got); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.MasterUserPassword != nil {
		t.Errorf("MasterUserPassword = %q, want nil in attach mode", *got.MasterUserPassword)
	}
}

// commandsResult mirrors the gateway's PollDBCommands output on both sides of
// the wire: the locationName tags drive the gateway's marshal, the xml tags the
// agent's decode.
type commandsResult struct {
	Commands []handlers_rds.Command `locationName:"Commands" locationNameList:"member" xml:"Commands>member"`
}

func TestCall_RoundTripsCommands(t *testing.T) {
	issued := time.Date(2026, 7, 28, 4, 5, 6, 0, time.UTC)
	want := commandsResult{Commands: []handlers_rds.Command{{
		CommandID:  "cmd-1",
		Type:       "set-master-password",
		Parameters: []handlers_rds.Parameter{{Name: "MasterUserPassword", Value: "s3cret"}},
		IssuedAt:   &issued,
	}}}
	stub := newGatewayStub(t, http.StatusOK, func(url.Values) []byte {
		return xmlResult(t, "PollDBCommands", &want)
	})

	var got commandsResult
	if err := newTestClient(t, stub).Call(context.Background(), "PollDBCommands", nil, &got); err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(got.Commands) != 1 {
		t.Fatalf("decoded %d commands, want 1", len(got.Commands))
	}
	cmd := got.Commands[0]
	if cmd.CommandID != "cmd-1" || cmd.Type != "set-master-password" {
		t.Errorf("command = %+v, want cmd-1/set-master-password", cmd)
	}
	if len(cmd.Parameters) != 1 || cmd.Parameters[0].Name != "MasterUserPassword" {
		t.Errorf("Parameters = %+v, want the seeded member", cmd.Parameters)
	}
	if cmd.IssuedAt == nil || !cmd.IssuedAt.Equal(issued) {
		t.Errorf("IssuedAt = %v, want %v", cmd.IssuedAt, issued)
	}
}

// An empty poll is the steady state, and it must decode as no commands rather
// than as one zero-valued command the agent would then try to execute.
func TestCall_EmptyPollDecodesToNoCommands(t *testing.T) {
	stub := newGatewayStub(t, http.StatusOK, func(url.Values) []byte {
		return xmlResult(t, "PollDBCommands", &commandsResult{})
	})

	var got commandsResult
	if err := newTestClient(t, stub).Call(context.Background(), "PollDBCommands", nil, &got); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(got.Commands) != 0 {
		t.Errorf("decoded %d commands, want 0: %+v", len(got.Commands), got.Commands)
	}
}

func TestCall_ErrorEnvelopeBecomesAPIError(t *testing.T) {
	stub := newGatewayStub(t, http.StatusForbidden, func(url.Values) []byte {
		return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<ErrorResponse><Error><Type>Sender</Type><Code>AccessDenied</Code>` +
			`<Message>not authorized</Message></Error><RequestId>req-1</RequestId></ErrorResponse>`)
	})

	err := newTestClient(t, stub).Call(context.Background(), "PollDBCommands", nil, nil)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Call error = %v (%T), want *APIError", err, err)
	}
	if apiErr.Code != "AccessDenied" || apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("APIError = %+v, want AccessDenied/403", apiErr)
	}
	if !strings.Contains(apiErr.Error(), "not authorized") {
		t.Errorf("Error() = %q, want the gateway's message", apiErr.Error())
	}
}

// A failure that never reached the gateway's error rendering still has to
// surface as an APIError carrying what did arrive.
func TestCall_NonEnvelopeErrorBodyStillReported(t *testing.T) {
	stub := newGatewayStub(t, http.StatusBadGateway, func(url.Values) []byte {
		return []byte("upstream connect failed")
	})

	err := newTestClient(t, stub).Call(context.Background(), "SubmitDBStateChange", nil, nil)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Call error = %v (%T), want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusBadGateway || !strings.Contains(apiErr.Message, "upstream connect failed") {
		t.Errorf("APIError = %+v, want the 502 body preserved", apiErr)
	}
}

func TestCall_ContextCancellationPropagates(t *testing.T) {
	stub := newGatewayStub(t, http.StatusOK, func(url.Values) []byte { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := newTestClient(t, stub).Call(ctx, "PollDBCommands", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Call error = %v, want context.Canceled", err)
	}
}

func TestNew_RejectsIncompleteConfig(t *testing.T) {
	signer := gwsign.NewStatic("AKIATEST", "secret")
	if _, err := New("", "", signer, "us-east-1", 0); err == nil {
		t.Error("New with no baseURL succeeded, want an error")
	}
	if _, err := New("https://gw.example", "", nil, "us-east-1", 0); err == nil {
		t.Error("New with no signer succeeded, want an error")
	}
	if _, err := New("https://gw.example", "/nonexistent/ca.pem", signer, "us-east-1", 0); err == nil {
		t.Error("New with an unreadable CA succeeded, want an error")
	}
}
