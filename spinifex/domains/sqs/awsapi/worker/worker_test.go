package worker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/workerident"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	awsidentifiers "github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
)

const (
	region    = "ap-southeast-2"
	instance  = "i-0abc12345def67890"
	successor = "i-0fed98765cba43210"
	volume    = "vol-0123456789abcdef0"
	fpA       = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fpB       = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
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
	ident    workerident.Identity
	err      error
	asked    []string
	recorded []string
	recErr   error
}

func (f *fakeIdentities) Authorize(_ context.Context, instanceID string) (workerident.Identity, error) {
	f.asked = append(f.asked, instanceID)
	return f.ident, f.err
}

func (f *fakeIdentities) RecordReceiptKey(_ context.Context, _ workerident.Identity, fp string) error {
	f.recorded = append(f.recorded, fp)
	return f.recErr
}

func current() workerident.Identity {
	return workerident.Identity{Region: region, InstanceID: instance, Generation: 3, ClaimID: "claim-0003",
		NodeID: "node-a", DataVolumeID: volume, ReceiptInit: workerident.ReceiptPending}
}

func fakes() (*fakeIdentities, Deps) {
	ids := &fakeIdentities{ident: current()}
	return ids, Deps{Identities: ids, HostNodes: func() int { return 1 }}
}

// The receipt key claim reports only what the current record proves: pending
// for a new volume, the recorded fingerprint, or a refusal for neither.
func TestClaimReceiptKey_ReportsTheRecordsInitialization(t *testing.T) {
	for name, tc := range map[string]struct {
		init workerident.ReceiptInit
		fp   string
		want []string
		code string
	}{
		"pending":  {init: workerident.ReceiptPending, want: []string{"<Pending>true</Pending>", "<HostNodes>1</HostNodes>"}},
		"recorded": {init: workerident.ReceiptRecorded, fp: fpA, want: []string{"<Pending>false</Pending>", fpA}},
		"unknown":  {code: awserrors.ErrorInvalidState},
	} {
		t.Run(name, func(t *testing.T) {
			ids, deps := fakes()
			ids.ident.ReceiptInit, ids.ident.ReceiptFingerprint = tc.init, tc.fp
			rec, err := invoke(deps, "Action=ClaimReceiptKey", workerActor(instance), &call{})
			if tc.code != "" {
				assertCode(t, err, tc.code)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(rec.Body.String(), w) {
					t.Fatalf("body %q lacks %q", rec.Body.String(), w)
				}
			}
		})
	}
}

func TestRecordReceiptKey_ValidatesTheFingerprintBeforeTheStore(t *testing.T) {
	for body, code := range map[string]string{
		"Action=RecordReceiptKey":                                     awserrors.ErrorMissingParameter,
		"Action=RecordReceiptKey&Fingerprint=" + fpA[:63]:             awserrors.ErrorInvalidParameterValue,
		"Action=RecordReceiptKey&Fingerprint=" + strings.ToUpper(fpA): awserrors.ErrorInvalidParameterValue,
	} {
		ids, deps := fakes()
		_, err := invoke(deps, body, workerActor(instance), &call{})
		assertCode(t, err, code)
		if len(ids.recorded) != 0 {
			t.Fatalf("%s: reached the store", body)
		}
	}
}

func TestRequestSuppliedIdentityIsRefused(t *testing.T) {
	for _, body := range []string{
		"Action=ClaimReceiptKey&InstanceId=" + successor,
		"Action=ClaimReceiptKey&Generation=99",
		"Action=ClaimReceiptKey&Region=us-east-1",
		"Action=RecordReceiptKey&Fingerprint=" + fpA + "&Generation=99",
	} {
		ids, deps := fakes()
		_, err := invoke(deps, body, workerActor(instance), &call{})
		assertCode(t, err, awserrors.ErrorInvalidParameterValue)
		if len(ids.asked)+len(ids.recorded) != 0 {
			t.Fatalf("%s: consulted the store", body)
		}
	}
}

