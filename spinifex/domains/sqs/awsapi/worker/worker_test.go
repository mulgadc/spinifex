package worker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/firststore"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
)

const (
	region    = "ap-southeast-2"
	instance  = "i-0abc12345def67890"
	successor = "i-0fed98765cba43210"
	volume    = "vol-0123456789abcdef0"
)

func workerActor(session string) dispatch.Actor {
	return dispatch.Actor{
		AccountID:     awsidentifiers.GlobalAccountID,
		PrincipalType: principalTypeAssumedRole,
		PrincipalARN:  "arn:aws:iam::" + awsidentifiers.GlobalAccountID + ":role/" + WorkerRoleName,
		SessionName:   session,
	}
}

type call struct {
	policyChecks int
	policyErr    error
}

func invoke(deps Deps, body string, actor dispatch.Actor, c *call) (*httptest.ResponseRecorder, error) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	err := deps.dispatch(rec, dispatch.Invocation{
		Request:   req,
		AccountID: actor.AccountID,
		Service:   ServiceName,
		Region:    region,
		Authorize: func(service, action string, _ []string, _ iampolicy.ConditionKeys) error {
			c.policyChecks++
			if service != ServiceName {
				return errors.New(awserrors.ErrorAccessDenied)
			}
			return c.policyErr
		},
		Actor: func() (dispatch.Actor, error) { return actor, nil },
	})
	return rec, err
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil || err.Error() != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}

type fakeIdentities struct {
	ident workerident.Identity
	err   error
	asked []string
}

func (f *fakeIdentities) Authorize(_ context.Context, instanceID string) (workerident.Identity, error) {
	f.asked = append(f.asked, instanceID)
	return f.ident, f.err
}

type fakeFirstStore struct {
	decision firststore.Decision
	err      error
	regions  []string
	ids      []firststore.Identity
	keys     []string
}

func (f *fakeFirstStore) Claim(_ context.Context, region string, id firststore.Identity) (firststore.Decision, error) {
	f.regions, f.ids = append(f.regions, region), append(f.ids, id)
	return f.decision, f.err
}

func (f *fakeFirstStore) RecordPublicKey(_ context.Context, region string, id firststore.Identity, pub string) error {
	f.regions, f.ids, f.keys = append(f.regions, region), append(f.ids, id), append(f.keys, pub)
	return f.err
}

func current() workerident.Identity {
	return workerident.Identity{Region: region, InstanceID: instance, Generation: 3, ClaimID: "claim-0003", NodeID: "node-a", DataVolumeID: volume}
}

func fakes() (*fakeIdentities, *fakeFirstStore, Deps) {
	ids, fs := &fakeIdentities{ident: current()}, &fakeFirstStore{decision: firststore.Decision{CreateKey: true}}
	return ids, fs, Deps{Identities: ids, FirstStore: fs}
}

// The first-store identity is the authoritative record's, keyed by the session
// STS set, never anything the request carried.
func TestClaim_UsesTheCurrentRecordIdentity(t *testing.T) {
	ids, fs, deps := fakes()
	rec, err := invoke(deps, "Action=ClaimFirstStore", workerActor(instance), &call{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids.asked) != 1 || ids.asked[0] != instance {
		t.Fatalf("authorized %v, want the session's instance", ids.asked)
	}
	want := firststore.Identity{InstanceID: instance, ClaimID: "claim-0003", Generation: 3}
	if len(fs.ids) != 1 || fs.ids[0] != want || fs.regions[0] != region {
		t.Fatalf("claimed %v in %v, want %v in %s", fs.ids, fs.regions, want, region)
	}
	if !strings.Contains(rec.Body.String(), "<CreateKey>true</CreateKey>") {
		t.Fatalf("response %q does not authorize key creation", rec.Body.String())
	}
}

func TestClaim_ReturnsTheRecordedKey(t *testing.T) {
	_, fs, deps := fakes()
	pub := newUserKey(t).pub
	fs.decision = firststore.Decision{PublicKey: pub}
	rec, err := invoke(deps, "Action=ClaimFirstStore", workerActor(instance), &call{})
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<CreateKey>false</CreateKey>") || !strings.Contains(body, "<PublicKey>"+pub+"</PublicKey>") {
		t.Fatalf("response %q does not carry exactly the recorded key", body)
	}
}

