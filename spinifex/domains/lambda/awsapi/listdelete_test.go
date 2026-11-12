package awsapi_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/internal/testkit/gwtest"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/eventsource"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	listPath  = "/2015-03-31/functions"
	helloPath = "/2015-03-31/functions/hello"
	helloARN  = "arn:aws:lambda:" + gwtest.Region + ":" + gwtest.AccountID + ":function:hello"
)

// listAll walks ListFunctions from path, following NextMarker, and returns the
// pages it read.
func listAll(t *testing.T, e *env, path string) []lambda.ListFunctionsOutput {
	t.Helper()
	var pages []lambda.ListFunctionsOutput
	marker := ""
	for range 100 {
		p := path
		if marker != "" {
			p += "&Marker=" + url.QueryEscape(marker)
		}
		rec, err := e.do(http.MethodGet, p, nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, rec.Code)
		var out lambda.ListFunctionsOutput
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		pages = append(pages, out)
		if out.NextMarker == nil {
			return pages
		}
		marker = *out.NextMarker
	}
	t.Fatal("the walk does not end")
	return nil
}

func listedNames(pages []lambda.ListFunctionsOutput) []string {
	var names []string
	for _, p := range pages {
		for _, f := range p.Functions {
			names = append(names, *f.FunctionName)
		}
	}
	return names
}

func createActive(t *testing.T, e *env, names ...string) {
	t.Helper()
	for _, name := range names {
		_, err := e.do(http.MethodPost, listPath, createBody(name, nil))
		require.NoError(t, err)
	}
	e.activate(t)
}

func TestListFunctions_WalksEveryFunctionAcrossPagesOnBothPaths(t *testing.T) {
	e := newUpdateEnv(t)
	createActive(t, e, "second", "third")

	pages := listAll(t, e, listPath+"?MaxItems=1")
	assert.Len(t, pages, 3, "a page that fills on the last function has no NextMarker")
	assert.ElementsMatch(t, []string{"hello", "second", "third"}, listedNames(pages))
	slash := listAll(t, e, listPath+"/?MaxItems=1")
	assert.Equal(t, listedNames(pages), listedNames(slash))

	rec, err := e.do(http.MethodGet, listPath, nil)
	require.NoError(t, err)
	assert.NotContains(t, rec.Body.String(), "NextMarker")
}

// ListFunctions renders each function as GetFunctionConfiguration does, with
// its environment opened, less the state and last-update members.
func TestListFunctions_RendersTheConfigurationSubset(t *testing.T) {
	e := newUpdateEnv(t)
	rec, err := e.do(http.MethodGet, helloPath+"/configuration", nil)
	require.NoError(t, err)
	want := decodeConfig(t, rec.Body.Bytes())
	want.State, want.StateReason, want.StateReasonCode = nil, nil, nil
	want.LastUpdateStatus, want.LastUpdateStatusReason, want.LastUpdateStatusReasonCode = nil, nil, nil

	rec, err = e.do(http.MethodGet, listPath, nil)
	require.NoError(t, err)
	for _, member := range []string{`"State"`, `"StateReason`, `"LastUpdateStatus`} {
		assert.NotContains(t, rec.Body.String(), member)
	}
	var out lambda.ListFunctionsOutput
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Functions, 1)
	assert.Equal(t, want, *out.Functions[0])
	assert.Equal(t, "prod", *out.Functions[0].Environment.Variables["MODE"])
}

func TestListFunctions_EmptyAccountReturnsAnEmptyList(t *testing.T) {
	e := newEnv(t)
	rec, err := e.do(http.MethodGet, listPath+"?FunctionVersion=ALL", nil)
	require.NoError(t, err)
	assert.JSONEq(t, `{"Functions":[]}`, rec.Body.String())
}

func TestListFunctions_PageHoldsAtMostFifty(t *testing.T) {
	e := newEnv(t)
	for i := range function.MaxListPage + 1 {
		_, err := e.do(http.MethodPost, listPath, createBody(fmt.Sprintf("fn-%02d", i), nil))
		require.NoError(t, err)
	}
	pages := listAll(t, e, listPath+"?MaxItems=10000")
	require.Len(t, pages, 2)
	assert.Len(t, pages[0].Functions, function.MaxListPage)
	assert.Len(t, listedNames(pages), function.MaxListPage+1)
}

