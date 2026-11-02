package awsgw

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/s3"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/codestore"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const codeStoreTestRegion = "ap-southeast-2"

type nopJWKS struct{}

func (nopJWKS) LookupClusterJWKS(context.Context, string, string) ([]byte, bool, error) {
	return nil, false, nil
}

// fakeCodeBucket stands in for Predastore so the role path runs on real IAM.
type fakeCodeBucket struct{ tags map[string]string }

func (f *fakeCodeBucket) Create(_ context.Context, tags map[string]string) error {
	if f.tags != nil {
		return codestore.ErrExists
	}
	f.tags = tags
	return nil
}

func (f *fakeCodeBucket) Tags(context.Context) (map[string]string, error) { return f.tags, nil }

func newCodeStoreIAM(t *testing.T) (*iamdomain.IAMServiceImpl, *stsdomain.STSServiceImpl) {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	key, err := iamdomain.GenerateMasterKey()
	require.NoError(t, err)
	iamSvc, err := iamdomain.NewIAMServiceImpl(t.Context(), nc, key)
	require.NoError(t, err)
	stsSvc, err := stsdomain.NewSTSServiceImpl(t.Context(), nc, iamSvc, key, nopJWKS{})
	require.NoError(t, err)

	kv, err := js.KeyValue(t.Context(), iamdomain.KVBucketAccounts)
	require.NoError(t, err)
	acct, err := json.Marshal(iamdomain.Account{AccountID: identifiers.GlobalAccountID, AccountName: "system", Status: iamdomain.AccountStatusActive})
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), identifiers.GlobalAccountID, acct)
	require.NoError(t, err)
	return iamSvc, stsSvc
}

// Provisioning through the real IAM adapter yields a role the in-process STS
// entry point can assume, so the role name, trust and tags agree end to end.
func TestLambdaCodeStoreRole_ProvisionThenAssume(t *testing.T) {
	iamSvc, stsSvc := newCodeStoreIAM(t)
	role := &codeStoreRole{iam: iamSvc, name: stsdomain.CodeStoreRoleName(codeStoreTestRegion)}

	require.NoError(t, codestore.Provision(t.Context(), codeStoreTestRegion, role, &fakeCodeBucket{}))

	st, err := role.Describe()
	require.NoError(t, err)
	assert.Equal(t, codestore.OwnerTagValue, st.Tags[codestore.OwnerTagKey])
	assert.Equal(t, []string{codestore.PolicyName}, st.InlinePolicies)
	_, err = stsSvc.AssumeCodeStoreRole(codeStoreTestRegion, "awsgw-node1")
	require.NoError(t, err)
}

func TestLambdaCodeStoreRole_RetriesOwnCrashAndRefusesUntagged(t *testing.T) {
	iamSvc, _ := newCodeStoreIAM(t)
	name := stsdomain.CodeStoreRoleName(codeStoreTestRegion)
	role := &codeStoreRole{iam: iamSvc, name: name}

	// Lambda's own crash after CreateRole: tagged role, no inline policy.
	require.NoError(t, role.Create(codestore.TrustPolicy, map[string]string{codestore.OwnerTagKey: codestore.OwnerTagValue}))
	require.NoError(t, codestore.Provision(t.Context(), codeStoreTestRegion, role, &fakeCodeBucket{}))

	other := &codeStoreRole{iam: iamSvc, name: stsdomain.CodeStoreRoleName("us-east-1")}
	_, err := iamSvc.CreateRole(identifiers.GlobalAccountID, &iam.CreateRoleInput{
		RoleName: aws.String(other.name), AssumeRolePolicyDocument: aws.String(codestore.TrustPolicy)})
	require.NoError(t, err)
	err = codestore.Provision(t.Context(), "us-east-1", other, &fakeCodeBucket{})
	require.ErrorIs(t, err, codestore.ErrNotOwned)
	policies, err := iamSvc.ListRolePolicies(identifiers.GlobalAccountID, &iam.ListRolePoliciesInput{RoleName: aws.String(other.name)})
	require.NoError(t, err)
	assert.Empty(t, policies.PolicyNames, "no policy written to an untagged role")
}