// Identity fields in the request are refused outright rather than ignored, and
// before any store is consulted.
func TestRequestSuppliedIdentityIsRefused(t *testing.T) {
	for _, body := range []string{
		"Action=ClaimFirstStore&InstanceId=" + successor,
		"Action=ClaimFirstStore&Generation=99",
		"Action=ClaimFirstStore&ClaimId=claim-9999",
		"Action=ClaimFirstStore&Region=us-east-1",
		"Action=RecordWorkerPublicKey&PublicKey=" + newUserKey(t).pub + "&Generation=99",
	} {
		ids, fs, deps := fakes()
		_, err := invoke(deps, body, workerActor(instance), &call{})
		assertCode(t, err, awserrors.ErrorInvalidParameterValue)
		if len(ids.asked)+len(fs.ids) != 0 {
			t.Fatalf("%s: consulted the stores", body)
		}
	}
}

// Nothing but the worker role's own session in the system account passes, and
// a refused caller never reaches the policy evaluator or the stores.
func TestPrincipalGate(t *testing.T) {
	const customer = "123456789012"
	roleIn := func(acct, role string) string { return "arn:aws:iam::" + acct + ":role/" + role }
	cases := map[string]dispatch.Actor{
		"customer account role": {AccountID: customer, PrincipalType: principalTypeAssumedRole,
			PrincipalARN: roleIn(customer, WorkerRoleName), SessionName: instance},
		"system account, another role": {AccountID: awsidentifiers.GlobalAccountID, PrincipalType: principalTypeAssumedRole,
			PrincipalARN: roleIn(awsidentifiers.GlobalAccountID, "rdsInstanceRole"), SessionName: instance},
		"role in another account": {AccountID: awsidentifiers.GlobalAccountID, PrincipalType: principalTypeAssumedRole,
			PrincipalARN: roleIn(customer, WorkerRoleName), SessionName: instance},
		"system account user": {AccountID: awsidentifiers.GlobalAccountID, PrincipalType: "user",
			PrincipalARN: "arn:aws:iam::" + awsidentifiers.GlobalAccountID + ":user/" + WorkerRoleName, UserName: WorkerRoleName},
		"empty session":    {AccountID: awsidentifiers.GlobalAccountID, PrincipalType: principalTypeAssumedRole, PrincipalARN: roleIn(awsidentifiers.GlobalAccountID, WorkerRoleName)},
		"anonymous caller": {},
	}
	for name, actor := range cases {
		t.Run(name, func(t *testing.T) {
			ids, fs, deps := fakes()
			c := &call{}
			_, err := invoke(deps, "Action=ClaimFirstStore", actor, c)
			assertCode(t, err, awserrors.ErrorAccessDenied)
			if c.policyChecks != 0 || len(ids.asked)+len(fs.ids) != 0 {
				t.Fatal("a refused caller reached the policy check or the stores")
			}
		})
	}
	t.Run("invocation account differs from the actor", func(t *testing.T) {
		ids, fs, deps := fakes()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=ClaimFirstStore"))
		err := deps.dispatch(httptest.NewRecorder(), dispatch.Invocation{
			Request: req, AccountID: customer, Region: region,
			Authorize: func(string, string, []string, iampolicy.ConditionKeys) error { return nil },
			Actor:     func() (dispatch.Actor, error) { return workerActor(instance), nil },
		})
		assertCode(t, err, awserrors.ErrorAccessDenied)
		if len(ids.asked)+len(fs.ids) != 0 {
			t.Fatal("consulted the stores")
		}
	})
}

