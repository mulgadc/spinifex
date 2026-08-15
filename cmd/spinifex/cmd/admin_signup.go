package cmd

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/internal/gwsign"
	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// SignupPrincipalUserName is the IAM user in the super-admin account that the
// website's signup Worker signs with. It is deliberately separate from the
// operator's own admin key so revoking it costs nothing else.
const SignupPrincipalUserName = "signup"

// signupPrincipalPolicyName is the inline policy attached to that user.
const signupPrincipalPolicyName = "signup-createaccount"

// signupPrincipalPolicyDocument grants exactly one action. Widening it turns
// the credential into an account-existence oracle for arbitrary email
// addresses, so it is written out here rather than composed from a list.
const signupPrincipalPolicyDocument = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"spinifex:CreateAccount","Resource":"*"}]}`

// adminRequestTimeout bounds a remote /admin call. Provisioning waits on the
// default VPC, so it is comfortably longer than the gateway's own wait.
const adminRequestTimeout = 90 * time.Second

// adminMaxResponseBytes caps the response read. Most answers are a few hundred
// bytes; a teardown inventory is one line per resource, so the cap is sized for
// a large account rather than for the common case.
const adminMaxResponseBytes = 1 << 20

// retryableAdminErrors are the only codes worth retrying, and only with the
// same client token. Suggesting a retry for the rest invites a caller to send
// a fresh token, which is what produces a duplicate account.
var retryableAdminErrors = map[string]bool{
	awserrors.ErrorOperationInProgress: true,
	awserrors.ErrorServiceUnavailable:  true,
	awserrors.ErrorInternalError:       true,
}

var signupPrincipalCmd = &cobra.Command{
	Use:   "signup-principal",
	Short: "Manage the credential the public signup form uses",
	Long: `Manage the IAM user in the super-admin account that mulgadc.com/signup signs
its POST /admin/CreateAccount requests with.`,
}

var signupPrincipalCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create the signup principal and print its access key once",
	Long: `Create the "signup" IAM user in the super-admin account with an inline policy
allowing exactly spinifex:CreateAccount, then mint an access key.

The secret is printed once and never recoverable. Store it as a Cloudflare
Worker secret; never commit it and never place it in a Spinifex config file.

Re-running replaces the access key, which revokes the previous one.`,
	Run: runSignupPrincipalCreate,
}

var signupPrincipalAuditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Flag roles the signup credential could assume",
	Long: `Report roles in the super-admin account whose trust policy names the account,
its root ARN, or a wildcard.

STS does not evaluate the caller's identity policy on AssumeRole, so any such
role is assumable by the signup credential, which then inherits its
permissions. Roles trusting a service principal are unaffected.