// s3Responder answers the bucket admin's requests with fixed S3 replies.
func s3Responder(t *testing.T, create, tagging int, createCode string) (*codeStoreBucket, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, isTagging := r.URL.Query()["tagging"]
		calls = append(calls, r.Method+" tagging="+strconv.FormatBool(isTagging))
		switch {
		case r.Method == http.MethodPut && !isTagging:
			w.WriteHeader(create)
			if createCode != "" {
				fmt.Fprintf(w, "<Error><Code>%s</Code></Error>", createCode)
			}
		case r.Method == http.MethodGet && isTagging:
			w.WriteHeader(tagging)
			if tagging == http.StatusNotFound {
				_, _ = w.Write([]byte("<Error><Code>NoSuchTagSet</Code></Error>"))
				return
			}
			_, _ = w.Write([]byte(`<Tagging><TagSet><Tag><Key>spinifex:owner</Key><Value>lambda-code-store</Value></Tag></TagSet></Tagging>`))
		default:
			w.WriteHeader(tagging)
		}
	}))
	t.Cleanup(srv.Close)
	sess := session.Must(session.NewSession(&aws.Config{
		Endpoint: aws.String(srv.URL), Region: aws.String(codeStoreTestRegion), S3ForcePathStyle: aws.Bool(true),
		Credentials: credentials.NewStaticCredentials("AKIA", "secret", ""), MaxRetries: aws.Int(0),
	}))
	return &codeStoreBucket{s3: s3.New(sess), name: codestore.BucketName(codeStoreTestRegion)}, &calls
}

func TestLambdaCodeStoreBucket_MapsCreateOutcomes(t *testing.T) {
	ctx := t.Context()

	b, calls := s3Responder(t, http.StatusOK, http.StatusOK, "")
	require.NoError(t, b.Create(ctx, map[string]string{"k": "v"}))
	assert.Equal(t, []string{"PUT tagging=false", "PUT tagging=true"}, *calls, "a new bucket is tagged")

	b, calls = s3Responder(t, http.StatusConflict, http.StatusOK, "BucketAlreadyOwnedByYou")
	require.ErrorIs(t, b.Create(ctx, nil), codestore.ErrExists)
	assert.Len(t, *calls, 1, "an existing bucket is never tagged")

	b, _ = s3Responder(t, http.StatusConflict, http.StatusOK, "BucketAlreadyExists")
	require.ErrorIs(t, b.Create(ctx, nil), codestore.ErrOwnedElsewhere)
}

func TestLambdaCodeStoreBucket_ReadsTags(t *testing.T) {
	b, _ := s3Responder(t, http.StatusOK, http.StatusOK, "")
	tags, err := b.Tags(t.Context())
	require.NoError(t, err)
	assert.Equal(t, codestore.OwnerTagValue, tags[codestore.OwnerTagKey])

	b, _ = s3Responder(t, http.StatusOK, http.StatusNotFound, "")
	tags, err = b.Tags(t.Context())
	require.NoError(t, err)
	assert.Empty(t, tags)
}

// Composing the store at gateway start makes no IAM or S3 call; the first use
// provisions the role and bucket and opens a session through the real STS path.
func TestNewLambdaCodeStore_ProvisionsOnlyOnFirstUse(t *testing.T) {
	iamSvc, stsSvc := newCodeStoreIAM(t)
	var s3Calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, isTagging := r.URL.Query()["tagging"]
		s3Calls = append(s3Calls, r.Method+" "+r.URL.Path+" tagging="+strconv.FormatBool(isTagging))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	mgr, err := newLambdaCodeStore(codeStoreWiring{
		Region: codeStoreTestRegion, Node: "node1", Endpoint: srv.URL, SigningRegion: codeStoreTestRegion,
		RootAccessKey: "AKIAROOT", RootSecretKey: "root-secret",
	}, iamSvc, stsSvc)
	require.NoError(t, err)

	roleName := aws.String(stsdomain.CodeStoreRoleName(codeStoreTestRegion))
	_, err = iamSvc.GetRole(identifiers.GlobalAccountID, &iam.GetRoleInput{RoleName: roleName})
	require.EqualError(t, err, "NoSuchEntity", "no role before first use")
	assert.Empty(t, s3Calls, "no bucket request before first use")

	_, err = mgr.Store(t.Context())
	require.NoError(t, err)

	_, err = iamSvc.GetRole(identifiers.GlobalAccountID, &iam.GetRoleInput{RoleName: roleName})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"PUT /lambda-code-ap-southeast-2 tagging=false",
		"PUT /lambda-code-ap-southeast-2 tagging=true",
	}, s3Calls)
}
