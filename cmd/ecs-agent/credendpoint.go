package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mulgadc/spinifex/cmd/ecs-agent/credentials"
	"github.com/mulgadc/spinifex/internal/ecrauth"
	"github.com/mulgadc/spinifex/internal/stsauth"
)

const (
	// defaultCredEndpointIP is the AWS-reserved link-local address tasks reach for
	// container credentials; defaultCredEndpointPort is the standard HTTP port.
	defaultCredEndpointIP   = "169.254.170.2"
	defaultCredEndpointPort = 80
	// defaultCredProxyPort is the high port the listener actually binds; a nat
	// DNAT rule rewrites 169.254.170.2:80 to it so nothing holds a socket on :80
	// and host/bridge tasks sharing the netns can bind :80 themselves.
	defaultCredProxyPort = 51679
	// credRefreshMargin refreshes a cached credential set this long before expiry
	// so a container never starts a request with a token about to lapse.
	credRefreshMargin = 5 * time.Minute
	// credDummyIface is the dummy interface the endpoint IP is added to in the host
	// netns so the agent can bind it.
	credDummyIface = "ecs-cred"
)

// credResponse is the ECS task-credentials JSON the container's AWS SDK expects
// at AWS_CONTAINER_CREDENTIALS_RELATIVE_URI.
type credResponse struct {
	RoleArn         string `json:"RoleArn"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
}

// credEntry caches one task's assumed-role credentials alongside the role it was
// assumed from.
type credEntry struct {
	roleARN string
	creds   stsauth.Credentials
}

// credEndpoint is the host-netns HTTP server that hands a task its IAM role
// credentials. It maps credID -> taskRoleARN (registered as tasks start) and
// mints/caches credentials on demand by assuming the role over the gateway with
// the agent's instance credentials.
type credEndpoint struct {
	provider   credentials.CredentialsProvider
	region     string
	gatewayURL string
	caPath     string
	ip         string
	port       int
	proxyPort  int
	run        netCmdRunner

	// assume mints fresh credentials for a role; defaults to AssumeRole over the
	// gateway and is overridable in tests.
	assume func(ctx context.Context, roleARN, sessionName string) (stsauth.Credentials, error)

	mu         sync.Mutex
	roles      map[string]string    // credID -> taskRoleARN
	cache      map[string]credEntry // credID -> cached creds
	httpClient *http.Client

	srv *http.Server
	ln  net.Listener
}

// newCredEndpoint builds the endpoint. An empty ip/port falls back to the
// AWS-reserved 169.254.170.2:80; tests pass loopback + an ephemeral port. run
// adds the link-local address to the dummy interface (real or fake).
func newCredEndpoint(provider credentials.CredentialsProvider, region, gatewayURL, caPath, ip string, port int, run netCmdRunner) *credEndpoint {
	if ip == "" {
		ip = defaultCredEndpointIP
	}
	if port == 0 {
		port = defaultCredEndpointPort
	}
	c := &credEndpoint{
		provider:   provider,
		region:     region,
		gatewayURL: gatewayURL,
		caPath:     caPath,
		ip:         ip,
		port:       port,
		proxyPort:  defaultCredProxyPort,
		run:        run,
		roles:      map[string]string{},
		cache:      map[string]credEntry{},
	}
	c.assume = c.assumeOverGateway
	return c
}

// Register associates a credID with the task role it serves; Deregister removes
// it and drops any cached credentials. Both are no-ops for an empty credID.
func (c *credEndpoint) Register(credID, roleARN string) {
	if credID == "" || roleARN == "" {
		return
	}
	c.mu.Lock()
	c.roles[credID] = roleARN
	c.mu.Unlock()
}

func (c *credEndpoint) Deregister(credID string) {
	if credID == "" {
		return
	}
	c.mu.Lock()
	delete(c.roles, credID)
	delete(c.cache, credID)
	c.mu.Unlock()
}

// relativeURI is the value injected into a container's
// AWS_CONTAINER_CREDENTIALS_RELATIVE_URI for the given credID.
func credRelativeURI(credID string) string { return "/v2/credentials/" + credID }

// ServeHTTP serves GET /v2/credentials/{credID}. Unknown paths/credIDs return
// 404; non-GET returns 405; an AssumeRole failure returns 502.
func (c *credEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	credID, ok := strings.CutPrefix(r.URL.Path, "/v2/credentials/")
	if !ok || credID == "" || strings.Contains(credID, "/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	creds, roleARN, err := c.fetch(r.Context(), credID)
	if err != nil {
		if err == errUnknownCredID {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		slog.Error("ecs-agent: assume task role failed", "credId", credID, "err", err)
		http.Error(w, "credentials unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(credResponse{
		RoleArn:         roleARN,
		AccessKeyID:     creds.AccessKeyID,
		SecretAccessKey: creds.SecretAccessKey,
		Token:           creds.SessionToken,
		Expiration:      creds.Expiration.UTC().Format(time.RFC3339),
	}); err != nil {
		slog.Warn("ecs-agent: encode credential response", "credId", credID, "err", err)
	}
}

// errUnknownCredID signals a credID with no registered task role.
var errUnknownCredID = fmt.Errorf("unknown credId")

// fetch returns valid credentials for credID, reusing the cache until within the
// refresh margin and otherwise assuming the task role over the gateway.
func (c *credEndpoint) fetch(ctx context.Context, credID string) (stsauth.Credentials, string, error) {
	c.mu.Lock()
	roleARN, known := c.roles[credID]
	if !known {
		c.mu.Unlock()
		return stsauth.Credentials{}, "", errUnknownCredID
	}
	if e, ok := c.cache[credID]; ok && e.roleARN == roleARN && credValid(e.creds, credRefreshMargin) {
		c.mu.Unlock()
		return e.creds, roleARN, nil
	}
	c.mu.Unlock()

	creds, err := c.assume(ctx, roleARN, sessionName(credID))
	if err != nil {
		return stsauth.Credentials{}, "", err
	}

	c.mu.Lock()
	c.cache[credID] = credEntry{roleARN: roleARN, creds: creds}
	c.mu.Unlock()
	return creds, roleARN, nil
}

// AssumeProvider returns a CredentialsProvider that mints credentials by assuming
// roleARN over the gateway — the execution-role path for ECR image pulls. It
// caches the assumed set and re-assumes within the refresh margin. session scopes
// the STS session name.
func (c *credEndpoint) AssumeProvider(roleARN, session string) credentials.CredentialsProvider {
	return &assumeProvider{ep: c, roleARN: roleARN, session: session}
}

// assumeProvider adapts a credEndpoint's AssumeRole path to the agent's
// CredentialsProvider interface, caching the assumed credentials.
type assumeProvider struct {
	ep      *credEndpoint
	roleARN string
	session string

	mu     sync.Mutex
	cached stsauth.Credentials
}

var _ credentials.CredentialsProvider = (*assumeProvider)(nil)

func (p *assumeProvider) Retrieve(ctx context.Context) (credentials.Credentials, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !credValid(p.cached, credRefreshMargin) {
		creds, err := p.ep.assume(ctx, p.roleARN, p.session)
		if err != nil {
			return credentials.Credentials{}, err
		}
		p.cached = creds
	}
	return credentials.Credentials{
		AccessKeyID:     p.cached.AccessKeyID,
		SecretAccessKey: p.cached.SecretAccessKey,
		SessionToken:    p.cached.SessionToken,
		Expiration:      p.cached.Expiration,
	}, nil
}

// assumeOverGateway mints credentials for roleARN by SigV4-signing AssumeRole
// against the gateway with the agent's instance credentials.
func (c *credEndpoint) assumeOverGateway(ctx context.Context, roleARN, sessionName string) (stsauth.Credentials, error) {
	instCreds, err := c.provider.Retrieve(ctx)
	if err != nil {
		return stsauth.Credentials{}, fmt.Errorf("instance credentials: %w", err)
	}
	client, err := c.client()
	if err != nil {
		return stsauth.Credentials{}, err
	}
	return stsauth.AssumeRole(c.region, c.gatewayURL, client,
		instCreds.AccessKeyID, instCreds.SecretAccessKey, instCreds.SessionToken,
		roleARN, sessionName)
}

// client builds the pinned-CA gateway HTTP client on first use and caches it.
func (c *credEndpoint) client() (*http.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.httpClient != nil {
		return c.httpClient, nil
	}
	hc, err := ecrauth.GatewayHTTPClient(c.caPath)
	if err != nil {
		return nil, err
	}
	c.httpClient = hc
	return hc, nil
}

// Start adds the endpoint IP to a dummy interface in the host netns, serves HTTP
// on the high proxy port, and DNATs the advertised :80 to it. A bind, address,
// or nat failure is returned so the caller can log it without aborting
// register/heartbeat.
func (c *credEndpoint) Start() error {
	if err := c.addEndpointAddr(); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(c.ip, strconv.Itoa(c.listenPort())))
	if err != nil {
		return fmt.Errorf("listen %s:%d: %w", c.ip, c.listenPort(), err)
	}
	if err := c.addRedirect(); err != nil {
		_ = ln.Close()
		return err
	}
	c.ln = ln
	c.srv = &http.Server{Handler: c, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := c.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("ecs-agent: credential endpoint serve", "err", err)
		}
	}()
	slog.Info("ecs-agent: credential endpoint listening", "addr", ln.Addr().String())
	return nil
}

// Stop shuts the server down and removes the dummy interface. Safe on a nil/
// never-started endpoint.
func (c *credEndpoint) Stop() error {
	if c == nil {
		return nil
	}
	if c.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.srv.Shutdown(ctx)
	}
	c.delRedirect()
	c.delEndpointAddr()
	return nil
}

// listenPort is the port the socket binds. Loopback test mode binds the
// caller-supplied (ephemeral) port directly; the real endpoint binds the high
// proxy port and reaches :80 via the DNAT rules.
func (c *credEndpoint) listenPort() int {
	if c.ip == "127.0.0.1" {
		return c.port
	}
	return c.proxyPort
}

// addRedirect installs nat DNAT rules rewriting the advertised :80 to the proxy
// port, for both locally generated traffic (host/bridge task containers, OUTPUT)
// and traffic arriving over the awsvpc credential veth (PREROUTING). The dst IP
// is left on the endpoint IP so no route_localnet handling is needed. Skipped in
// loopback test mode, which binds the port directly.
func (c *credEndpoint) addRedirect() error {
	if c.run == nil || c.ip == "127.0.0.1" {
		return nil
	}
	for _, chain := range []string{"OUTPUT", "PREROUTING"} {
		if _, err := c.run.Run("iptables", c.natArgs("-C", chain)...); err == nil {
			continue // rule already present
		}
		if _, err := c.run.Run("iptables", c.natArgs("-A", chain)...); err != nil {
			return fmt.Errorf("add nat redirect %s: %w", chain, err)
		}
	}
	return nil
}

func (c *credEndpoint) delRedirect() {
	if c.run == nil || c.ip == "127.0.0.1" {
		return
	}
	for _, chain := range []string{"OUTPUT", "PREROUTING"} {
		_, _ = c.run.Run("iptables", c.natArgs("-D", chain)...)
	}
}

// natArgs builds the iptables nat rule for op (-A/-C/-D) on chain: DNAT the
// advertised endpoint :80 to the proxy port on the same IP.
func (c *credEndpoint) natArgs(op, chain string) []string {
	return []string{
		"-t", "nat", op, chain,
		"-d", c.ip, "-p", "tcp", "--dport", strconv.Itoa(c.port),
		"-j", "DNAT", "--to-destination", net.JoinHostPort(c.ip, strconv.Itoa(c.proxyPort)),
	}
}

// addEndpointAddr brings up the dummy interface holding the endpoint IP. Skipped
// for loopback (tests bind 127.0.0.1 directly, no interface plumbing needed).
func (c *credEndpoint) addEndpointAddr() error {
	if c.run == nil || c.ip == "127.0.0.1" {
		return nil
	}
	_, _ = c.run.Run("ip", "link", "add", credDummyIface, "type", "dummy")
	if _, err := c.run.Run("ip", "addr", "add", c.ip+"/32", "dev", credDummyIface); err != nil &&
		!strings.Contains(err.Error(), "exists") {
		return fmt.Errorf("add endpoint addr %s: %w", c.ip, err)
	}
	if _, err := c.run.Run("ip", "link", "set", credDummyIface, "up"); err != nil {
		return fmt.Errorf("bring up %s: %w", credDummyIface, err)
	}
	return nil
}

func (c *credEndpoint) delEndpointAddr() {
	if c.run == nil || c.ip == "127.0.0.1" {
		return
	}
	_, _ = c.run.Run("ip", "link", "del", credDummyIface)
}

// credValid reports whether creds are usable now with margin to spare.
func credValid(c stsauth.Credentials, margin time.Duration) bool {
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return false
	}
	if c.Expiration.IsZero() {
		return true
	}
	return time.Now().Add(margin).Before(c.Expiration)
}

// sessionName derives an AssumeRole RoleSessionName from credID, satisfying the
// STS [\w+=,.@-]{2,64} constraint.
func sessionName(credID string) string {
	name := "ecs-" + credID
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}