func TestPolicyDenialStops(t *testing.T) {
	ids, fs, deps := fakes()
	c := &call{policyErr: errors.New(awserrors.ErrorAccessDenied)}
	_, err := invoke(deps, "Action=ClaimFirstStore", workerActor(instance), c)
	assertCode(t, err, awserrors.ErrorAccessDenied)
	if c.policyChecks != 1 || len(ids.asked)+len(fs.ids) != 0 {
		t.Fatal("the role policy was not the gate before the stores")
	}
}

// An unknown, superseded or foreign-Region worker never reaches the
// first-store decision.
func TestNonCurrentWorkerIsRefused(t *testing.T) {
	for name, set := range map[string]func(*fakeIdentities){
		"unknown instance":    func(f *fakeIdentities) { f.err = workerident.ErrUnknownInstance },
		"superseded instance": func(f *fakeIdentities) { f.err = workerident.ErrNotCurrent },
		"another Region":      func(f *fakeIdentities) { f.ident.Region = "us-east-1" },
	} {
		t.Run(name, func(t *testing.T) {
			ids, fs, deps := fakes()
			set(ids)
			_, err := invoke(deps, "Action=ClaimFirstStore", workerActor(instance), &call{})
			assertCode(t, err, awserrors.ErrorAccessDenied)
			if len(fs.ids) != 0 {
				t.Fatal("a non-current worker reached the first-store decision")
			}
		})
	}
}

type userKey struct{ pub, seed string }

func newUserKey(t *testing.T) userKey {
	t.Helper()
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := kp.PublicKey()
	seed, _ := kp.Seed()
	return userKey{pub: pub, seed: string(seed)}
}

// corrupt always changes the last character, which the key's checksum covers.
func corrupt(pub string) string {
	last := "A"
	if strings.HasSuffix(pub, "A") {
		last = "B"
	}
	return pub[:len(pub)-1] + last
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// A seed, or anything else that is not a public user key, is refused without
// being recorded, logged or echoed.
func TestRecordPublicKey_RefusesSeedsAndNonKeys(t *testing.T) {
	k := newUserKey(t)
	acct, _ := nkeys.CreateAccount()
	acctPub, _ := acct.PublicKey()
	for name, value := range map[string]string{
		"user seed":          k.seed,
		"account public key": acctPub,
		"corrupt key":        corrupt(k.pub),
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			_, fs, deps := fakes()
			rec, err := invoke(deps, "Action=RecordWorkerPublicKey&PublicKey="+value, workerActor(instance), &call{})
			assertCode(t, err, awserrors.ErrorInvalidParameterValue)
			if len(fs.keys) != 0 {
				t.Fatal("recorded a value that is not a public user key")
			}
			if strings.Contains(rec.Body.String()+err.Error()+logs.String(), value) {
				t.Fatal("the refused value was echoed or logged")
			}
		})
	}
	t.Run("missing key", func(t *testing.T) {
		_, fs, deps := fakes()
		_, err := invoke(deps, "Action=RecordWorkerPublicKey", workerActor(instance), &call{})
		assertCode(t, err, awserrors.ErrorMissingParameter)
		if len(fs.keys) != 0 {
			t.Fatal("recorded an empty key")
		}
	})
	t.Run("public key is recorded under the current identity", func(t *testing.T) {
		_, fs, deps := fakes()
		if _, err := invoke(deps, "Action=RecordWorkerPublicKey&PublicKey="+k.pub, workerActor(instance), &call{}); err != nil {
			t.Fatal(err)
		}
		want := firststore.Identity{InstanceID: instance, ClaimID: "claim-0003", Generation: 3}
		if len(fs.keys) != 1 || fs.keys[0] != k.pub || fs.ids[0] != want {
			t.Fatalf("recorded %v for %v", fs.keys, fs.ids)
		}
	})
}