// Two accounts holding the same function names see only their own functions,
// with their own environments, on first and continuation pages, and neither
// can continue the other's walk.
func TestListFunctions_AccountsAreIsolated(t *testing.T) {
	e := newEnv(t)
	const other = "210987654321"
	e.deps.Roles = roleSet{roleARN: true, "arn:aws:iam::" + other + ":role/lambda-exec": true}
	names := map[string][]string{
		gwtest.AccountID: {"shared-1", "only-a", "shared-2", "shared-3"},
		other:            {"shared-2", "shared-1", "only-b", "shared-3"},
	}
	for _, acct := range []string{gwtest.AccountID, other} {
		e.h.AccountID = acct
		for _, name := range names[acct] {
			rec, err := e.do(http.MethodPost, listPath, createBody(name, func(b map[string]any) {
				b["Role"] = "arn:aws:iam::" + acct + ":role/lambda-exec"
				b["Environment"] = map[string]any{"Variables": map[string]string{"OWNER": acct}}
			}))
			require.NoError(t, err)
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		}
	}
	e.activate(t)

	markers := map[string]string{}
	for _, acct := range []string{gwtest.AccountID, other} {
		e.h.AccountID = acct
		pages := listAll(t, e, listPath+"?MaxItems=2")
		require.Len(t, pages, 2, "the walk has a continuation page")
		markers[acct] = *pages[0].NextMarker
		assert.Equal(t, names[acct], listedNames(pages))
		for _, p := range pages {
			for _, f := range p.Functions {
				assert.Equal(t, "arn:aws:lambda:"+gwtest.Region+":"+acct+":function:"+*f.FunctionName, *f.FunctionArn)
				assert.Equal(t, "arn:aws:iam::"+acct+":role/lambda-exec", *f.Role)
				assert.Equal(t, acct, *f.Environment.Variables["OWNER"])
			}
		}
	}
	for acct, foreign := range map[string]string{gwtest.AccountID: markers[other], other: markers[gwtest.AccountID]} {
		e.h.AccountID = acct
		_, err := e.do(http.MethodGet, listPath+"?Marker="+url.QueryEscape(foreign), nil)
		testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
	}
}

func TestListFunctions_ParameterValidation(t *testing.T) {
	e := newUpdateEnv(t)
	for name, query := range map[string]string{
		"MaxItems zero":           "MaxItems=0",
		"MaxItems over 10000":     "MaxItems=10001",
		"MaxItems not a number":   "MaxItems=x",
		"MaxItems repeated":       "MaxItems=1&MaxItems=2",
		"empty Marker":            "Marker=",
		"malformed Marker":        "Marker=not-a-marker",
		"MasterRegion":            "MasterRegion=us-east-1",
		"unknown FunctionVersion": "FunctionVersion=LATEST",
		"malformed query":         "MaxItems=%zz",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.do(http.MethodGet, listPath+"?"+query, nil)
			testutil.RequireAWSCode(t, err, awserrors.ErrorInvalidParameterValueException)
		})
	}
}

func TestListFunctions_AuthorizesOnEveryResource(t *testing.T) {
	e := newUpdateEnv(t)
	e.h = gwtest.Scoped(gwtest.Statement("Allow", "lambda:ListFunctions", helloARN))
	_, err := e.do(http.MethodGet, listPath, nil)
	gwtest.AssertDenied(t, err)
	e.h = gwtest.Scoped(gwtest.Statement("Allow", "lambda:ListFunctions", "*"))
	_, err = e.do(http.MethodGet, listPath, nil)
	require.NoError(t, err)
}

func TestListFunctions_OwnerFailuresAreRetryable(t *testing.T) {
	e := newUpdateEnv(t)
	e.meta.listErr = fmt.Errorf("NATS request to %s: %w", function.SubjectList, nats.ErrNoResponders)
	_, err := e.do(http.MethodGet, listPath, nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)
	assert.False(t, awserrors.IsTerminal(err))

	e.meta.listErr = nil
	e.deps.AEAD = nil
	_, err = e.do(http.MethodGet, listPath, nil)
	// An environment that cannot be opened fails the page.
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)
}

