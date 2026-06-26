package stsauth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// assumeRoleXML is a minimal AWS AssumeRole response the aws-sdk STS client
// decodes into Credentials.
const assumeRoleXML = `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <AssumeRoleResult>
    <Credentials>
      <AccessKeyId>ASIATESTKEY</AccessKeyId>
      <SecretAccessKey>testsecret</SecretAccessKey>
      <SessionToken>testtoken</SessionToken>
      <Expiration>2031-01-02T03:04:05Z</Expiration>
    </Credentials>
  </AssumeRoleResult>
</AssumeRoleResponse>`

func TestAssumeRole_DecodesCredentials(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/xml")
		_, _ = w.Write([]byte(assumeRoleXML))
	}))
	defer srv.Close()

	creds, err := AssumeRole("us-east-1", srv.URL, srv.Client(),
		"AKIAINSTANCE", "instancesecret", "", "arn:aws:iam::111122223333:role/task", "ecs-cred-1")
	if err != nil {
		t.Fatalf("AssumeRole: %v", err)
	}
	if creds.AccessKeyID != "ASIATESTKEY" || creds.SecretAccessKey != "testsecret" || creds.SessionToken != "testtoken" {
		t.Errorf("bad creds: %+v", creds)
	}
	want, _ := time.Parse(time.RFC3339, "2031-01-02T03:04:05Z")
	if !creds.Expiration.Equal(want) {
		t.Errorf("Expiration = %v, want %v", creds.Expiration, want)
	}
	// The request must carry the AssumeRole action and the role ARN we asked for.
	if !strings.Contains(gotBody, "Action=AssumeRole") {
		t.Errorf("request not an AssumeRole call: %s", gotBody)
	}
	if !strings.Contains(gotBody, "role%2Ftask") && !strings.Contains(gotBody, "role/task") {
		t.Errorf("request missing role ARN: %s", gotBody)
	}
}

func TestAssumeRole_RequiresRoleAndSession(t *testing.T) {
	if _, err := AssumeRole("us-east-1", "https://gw", http.DefaultClient, "a", "b", "", "", "sess"); err == nil {
		t.Error("want error for empty roleARN")
	}
	if _, err := AssumeRole("us-east-1", "https://gw", http.DefaultClient, "a", "b", "", "arn:role", ""); err == nil {
		t.Error("want error for empty sessionName")
	}
}