func TestStoreErrorsMapToWorkerCodes(t *testing.T) {
	for err, code := range map[error]string{
		firststore.ErrClaimHeld:          awserrors.ErrorAccessDenied,
		firststore.ErrStaleGeneration:    awserrors.ErrorAccessDenied,
		firststore.ErrKeyMismatch:        awserrors.ErrorInvalidParameterValue,
		firststore.ErrNotClaimed:         awserrors.ErrorInvalidState,
		firststore.ErrContended:          awserrors.ErrorServiceUnavailable,
		workerident.ErrContended:         awserrors.ErrorServiceUnavailable,
		firststore.ErrUnreadable:         awserrors.ErrorServerInternal,
		errors.New("leader unreachable"): awserrors.ErrorServerInternal,
	} {
		_, fs, deps := fakes()
		fs.err = err
		_, got := invoke(deps, "Action=ClaimFirstStore", workerActor(instance), &call{})
		assertCode(t, got, code)
	}
}

// Without its stores the surface refuses every worker, after the gate has
// refused everyone else.
func TestMissingDependenciesFailClosed(t *testing.T) {
	for name, deps := range map[string]Deps{
		"no deps":        {},
		"no first store": {Identities: &fakeIdentities{ident: current()}},
		"no identities":  {FirstStore: &fakeFirstStore{decision: firststore.Decision{CreateKey: true}}},
		"nil JetStream":  JetStreamDeps(nil),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := invoke(deps, "Action=ClaimFirstStore", workerActor(instance), &call{})
			assertCode(t, err, awserrors.ErrorServiceUnavailable)
			_, err = invoke(deps, "Action=ClaimFirstStore", dispatch.Actor{}, &call{})
			assertCode(t, err, awserrors.ErrorAccessDenied)
		})
	}
}

func TestUnknownAndMissingActions(t *testing.T) {
	_, _, deps := fakes()
	_, err := invoke(deps, "Action=SendMessage", workerActor(instance), &call{})
	assertCode(t, err, awserrors.ErrorInvalidAction)
	_, err = invoke(deps, "", workerActor(instance), &call{})
	assertCode(t, err, awserrors.ErrorMissingAction)
}

// --- Against the real stores on embedded JetStream. ---

type cluster struct {
	js   jetstream.JetStream
	ids  workerident.Store
	deps Deps
}

func newCluster(t *testing.T) cluster {
	t.Helper()
	clustersize.DeclareForTest(t, 1)
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	ids, err := workerident.Open(ctx, js)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := firststore.Open(ctx, js); err != nil {
		t.Fatal(err)
	}
	return cluster{js: js, ids: ids, deps: JetStreamDeps(js)}
}

