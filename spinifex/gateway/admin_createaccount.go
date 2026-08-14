package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/mulgadc/spinifex/spinifex/kvutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go/jetstream"
)

// KVBucketAdminIdempotency holds per-client-token provisioning state so a lost
// response can be replayed. Entries carry the account's first secret access
// key, encrypted with the cluster master key, and age out with the bucket TTL.
const KVBucketAdminIdempotency = "spinifex-admin-idem"

// adminIdempotencyTTL bounds how long a client token can replay. Long enough to
// survive any realistic retry, short enough that an admin secret is not held at
// rest indefinitely to serve a hypothetical one.
const adminIdempotencyTTL = 24 * time.Hour

// ensureDefaultVpcTimeout bounds the wait for the daemon to build the account's
// default VPC. Exceeding it fails the call rather than returning credentials
// for an account that cannot launch anything.
const ensureDefaultVpcTimeout = 30 * time.Second

const (
	clientTokenMinLen = 32
	clientTokenMaxLen = 128
	accountNameMaxLen = 254
	sourceMaxLen      = 64
)

var clientTokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// CreateAccountRequest is the /admin/CreateAccount request body.
type CreateAccountRequest struct {
	Name        string `json:"name"`
	ClientToken string `json:"clientToken"`
	Source      string `json:"source,omitempty"`
}

// CreateAccountResponse is the /admin/CreateAccount success body. It is stored
// verbatim (with the secret encrypted) so a replay returns an identical answer.
type CreateAccountResponse struct {
	AccountID       string `json:"accountId"`
	AccountName     string `json:"accountName"`
	AdminUser       string `json:"adminUser"`
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	DefaultVpcID    string `json:"defaultVpcId"`
}

// adminIdemRecord is the stored provisioning state for one client token.
type adminIdemRecord struct {
	State string `json:"state"` // "in-progress" or "done"
	Name  string `json:"name"`
	// Response is populated when State is "done". SecretAccessKey inside it is
	// ciphertext, never plaintext.
	Response  *CreateAccountResponse `json:"response,omitempty"`
	StartedAt string                 `json:"started_at"`
}

const (
	adminIdemInProgress = "in-progress"
	adminIdemDone       = "done"
)