// deletion reads the retained obligation for functionID.
func (e *env) deletion(t *testing.T, functionID string) (function.Deletion, bool) {
	t.Helper()
	entry, err := e.kv(t).Get(t.Context(), function.KVDeletedKey(gwtest.Region, functionID))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return function.Deletion{}, false
	}
	require.NoError(t, err)
	var d function.Deletion
	require.NoError(t, json.Unmarshal(entry.Value(), &d))
	return d, true
}

func TestDeleteFunction_RemovesTheFunctionAndRetainsItsRecordsAndCode(t *testing.T) {
	e := newUpdateEnv(t)
	before, _ := e.record(t, "hello")
	copies := e.store.count()
	// Deletion reads neither the code store nor the environment.
	codes, aead := e.deps.Codes, e.deps.AEAD
	e.deps.Codes, e.deps.AEAD = nil, nil

	rec, err := e.do(http.MethodDelete, helloPath, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.Bytes())

	_, err = e.do(http.MethodGet, helloPath+"/configuration", nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
	assert.Empty(t, listedNames(listAll(t, e, listPath+"?MaxItems=1")))
	_, err = e.do(http.MethodDelete, helloPath, nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)

	d, ok := e.deletion(t, before.FunctionID)
	require.True(t, ok)
	assert.Equal(t, "hello", d.FunctionName)
	assert.Contains(t, e.uploads(t), before.FunctionID)
	assert.Equal(t, copies, e.store.count(), "no code copy is removed")

	e.deps.Codes, e.deps.AEAD = codes, aead
	_, err = e.do(http.MethodPost, listPath, createBody("hello", nil))
	require.NoError(t, err)
	again, _ := e.record(t, "hello")
	assert.NotEqual(t, before.FunctionID, again.FunctionID)
	_, ok = e.deletion(t, before.FunctionID)
	assert.True(t, ok, "the old obligation survives the name's reuse")
}

// Every explicit qualifier, in the query or the name, and every query that
// cannot be read exactly, is refused before the owner is asked to delete.
func TestDeleteFunction_RefusesEveryQualifierWithoutMutation(t *testing.T) {
	cases := map[string]struct {
		path string
		code string
	}{
		"query $LATEST":         {helloPath + "?Qualifier=%24LATEST", awserrors.ErrorInvalidParameterValueException},
		"query version":         {helloPath + "?Qualifier=1", awserrors.ErrorResourceNotFoundException},
		"query alias":           {helloPath + "?Qualifier=live", awserrors.ErrorResourceNotFoundException},
		"empty query":           {helloPath + "?Qualifier=", awserrors.ErrorInvalidParameterValueException},
		"bare query key":        {helloPath + "?Qualifier", awserrors.ErrorInvalidParameterValueException},
		"repeated query":        {helloPath + "?Qualifier=1&Qualifier=1", awserrors.ErrorInvalidParameterValueException},
		"malformed escape":      {helloPath + "?Qualifier=%zz", awserrors.ErrorInvalidParameterValueException},
		"malformed other pair":  {helloPath + "?x=%zz&Qualifier=1", awserrors.ErrorInvalidParameterValueException},
		"semicolon":             {helloPath + "?Qualifier=1;x=2", awserrors.ErrorInvalidParameterValueException},
		"invalid pattern":       {helloPath + "?Qualifier=a.b", awserrors.ErrorInvalidParameterValueException},
		"name suffix $LATEST":   {helloPath + ":%24LATEST", awserrors.ErrorInvalidParameterValueException},
		"name suffix version":   {helloPath + ":1", awserrors.ErrorResourceNotFoundException},
		"ARN suffix version":    {"/2015-03-31/functions/" + url.PathEscape(helloARN+":1"), awserrors.ErrorResourceNotFoundException},
		"empty name suffix":     {helloPath + ":", awserrors.ErrorInvalidParameterValueException},
		"mismatched qualifiers": {helloPath + ":1?Qualifier=2", awserrors.ErrorInvalidParameterValueException},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newUpdateEnv(t)
			before, beforeRev := e.record(t, "hello")
			_, err := e.do(http.MethodDelete, tc.path, nil)
			testutil.RequireAWSCode(t, err, tc.code)
			assert.Zero(t, e.meta.deleteCalls)
			requireConfigUnchanged(t, e, before, beforeRev)
			_, ok := e.deletion(t, before.FunctionID)
			assert.False(t, ok)
		})
	}
}

