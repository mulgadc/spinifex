package gwsign

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/mulgadc/predastore/pkg/sigv4"
)

// failingProvider models a cold IMDS datapath: Retrieve always errors, as the
// SDK's IMDS client does before the per-tap datapath is up.
type failingProvider struct{ err error }

func (p failingProvider) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{}, p.err
}

const (
	testAccessKey = "AKIAIOSFODNN7EXAMPLE"
	testSecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	testRegion    = "us-east-1"
	testService   = "elasticloadbalancing"
)

// TestSign_StaticVerifies confirms a statically-keyed Signer produces a request
// the gateway's SigV4 verifier accepts, with X-Amz-Content-Sha256 set to the
// body hash.
func TestSign_StaticVerifies(t *testing.T) {
	body := []byte("Action=LBAgentHeartbeat")
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])

	var verifyErr error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// sigv4.Parse reads and hashes the body itself, so no explicit body hash.
		sr, err := sigv4.Parse(r)
		if err != nil {
			verifyErr = err
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, verifyErr = sr.Verify(testSecretKey, testRegion, testService)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(string(body)))
	if err := NewStatic(testAccessKey, testSecretKey).Sign(req, payloadHash, testService, testRegion); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if got := req.Header.Get("X-Amz-Content-Sha256"); got != payloadHash {
		t.Fatalf("X-Amz-Content-Sha256 = %q, want %q", got, payloadHash)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if verifyErr != nil {
		t.Fatalf("server-side verify failed: %v", verifyErr)
	}
}

// TestSign_SessionTokenHeader confirms temporary credentials surface as a signed
// X-Amz-Security-Token header — the path the gateway uses to verify ASIA
// instance-role credentials.
func TestSign_SessionTokenHeader(t *testing.T) {
	const token = "FwoGZXIvYXdzEXAMPLEsessiontoken"
	s := &Signer{provider: credentials.NewStaticCredentialsProvider(testAccessKey, testSecretKey, token)}

	body := []byte("Action=LBAgentHeartbeat")
	sum := sha256.Sum256(body)
	req, _ := http.NewRequest(http.MethodPost, "https://gw:9999/", strings.NewReader(string(body)))
	if err := s.Sign(req, hex.EncodeToString(sum[:]), testService, testRegion); err != nil {
		t.Fatalf("Sign: %v", err)
	}

	if got := req.Header.Get("X-Amz-Security-Token"); got != token {
		t.Fatalf("X-Amz-Security-Token = %q, want %q", got, token)
	}
	authHdr := req.Header.Get("Authorization")
	if !strings.Contains(authHdr, "x-amz-security-token") {
		t.Fatalf("session token not in SignedHeaders: %q", authHdr)
	}
}

// TestEnsureCredentials confirms a resolvable provider warms clean while a cold
// datapath surfaces its retrieve error (the signal a warm-up loop retries on).
func TestEnsureCredentials(t *testing.T) {
	ok := &Signer{provider: credentials.NewStaticCredentialsProvider(testAccessKey, testSecretKey, "")}
	if err := ok.EnsureCredentials(t.Context()); err != nil {
		t.Fatalf("EnsureCredentials(static) = %v, want nil", err)
	}

	coldErr := errors.New("no EC2 IMDS role found")
	cold := &Signer{provider: failingProvider{err: coldErr}}
	err := cold.EnsureCredentials(t.Context())
	if err == nil {
		t.Fatal("EnsureCredentials(cold) = nil, want error")
	}
	if !errors.Is(err, coldErr) {
		t.Fatalf("EnsureCredentials(cold) = %v, want wrap of %v", err, coldErr)
	}
}

// TestNewIMDS_BuildsProvider confirms the IMDS constructor resolves a provider
// without performing network I/O (the chain retrieves lazily at first sign).
func TestNewIMDS_BuildsProvider(t *testing.T) {
	s, err := NewIMDS(t.Context(), testRegion)
	if err != nil {
		t.Fatalf("NewIMDS: %v", err)
	}
	if s.provider == nil {
		t.Fatal("expected non-nil credentials provider")
	}
}
