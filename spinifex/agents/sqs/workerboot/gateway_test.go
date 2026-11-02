package workerboot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	"github.com/mulgadc/spinifex/internal/gwsign"
	"github.com/mulgadc/spinifex/internal/sqsworkergw"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/agents/sqs/workerkey"
	sqsworkerapi "github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi/worker"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerrole"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

const (
	gwInstance = "i-0abc12345def67890"
	gwVolume   = "vol-0a1b2c3d4e5f60718"
)

// session is one instance-role credential: its secret and the instance ID
// STS names the session after.
type session struct{ secret, instance string }

// localGateway stands for the gateway's ingress in front of the real
// spinifex-sqs registration: it verifies each SigV4 signature against the
// issued credential, names the caller from that session alone, and grants
// only the worker role's actions. Every request body is kept for inspection.
type localGateway struct {
	t        *testing.T
	srv      *httptest.Server
	reg      dispatch.Registration
	mu       sync.Mutex
	sessions map[string]session
	bodies   []string
	// dropReply, while set, applies a request and then drops its reply.
	dropReply func(action string) bool
	// replied, when set, receives each reply's error code, "" on success.
	replied chan string
}

func newLocalGateway(t *testing.T, deps sqsworkerapi.Deps) *localGateway {
	t.Helper()
	g := &localGateway{t: t, reg: sqsworkerapi.NewRegistration(deps), sessions: map[string]session{}}
	g.srv = httptest.NewTLSServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *localGateway) issue(accessKey, secret, instance string) *gwsign.Signer {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessions[accessKey] = session{secret: secret, instance: instance}
	return gwsign.NewStatic(accessKey, secret)
}

func (g *localGateway) caPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: g.srv.Certificate().Raw})
}