// Nothing but the worker role's own session in the system account passes, and
// a refused caller never reaches the policy evaluator or the store.
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
			ids, deps := fakes()
			c := &call{}
			_, err := invoke(deps, "Action=ClaimReceiptKey", actor, c)
			assertCode(t, err, awserrors.ErrorAccessDenied)
			if c.policyChecks != 0 || len(ids.asked) != 0 {
				t.Fatal("a refused caller reached the policy check or the store")
			}
		})
	}
	t.Run("invocation account differs from the actor", func(t *testing.T) {
		ids, deps := fakes()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("Action=ClaimReceiptKey"))
		err := deps.dispatch(httptest.NewRecorder(), dispatch.Invocation{
			Request: req, AccountID: customer, Region: region,
			Authorize: func(string, string, []string, iampolicy.ConditionKeys) error { return nil },
			Actor:     func() (dispatch.Actor, error) { return workerActor(instance), nil },
		})
		assertCode(t, err, awserrors.ErrorAccessDenied)
		if len(ids.asked) != 0 {
			t.Fatal("consulted the store")
		}
	})
}

func TestPolicyDenialStops(t *testing.T) {
	ids, deps := fakes()
	c := &call{policyErr: errors.New(awserrors.ErrorAccessDenied)}
	_, err := invoke(deps, "Action=ClaimReceiptKey", workerActor(instance), c)
	assertCode(t, err, awserrors.ErrorAccessDenied)
	if c.policyChecks != 1 || len(ids.asked) != 0 {
		t.Fatal("the role policy was not the gate before the store")
	}
}

// An unknown, superseded or foreign-Region worker never records a key.
func TestNonCurrentWorkerIsRefused(t *testing.T) {
	for name, set := range map[string]func(*fakeIdentities){
		"unknown instance":    func(f *fakeIdentities) { f.err = workerident.ErrUnknownInstance },
		"superseded instance": func(f *fakeIdentities) { f.err = workerident.ErrNotCurrent },
		"another Region":      func(f *fakeIdentities) { f.ident.Region = "us-east-1" },
	} {
		t.Run(name, func(t *testing.T) {
			ids, deps := fakes()
			set(ids)
			_, err := invoke(deps, "Action=RecordReceiptKey&Fingerprint="+fpA, workerActor(instance), &call{})
			assertCode(t, err, awserrors.ErrorAccessDenied)
			if len(ids.recorded) != 0 {
				t.Fatal("a non-current worker recorded a key")
			}
		})
	}
}

func TestStoreErrorsMapToWorkerCodes(t *testing.T) {
	for err, code := range map[error]string{
		workerident.ErrNotCurrent:        awserrors.ErrorAccessDenied,
		workerident.ErrReceiptRecorded:   awserrors.ErrorInvalidParameterValue,
		workerident.ErrReceiptUnknown:    awserrors.ErrorInvalidState,
		workerident.ErrContended:         awserrors.ErrorServiceUnavailable,
		errors.New("leader unreachable"): awserrors.ErrorServerInternal,
	} {
		ids, deps := fakes()
		ids.recErr = err
		_, got := invoke(deps, "Action=RecordReceiptKey&Fingerprint="+fpA, workerActor(instance), &call{})
		assertCode(t, got, code)
	}
}

// Without its store the surface refuses every worker, after the gate has
// refused everyone else.
func TestMissingDependenciesFailClosed(t *testing.T) {
	for name, deps := range map[string]Deps{
		"no deps":       {},
		"nil JetStream": JetStreamDeps(nil),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := invoke(deps, "Action=ClaimReceiptKey", workerActor(instance), &call{})
			assertCode(t, err, awserrors.ErrorServiceUnavailable)
			_, err = invoke(deps, "Action=ClaimReceiptKey", dispatch.Actor{}, &call{})
			assertCode(t, err, awserrors.ErrorAccessDenied)
		})
	}
	_, deps := fakes()
	_, err := invoke(deps, "Action=PollWork", workerActor(instance), &call{})
	assertCode(t, err, awserrors.ErrorServiceUnavailable)
}

