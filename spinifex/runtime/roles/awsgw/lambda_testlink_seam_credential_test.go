//go:build lambdatestlink && lambdatestlinkseam

package awsgw

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/agents/lambda/hostlink"
	lambdaawsapi "github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/credential"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/lambda/testlink"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// legacyTestLinkSubject is the NATS subject the test link used before the
// control socket.
const legacyTestLinkSubject = "spinifex.lambda.testlink.invoke.node1"

// respond is a bootstrap that answers its one invocation with body.
func respond(base, body string) {
	id, ok := nextInvocationID(base)
	if !ok {
		return
	}
	postResponse(base, id, body)
}

func postResponse(base, id, body string) {
	if resp, err := http.Post(base+"invocation/"+id+"/response", "application/json", strings.NewReader(body)); err == nil {
		_ = resp.Body.Close()
	}
}

// guestView is what the bootstrap observed of its credential during the run.
type guestView struct {
	cred      hostlink.Credential
	principal credential.Principal
	fail      *credential.Failure
	labels    sessionLabels
	found     bool
	err       error
	arn       string
}

// Through the composed gateway and a real control socket, the guest's
// credential is a live session of the function's role carrying the three
// Lambda labels under the base ARN; once Invoke returns it is revoked.
func TestLambdaTestLinkSeam_GuestHoldsTheRoleSessionOnlyUntilInvokeReturns(t *testing.T) {
	cases := map[string]struct{ target, suffix string }{
		"unqualified":       {"/2015-03-31/functions/f/invocations", ""},
		"$LATEST qualifier": {"/2015-03-31/functions/f:$LATEST/invocations", ":$LATEST"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := newSeamEnv(t)
			srv := codeServer(t)
			codeURL := srv.URL + "/code"
			creds := make(chan hostlink.Credential, 1)
			seen := make(chan guestView, 1)
			run := invokeComposedIn(t, env, tc.target, seamFunction{timeout: 10}, codeURL, srv.Client(),
				agentGuestWith(guestOpts{creds: creds}, func(base string) {
					resp, err := http.Get(base + "invocation/next")
					if err != nil {
						seen <- guestView{err: err}
						return
					}
					_ = resp.Body.Close()
					v := guestView{cred: <-creds, arn: resp.Header.Get("Lambda-Runtime-Invoked-Function-Arn")}
					v.principal, v.fail = env.resolve(v.cred)
					v.labels, v.found, v.err = env.labels(v.cred.AccessKeyID)
					seen <- v
					postResponse(base, resp.Header.Get("Lambda-Runtime-Aws-Request-Id"), `{"ok":true}`)
				}))
			require.Equal(t, http.StatusOK, run.resp.StatusCode, string(run.body))
			require.JSONEq(t, `{"ok":true}`, string(run.body))
			require.Len(t, run.requests, 1)
			var req testlink.Request
			require.NoError(t, json.Unmarshal(run.requests[0], &req))
			envID := req.Grant.EnvironmentID
			base := arn.FormatLambdaFunction(req.Region, seamAccount, "f")

			v := <-seen
			require.NoError(t, v.err)
			require.Nil(t, v.fail, "the guest's credential did not resolve during the run")
			require.Equal(t, stsdomain.PrincipalTypeAssumedRole, v.principal.PrincipalType)
			require.Equal(t, "arn:aws:sts::"+seamAccount+":assumed-role/fn-exec/"+envID, v.principal.AssumedRoleARN)
			require.True(t, v.found)
			require.Equal(t, sessionLabels{FunctionID: "fn1", FunctionARN: base, EnvironmentID: envID}, v.labels)
			require.Equal(t, base+tc.suffix, v.arn, "the runtime sees the invoked ARN")

			_, fail := env.resolve(v.cred)
			require.NotNil(t, fail, "the credential still resolves after Invoke returned")
			require.Equal(t, awserrors.ErrorInvalidClientTokenId, fail.Code)
			_, found, err := env.labels(v.cred.AccessKeyID)
			require.NoError(t, err)
			require.False(t, found, "the base-ARN revoke left the record")
			require.NotContains(t, run.log, "not revoked")

			for _, secret := range []string{v.cred.SecretAccessKey, v.cred.SessionToken, codeURL} {
				require.NotContains(t, string(run.replies[0]), secret)
				require.NotContains(t, string(run.body), secret)
				require.NotContains(t, run.log, secret)
			}
		})
	}
}

