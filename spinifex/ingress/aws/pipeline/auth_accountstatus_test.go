package pipeline

//test:in-package — the status gate is an unexported step of the auth
// middleware, and its cache is deliberately not part of the package's API.

import (
	"errors"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
)

// statusGateRouter builds a signed-request harness whose only job is to report
// whether the auth middleware let the request through.
func statusGateRouter(t *testing.T, svc iamdomain.IAMService) (chi.Router, *http.Request) {
	t.Helper()

	gw := withEC2(withRealAuth(gwfix.WithDeps(&GatewayConfig{DisableLogging: true, Region: gwfix.Region}, gwfix.Deps{IAM: svc})))
	r := chi.NewRouter()
	r.Use(gw.SigV4AuthMiddleware())
	r.HandleFunc("/*", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "localhost:9999"
	gwfix.SignRequest(t, req, nil, gwfix.AccessKey, gwfix.SecretKey)
	return r, req
}

// statusGateService returns a mock holding one usable access key for account,
// with that account pinned to status.
func statusGateService(t *testing.T, accountID, status string) *gwfix.IAM {
	t.Helper()

	encryptedSecret, err := iamdomain.EncryptSecret(gwfix.SecretKey, gwfix.MasterKey)
	if err != nil {
		t.Fatalf("encrypt secret: %v", err)
	}
	return &gwfix.IAM{
		MasterKey: gwfix.MasterKey,
		AccessKeys: map[string]*iamdomain.AccessKey{
			gwfix.AccessKey: {
				AccessKeyID:     gwfix.AccessKey,
				SecretAccessKey: encryptedSecret,
				UserName:        "alice",
				AccountID:       accountID,
				Status:          iamdomain.AccessKeyStatusActive,
			},
		},
		Accounts: map[string]*iamdomain.Account{
			accountID: {AccountID: accountID, Status: status},
		},
	}
}

func TestAuthAllowsAnActiveAccount(t *testing.T) {
	r, req := statusGateRouter(t, statusGateService(t, "123456789012", iamdomain.AccountStatusActive))

	if resp := doRequest(r, req); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for an ACTIVE account, got %d", resp.StatusCode)
	}
}

// The quiesce that makes teardown converge: a terminating account must not be
// able to create resources the cascade has already walked past.
func TestAuthDeniesATerminatingAccount(t *testing.T) {
	r, req := statusGateRouter(t, statusGateService(t, "123456789012", iamdomain.AccountStatusTerminating))

	if resp := doRequest(r, req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a TERMINATING account, got %d", resp.StatusCode)
	}
}

// unreadableAccountService answers key lookups but never account lookups.
type unreadableAccountService struct {
	*gwfix.IAM
}

func (s *unreadableAccountService) GetAccount(string) (*iamdomain.Account, error) {
	return nil, errors.New("account store unavailable")
}

// An account we cannot read is not evidence of an account in good standing.
func TestAuthFailsClosedWhenTheAccountCannotBeRead(t *testing.T) {
	svc := &unreadableAccountService{statusGateService(t, "123456789012", iamdomain.AccountStatusActive)}
	r, req := statusGateRouter(t, svc)

	if resp := doRequest(r, req); resp.StatusCode == http.StatusOK {
		t.Fatal("expected an unreadable account to be denied, got 200")
	}
}
