package handlers_iam

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

const testIssuer = "https://10.0.0.1:9999/oidc/eks/ap-southeast-2/000000000001/h1"

func TestCreateOpenIDConnectProvider(t *testing.T) {
	svc := setupTestIAMService(t)
	acct := "000000000001"

	out, err := svc.CreateOpenIDConnectProvider(acct, &iam.CreateOpenIDConnectProviderInput{
		Url:          aws.String(testIssuer),
		ClientIDList: aws.StringSlice([]string{"sts.amazonaws.com"}),
	})
	if err != nil {
		t.Fatalf("CreateOpenIDConnectProvider: %v", err)
	}

	wantARN := OIDCProviderARN(acct, strings.TrimPrefix(testIssuer, "https://"))
	if aws.StringValue(out.OpenIDConnectProviderArn) != wantARN {
		t.Fatalf("ARN = %q, want %q", aws.StringValue(out.OpenIDConnectProviderArn), wantARN)
	}

	// The registry key STS reads must now exist for this exact issuer.
	kv, err := svc.js.KeyValue(IAMAccountBucketName(acct))
	if err != nil {
		t.Fatalf("open account bucket: %v", err)
	}
	if _, err := kv.Get(OIDCProviderKey(testIssuer)); err != nil {
		t.Fatalf("provider key not written for issuer: %v", err)
	}
}

func TestCreateOpenIDConnectProvider_Idempotent(t *testing.T) {
	svc := setupTestIAMService(t)
	acct := "000000000001"
	in := &iam.CreateOpenIDConnectProviderInput{Url: aws.String(testIssuer)}

	if _, err := svc.CreateOpenIDConnectProvider(acct, in); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := svc.CreateOpenIDConnectProvider(acct, in)
	if err == nil || err.Error() != awserrors.ErrorIAMEntityAlreadyExists {
		t.Fatalf("duplicate create err = %v, want EntityAlreadyExists", err)
	}
}

func TestCreateOpenIDConnectProvider_InvalidURL(t *testing.T) {
	svc := setupTestIAMService(t)
	for _, bad := range []string{"", "http://insecure.example", "://nohost", "ftp://x"} {
		_, err := svc.CreateOpenIDConnectProvider("000000000001",
			&iam.CreateOpenIDConnectProviderInput{Url: aws.String(bad)})
		if err == nil || err.Error() != awserrors.ErrorIAMInvalidInput {
			t.Fatalf("url %q: err = %v, want InvalidInput", bad, err)
		}
	}
}

func TestGetOpenIDConnectProvider(t *testing.T) {
	svc := setupTestIAMService(t)
	acct := "000000000001"
	created, err := svc.CreateOpenIDConnectProvider(acct, &iam.CreateOpenIDConnectProviderInput{
		Url:          aws.String(testIssuer),
		ClientIDList: aws.StringSlice([]string{"sts.amazonaws.com"}),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := svc.GetOpenIDConnectProvider(acct, &iam.GetOpenIDConnectProviderInput{
		OpenIDConnectProviderArn: created.OpenIDConnectProviderArn,
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if aws.StringValue(got.Url) != strings.TrimPrefix(testIssuer, "https://") {
		t.Fatalf("Url = %q, want %q", aws.StringValue(got.Url), strings.TrimPrefix(testIssuer, "https://"))
	}
	if len(got.ClientIDList) != 1 || aws.StringValue(got.ClientIDList[0]) != "sts.amazonaws.com" {
		t.Fatalf("ClientIDList = %v", aws.StringValueSlice(got.ClientIDList))
	}
}

func TestGetOpenIDConnectProvider_NotFound(t *testing.T) {
	svc := setupTestIAMService(t)
	_, err := svc.GetOpenIDConnectProvider("000000000001", &iam.GetOpenIDConnectProviderInput{
		OpenIDConnectProviderArn: aws.String(OIDCProviderARN("000000000001", "10.0.0.1:9999/oidc/eks/x/y/z")),
	})
	if err == nil || err.Error() != awserrors.ErrorIAMNoSuchEntity {
		t.Fatalf("err = %v, want NoSuchEntity", err)
	}
}

func TestListOpenIDConnectProviders(t *testing.T) {
	svc := setupTestIAMService(t)
	acct := "000000000001"

	// Empty account (bucket not yet created) lists nothing, not an error.
	empty, err := svc.ListOpenIDConnectProviders(acct, &iam.ListOpenIDConnectProvidersInput{})
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(empty.OpenIDConnectProviderList) != 0 {
		t.Fatalf("empty list = %d entries", len(empty.OpenIDConnectProviderList))
	}

	issuers := []string{testIssuer, "https://10.0.0.1:9999/oidc/eks/ap-southeast-2/000000000001/h2"}
	for _, iss := range issuers {
		if _, err := svc.CreateOpenIDConnectProvider(acct, &iam.CreateOpenIDConnectProviderInput{Url: aws.String(iss)}); err != nil {
			t.Fatalf("create %s: %v", iss, err)
		}
	}

	out, err := svc.ListOpenIDConnectProviders(acct, &iam.ListOpenIDConnectProvidersInput{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out.OpenIDConnectProviderList) != 2 {
		t.Fatalf("list = %d entries, want 2", len(out.OpenIDConnectProviderList))
	}
	want := map[string]bool{
		OIDCProviderARN(acct, strings.TrimPrefix(issuers[0], "https://")): true,
		OIDCProviderARN(acct, strings.TrimPrefix(issuers[1], "https://")): true,
	}
	for _, e := range out.OpenIDConnectProviderList {
		if !want[aws.StringValue(e.Arn)] {
			t.Fatalf("unexpected ARN %q", aws.StringValue(e.Arn))
		}
	}
}

func TestDeleteOpenIDConnectProvider(t *testing.T) {
	svc := setupTestIAMService(t)
	acct := "000000000001"
	created, err := svc.CreateOpenIDConnectProvider(acct, &iam.CreateOpenIDConnectProviderInput{Url: aws.String(testIssuer)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := svc.DeleteOpenIDConnectProvider(acct, &iam.DeleteOpenIDConnectProviderInput{
		OpenIDConnectProviderArn: created.OpenIDConnectProviderArn,
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Second delete is NoSuchEntity.
	_, err = svc.DeleteOpenIDConnectProvider(acct, &iam.DeleteOpenIDConnectProviderInput{
		OpenIDConnectProviderArn: created.OpenIDConnectProviderArn,
	})
	if err == nil || err.Error() != awserrors.ErrorIAMNoSuchEntity {
		t.Fatalf("second delete err = %v, want NoSuchEntity", err)
	}
}

func TestIssuerProviderARNRoundTrip(t *testing.T) {
	arn := OIDCProviderARN("000000000001", strings.TrimPrefix(testIssuer, "https://"))
	got, err := issuerFromOIDCProviderARN(arn)
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if got != testIssuer {
		t.Fatalf("round-trip issuer = %q, want %q", got, testIssuer)
	}
	if OIDCProviderKey(got) != OIDCProviderKey(testIssuer) {
		t.Fatal("round-trip key mismatch")
	}
}
