package issuer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// SessionDurationSeconds is the CreateSession duration: the shortest AWS
// allows, which bounds how long a session outlives a disconnect.
const SessionDurationSeconds = 900

const maxResponseBytes = 1 << 20

var (
	// Roles Anywhere ARNs. The role path allows any printable ASCII segment
	// and the name the IAM role name characters.
	trustAnchorARNPattern = regexp.MustCompile(`^arn:(aws|aws-us-gov|aws-cn):rolesanywhere:([a-z0-9-]+):[0-9]{12}:trust-anchor/[A-Za-z0-9-]+$`)
	profileARNPattern     = regexp.MustCompile(`^arn:(aws|aws-us-gov|aws-cn):rolesanywhere:[a-z0-9-]+:[0-9]{12}:profile/[A-Za-z0-9-]+$`)
	roleARNPattern        = regexp.MustCompile(`^arn:(aws|aws-us-gov|aws-cn):iam::[0-9]{12}:role(/[\x21-\x7E]+)*/[A-Za-z0-9_+=,.@-]+$`)
)

// Session holds AWS temporary credentials. It must stay in memory.
type Session struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// LogValue keeps the credentials out of logs.
func (s Session) LogValue() slog.Value {
	return slog.GroupValue(slog.Time("expiration", s.Expiration))
}

// String never prints the credentials.
func (s Session) String() string { return "Session{redacted}" }

// GoString never prints the credentials.
func (s Session) GoString() string { return s.String() }

// MarshalJSON refuses, so a session cannot reach a record or a log line.
func (s Session) MarshalJSON() ([]byte, error) {
	return nil, errors.New("rolesanywhere session must not be serialised")
}

// SessionRequest names what CreateSession assumes.
type SessionRequest struct {
	TrustAnchorARN string
	ProfileARN     string
	RoleARN        string
}

// SessionCreator calls CreateSession.
type SessionCreator interface {
	CreateSession(ctx context.Context, cert *ClientCertificate, req SessionRequest) (Session, error)
}

// APIError is an error answer from Roles Anywhere. Message is AWS text.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("rolesanywhere CreateSession: status %d %s: %s", e.StatusCode, e.Code, e.Message)
}

// Refused reports an answer where AWS rejects the identity or the request
// rather than failing itself.
func (e *APIError) Refused() bool {
	return e.StatusCode >= 400 && e.StatusCode < 500 && e.StatusCode != http.StatusTooManyRequests
}

// Client signs and sends CreateSession.
type Client struct {
	HTTP *http.Client
	// Endpoint returns the base URL for a Region. Nil means the AWS endpoint.
	Endpoint func(region string) string
	Now      func() time.Time
}

var _ SessionCreator = (*Client)(nil)

// DefaultEndpoint is the public Roles Anywhere endpoint of a Region.
func DefaultEndpoint(region string) string {
	if strings.HasPrefix(region, "cn-") {
		return "https://rolesanywhere." + region + ".amazonaws.com.cn"
	}
	return "https://rolesanywhere." + region + ".amazonaws.com"
}

type createSessionBody struct {
	DurationSeconds int    `json:"durationSeconds"`
	ProfileARN      string `json:"profileArn"`
	RoleARN         string `json:"roleArn"`
	TrustAnchorARN  string `json:"trustAnchorArn"`
}

type createSessionResponse struct {
	CredentialSet []struct {
		Credentials struct {
			AccessKeyID     string `json:"accessKeyId"`
			SecretAccessKey string `json:"secretAccessKey"`
			SessionToken    string `json:"sessionToken"`
			Expiration      string `json:"expiration"`
		} `json:"credentials"`
	} `json:"credentialSet"`
}

// CreateSession returns a 900-second session for req.RoleARN.
func (c *Client) CreateSession(ctx context.Context, cert *ClientCertificate, req SessionRequest) (Session, error) {
	m := trustAnchorARNPattern.FindStringSubmatch(req.TrustAnchorARN)
	if m == nil || !profileARNPattern.MatchString(req.ProfileARN) || !roleARNPattern.MatchString(req.RoleARN) {
		return Session{}, fmt.Errorf("rolesanywhere CreateSession: invalid ARN in request")
	}
	region := m[2]
	endpoint := DefaultEndpoint
	if c.Endpoint != nil {
		endpoint = c.Endpoint
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}

	body, err := json.Marshal(createSessionBody{
		DurationSeconds: SessionDurationSeconds,
		ProfileARN:      req.ProfileARN,
		RoleARN:         req.RoleARN,
		TrustAnchorARN:  req.TrustAnchorARN,
	})
	if err != nil {
		return Session{}, fmt.Errorf("rolesanywhere CreateSession: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(endpoint(region), "/")+"/sessions", bytes.NewReader(body))
	if err != nil {
		return Session{}, fmt.Errorf("rolesanywhere CreateSession: %w", err)
	}
	if err := sign(httpReq, body, cert, region, now()); err != nil {
		return Session{}, err
	}
	resp, err := httpc.Do(httpReq)
	if err != nil {
		return Session{}, fmt.Errorf("rolesanywhere CreateSession: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return Session{}, fmt.Errorf("rolesanywhere CreateSession: read response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return Session{}, apiError(resp, raw)
	}

	var out createSessionResponse
	if err := json.Unmarshal(raw, &out); err != nil || len(out.CredentialSet) == 0 {
		return Session{}, fmt.Errorf("rolesanywhere CreateSession: malformed response")
	}
	cr := out.CredentialSet[0].Credentials
	exp, err := time.Parse(time.RFC3339, cr.Expiration)
	if err != nil || cr.AccessKeyID == "" || cr.SecretAccessKey == "" || cr.SessionToken == "" {
		return Session{}, fmt.Errorf("rolesanywhere CreateSession: malformed credentials")
	}
	return Session{AccessKeyID: cr.AccessKeyID, SecretAccessKey: cr.SecretAccessKey, SessionToken: cr.SessionToken, Expiration: exp}, nil
}

func apiError(resp *http.Response, raw []byte) *APIError {
	e := &APIError{StatusCode: resp.StatusCode, Code: resp.Header.Get("X-Amzn-Errortype")}
	var body struct {
		Message string `json:"message"`
		Type    string `json:"__type"`
		Code    string `json:"code"`
	}
	if json.Unmarshal(raw, &body) == nil {
		e.Message = body.Message
		if e.Code == "" {
			e.Code = body.Type
		}
		if e.Code == "" {
			e.Code = body.Code
		}
	}
	// restJson1: the error name follows any "namespace#" and precedes any ":uri".
	if i := strings.Index(e.Code, ":"); i >= 0 {
		e.Code = e.Code[:i]
	}
	if i := strings.LastIndex(e.Code, "#"); i >= 0 {
		e.Code = e.Code[i+1:]
	}
	return e
}
