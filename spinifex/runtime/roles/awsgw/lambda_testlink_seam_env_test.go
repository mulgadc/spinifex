//go:build lambdatestlink && lambdatestlinkseam

//test:in-package — builds the unexported test-link executor and seam fixtures
package awsgw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/testlink"
	"github.com/mulgadc/spinifex/spinifex/runtime/roles/awsgw/credverify"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

// seamAccount is the account the composed gateway's signed caller belongs to.
const seamAccount = "123456789012"

const seamLambdaTrust = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}`

type seamNoJWKS struct{}

func (seamNoJWKS) LookupClusterJWKS(context.Context, string, string) ([]byte, bool, error) {
	return nil, false, nil
}

// seamEnv is one embedded JetStream holding the real IAM, STS and execution
// owner state the composed seam runs against.
type seamEnv struct {
	nc       *nats.Conn
	iam      *iamdomain.IAMServiceImpl
	sts      *stsdomain.STSServiceImpl
	roleARN  string
	grants   *execution.Store
	sessions jetstream.KeyValue
}

func newSeamEnv(t *testing.T) *seamEnv {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	masterKey, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)
	iamSvc, err := iamdomain.NewIAMServiceImpl(t.Context(), nc, masterKey)
	require.NoError(t, err)
	stsSvc, err := stsdomain.NewSTSServiceImpl(t.Context(), nc, iamSvc, masterKey, seamNoJWKS{})
	require.NoError(t, err)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	accounts, err := js.KeyValue(t.Context(), iamdomain.KVBucketAccounts)
	require.NoError(t, err)
	acct, err := json.Marshal(iamdomain.Account{AccountID: seamAccount, AccountName: "seam", Status: iamdomain.AccountStatusActive})
	require.NoError(t, err)
	_, err = accounts.Put(t.Context(), seamAccount, acct)
	require.NoError(t, err)
	role, err := iamSvc.CreateRole(seamAccount, &iam.CreateRoleInput{
		RoleName: aws.String("fn-exec"), AssumeRolePolicyDocument: aws.String(seamLambdaTrust),
	})
	require.NoError(t, err)
	grants, err := execution.Open(t.Context(), testutil.NewJetStream(t, nc))
	require.NoError(t, err)
	sessions, err := js.KeyValue(t.Context(), stsdomain.KVBucketSessionCredentials)
	require.NoError(t, err)
	return &seamEnv{nc: nc, iam: iamSvc, sts: stsSvc, roleARN: aws.StringValue(role.Role.Arn), grants: grants, sessions: sessions}
}

// resolve authenticates cred through the real verifier and resolves its
// principal; the secret callback stands in for SigV4.
func (e *seamEnv) resolve(cred hostlink.Credential) (credential.Principal, *credential.Failure) {
	v := credverify.New(e.iam, e.sts)
	req := credential.Request{AccessKeyID: cred.AccessKeyID, SecurityToken: cred.SessionToken}
	auth, fail := v.Authenticate(req, func(got string) error {
		if got != cred.SecretAccessKey {
			return errors.New(awserrors.ErrorSignatureDoesNotMatch)
		}
		return nil
	})
	if fail != nil {
		return credential.Principal{}, fail
	}
	return v.Resolve(req, auth)
}

// sessionLabels is the Lambda part of a stored session record.
type sessionLabels struct {
	FunctionID    string `json:"lambda_function_id"`
	FunctionARN   string `json:"lambda_function_arn"`
	EnvironmentID string `json:"lambda_environment_id"`
}

// labels reads the stored record for accessKeyID; found is false once deleted.
// It reports errors rather than failing, so a guest goroutine can call it.
func (e *seamEnv) labels(accessKeyID string) (l sessionLabels, found bool, err error) {
	entry, err := e.sessions.Get(context.Background(), accessKeyID)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return sessionLabels{}, false, nil
	}
	if err != nil {
		return sessionLabels{}, false, err
	}
	return l, true, json.Unmarshal(entry.Value(), &l)
}

// sessionCount is how many readable session records the store holds. Only
// temporary access-key IDs name sessions; the bucket's version key does not.
func (e *seamEnv) sessionCount(t *testing.T) int {
	t.Helper()
	keys, err := e.sessions.Keys(context.Background())
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return 0
	}
	require.NoError(t, err)
	n := 0
	for _, k := range keys {
		if !strings.HasPrefix(k, "ASIA") {
			continue
		}
		_, found, err := e.labels(k)
		require.NoError(t, err)
		if found {
			n++
		}
	}
	return n
}

// seamDaemon serves the daemon's real attempt steps on a real abstract control
// socket, with a loopback guest in place of a VM.
type seamDaemon struct {
	client  *testlink.ControlClient
	entered chan testlink.Request
	mu      sync.Mutex
	reqs    [][]byte
	replies [][]byte
	done    chan testlink.Reply
}

func startSeamDaemon(t *testing.T, d *testlink.Daemon, guest guestFunc) *seamDaemon {
	t.Helper()
	b := make([]byte, 8)
	_, err := rand.Read(b)
	require.NoError(t, err)
	name := "spinifex-seam-" + hex.EncodeToString(b)
	ln, err := testlink.ListenControl(name)
	require.NoError(t, err)
	s := &seamDaemon{entered: make(chan testlink.Request, 4), done: make(chan testlink.Reply, 4)}
	srv := &testlink.ControlServer{GatewayUID: os.Getuid(), RequestLimit: 1 << 20, ReplyLimit: 1 << 20, IOTimeout: 5 * time.Second,
		Handle: func(ctx context.Context, req []byte, cred testlink.Credential) testlink.Reply {
			var parsed testlink.Request
			_ = json.Unmarshal(req, &parsed)
			s.mu.Lock()
			s.reqs = append(s.reqs, req)
			s.mu.Unlock()
			s.entered <- parsed
			rep := d.HandleWithGuest(ctx, req, cred, guest)
			body, _ := json.Marshal(rep)
			s.mu.Lock()
			s.replies = append(s.replies, body)
			s.mu.Unlock()
			s.done <- rep
			return rep
		}}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			require.NoError(t, err)
		case <-time.After(seamWait):
			t.Errorf("control server did not stop within %s", seamWait)
		}
	})
	s.client = &testlink.ControlClient{Name: name, DaemonUID: os.Getuid(), RequestLimit: 1 << 20, ReplyLimit: 1 << 20, IOTimeout: 5 * time.Second}
	return s
}

func (s *seamDaemon) recorded() (reqs, replies [][]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.reqs...), append([][]byte(nil), s.replies...)
}

// credTap passes the guest's link through, sending the first frame's
// credential to creds and, if hold is set, delaying that first read until it
// closes.
type credTap struct {
	hostlink.Link
	creds chan<- hostlink.Credential
	hold  <-chan struct{}
	first sync.Once
}

func (l *credTap) Next(ctx context.Context) (hostlink.Request, error) {
	first := false
	l.first.Do(func() { first = true })
	if first && l.hold != nil {
		select {
		case <-l.hold:
		case <-ctx.Done():
			return hostlink.Request{}, ctx.Err()
		}
	}
	r, err := l.Link.Next(ctx)
	if first && err == nil && r.Credential != nil && l.creds != nil {
		l.creds <- *r.Credential
	}
	return r, err
}

// seamWait bounds every wait in the composed seam tests.
const seamWait = 10 * time.Second

// recv receives one value from ch or fails the test after seamWait.
func recv[T any](t *testing.T, ch chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(seamWait):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// within receives one value from ch, giving up after seamWait; guest
// goroutines use it because they cannot fail the test.
func within[T any](ch chan T) (T, bool) {
	select {
	case v := <-ch:
		return v, true
	case <-time.After(seamWait):
		var zero T
		return zero, false
	}
}

// gate is a channel the test opens when ready; open is idempotent and is also
// registered at cleanup, so a failed test never leaves a guest waiting on it.
func gate(t *testing.T) (ch chan struct{}, open func()) {
	t.Helper()
	ch = make(chan struct{})
	var once sync.Once
	open = func() { once.Do(func() { close(ch) }) }
	t.Cleanup(open)
	return ch, open
}
