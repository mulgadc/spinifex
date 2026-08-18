package gateway

//test:in-package — the status gate is an unexported step of the auth
// middleware, and its cache is deliberately not part of the package's API.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
)

// statusGateRouter builds a signed-request harness whose only job is to report
// whether the auth middleware let the request through.
func statusGateRouter(t *testing.T, svc handlers_iam.IAMService) (chi.Router, *http.Request) {
	t.Helper()

	gw := &GatewayConfig{DisableLogging: true, Region: testRegion, IAMService: svc}
	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	signTestRequest(t, req, nil, testAccessKey, testSecretKey)
	return r, req
}

// statusGateService returns a mock holding one usable access key for account,
// with that account pinned to status.
func statusGateService(t *testing.T, accountID, status string) *mockIAMService {
	t.Helper()

	encryptedSecret, err := handlers_iam.EncryptSecret(testSecretKey, testMasterKey)
	if err != nil {
		t.Fatalf("encrypt secret: %v", err)
	}
	return &mockIAMService{
		masterKey: testMasterKey,
		accessKeys: map[string]*handlers_iam.AccessKey{
			testAccessKey: {
				AccessKeyID:     testAccessKey,
				SecretAccessKey: encryptedSecret,
				UserName:        "alice",
				AccountID:       accountID,
				Status:          handlers_iam.AccessKeyStatusActive,
			},
		},
		accounts: map[string]*handlers_iam.Account{
			accountID: {AccountID: accountID, Status: status},
		},
	}
}

func TestAuthAllowsAnActiveAccount(t *testing.T) {
	r, req := statusGateRouter(t, statusGateService(t, "123456789012", handlers_iam.AccountStatusActive))

	if resp := doRequest(r, req); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for an ACTIVE account, got %d", resp.StatusCode)
	}
}

// Until this gate existed, account status was enforced only in ECR's principal
// resolution, so suspending an account left EC2, S3 and STS wide open.
func TestAuthDeniesASuspendedAccount(t *testing.T) {
	r, req := statusGateRouter(t, statusGateService(t, "123456789012", handlers_iam.AccountStatusSuspended))

	if resp := doRequest(r, req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a SUSPENDED account, got %d", resp.StatusCode)
	}
}

// The quiesce that makes teardown converge: a terminating account must not be
// able to create resources the cascade has already walked past.
func TestAuthDeniesATerminatingAccount(t *testing.T) {
	r, req := statusGateRouter(t, statusGateService(t, "123456789012", handlers_iam.AccountStatusTerminating))

	if resp := doRequest(r, req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a TERMINATING account, got %d", resp.StatusCode)
	}
}

// unreadableAccountService answers key lookups but never account lookups.
type unreadableAccountService struct {
	*mockIAMService
}

func (s *unreadableAccountService) GetAccount(string) (*handlers_iam.Account, error) {
	return nil, errors.New("account store unavailable")
}

// An account we cannot read is not evidence of an account in good standing.
func TestAuthFailsClosedWhenTheAccountCannotBeRead(t *testing.T) {
	svc := &unreadableAccountService{statusGateService(t, "123456789012", handlers_iam.AccountStatusActive)}
	r, req := statusGateRouter(t, svc)

	if resp := doRequest(r, req); resp.StatusCode == http.StatusOK {
		t.Fatal("expected an unreadable account to be denied, got 200")
	}
}

func TestAccountStatusCacheOnlyRemembersActive(t *testing.T) {
	cache := newAccountStatusCache()
	now := time.Now()

	if cache.activeUntil("123456789012", now) {
		t.Fatal("an unseen account must not read as active")
	}

	cache.markActive("123456789012", now)
	if !cache.activeUntil("123456789012", now) {
		t.Fatal("a just-marked account must read as active")
	}
	if cache.activeUntil("123456789012", now.Add(accountActiveTTL+time.Second)) {
		t.Fatal("the entry must expire with the TTL")
	}

	cache.markActive("123456789012", now)
	cache.forget("123456789012")
	if cache.activeUntil("123456789012", now) {
		t.Fatal("forget must drop the entry so a status change takes effect at once")
	}
}
