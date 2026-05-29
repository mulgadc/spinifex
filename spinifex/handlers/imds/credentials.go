package handlers_imds

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
)

const (
	// credDurationSeconds is the lifetime of instance-role credentials. 1 hour
	// is the AWS default and matches the storage-backed STS v1 design.
	credDurationSeconds = 3600

	// credRefreshWindow is how far before expiry a cached credential is treated
	// as stale and re-minted. AWS SDKs refresh at the same 5-minute mark.
	credRefreshWindow = 5 * time.Minute
)

// instanceCredential is the JSON body served at
// /latest/meta-data/iam/security-credentials/<role>, byte-for-byte matching the
// AWS shape so unmodified SDK credential providers parse it.
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

// stsAssumer is the narrow slice of STSService the IMDS credential path needs:
// the in-process, EC2-service-principal AssumeRole entry point. Narrowed to an
// interface so the cred cache is unit-testable with a fake.
type stsAssumer interface {
	AssumeRoleForInstance(accountID, roleARN, instanceID string, durationSeconds int64) (*sts.AssumeRoleOutput, error)
}

// cachedCred holds a rendered credential body and the instant at which it should
// be re-minted (expiry minus the refresh window).
type cachedCred struct {
	body      []byte
	refreshAt time.Time
}

// credCache memoises minted instance-role credentials per (ENI, role) until the
// refresh window. It keeps STS off the hot path: the typical request returns a
// cached body, and only a near-expiry request triggers a fresh assume.
type credCache struct {
	sts     stsAssumer
	mu      sync.Mutex
	entries map[string]cachedCred
}

func newCredCache(assumer stsAssumer) *credCache {
	return &credCache{sts: assumer, entries: make(map[string]cachedCred)}
}

// get returns the credential JSON body for an instance's role, minting fresh
// credentials via STS when the cache is cold or within the refresh window. The
// cache key is (eniID, roleName) so two ENIs sharing a role don't collide.
func (c *credCache) get(eni *eniFacts, roleName, roleARN string, now time.Time) ([]byte, error) {
	key := eni.eniID + "\x00" + roleName

	c.mu.Lock()
	if entry, ok := c.entries[key]; ok && now.Before(entry.refreshAt) {
		body := entry.body
		c.mu.Unlock()
		return body, nil
	}
	c.mu.Unlock()

	out, err := c.sts.AssumeRoleForInstance(eni.accountID, roleARN, eni.instanceID, credDurationSeconds)
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
		AccountId:       eni.accountID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal instance credential: %w", err)
	}

	c.mu.Lock()
	c.entries[key] = cachedCred{body: body, refreshAt: expiration.Add(-credRefreshWindow)}
	c.mu.Unlock()

	return body, nil
}
