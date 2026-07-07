package handlers_imds

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
)

const (
	// credDurationSeconds is the lifetime of instance-role credentials (AWS default: 1 hour).
	credDurationSeconds = 3600

	// credRefreshWindow is how early a cached credential is re-minted before expiry.
	credRefreshWindow = 5 * time.Minute
)

// instanceCredential is the JSON body served at /latest/meta-data/iam/security-credentials/<role>.
type instanceCredential struct {
	Code            string `json:"Code"`
	LastUpdated     string `json:"LastUpdated"`
	Type            string `json:"Type"`
	AccessKeyId     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
	AccountId       string `json:"AccountId"`
}

// stsAssumer is the narrow STSService slice needed by the IMDS credential path.
type stsAssumer interface {
	AssumeRoleForInstance(ctx context.Context, accountID, roleARN, instanceID string, durationSeconds int64) (*sts.AssumeRoleOutput, error)
}

// cachedCred holds a rendered credential body and the time it should be re-minted.
type cachedCred struct {
	body      []byte
	refreshAt time.Time
}

// credCache memoises instance-role credentials per (ENI, role) until the refresh window.
type credCache struct {
	sts     stsAssumer
	mu      sync.Mutex
	entries map[string]cachedCred
}

func newCredCache(assumer stsAssumer) *credCache {
	return &credCache{sts: assumer, entries: make(map[string]cachedCred)}
}

// get returns the credential JSON body, minting via STS when the cache is cold or near expiry.
func (c *credCache) get(ctx context.Context, eni *eniFacts, roleName, roleARN string, now time.Time) ([]byte, error) {
	key := eni.eniID + "\x00" + roleName

	c.mu.Lock()
	if entry, ok := c.entries[key]; ok && now.Before(entry.refreshAt) {
		body := entry.body
		c.mu.Unlock()
		return body, nil
	}
	c.mu.Unlock()

	out, err := c.sts.AssumeRoleForInstance(ctx, eni.iamAccountID(), roleARN, eni.instanceID, credDurationSeconds)
	if err != nil {
		return nil, err
	}
	if out == nil || out.Credentials == nil {
		return nil, fmt.Errorf("assume role for instance %s returned no credentials", eni.instanceID)
	}

	expiration := aws.TimeValue(out.Credentials.Expiration)
	body, err := json.Marshal(instanceCredential{
		Code:            "Success",
		LastUpdated:     now.UTC().Format(time.RFC3339),
		Type:            "AWS-HMAC",
		AccessKeyId:     aws.StringValue(out.Credentials.AccessKeyId),
		SecretAccessKey: aws.StringValue(out.Credentials.SecretAccessKey),
		Token:           aws.StringValue(out.Credentials.SessionToken),
		Expiration:      expiration.UTC().Format(time.RFC3339),
		AccountId:       eni.iamAccountID(),
	})
	if err != nil {
		return nil, fmt.Errorf("marshal instance credential: %w", err)
	}

	c.mu.Lock()
	c.entries[key] = cachedCred{body: body, refreshAt: expiration.Add(-credRefreshWindow)}
	c.mu.Unlock()

	return body, nil
}

// sweep removes fully-expired entries to bound the cache against ENI churn.
func (c *credCache) sweep(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		if now.After(entry.refreshAt.Add(credRefreshWindow)) {
			delete(c.entries, key)
		}
	}
}
