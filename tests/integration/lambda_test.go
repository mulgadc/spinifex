//go:build integration

package integration

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/aws/aws-sdk-go/service/s3"
	ststypes "github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/bluebottle/pkg/masterkey"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	lambdaawsapi "github.com/mulgadc/spinifex/spinifex/domains/lambda/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/codestore"
	lambdafunction "github.com/mulgadc/spinifex/spinifex/domains/lambda/function"
	stsdomain "github.com/mulgadc/spinifex/spinifex/domains/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/passrole"
	awsdispatch "github.com/mulgadc/spinifex/spinifex/ingress/aws/dispatch"
	testpredastore "github.com/mulgadc/spinifex/tests/fixtures/predastore"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

const lambdaTrustPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}`

// The gateway's NATS connection and IAM service exist only once
// StartGatewayWithServices has built them, after the registration list.
type lateMeta struct{ lambdafunction.Client }

type lateRoles struct{ passrole.Resolver }

// startLambdaDaemonLite subscribes the production MetaService to the subjects
// the gateway's Lambda client dials, on JetStream KV like the live daemon.
func startLambdaDaemonLite(t *testing.T, nc *nats.Conn) {
	t.Helper()
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	svc := lambdafunction.NewMetaService(js)
	sub(t, nc, lambdafunction.SubjectCreate, func(m *nats.Msg) { dispatch(m, svc.Create) })
	sub(t, nc, lambdafunction.SubjectGet, func(m *nats.Msg) { dispatch(m, svc.Get) })
	sub(t, nc, lambdafunction.SubjectCodeStored, func(m *nats.Msg) { dispatch(m, svc.CodeStored) })
	sub(t, nc, lambdafunction.SubjectRollbackCreate, func(m *nats.Msg) { dispatch(m, svc.RollbackCreate) })
}

// TestLambda_SignedCreateAndGetDownloadsTheStoredCode drives CreateFunction and
// GetFunction through signed ingress, the NATS-owned function record and the
// real code store on the Predastore fixture, then downloads Code.Location.
// The fixture authenticates only its service key, so that key stands in for
// the scoped code-store session; provisioning and session scoping are not here.
func TestLambda_SignedCreateAndGetDownloadsTheStoredCode(t *testing.T) {
	fixture := testpredastore.Start(t)
	bucket := codestore.BucketName(testRegion)
	if _, err := fixtureS3(t, fixture).CreateBucket(&s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		var ae awserr.Error
		require.True(t, errors.As(err, &ae) && ae.Code() == s3.ErrCodeBucketAlreadyOwnedByYou, "create %s: %v", bucket, err)
	}
	store, err := codestore.Open(codestore.OpenConfig{
		Region:          testRegion,
		Endpoint:        "https://" + fixture.Host,
		PresignEndpoint: "https://" + fixture.Host,
		SigningRegion:   fixture.Region,
		Assume: func() (*ststypes.AssumeRoleOutput, error) {
			return &ststypes.AssumeRoleOutput{Credentials: &ststypes.Credentials{
				AccessKeyId:     aws.String(fixture.AccessKey),
				SecretAccessKey: aws.String(fixture.SecretKey),
				Expiration:      aws.Time(time.Now().Add(time.Hour)),
			}}, nil
		},
	})
	require.NoError(t, err)

	keyBytes := make([]byte, 32)
	_, err = rand.Read(keyBytes)
	require.NoError(t, err)
	envKey, err := masterkey.New(keyBytes)
	require.NoError(t, err)

	meta, roles := &lateMeta{}, &lateRoles{}
	var trusts func(accountID, roleARN string) (bool, error)
	gw := StartGatewayWithServices(t, []awsdispatch.Registration{lambdaawsapi.NewRegistration(lambdaawsapi.Deps{
		Meta:   meta,
		Codes:  func(context.Context) (lambdaawsapi.CodeStore, error) { return store, nil },
		Roles:  roles,
		Trusts: func(accountID, roleARN string) (bool, error) { return trusts(accountID, roleARN) },
		AEAD:   envKey.AEAD,
	})})
	iamSvc, ok := gwfix.DepsOf(gw.Config).IAM.(*iamdomain.IAMServiceImpl)
	require.True(t, ok, "harness IAM service type")
	meta.Client = lambdafunction.NewNATSClient(gw.NATSConn)
	roles.Resolver = iamdomain.NewRoleResolver(iamSvc)
	trusts = stsdomain.RoleTrusts(iamSvc, "lambda.amazonaws.com")
	startLambdaDaemonLite(t, gw.NATSConn)

	role, err := gw.IAMClient(t).CreateRole(&iam.CreateRoleInput{
		RoleName: aws.String("lambda-e2e-exec"), AssumeRolePolicyDocument: aws.String(lambdaTrustPolicy),
	})
	require.NoError(t, err)

	code := lambdaTestZip(t)
	sum := sha256.Sum256(code)
	cli := lambda.New(gw.session(t))
	created, err := cli.CreateFunction(&lambda.CreateFunctionInput{
		FunctionName: aws.String("e2e-fn"),
		Runtime:      aws.String("provided.al2023"),
		Handler:      aws.String("bootstrap"),
		Role:         role.Role.Arn,
		Code:         &lambda.FunctionCode{ZipFile: code},
		Environment:  &lambda.Environment{Variables: map[string]*string{"MODE": aws.String("e2e")}},
	})
	require.NoError(t, err)
	require.Equal(t, base64.StdEncoding.EncodeToString(sum[:]), aws.StringValue(created.CodeSha256))

	got, err := cli.GetFunction(&lambda.GetFunctionInput{FunctionName: aws.String("e2e-fn")})
	require.NoError(t, err)
	require.Equal(t, "e2e", aws.StringValue(got.Configuration.Environment.Variables["MODE"]))
	require.NotNil(t, got.Code.Location)

	// SSL_CERT_FILE (set by the fixture) makes the default client trust it.
	resp, err := http.Get(aws.StringValue(got.Code.Location)) //nolint:gosec,noctx // URL is the presigned location under test
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	downloaded, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, code, downloaded)
}

// fixtureS3 signs as the fixture's service key; the fixture's certificate is in
// the process trust store via SSL_CERT_FILE.
func fixtureS3(t *testing.T, fixture *testpredastore.Fixture) *s3.S3 {
	t.Helper()
	sess, err := session.NewSession(&aws.Config{
		Region:           aws.String(fixture.Region),
		Endpoint:         aws.String("https://" + fixture.Host),
		Credentials:      credentials.NewStaticCredentials(fixture.AccessKey, fixture.SecretKey, ""),
		S3ForcePathStyle: aws.Bool(true),
	})
	require.NoError(t, err)
	return s3.New(sess)
}

func lambdaTestZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("bootstrap")
	require.NoError(t, err)
	_, err = w.Write([]byte("#!/bin/sh\necho e2e\n"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}