func TestUnknownAndMissingActions(t *testing.T) {
	_, deps := fakes()
	for body, code := range map[string]string{
		"Action=SendMessage":     awserrors.ErrorInvalidAction,
		"Action=ClaimFirstStore": awserrors.ErrorInvalidAction,
		"Action=DescribeStore":   awserrors.ErrorInvalidAction,
		"":                       awserrors.ErrorMissingAction,
	} {
		_, err := invoke(deps, body, workerActor(instance), &call{})
		assertCode(t, err, code)
	}
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestUnexpectedParameterNameIsNotLogged(t *testing.T) {
	logs := captureLogs(t)
	_, deps := fakes()
	const secret = "SUAKSECRETPARAMETERNAME0123456789"
	_, err := invoke(deps, "Action=ClaimReceiptKey&"+secret+"=1", workerActor(instance), &call{})
	assertCode(t, err, awserrors.ErrorInvalidParameterValue)
	if !strings.Contains(logs.String(), "unexpected parameter refused") {
		t.Fatalf("refusal not logged: %q", logs.String())
	}
	if strings.Contains(logs.String(), secret[2:12]) {
		t.Fatal("parameter name reached the log")
	}
}

// --- Against the real identity store on embedded JetStream. ---

// A worker on a newly created volume is told to create its key, records it
// once, and its successor on the same volume is given only that fingerprint.
func TestRealStore_PendingRecordAndReplace(t *testing.T) {
	clustersize.DeclareForTest(t, 1)
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	ids, err := workerident.Open(ctx, js)
	if err != nil {
		t.Fatal(err)
	}
	deps := JetStreamDeps(js)
	deps.HostNodes = func() int { return 1 }

	first, err := ids.BeginLaunch(ctx, region, workerident.LaunchRequest{InstanceType: "t3.small",
		ClaimID: "claim-0001", NodeID: "node-a", SizeGiB: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.MarkVolumeAttempted(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := ids.RecordVolume(ctx, first, volume); err != nil {
		t.Fatal(err)
	}
	if err := ids.BindInstance(ctx, first, instance); err != nil {
		t.Fatal(err)
	}

	rec, err := invoke(deps, "Action=ClaimReceiptKey", workerActor(instance), &call{})
	if err != nil || !strings.Contains(rec.Body.String(), "<Pending>true</Pending>") {
		t.Fatalf("first claim: %v %q", err, rec.Body.String())
	}
	if _, err := invoke(deps, "Action=RecordReceiptKey&Fingerprint="+fpA, workerActor(instance), &call{}); err != nil {
		t.Fatal(err)
	}
	// A lost reply is retried with the same fingerprint; any other is refused.
	if _, err := invoke(deps, "Action=RecordReceiptKey&Fingerprint="+fpA, workerActor(instance), &call{}); err != nil {
		t.Fatalf("retried record: %v", err)
	}
	_, err = invoke(deps, "Action=RecordReceiptKey&Fingerprint="+fpB, workerActor(instance), &call{})
	assertCode(t, err, awserrors.ErrorInvalidParameterValue)

	_, err = invoke(deps, "Action=ClaimReceiptKey", workerActor(successor), &call{})
	assertCode(t, err, awserrors.ErrorAccessDenied)

	if err := ids.RecordStopped(ctx, first, instance); err != nil {
		t.Fatal(err)
	}
	next, err := ids.BeginLaunch(ctx, region, workerident.LaunchRequest{InstanceType: "t3.small",
		ClaimID: "claim-0002", Supersedes: first.Generation, NodeID: "node-a",
		DataVolumeID: volume, DataVolumeEncrypted: true, SizeGiB: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.BindInstance(ctx, next, successor); err != nil {
		t.Fatal(err)
	}
	_, err = invoke(deps, "Action=ClaimReceiptKey", workerActor(instance), &call{})
	assertCode(t, err, awserrors.ErrorAccessDenied)
	rec, err = invoke(deps, "Action=ClaimReceiptKey", workerActor(successor), &call{})
	if err != nil {
		t.Fatal(err)
	}
	if body := rec.Body.String(); !strings.Contains(body, "<Pending>false</Pending>") || !strings.Contains(body, fpA) {
		t.Fatalf("successor got %q, want only the recorded fingerprint", body)
	}
}

// A record created on a supplied volume is never pending: its volume is not
// proven new, so no key is created for it.
func TestRealStore_SuppliedVolumeIsNotPending(t *testing.T) {
	clustersize.DeclareForTest(t, 1)
	_, _, js := testutil.StartTestJetStream(t)
	ctx := context.Background()
	ids, err := workerident.Open(ctx, js)
	if err != nil {
		t.Fatal(err)
	}
	in, err := ids.BeginLaunch(ctx, region, workerident.LaunchRequest{InstanceType: "t3.small",
		ClaimID: "claim-0001", NodeID: "node-a", DataVolumeID: volume, DataVolumeEncrypted: true, SizeGiB: 8})
	if err != nil {
		t.Fatal(err)
	}
	if err := ids.BindInstance(ctx, in, instance); err != nil {
		t.Fatal(err)
	}
	_, err = invoke(JetStreamDeps(js), "Action=ClaimReceiptKey", workerActor(instance), &call{})
	assertCode(t, err, awserrors.ErrorInvalidState)
}