func (c cluster) launch(t *testing.T, claim string, supersedes uint64, inst string) workerident.Intent {
	t.Helper()
	ctx := context.Background()
	in, err := c.ids.BeginLaunch(ctx, region, workerident.LaunchRequest{
		ClaimID: claim, Supersedes: supersedes, NodeID: "node-a", DataVolumeID: volume, DataVolumeEncrypted: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ids.BindInstance(ctx, in, inst); err != nil {
		t.Fatal(err)
	}
	return in
}

// The full path: a bound worker claims, records its key, and after a confirmed
// replacement the old instance is refused while the new one gets only the key.
func TestRealStores_ClaimRecordAndReplace(t *testing.T) {
	c := newCluster(t)
	k := newUserKey(t)
	first := c.launch(t, "claim-0001", 0, instance)

	rec, err := invoke(c.deps, "Action=ClaimFirstStore", workerActor(instance), &call{})
	if err != nil || !strings.Contains(rec.Body.String(), "<CreateKey>true</CreateKey>") {
		t.Fatalf("first claim: %v %q", err, rec.Body.String())
	}
	if _, err := invoke(c.deps, "Action=RecordWorkerPublicKey&PublicKey="+k.pub, workerActor(instance), &call{}); err != nil {
		t.Fatal(err)
	}
	// A lost reply is retried with the same key.
	if _, err := invoke(c.deps, "Action=RecordWorkerPublicKey&PublicKey="+k.pub, workerActor(instance), &call{}); err != nil {
		t.Fatalf("retried record: %v", err)
	}
	other := newUserKey(t)
	_, err = invoke(c.deps, "Action=RecordWorkerPublicKey&PublicKey="+other.pub, workerActor(instance), &call{})
	assertCode(t, err, awserrors.ErrorInvalidParameterValue)

	// An instance that was never bound is refused.
	_, err = invoke(c.deps, "Action=ClaimFirstStore", workerActor(successor), &call{})
	assertCode(t, err, awserrors.ErrorAccessDenied)

	ctx := context.Background()
	if err := c.ids.RecordStopped(ctx, first, instance); err != nil {
		t.Fatal(err)
	}
	// Stopped but not yet replaced: the old instance is no longer current.
	_, err = invoke(c.deps, "Action=ClaimFirstStore", workerActor(instance), &call{})
	assertCode(t, err, awserrors.ErrorAccessDenied)

	c.launch(t, "claim-0002", first.Generation, successor)
	_, err = invoke(c.deps, "Action=ClaimFirstStore", workerActor(instance), &call{})
	assertCode(t, err, awserrors.ErrorAccessDenied)
	rec, err = invoke(c.deps, "Action=ClaimFirstStore", workerActor(successor), &call{})
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<CreateKey>false</CreateKey>") || !strings.Contains(body, k.pub) {
		t.Fatalf("successor got %q, want only the recorded key", body)
	}
}

// The race between authorization and the first-store call: a request that
// passed Authorize just before a replacement still carries the old generation,
// and once the successor has claimed, firststore refuses it.
func TestRealStores_InFlightOldGenerationLosesToTheSuccessor(t *testing.T) {
	c := newCluster(t)
	first := c.launch(t, "claim-0001", 0, instance)
	stale, err := c.ids.Authorize(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ids.RecordStopped(context.Background(), first, instance); err != nil {
		t.Fatal(err)
	}
	c.launch(t, "claim-0002", first.Generation, successor)
	if _, err := invoke(c.deps, "Action=ClaimFirstStore", workerActor(successor), &call{}); err != nil {
		t.Fatal(err)
	}

	inFlight := Deps{Identities: &fakeIdentities{ident: stale}, FirstStore: c.deps.FirstStore}
	_, err = invoke(inFlight, "Action=ClaimFirstStore", workerActor(instance), &call{})
	assertCode(t, err, awserrors.ErrorAccessDenied)
	_, err = invoke(inFlight, "Action=RecordWorkerPublicKey&PublicKey="+newUserKey(t).pub, workerActor(instance), &call{})
	assertCode(t, err, awserrors.ErrorAccessDenied)
}

// Concurrent claims from the one current worker decide once.
func TestRealStores_ConcurrentClaimsAgree(t *testing.T) {
	c := newCluster(t)
	c.launch(t, "claim-0001", 0, instance)
	var wg sync.WaitGroup
	bodies := make([]string, 8)
	errs := make([]error, 8)
	for i := range bodies {
		wg.Go(func() {
			rec, err := invoke(c.deps, "Action=ClaimFirstStore", workerActor(instance), &call{})
			bodies[i], errs[i] = rec.Body.String(), err
		})
	}
	wg.Wait()
	for i := range bodies {
		if errs[i] != nil && errs[i].Error() != awserrors.ErrorServiceUnavailable {
			t.Fatalf("claim %d: %v", i, errs[i])
		}
		if errs[i] == nil && !strings.Contains(bodies[i], "<CreateKey>true</CreateKey>") {
			t.Fatalf("claim %d: %q", i, bodies[i])
		}
	}
}

// Absent buckets refuse the worker and stay absent: the gateway provisions
// nothing.
func TestRealStores_AbsentBucketsFailClosedAndStayAbsent(t *testing.T) {
	clustersize.DeclareForTest(t, 1)
	_, _, js := testutil.StartTestJetStream(t)
	deps := JetStreamDeps(js)
	_, err := invoke(deps, "Action=ClaimFirstStore", workerActor(instance), &call{})
	assertCode(t, err, awserrors.ErrorServiceUnavailable)
	for _, name := range []string{workerident.Bucket, firststore.Bucket} {
		if _, err := js.KeyValue(context.Background(), name); !errors.Is(err, jetstream.ErrBucketNotFound) {
			t.Fatalf("the gateway provisioned %s: %v", name, err)
		}
	}
}