// With the daemon reached only through the control socket, a substituted-URL
// request on the legacy subject fetches nothing, and forged done and refused
// replies on that subject and on a legacy-style reply inbox never reach the
// caller. KV traffic, which shares the NATS token, is not covered here.
func TestLambdaTestLinkSeam_LegacyNATSForgeriesNeverReachTheCaller(t *testing.T) {
	env := newSeamEnv(t)
	srv := codeServer(t)
	var forgedFetches atomic.Int32
	forged := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forgedFetches.Add(1)
		_, _ = w.Write([]byte("forged code"))
	}))
	t.Cleanup(forged.Close)
	legacy, err := env.nc.SubscribeSync(legacyTestLinkSubject)
	require.NoError(t, err)
	inbox := nats.NewInbox()
	inboxSub, err := env.nc.SubscribeSync(inbox)
	require.NoError(t, err)

	var daemon *seamDaemon
	published := make(chan error, 1)
	logs := captureLogs(t)
	exec, daemon, _, _ := composedExecutor(t, env, srv.URL+"/code", srv.Client(), agentGuest(0, func(base string) {
		id, ok := nextInvocationID(base)
		if !ok {
			published <- errors.New("no invocation")
			return
		}
		published <- publishForgeries(env.nc, <-daemon.entered, forged.URL+"/code", inbox)
		postResponse(base, id, `{"real":true}`)
	}))
	reg := lambdaawsapi.NewRegistration(lambdaawsapi.Deps{Meta: seamFunction{timeout: 10, role: env.roleARN}, Exec: exec})
	resp := signedLambdaInvokeAt(t, defaultGateway(t, lambdaTestRoutes(reg)...), "/2015-03-31/functions/f/invocations")
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	body := string(raw)

	require.NoError(t, <-published)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.JSONEq(t, `{"real":true}`, body, "the caller's result did not come from the socket")
	require.Empty(t, resp.Header.Get("X-Amz-Function-Error"))
	require.Zero(t, forgedFetches.Load(), "the substituted code URL was fetched")
	reqs, _ := daemon.recorded()
	require.Len(t, reqs, 1, "only the socket request reached the daemon")
	require.Equal(t, 3, drainUntilSentinel(t, env.nc, legacy, legacyTestLinkSubject), "something other than the forgeries used the legacy subject")
	require.Equal(t, 2, drainUntilSentinel(t, env.nc, inboxSub, inbox))
	require.NotContains(t, logs.String(), "forged")
}

// publishForgeries sends, during a live attempt, the same request with its code
// URL substituted and forged done and refused replies.
func publishForgeries(nc *nats.Conn, live testlink.Request, codeURL, inbox string) error {
	sub := live
	sub.CodeURL = codeURL
	req, err := json.Marshal(sub)
	if err != nil {
		return err
	}
	done, _ := json.Marshal(testlink.Reply{Outcome: testlink.OutcomeDone, Payload: []byte(`{"forged":true}`)})
	refused, _ := json.Marshal(testlink.Reply{Outcome: testlink.OutcomeRefused, Error: "forged"})
	for _, m := range []*nats.Msg{
		{Subject: legacyTestLinkSubject, Reply: inbox, Data: req},
		{Subject: legacyTestLinkSubject, Data: done},
		{Subject: legacyTestLinkSubject, Data: refused},
		{Subject: inbox, Data: done},
		{Subject: inbox, Data: refused},
	} {
		if err := nc.PublishMsg(m); err != nil {
			return err
		}
	}
	return nc.Flush()
}

