// Package imdscreds fetches AWS instance-role credentials from the in-guest
// IMDS endpoint (169.254.169.254) using IMDSv2. It is shared by the binaries
// that run inside a Spinifex guest and need the node/instance role credentials:
// the EKS ecr-credential-provider and the ECS ecs-agent.
package imdscreds

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// imdsTokenTTL is the IMDSv2 session-token lifetime requested on the PUT.
const imdsTokenTTL = "21600"

// Credentials is a resolved instance-role credential set with parsed expiry.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// imdsCreds mirrors the IMDS security-credentials JSON shape.
type imdsCreds struct {
	Code            string `json:"Code"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
}

// Fetch resolves the instance role name then its credentials via IMDSv2
// (PUT token, then GET with the token header). base is the IMDS root, e.g.
// http://169.254.169.254/latest.
func Fetch(client *http.Client, base string) (Credentials, error) {
	base = strings.TrimRight(base, "/")
	token := v2Token(client, base)

	roleName, err := get(client, base+"/meta-data/iam/security-credentials/", token)
	if err != nil {
		return Credentials{}, fmt.Errorf("fetch role name: %w", err)
	}
	roleName = strings.TrimSpace(roleName)
	if roleName == "" {
		return Credentials{}, fmt.Errorf("IMDS returned empty role name")
	}

	body, err := get(client, base+"/meta-data/iam/security-credentials/"+roleName, token)
	if err != nil {
		return Credentials{}, fmt.Errorf("fetch role credentials: %w", err)
	}
	var raw imdsCreds
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return Credentials{}, fmt.Errorf("decode role credentials: %w", err)
	}
	if raw.AccessKeyID == "" || raw.SecretAccessKey == "" {
		return Credentials{}, fmt.Errorf("IMDS credentials missing access/secret key")
	}

	creds := Credentials{
		AccessKeyID:     raw.AccessKeyID,
		SecretAccessKey: raw.SecretAccessKey,
		SessionToken:    raw.Token,
	}
	if raw.Expiration != "" {
		// IMDS emits RFC3339; a parse failure leaves a zero expiry (treated as
		// unknown by callers) rather than failing the whole fetch.
		if exp, perr := time.Parse(time.RFC3339, raw.Expiration); perr == nil {
			creds.Expiration = exp
		}
	}
	return creds, nil
}

// v2Token requests an IMDSv2 session token; returns "" if the service does not
// enforce v2 (tokenless v1 then applies).
func v2Token(client *http.Client, base string) string {
	req, err := http.NewRequest(http.MethodPut, base+"/api/token", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", imdsTokenTTL)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(body))
}

// get issues an IMDS GET, attaching the IMDSv2 token header when present.
func get(client *http.Client, url, token string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("X-Aws-Ec2-Metadata-Token", token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("IMDS %s returned %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}