// adminCreateAccount provisions a tenant account and returns its first
// credentials.
//
// Correctness rests on two CAS creates rather than a transaction: the client
// token admits exactly one in-flight attempt, and the name reservation makes
// duplicate names impossible. Provisioning itself is resumable, so a failure
// anywhere is recovered by retrying with the same token.
func (gw *GatewayConfig) adminCreateAccount(ctx context.Context, body []byte) (any, error) {
	var req CreateAccountRequest
	if err := json.Unmarshal(body, &req); err != nil {
		slog.Debug("CreateAccount: malformed JSON body", "err", err)
		return nil, errors.New(awserrors.ErrorInvalidRequest)
	}
	if err := validateCreateAccountRequest(&req); err != nil {
		return nil, err
	}
	if gw.IAMService == nil {
		slog.Error("CreateAccount: IAM service not available")
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	names, idem, err := gw.adminStores(ctx)
	if err != nil {
		return nil, err
	}

	// Claim the token first: it serialises every check below, so the account
	// cap and the name scan cannot race a concurrent duplicate request.
	replay, err := claimClientToken(ctx, idem, req.ClientToken, req.Name)
	if err != nil {
		return nil, err
	}
	if replay != nil {
		secret, err := gw.IAMService.DecryptSecret(replay.SecretAccessKey)
		if err != nil {
			slog.Error("CreateAccount: cannot decrypt replayed secret", "accountID", replay.AccountID, "err", err)
			return nil, errors.New(awserrors.ErrorInternalError)
		}
		out := *replay
		out.SecretAccessKey = secret
		slog.Info("CreateAccount: replayed stored result", "accountID", out.AccountID)
		return &out, nil
	}

	response, err := gw.provisionNewAccount(ctx, names, &req)
	if err != nil {
		// Drop the token claim so the caller can retry rather than being told
		// OperationInProgress until the TTL expires.
		if delErr := idem.Delete(ctx, clientTokenKey(req.ClientToken)); delErr != nil {
			slog.Error("CreateAccount: failed to release client token after error",
				"err", delErr, "cause", err)
		}
		return nil, err
	}

	stored := *response
	ciphertext, err := gw.IAMService.EncryptSecret(response.SecretAccessKey)
	if err != nil {
		slog.Error("CreateAccount: failed to encrypt secret for replay", "accountID", response.AccountID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	stored.SecretAccessKey = ciphertext

	record, err := json.Marshal(adminIdemRecord{
		State:     adminIdemDone,
		Name:      handlers_iam.NormalizeAccountName(req.Name),
		Response:  &stored,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		slog.Error("CreateAccount: failed to marshal idempotency record", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	if _, err := idem.Put(ctx, clientTokenKey(req.ClientToken), record); err != nil {
		// The account exists and is usable; only replay is lost. Returning an
		// error here would strand credentials the caller never receives.
		slog.Error("CreateAccount: failed to store idempotency record — replay unavailable",
			"accountID", response.AccountID, "err", err)
	}

	return response, nil
}

// provisionNewAccount reserves the name, creates the account, provisions its
// admin identity and waits for the default VPC.
func (gw *GatewayConfig) provisionNewAccount(
	ctx context.Context,
	names *handlers_iam.AccountNameIndex,
	req *CreateAccountRequest,
) (*CreateAccountResponse, error) {
	// Accounts that predate the reservation index have no entry to collide
	// with, so they are found by name scan. The same listing bounds the cap.
	existing, err := handlers_iam.FindAccountByName(gw.IAMService, req.Name)
	if err != nil {
		slog.Error("CreateAccount: failed to list accounts", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}
	if existing != nil {
		return nil, errors.New(awserrors.ErrorAccountAlreadyExists)
	}
	if err := gw.checkAccountCap(); err != nil {
		return nil, err
	}

	switch err := names.Reserve(ctx, req.Name, req.ClientToken); {
	case errors.Is(err, handlers_iam.ErrNameTaken):
		return nil, errors.New(awserrors.ErrorAccountAlreadyExists)
	case errors.Is(err, handlers_iam.ErrNameInFlight):
		return nil, errors.New(awserrors.ErrorOperationInProgress)
	case err != nil:
		slog.Error("CreateAccount: failed to reserve account name", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	account, err := gw.IAMService.CreateAccount(handlers_iam.NormalizeAccountName(req.Name))
	if err != nil {
		if relErr := names.Release(ctx, req.Name, req.ClientToken); relErr != nil {
			slog.Error("CreateAccount: failed to release name reservation", "err", relErr)
		}
		slog.Error("CreateAccount: failed to create account", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	// From here the account exists and cannot be deleted, so every failure
	// leaves the reservation in place: a retry with the same token resumes it
	// rather than allocating a second account for the same name.
	if err := names.Commit(ctx, req.Name, account.AccountID, req.ClientToken); err != nil {
		slog.Error("CreateAccount: failed to commit name reservation",
			"accountID", account.AccountID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	provisioned, err := handlers_iam.ProvisionAccount(gw.IAMService, account.AccountID, account.AccountName)
	if err != nil {
		slog.Error("CreateAccount: failed to provision account",
			"accountID", account.AccountID, "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	vpcID, err := gw.ensureDefaultVPC(ctx, account.AccountID)
	if err != nil {
		slog.Error("CreateAccount: default VPC not ready",
			"accountID", account.AccountID, "err", err)
		return nil, errors.New(awserrors.ErrorServiceUnavailable)
	}

	slog.Info("CreateAccount: account created",
		"accountID", account.AccountID, "source", req.Source, "defaultVpcID", vpcID)

	return &CreateAccountResponse{
		AccountID:       provisioned.AccountID,
		AccountName:     provisioned.AccountName,
		AdminUser:       provisioned.AdminUser,
		AccessKeyID:     provisioned.AccessKeyID,
		SecretAccessKey: provisioned.SecretAccessKey,
		DefaultVpcID:    vpcID,
	}, nil
}

// claimClientToken CAS-creates the in-progress record. It returns a non-nil
// response when the token has already completed, meaning the caller should
// replay it rather than provision again.
func claimClientToken(ctx context.Context, idem jetstream.KeyValue, clientToken, name string) (*CreateAccountResponse, error) {
	normalized := handlers_iam.NormalizeAccountName(name)
	claim, err := json.Marshal(adminIdemRecord{
		State:     adminIdemInProgress,
		Name:      normalized,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		slog.Error("CreateAccount: failed to marshal token claim", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	if _, err := idem.Create(ctx, clientTokenKey(clientToken), claim); err == nil {
		return nil, nil
	} else if !errors.Is(err, jetstream.ErrKeyExists) {
		slog.Error("CreateAccount: failed to claim client token", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	entry, err := idem.Get(ctx, clientTokenKey(clientToken))
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			// Expired between Create and Get. Retryable rather than a fault.
			return nil, errors.New(awserrors.ErrorOperationInProgress)
		}
		slog.Error("CreateAccount: failed to read client token record", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	var record adminIdemRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		slog.Error("CreateAccount: failed to unmarshal client token record", "err", err)
		return nil, errors.New(awserrors.ErrorInternalError)
	}

	// A token bound to a different name is a client bug, not a retry.
	if record.Name != normalized {
		return nil, errors.New(awserrors.ErrorIdempotentParameterMismatch)
	}
	if record.State != adminIdemDone || record.Response == nil {
		return nil, errors.New(awserrors.ErrorOperationInProgress)
	}
	return record.Response, nil
}

// checkAccountCap enforces [signup] max_accounts. Zero or absent means no cap,
// which is the pre-existing behaviour for a cluster that never opted in.
func (gw *GatewayConfig) checkAccountCap() error {
	if gw.SignupMaxAccounts <= 0 {
		return nil
	}
	accounts, err := gw.IAMService.ListAccounts()
	if err != nil {
		slog.Error("CreateAccount: failed to count accounts for cap", "err", err)
		return errors.New(awserrors.ErrorInternalError)
	}
	if len(accounts) >= gw.SignupMaxAccounts {
		slog.Warn("CreateAccount: account cap reached",
			"accounts", len(accounts), "maxAccounts", gw.SignupMaxAccounts)
		return errors.New(awserrors.ErrorIAMLimitExceeded)
	}
	return nil
}

// ensureDefaultVPC asks the daemon to build the account's default VPC and waits
// for the acknowledgement, so the response never promises an account that
// cannot launch anything.
func (gw *GatewayConfig) ensureDefaultVPC(ctx context.Context, accountID string) (string, error) {
	payload, err := json.Marshal(struct {
		AccountID string `json:"account_id"`
	}{AccountID: accountID})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, ensureDefaultVpcTimeout)
	defer cancel()

	msg, err := gw.NATSConn.RequestWithContext(reqCtx, utils.SubjectEnsureDefaultVpc, payload)
	if err != nil {
		return "", fmt.Errorf("request default VPC: %w", err)
	}

	var reply struct {
		VpcID string `json:"vpc_id"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(msg.Data, &reply); err != nil {
		return "", fmt.Errorf("unmarshal reply: %w", err)
	}
	if reply.Error != "" {
		return "", errors.New(reply.Error)
	}
	if reply.VpcID == "" {
		return "", errors.New("daemon returned no VPC ID")
	}
	return reply.VpcID, nil
}

// adminStores opens the name index and the idempotency bucket. Both are opened
// per request rather than cached on the gateway so a bucket created after
// startup (a cluster upgraded into this feature) is picked up without a restart.
func (gw *GatewayConfig) adminStores(ctx context.Context) (*handlers_iam.AccountNameIndex, jetstream.KeyValue, error) {
	js, err := jetstream.New(gw.NATSConn)
	if err != nil {
		slog.Error("CreateAccount: failed to get JetStream context", "err", err)
		return nil, nil, errors.New(awserrors.ErrorServiceUnavailable)
	}

	names, err := handlers_iam.NewAccountNameIndex(ctx, js)
	if err != nil {
		slog.Error("CreateAccount: failed to open account name index", "err", err)
		return nil, nil, errors.New(awserrors.ErrorServiceUnavailable)
	}

	idem, err := kvutil.GetOrCreateBucketWithTTL(ctx, js, KVBucketAdminIdempotency, 1, adminIdempotencyTTL)
	if err != nil {
		slog.Error("CreateAccount: failed to open idempotency bucket", "err", err)
		return nil, nil, errors.New(awserrors.ErrorServiceUnavailable)
	}
	return names, idem, nil
}

// clientTokenKey hashes the token so the raw value never becomes a KV key name.
func clientTokenKey(clientToken string) string {
	sum := sha256.Sum256([]byte(clientToken))
	return hex.EncodeToString(sum[:])
}

// validateCreateAccountRequest rejects malformed input. Messages name the
// offending field but never echo its value, which for `name` is a customer
// email address.
func validateCreateAccountRequest(req *CreateAccountRequest) error {
	req.Name = strings.TrimSpace(req.Name)
	req.ClientToken = strings.TrimSpace(req.ClientToken)

	if req.Name == "" || req.ClientToken == "" {
		return errors.New(awserrors.ErrorMissingParameter)
	}
	if len(req.Name) > accountNameMaxLen || !validEmail(req.Name) {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if len(req.ClientToken) < clientTokenMinLen || len(req.ClientToken) > clientTokenMaxLen ||
		!clientTokenRE.MatchString(req.ClientToken) {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if len(req.Source) > sourceMaxLen {
		return errors.New(awserrors.ErrorInvalidParameterValue)
	}
	return nil
}

// validEmail is a deliberately loose structural check: exactly one @, a
// non-empty local part, and a dotted domain. Deliverability is proven by the
// verification code the website sends, not by this.
func validEmail(name string) bool {
	local, domain, ok := strings.Cut(name, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") {
		return false
	}
	if strings.ContainsAny(name, " \t\r\n") {
		return false
	}
	dot := strings.LastIndex(domain, ".")
	return dot > 0 && dot < len(domain)-1
}