func TestDeleteFunction_Authorization(t *testing.T) {
	allowAll := gwtest.Statement("Allow", "*", "*")
	t.Run("denied on the function ARN", func(t *testing.T) {
		e := newUpdateEnv(t)
		e.h = gwtest.Scoped(allowAll, gwtest.Statement("Deny", "lambda:DeleteFunction", helloARN))
		before, beforeRev := e.record(t, "hello")
		_, err := e.do(http.MethodDelete, helloPath, nil)
		gwtest.AssertDenied(t, err)
		requireConfigUnchanged(t, e, before, beforeRev)
	})
	t.Run("another function's grant does not reach it", func(t *testing.T) {
		e := newUpdateEnv(t)
		e.h = gwtest.Scoped(gwtest.Statement("Allow", "lambda:DeleteFunction", helloARN+"-other"))
		_, err := e.do(http.MethodDelete, helloPath, nil)
		gwtest.AssertDenied(t, err)
	})
	t.Run("allowed on the function ARN", func(t *testing.T) {
		e := newUpdateEnv(t)
		e.h = gwtest.Scoped(gwtest.Statement("Allow", "lambda:DeleteFunction", helloARN))
		rec, err := e.do(http.MethodDelete, helloPath, nil)
		require.NoError(t, err)
		assert.Equal(t, http.StatusNoContent, rec.Code)
	})
}

func TestDeleteFunction_LostReplyIsAnErrorButTheDeleteStands(t *testing.T) {
	e := newUpdateEnv(t)
	e.meta.loseDeleteReply = true
	_, err := e.do(http.MethodDelete, helloPath, nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)
	assert.False(t, awserrors.IsTerminal(err))
	_, err = e.do(http.MethodGet, helloPath+"/configuration", nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
}

func TestDeleteFunction_OwnerWithoutTheSubjectIsRetryableAndChangesNothing(t *testing.T) {
	e := newUpdateEnv(t)
	before, beforeRev := e.record(t, "hello")
	e.meta.deleteErr = fmt.Errorf("NATS request to %s: %w", function.SubjectDelete, nats.ErrNoResponders)
	_, err := e.do(http.MethodDelete, helloPath, nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorServiceException)
	assert.False(t, awserrors.IsTerminal(err))
	requireConfigUnchanged(t, e, before, beforeRev)
}

func TestDeleteFunction_UpdateBetweenReadAndDeleteWins(t *testing.T) {
	e := newConfigEnv(t)
	e.meta.beforeCommit = func() {
		_, err := e.do(http.MethodPut, configPath, map[string]any{"Handler": "handler.winner"})
		assert.NoError(t, err)
	}
	_, err := e.do(http.MethodDelete, helloPath, nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceConflictException)
	after, _ := e.record(t, "hello")
	assert.Equal(t, "handler.winner", after.Handler)
}

func TestDeleteFunction_LeavesMappingsInPlace(t *testing.T) {
	e, store, _ := newMappingEnv(t)
	rec, err := e.do(http.MethodPost, mappingsPath, mappingBody(nil))
	require.NoError(t, err)
	uuid, _ := decodeMapping(t, rec.Body.Bytes())["UUID"].(string)
	before, beforeRev, err := store.Get(t.Context(), uuid)
	require.NoError(t, err)

	_, err = e.do(http.MethodDelete, helloPath, nil)
	require.NoError(t, err)
	after, afterRev, err := store.Get(t.Context(), uuid)
	require.NoError(t, err)
	assert.Equal(t, beforeRev, afterRev)
	assert.Equal(t, before, after)
	assert.Equal(t, eventsource.StateEnabled, after.State)
}

func TestDeleteFunction_LaterInvokeIsNotFound(t *testing.T) {
	e, ex := newInvokeEnv(t)
	_, err := e.do(http.MethodDelete, helloPath, nil)
	require.NoError(t, err)
	_, err = e.invoke(t.Context(), invokePath, nil, nil)
	testutil.RequireAWSCode(t, err, awserrors.ErrorResourceNotFoundException)
	assert.Empty(t, ex.calls)
}
