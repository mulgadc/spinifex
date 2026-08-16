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

	"github.com/mulgadc/spinifex/internal/gwsign"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

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

func init() {
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

// runAccountCreateRemote drives POST /admin/CreateAccount, exercising the same
// path a self-service signup form uses. Credentials come from the standard AWS
// chain (env vars or AWS_PROFILE), so this never reads the cluster master key.
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
// key — it is the same request a self-service signup form makes.
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
