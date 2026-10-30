package workerboot

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
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
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/firststore"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerrole"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

const (
	gwInstance  = "i-0abc12345def67890"
	gwSuccessor = "i-0fedcba9876543210"
	gwVolume    = "vol-0a1b2c3d4e5f60718"
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
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<ErrorResponse><Error><Code>%s</Code></Error></ErrorResponse>", code)
}

func (g *localGateway) sentBodies() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.bodies)
}

// gwCluster is the domain's real worker-identity and first-store records.
type gwCluster struct {
	ids   workerident.Store
	fence firststore.Store
	gw    *localGateway
}

func newGWCluster(t *testing.T) gwCluster {
	t.Helper()
	clustersize.DeclareForTest(t, 1)
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	ids, err := workerident.Open(ctx, js)
	if err != nil {
		t.Fatal(err)
	}
	fence, err := firststore.Open(ctx, js)
	if err != nil {
		t.Fatal(err)
	}
	return gwCluster{ids: ids, fence: fence, gw: newLocalGateway(t, sqsworkerapi.JetStreamDeps(js))}
}

func (c gwCluster) launch(t *testing.T, claim string, supersedes uint64, inst string) workerident.Intent {
	t.Helper()
	ctx := context.Background()
	in, err := c.ids.BeginLaunch(ctx, testRegion, workerident.LaunchRequest{
		ClaimID: claim, Supersedes: supersedes, NodeID: "node-a", DataVolumeID: gwVolume, DataVolumeEncrypted: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ids.BindInstance(ctx, in, inst); err != nil {
		t.Fatal(err)
	}
	return in
}

// guestFor points the fake guest's boot at the local gateway, signing with
// the instance's own credential through the real client.
func (c gwCluster) guestFor(t *testing.T, g *guest, accessKey, inst string) {
	t.Helper()
	signer := c.gw.issue(accessKey, "secret-"+accessKey, inst)
	g.fw("data-serial", testSerial)
	g.fw("region", testRegion)
	g.fw("gateway-url", c.gw.srv.URL)
	g.cfg.Wait = 10 * time.Millisecond
	g.cfg.NewClaimer = func(region, url string) (workerkey.Claimer, error) {
		return sqsworkergw.New(url, region, c.gw.caPEM(), signer)
	}
}

func (c gwCluster) assertNoSeedSent(t *testing.T, seed string) {
	t.Helper()
	seed = strings.TrimSpace(seed)
	for _, b := range c.gw.sentBodies() {
		if strings.Contains(b, seed) {
			t.Fatal("a request carried seed material")
		}
	}
}

// A bound worker boots: the gateway authorizes creation, the guest creates
// its key on the mounted disk and records only the public key; reboots get
// that same key back from the domain.
func TestGateway_FirstBootCreatesKeyAndRebootsReuseIt(t *testing.T) {
	c := newGWCluster(t)
	c.launch(t, "claim-0001", 0, gwInstance)
	g := newGuest(t)
	c.guestFor(t, g, "ASIAWORKER1", gwInstance)

	pub, err := g.boot()
	if err != nil {
		t.Fatal(err)
	}
	seed := g.seed()
	for boot := 2; boot <= 3; boot++ {
		g.resetMounts()
		got, err := g.boot()
		if err != nil || got != pub || g.seed() != seed {
			t.Fatalf("reboot %d = %q, %v", boot, got, err)
		}
	}
	records := 0
	for _, b := range c.gw.sentBodies() {
		if strings.Contains(b, "Action=RecordWorkerPublicKey") {
			records++
			if !strings.Contains(b, "PublicKey="+pub) {
				t.Fatal("a record carried a different key")
			}
		}
	}
	if records != 1 {
		t.Fatalf("records = %d, want 1", records)
	}
	c.assertNoSeedSent(t, seed)
	g.assertNoSeedLeak(nil, seed)
}

// The record's reply is lost after it landed: the boot retries, the domain
// answers with the recorded key, and the same key is used.
func TestGateway_LostRecordReplyKeepsTheKey(t *testing.T) {
	c := newGWCluster(t)
	c.launch(t, "claim-0001", 0, gwInstance)
	g := newGuest(t)
	c.guestFor(t, g, "ASIAWORKER1", gwInstance)
	var dropped bool
	c.gw.dropReply = func(action string) bool {
		if action == "RecordWorkerPublicKey" && !dropped {
			dropped = true
			return true
		}
		return false
	}
	pub, err := g.boot()
	if err != nil || !dropped {
		t.Fatalf("boot = %q, %v (dropped=%v)", pub, err, dropped)
	}
	g.resetMounts()
	if again, err := g.boot(); err != nil || again != pub {
		t.Fatalf("reboot = %q, %v", again, err)
	}
	c.assertNoSeedSent(t, g.seed())
}

// After the key is recorded, a lost guest copy fails closed: the domain
// answers with the recorded key and no replacement is created or sent.
func TestGateway_KeyLostAfterRecordingFailsClosed(t *testing.T) {
	c := newGWCluster(t)
	c.launch(t, "claim-0001", 0, gwInstance)
	g := newGuest(t)
	c.guestFor(t, g, "ASIAWORKER1", gwInstance)
	if _, err := g.boot(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(g.keyFile()); err != nil {
		t.Fatal(err)
	}
	before := len(c.gw.sentBodies())
	g.resetMounts()
	if _, err := g.boot(); !errors.Is(err, workerkey.ErrKeyMissing) {
		t.Fatalf("err = %v, want ErrKeyMissing", err)
	}
	if _, err := os.Lstat(g.keyFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a replacement key was created")
	}
	for _, b := range c.gw.sentBodies()[before:] {
		if strings.Contains(b, "RecordWorkerPublicKey") {
			t.Fatal("a replacement key was offered")
		}
	}
}

// An instance the domain never bound, or one already replaced, is refused
// at the first call and creates no key; AccessDenied is not retried.
func TestGateway_UnboundOrReplacedInstanceIsRefused(t *testing.T) {
	t.Run("unbound", func(t *testing.T) {
		c := newGWCluster(t)
		c.launch(t, "claim-0001", 0, gwInstance)
		g := newGuest(t)
		c.guestFor(t, g, "ASIAOTHER", gwSuccessor)
		_, err := g.boot()
		api, ok := errors.AsType[*sqsworkergw.APIError](err)
		if !ok || api.Code != "AccessDenied" {
			t.Fatalf("err = %v, want AccessDenied", err)
		}
		if n := len(c.gw.sentBodies()); n != 1 {
			t.Fatalf("calls = %d; a denial must not be retried", n)
		}
		g.assertNoKey()
	})
	t.Run("replaced", func(t *testing.T) {
		c := newGWCluster(t)
		first := c.launch(t, "claim-0001", 0, gwInstance)
		ctx := context.Background()
		if err := c.ids.RecordStopped(ctx, first, gwInstance); err != nil {
			t.Fatal(err)
		}
		if err := c.fence.Fence(ctx, testRegion, first.Generation+1); err != nil {
			t.Fatal(err)
		}
		c.launch(t, "claim-0002", first.Generation, gwSuccessor)
		g := newGuest(t)
		c.guestFor(t, g, "ASIAOLD", gwInstance)
		if _, err := g.boot(); err == nil {
			t.Fatal("the replaced instance booted")
		}
		g.assertNoKey()
	})
}

// A bad signature is refused before any decision, and creates no key.
func TestGateway_BadSignatureCreatesNoKey(t *testing.T) {
	c := newGWCluster(t)
	c.launch(t, "claim-0001", 0, gwInstance)
	g := newGuest(t)
	c.guestFor(t, g, "ASIAWORKER1", gwInstance)
	forged := gwsign.NewStatic("ASIAWORKER1", "not-the-secret")
	g.cfg.NewClaimer = func(region, url string) (workerkey.Claimer, error) {
		return sqsworkergw.New(url, region, c.gw.caPEM(), forged)
	}
	if _, err := g.boot(); err == nil {
		t.Fatal("a forged signature booted")
	}
	g.assertNoKey()
}

// The client trusts only the cluster CA it was given: a gateway whose
// certificate another CA signed is never sent a request.
func TestGateway_UntrustedServerIsNotContacted(t *testing.T) {
	c := newGWCluster(t)
	c.launch(t, "claim-0001", 0, gwInstance)
	g := newGuest(t)
	c.guestFor(t, g, "ASIAWORKER1", gwInstance)
	g.cfg.Attempts = 1
	signer := c.gw.issue("ASIAWORKER1", "secret-ASIAWORKER1", gwInstance)
	g.cfg.NewClaimer = func(region, url string) (workerkey.Claimer, error) {
		return sqsworkergw.New(url, region, otherCA(t), signer)
	}
	if _, err := g.boot(); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("err = %v, want a TLS refusal", err)
	}
	if len(c.gw.sentBodies()) != 0 {
		t.Fatal("a request reached a server outside the trusted CA")
	}
	g.assertNoKey()
}

// otherCA is a fresh self-signed CA unrelated to the test server's.
func otherCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "other"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