Exits non-zero if any role is flagged.`,
	RunE:         runSignupPrincipalAudit,
	SilenceUsage: true,
}

func init() {
	adminCmd.AddCommand(signupPrincipalCmd)
	signupPrincipalCmd.AddCommand(signupPrincipalCreateCmd)
	signupPrincipalCmd.AddCommand(signupPrincipalAuditCmd)

	accountCreateCmd.Flags().Bool("remote", false, "Create the account over POST /admin/CreateAccount instead of connecting to NATS")
	accountCreateCmd.Flags().String("endpoint", "", "Gateway endpoint for --remote (default: this node's AWS gateway)")
	accountCreateCmd.Flags().String("region", "", "SigV4 region for --remote (default: this node's region)")
	accountCreateCmd.Flags().String("ca-bundle", "", "CA certificate for --remote (default: this node's CA)")
	accountCreateCmd.Flags().String("client-token", "", "Idempotency token for --remote (default: generated; reuse it to retry)")
	accountCreateCmd.Flags().String("source", "spx-cli", "Provenance tag recorded in the gateway log for --remote")

	accountListCmd.Flags().Bool("remote", false, "List over POST /admin/ListAccounts instead of connecting to NATS")
	accountListCmd.Flags().String("endpoint", "", "Gateway endpoint for --remote (default: this node's AWS gateway)")
	accountListCmd.Flags().String("region", "", "SigV4 region for --remote (default: this node's region)")
	accountListCmd.Flags().String("ca-bundle", "", "CA certificate for --remote (default: this node's CA)")
}

// runSignupPrincipalCreate provisions the signup principal in the super-admin
// account. Every step is create-if-absent except the access key, which is
// replaced so exactly one key is ever live.
func runSignupPrincipalCreate(cmd *cobra.Command, args []string) {
	svc, _, _, cleanup, err := initIAMServiceFromConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	defer cleanup()

	accountID := admin.DefaultAccountID()

	if _, err := svc.CreateUser(accountID, &iam.CreateUserInput{
		UserName: aws.String(SignupPrincipalUserName),
	}); err != nil && !strings.Contains(err.Error(), "EntityAlreadyExists") {
		fmt.Fprintf(os.Stderr, "Error creating signup user: %v\n", err)
		os.Exit(1)
	}

	if _, err := svc.PutUserPolicy(accountID, &iam.PutUserPolicyInput{
		UserName:       aws.String(SignupPrincipalUserName),
		PolicyName:     aws.String(signupPrincipalPolicyName),
		PolicyDocument: aws.String(signupPrincipalPolicyDocument),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Error attaching signup policy: %v\n", err)
		os.Exit(1)
	}

	// Any key from a previous run is unrecoverable to its holder but still
	// authenticates, so it is removed rather than left live alongside the new one.
	listed, err := svc.ListAccessKeys(accountID, &iam.ListAccessKeysInput{
		UserName: aws.String(SignupPrincipalUserName),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing signup access keys: %v\n", err)
		os.Exit(1)
	}
	if listed != nil {
		for _, meta := range listed.AccessKeyMetadata {
			if meta == nil || meta.AccessKeyId == nil {
				continue
			}
			if _, err := svc.DeleteAccessKey(accountID, &iam.DeleteAccessKeyInput{
				UserName:    aws.String(SignupPrincipalUserName),
				AccessKeyId: meta.AccessKeyId,
			}); err != nil {
				fmt.Fprintf(os.Stderr, "Error removing previous signup access key: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Revoked previous access key %s\n", aws.StringValue(meta.AccessKeyId))
		}
	}

	akOut, err := svc.CreateAccessKey(accountID, &iam.CreateAccessKeyInput{
		UserName: aws.String(SignupPrincipalUserName),
	})
	if err != nil || akOut == nil || akOut.AccessKey == nil {
		fmt.Fprintf(os.Stderr, "Error creating signup access key: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\nSignup principal ready.")
	fmt.Printf("  Account ID:        %s\n", accountID)
	fmt.Printf("  User:              %s\n", SignupPrincipalUserName)
	fmt.Printf("  Permitted action:  spinifex:CreateAccount\n")
	fmt.Printf("  Access Key ID:     %s\n", aws.StringValue(akOut.AccessKey.AccessKeyId))
	fmt.Printf("  Secret Access Key: %s\n", aws.StringValue(akOut.AccessKey.SecretAccessKey))
	fmt.Println("\nThe secret is shown once. Store it as a Cloudflare Worker secret:")
	fmt.Println("  wrangler secret put SPX_SIGNUP_ACCESS_KEY_ID")
	fmt.Println("  wrangler secret put SPX_SIGNUP_SECRET_ACCESS_KEY")
}

// runSignupPrincipalAudit reports roles in the super-admin account that any
// principal in the account can assume, which today includes the signup user.
func runSignupPrincipalAudit(_ *cobra.Command, _ []string) error {
	svc, _, _, cleanup, err := initIAMServiceFromConfig()
	if err != nil {
		return err
	}
	defer cleanup()

	accountID := admin.DefaultAccountID()
	roles, err := svc.ListRoles(accountID, &iam.ListRolesInput{})
	if err != nil {
		return fmt.Errorf("list roles: %w", err)
	}

	var flagged []string
	for _, role := range roles.Roles {
		if role == nil || role.AssumeRolePolicyDocument == nil {
			continue
		}
		if trustsWholeAccount(aws.StringValue(role.AssumeRolePolicyDocument), accountID) {
			flagged = append(flagged, aws.StringValue(role.RoleName))
		}
	}

	if len(flagged) == 0 {
		fmt.Printf("No roles in %s trust the account as a whole.\n", accountID)
		return nil
	}

	fmt.Fprintf(os.Stderr, "Roles in %s assumable by any principal in the account, including the signup credential:\n", accountID)
	for _, name := range flagged {
		fmt.Fprintf(os.Stderr, "  %s\n", name)
	}
	return fmt.Errorf("%d role(s) trust the account as a whole — scope their trust policies to a specific principal", len(flagged))
}

// trustsWholeAccount reports whether an AssumeRolePolicyDocument admits every
// principal in accountID. A malformed document is reported as trusting, since
// a document nobody can parse is not a document anybody has verified.
func trustsWholeAccount(document, accountID string) bool {
	doc, err := handlers_iam.ValidateTrustPolicyDocument(document)
	if err != nil {
		return true
	}
	for _, stmt := range doc.Statement {
		if stmt.Effect != "Allow" {
			continue
		}
		var principal struct {
			AWS handlers_iam.StringOrArr `json:"AWS"`
		}
		if err := json.Unmarshal(stmt.Principal, &principal); err != nil {
			continue
		}
		for _, entry := range principal.AWS {
			switch entry {
			case "*", accountID, "arn:aws:iam::" + accountID + ":root":
				return true
			}
		}
	}
	return false
}

// runAccountCreateRemote drives POST /admin/CreateAccount, exercising the same
// path the signup Worker uses. Credentials come from the standard AWS chain
// (env vars or AWS_PROFILE), so this never reads the cluster master key.
func runAccountCreateRemote(cmd *cobra.Command, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), adminRequestTimeout)
	defer cancel()

	clientToken, _ := cmd.Flags().GetString("client-token")
	source, _ := cmd.Flags().GetString("source")

	target, err := resolveAdminTarget(cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if clientToken == "" {
		clientToken = newClientToken()
	}

	out, err := createAccountRemote(ctx, target,
		gateway.CreateAccountRequest{Name: name, ClientToken: clientToken, Source: source})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		var adminErr *adminError
		if errors.As(err, &adminErr) && retryableAdminErrors[adminErr.Code] {
			fmt.Fprintf(os.Stderr, "Retry with --client-token %s to resume; a new token would create a second account.\n", clientToken)
		}
		os.Exit(1)
	}

	fmt.Println("\nAccount created successfully!")
	fmt.Printf("  Account ID:        %s\n", out.AccountID)
	fmt.Printf("  Account Name:      %s\n", out.AccountName)
	fmt.Printf("  Admin User:        %s\n", out.AdminUser)
	fmt.Printf("  Access Key ID:     %s\n", out.AccessKeyID)
	fmt.Printf("  Secret Access Key: %s\n", out.SecretAccessKey)
	fmt.Printf("  Default VPC:       %s\n", out.DefaultVpcID)
	fmt.Printf("  Client Token:      %s\n", clientToken)
}

// adminTarget is where a remote /admin call goes and how it is signed.
type adminTarget struct {
	endpoint string
	region   string
	caBundle string
}

// resolveAdminTarget fills the endpoint, region and CA from this node's config
// when the command runs on a cluster member. Off-cluster callers pass flags,
// which always win over what the local config says.
func resolveAdminTarget(cmd *cobra.Command) (adminTarget, error) {
	endpoint, _ := cmd.Flags().GetString("endpoint")
	region, _ := cmd.Flags().GetString("region")
	caBundle, _ := cmd.Flags().GetString("ca-bundle")

	if endpoint == "" || region == "" || caBundle == "" {
		if cfg, err := loadLocalConfig(); err == nil {
			node := cfg.Nodes[cfg.Node]
			if endpoint == "" {
				endpoint = localGatewayEndpoint(node)
			}
			if region == "" {
				region = node.Region
			}
			if caBundle == "" {
				caBundle = filepath.Join(cfg.NodeBaseDir(), "config", "ca.pem")
			}
		}
	}
	if endpoint == "" || region == "" {
		return adminTarget{}, errors.New("--endpoint and --region are required when no local node config is available")
	}
	return adminTarget{endpoint: endpoint, region: region, caBundle: caBundle}, nil
}

// adminError is a structured error from the /admin surface. The code is
// carried separately from the message so the caller can decide whether a
// retry is safe without matching on text.
type adminError struct {
	Code       string
	Message    string
	RequestID  string
	StatusCode int
}

func (e *adminError) Error() string {
	return fmt.Sprintf("%s: %s (HTTP %d, requestId %s)", e.Code, e.Message, e.StatusCode, e.RequestID)
}

// createAccountRemote signs and sends POST /admin/CreateAccount. Credentials
// come from the standard AWS chain, so this never reads the cluster master
// key — it is the same request the signup Worker makes.
func createAccountRemote(ctx context.Context, target adminTarget, req gateway.CreateAccountRequest) (*gateway.CreateAccountResponse, error) {
	var out gateway.CreateAccountResponse
	if err := callAdmin(ctx, target, "CreateAccount", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// callAdmin signs and sends one POST /admin/<method>, decoding the success body
// into out. Credentials come from the standard AWS chain, so this never reads
// the cluster master key.
func callAdmin(ctx context.Context, target adminTarget, method string, req, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(target.endpoint, "/")+"/admin/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	signer, err := gwsign.NewIMDS(ctx, target.region)
	if err != nil {
		return fmt.Errorf("resolve AWS credentials: %w", err)
	}
	sum := sha256.Sum256(body)
	if err := signer.Sign(httpReq, hex.EncodeToString(sum[:]), "spinifex", target.region); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	client, err := adminHTTPClient(target.caBundle)
	if err != nil {
		return err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("call %s: %w", target.endpoint, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, adminMaxResponseBytes))
	if err != nil {
		return fmt.Errorf("gateway returned HTTP %d with an unreadable body: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		return decodeAdminError(resp.StatusCode, payload)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// decodeAdminError turns a non-200 into an adminError. A body that is not the
// JSON envelope is reported verbatim rather than as a decode failure, since
// then the response came from something other than the gateway.
func decodeAdminError(statusCode int, payload []byte) error {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(payload, &body); err != nil || body.Error.Code == "" {
		return fmt.Errorf("gateway returned HTTP %d: %s", statusCode, payload)
	}
	return &adminError{
		Code:       body.Error.Code,
		Message:    body.Error.Message,
		RequestID:  body.RequestID,
		StatusCode: statusCode,
	}
}

// newClientToken returns a fresh idempotency token in the character set the
// endpoint accepts.
func newClientToken() string {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail on any supported platform; if it does, a
		// weaker token would silently break idempotency.
		fmt.Fprintf(os.Stderr, "Error generating client token: %v\n", err)
		os.Exit(1)
	}
	return hex.EncodeToString(buf[:])
}

// adminHTTPClient trusts caBundle in addition to the system roots so the same
// command works against a cluster's self-signed CA and against a public
// certificate. An unreadable bundle is an error, never a silent downgrade.
func adminHTTPClient(caBundle string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caBundle != "" {
		pem, err := os.ReadFile(caBundle)
		if err != nil {
			return nil, fmt.Errorf("read CA bundle %s: %w", caBundle, err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA bundle %s contains no certificates", caBundle)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: transport}, nil
}

// loadLocalConfig reads this node's cluster config without connecting to NATS.
func loadLocalConfig() (*config.ClusterConfig, error) {
	cfgPath := viper.GetString("config")
	if cfgPath == "" {
		cfgPath = DefaultConfigFile()
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if nodeConfig, ok := cfg.Nodes[cfg.Node]; ok && nodeConfig.BaseDir == "" {
		if isProductionLayout() {
			nodeConfig.BaseDir = DefaultDataDir()
		} else {
			nodeConfig.BaseDir = filepath.Dir(filepath.Dir(cfgPath))
		}
		cfg.Nodes[cfg.Node] = nodeConfig
	}
	return cfg, nil
}

// localGatewayEndpoint is this node's AWS gateway URL. A wildcard bind address
// is not dialable, so it resolves to localhost.
func localGatewayEndpoint(node config.Config) string {
	host, port, err := net.SplitHostPort(node.AWSGW.Host)
	if err != nil {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "https://" + net.JoinHostPort(host, port)
}