func (g *localGateway) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		g.fail(w, http.StatusBadRequest, "MalformedRequest")
		return
	}
	g.mu.Lock()
	g.bodies = append(g.bodies, string(body))
	g.mu.Unlock()

	sess, service, region, ok := g.verify(r, body)
	if !ok {
		g.fail(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	}
	if service != sqsworkerapi.ServiceName {
		g.fail(w, http.StatusBadRequest, "InvalidAction")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	action := ""
	if q, err := parseForm(body); err == nil {
		action = q
	}
	rec := httptest.NewRecorder()
	err = g.reg.Dispatch(rec, dispatch.Invocation{
		Request:   r,
		AccountID: awsidentifiers.GlobalAccountID,
		Service:   service,
		Region:    region,
		Authorize: func(svc, act string, _ []string, _ iampolicy.ConditionKeys) error {
			if svc != sqsworkerapi.ServiceName || !slices.Contains(workerrole.Actions(), act) {
				return &dispatch.PolicyDenialError{}
			}
			return nil
		},
		Actor: func() (dispatch.Actor, error) {
			return dispatch.Actor{
				AccountID:     awsidentifiers.GlobalAccountID,
				PrincipalType: "assumed-role",
				PrincipalARN:  "arn:aws:iam::" + awsidentifiers.GlobalAccountID + ":role/" + workerrole.RoleName,
				SessionName:   sess.instance,
			}, nil
		},
	})
	if err != nil {
		code := err.Error()
		status := http.StatusBadRequest
		switch code {
		case "AccessDenied":
			status = http.StatusForbidden
		case "ServiceUnavailable":
			status = http.StatusServiceUnavailable
		}
		g.fail(w, status, code)
		return
	}
	if g.dropReply != nil && g.dropReply(action) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			g.t.Error("cannot drop a reply")
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return
	}
	g.reply("")
	maps.Copy(w.Header(), rec.Header())
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

func parseForm(body []byte) (string, error) {
	r, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r.ParseForm(); err != nil {
		return "", err
	}
	return r.PostForm.Get("Action"), nil
}

// verify recomputes the signature with the issued secret. It returns the
// session, and the service and Region the credential scope names.
func (g *localGateway) verify(r *http.Request, body []byte) (session, string, string, bool) {
	authz := r.Header.Get("Authorization")
	const prefix = "AWS4-HMAC-SHA256 Credential="
	if !strings.HasPrefix(authz, prefix) {
		return session{}, "", "", false
	}
	scope, _, _ := strings.Cut(strings.TrimPrefix(authz, prefix), ",")
	parts := strings.Split(scope, "/")
	if len(parts) != 5 || parts[4] != "aws4_request" {
		return session{}, "", "", false
	}
	g.mu.Lock()
	sess, ok := g.sessions[parts[0]]
	g.mu.Unlock()
	if !ok {
		return session{}, "", "", false
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	if r.Header.Get("X-Amz-Content-Sha256") != hash {
		return session{}, "", "", false
	}
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		return session{}, "", "", false
	}
	check, err := http.NewRequest(r.Method, "https://"+r.Host+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		return session{}, "", "", false
	}
	check.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	check.Header.Set("X-Amz-Content-Sha256", hash)
	creds := aws.Credentials{AccessKeyID: parts[0], SecretAccessKey: sess.secret}
	if err := v4.NewSigner().SignHTTP(context.Background(), creds, check, hash, parts[3], parts[2], at); err != nil {
		return session{}, "", "", false
	}
	return sess, parts[3], parts[2], check.Header.Get("Authorization") == authz
}

func (g *localGateway) fail(w http.ResponseWriter, status int, code string) {
	g.reply(code)
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<ErrorResponse><Error><Code>%s</Code></Error></ErrorResponse>", code)
}

func (g *localGateway) reply(code string) {
	if g.replied != nil {
		g.replied <- code
	}
}

// gwCluster is the domain's real worker-identity record behind the gateway.
type gwCluster struct {
	ids workerident.Store
	gw  *localGateway
}

// newGWCluster hands the gateway's work actions to work and has it report
// hostNodes as its cluster size; nil hostNodes is the declared size.
func newGWCluster(t *testing.T, work sqsworkerapi.Work, hostNodes func() int) gwCluster {
	t.Helper()
	clustersize.DeclareForTest(t, 1)
	_, _, js := testutil.StartTestJetStream(t)
	ids, err := workerident.Open(context.Background(), js)
	if err != nil {
		t.Fatal(err)
	}
	deps := sqsworkerapi.JetStreamDeps(js)
	deps.Work = work
	deps.HostNodes = hostNodes
	return gwCluster{ids: ids, gw: newLocalGateway(t, deps)}
}

// launch records a worker on a newly created volume, so its receipt key is
// pending, and binds it to inst.
func (c gwCluster) launch(t *testing.T, inst string) workerident.Intent {
	t.Helper()
	ctx := context.Background()
	in, err := c.ids.BeginLaunch(ctx, testRegion, workerident.LaunchRequest{
		InstanceType: "t3.small", ClaimID: "claim-0001", NodeID: "node-a", DataVolumeEncrypted: true, SizeGiB: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() error{
		func() error { return c.ids.MarkVolumeAttempted(ctx, in) },
		func() error { return c.ids.RecordVolume(ctx, in, gwVolume) },
		func() error { return c.ids.MarkDispatched(ctx, in) },
		func() error { return c.ids.BindInstance(ctx, in, inst) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	return in
}

// guestFor points the fake guest's boot at the local gateway, signing with
// the instance's own credential through the real client.
func (c gwCluster) guestFor(t *testing.T, g *guest, accessKey, inst string) *gwsign.Signer {
	t.Helper()
	signer := c.gw.issue(accessKey, "secret-"+accessKey, inst)
	g.fw("data-serial", testSerial)
	g.fw("region", testRegion)
	g.fw("gateway-url", c.gw.srv.URL)
	g.cfg.Wait = 10 * time.Millisecond
	g.cfg.NewClaimer = func(region, url string) (workerkey.Claimer, error) {
		cl, err := sqsworkergw.New(url, region, c.gw.caPEM(), signer)
		return GatewayClaimer{Client: cl}, err
	}
	return signer
}

func (g *localGateway) sentBodies() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.bodies)
}