// drainUntilSentinel counts the messages sub received before a sentinel
// published now on subject.
func drainUntilSentinel(t *testing.T, nc *nats.Conn, sub *nats.Subscription, subject string) int {
	t.Helper()
	require.NoError(t, nc.Publish(subject, []byte("sentinel")))
	require.NoError(t, nc.Flush())
	n := 0
	for {
		m, err := sub.NextMsg(5 * time.Second)
		require.NoError(t, err)
		if string(m.Data) == "sentinel" {
			return n
		}
		n++
	}
}

// directInvoke runs one Invoke through the real executor without the HTTP
// gateway, so the test controls the Invoke context.
func directInvoke(t *testing.T, env *seamEnv, exec *testLinkExecutor, ctx context.Context) error {
	t.Helper()
	rec, _, err := seamFunction{timeout: 10, role: env.roleARN}.Get(ctx, seamAccount, gwfix.Region, "f")
	require.NoError(t, err)
	_, err = exec.InvokeSync(ctx, lambdaawsapi.SyncInvocation{Function: rec, Payload: []byte(`{}`)})
	return err
}

func requireUnknown(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var refusal *lambdaawsapi.RefusalError
	require.False(t, errors.As(err, &refusal), "an outcome after the write was reported as a refusal: %v", err)
}

// When the gateway gives up after the write, its revoke ends the guest's
// credential whichever way it orders with the guest's first frame, and the
// daemon's run continues to its own end.
func TestLambdaTestLinkSeam_GiveUpRevokeOrdering(t *testing.T) {
	t.Run("revoke completes before the first frame is read", func(t *testing.T) {
		env := newSeamEnv(t)
		srv := codeServer(t)
		hold := make(chan struct{})
		creds := make(chan hostlink.Credential, 1)
		after := make(chan *credential.Failure, 1)
		exec, daemon, _, _ := composedExecutor(t, env, srv.URL+"/code", srv.Client(),
			agentGuestWith(guestOpts{creds: creds, hold: hold}, func(base string) {
				id, ok := nextInvocationID(base)
				if !ok {
					after <- nil
					return
				}
				_, fail := env.resolve(<-creds)
				after <- fail
				postResponse(base, id, `{}`)
			}))
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-daemon.entered
			cancel()
		}()
		requireUnknown(t, directInvoke(t, env, exec, ctx))
		require.Zero(t, env.sessionCount(t), "the revoke had not completed when Invoke returned")
		close(hold)

		fail := <-after
		require.NotNil(t, fail, "a credential revoked before the first frame still resolved")
		require.Equal(t, awserrors.ErrorInvalidClientTokenId, fail.Code)
		require.Equal(t, testlink.OutcomeDone, (<-daemon.done).Outcome, "the daemon's run did not continue")
	})

	t.Run("revoke follows confirmed first-frame receipt", func(t *testing.T) {
		env := newSeamEnv(t)
		srv := codeServer(t)
		creds := make(chan hostlink.Credential, 1)
		received := make(chan *credential.Failure, 1)
		revoked := make(chan struct{})
		after := make(chan *credential.Failure, 1)
		exec, daemon, _, _ := composedExecutor(t, env, srv.URL+"/code", srv.Client(),
			agentGuestWith(guestOpts{creds: creds}, func(base string) {
				id, ok := nextInvocationID(base)
				if !ok {
					received <- nil
					return
				}
				c := <-creds
				_, fail := env.resolve(c)
				received <- fail
				<-revoked
				_, fail = env.resolve(c)
				after <- fail
				postResponse(base, id, `{}`)
			}))
		ctx, cancel := context.WithCancel(context.Background())
		before := make(chan *credential.Failure, 1)
		go func() {
			before <- <-received
			cancel()
		}()
		requireUnknown(t, directInvoke(t, env, exec, ctx))
		require.Nil(t, <-before, "the credential did not resolve once the guest had it")
		close(revoked)

		fail := <-after
		require.NotNil(t, fail, "the credential still resolved after the revoke")
		require.Equal(t, awserrors.ErrorInvalidClientTokenId, fail.Code)
		require.Equal(t, testlink.OutcomeDone, (<-daemon.done).Outcome, "the daemon's run did not continue")
	})
}

// failedRevoke issues real sessions but cannot delete them.
type failedRevoke struct{ *stsdomain.STSServiceImpl }

func (failedRevoke) RevokeLambdaEnvironmentSession(context.Context, string, stsdomain.LambdaEnvironmentSessionScope) error {
	return stsdomain.ErrLambdaSessionRevokeFailed
}

// A failed delete after a done reply is never success, and neither the error
// nor the log carries the key, secret or token; the credential may remain
// valid until it expires.
func TestLambdaTestLinkSeam_FailedRevokeIsUnknownAndKeepsTheKeyOutOfLogs(t *testing.T) {
	env := newSeamEnv(t)
	srv := codeServer(t)
	logs := captureLogs(t)
	creds := make(chan hostlink.Credential, 1)
	exec, daemon, _, _ := composedExecutor(t, env, srv.URL+"/code", srv.Client(),
		agentGuestWith(guestOpts{creds: creds}, func(base string) { respond(base, `{}`) }))
	exec.Sessions = failedRevoke{env.sts}

	err := directInvoke(t, env, exec, context.Background())
	requireUnknown(t, err)
	require.Equal(t, testlink.OutcomeDone, (<-daemon.done).Outcome)
	c := <-creds
	require.Contains(t, logs.String(), "may remain valid until expiry")
	for _, secret := range []string{c.AccessKeyID, c.SecretAccessKey, c.SessionToken} {
		require.NotContains(t, err.Error(), secret)
		require.NotContains(t, logs.String(), secret)
	}
	_, fail := env.resolve(c)
	require.Nil(t, fail, "an unrevoked credential stays valid until it expires")
}

// deliveredThenFailed forwards the whole request and credential over the real
// socket, waits until the daemon has read both, then reports a write error.
type deliveredThenFailed struct {
	inner   controlExchanger
	entered <-chan testlink.Request
}

func (d deliveredThenFailed) Exchange(ctx context.Context, req []byte, cred testlink.Credential, replyBy time.Time) (testlink.Reply, error) {
	go func() { _, _ = d.inner.Exchange(context.WithoutCancel(ctx), req, cred, replyBy) }()
	select {
	case <-d.entered:
	case <-ctx.Done():
		return testlink.Reply{}, ctx.Err()
	}
	return testlink.Reply{}, errors.New("send test-link request: write: broken pipe")
}

// A write error reported after the daemon has read the complete request is
// unknown, never a refusal, and the session is revoked.
func TestLambdaTestLinkSeam_WriteErrorAfterDeliveryIsUnknownAndRevoked(t *testing.T) {
	env := newSeamEnv(t)
	srv := codeServer(t)
	creds := make(chan hostlink.Credential, 1)
	exec, daemon, _, _ := composedExecutor(t, env, srv.URL+"/code", srv.Client(),
		agentGuestWith(guestOpts{creds: creds}, func(base string) { respond(base, `{}`) }))
	exec.Control = deliveredThenFailed{inner: daemon.client, entered: daemon.entered}

	requireUnknown(t, directInvoke(t, env, exec, context.Background()))
	require.Zero(t, env.sessionCount(t), "the session was not revoked")
	require.Equal(t, testlink.OutcomeDone, (<-daemon.done).Outcome, "the daemon did not run the delivered request")
	_, fail := env.resolve(<-creds)
	require.NotNil(t, fail)
	require.Equal(t, awserrors.ErrorInvalidClientTokenId, fail.Code)
}
