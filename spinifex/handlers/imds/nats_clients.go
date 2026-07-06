package handlers_imds

import (
	"context"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	handlers_ec2_key "github.com/mulgadc/spinifex/spinifex/handlers/ec2/key"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const (
	// imdsRPCTimeout bounds each internal control-plane round-trip to awsgw.
	imdsRPCTimeout = 10 * time.Second

	// profileCacheTTL memoises profile/role lookups to avoid per-request NATS round-trips.
	profileCacheTTL = 5 * time.Minute

	// pubKeyCacheTTL memoises SSH public-key material (immutable for a key's lifetime).
	pubKeyCacheTTL = 5 * time.Minute
)

// NATSSTSAssumer is the NATS-backed stsAssumer for minting instance-role credentials.
type NATSSTSAssumer struct {
	nc *nats.Conn
}

var _ stsAssumer = (*NATSSTSAssumer)(nil)

// NewNATSSTSAssumer constructs a NATSSTSAssumer over nc.
func NewNATSSTSAssumer(nc *nats.Conn) *NATSSTSAssumer {
	return &NATSSTSAssumer{nc: nc}
}

func (a *NATSSTSAssumer) AssumeRoleForInstance(ctx context.Context, accountID, roleARN, instanceID string, durationSeconds int64) (*sts.AssumeRoleOutput, error) {
	return utils.NATSRequestCtx[sts.AssumeRoleOutput](ctx, a.nc, handlers_sts.SubjectAssumeRoleForInstance, handlers_sts.AssumeRoleForInstanceRequest{
		AccountID:       accountID,
		RoleARN:         roleARN,
		InstanceID:      instanceID,
		DurationSeconds: durationSeconds,
	}, imdsRPCTimeout, accountID)
}

// NATSProfileLookup is the NATS-backed profileLookup with a short-TTL cache.
type NATSProfileLookup struct {
	nc       *nats.Conn
	profiles *ttlCache[*handlers_iam.InstanceProfile]
	roles    *ttlCache[*iam.GetRoleOutput]
}

var _ profileLookup = (*NATSProfileLookup)(nil)

// NewNATSProfileLookup constructs a NATSProfileLookup over nc.
func NewNATSProfileLookup(nc *nats.Conn) *NATSProfileLookup {
	return &NATSProfileLookup{
		nc:       nc,
		profiles: newTTLCache[*handlers_iam.InstanceProfile](profileCacheTTL),
		roles:    newTTLCache[*iam.GetRoleOutput](profileCacheTTL),
	}
}

func (p *NATSProfileLookup) ResolveInstanceProfile(ctx context.Context, accountID, nameOrARN string) (*handlers_iam.InstanceProfile, error) {
	key := accountID + "\x00" + nameOrARN
	if v, ok := p.profiles.get(key); ok {
		return v, nil
	}
	out, err := utils.NATSRequestCtx[handlers_iam.InstanceProfile](ctx, p.nc, handlers_iam.SubjectResolveInstanceProfile, handlers_iam.ResolveInstanceProfileRequest{
		AccountID: accountID,
		NameOrARN: nameOrARN,
	}, imdsRPCTimeout, accountID)
	if err != nil {
		return nil, err
	}
	p.profiles.put(key, out)
	return out, nil
}

func (p *NATSProfileLookup) GetRole(ctx context.Context, accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	key := accountID + "\x00" + aws.StringValue(input.RoleName)
	if v, ok := p.roles.get(key); ok {
		return v, nil
	}
	out, err := utils.NATSRequestCtx[iam.GetRoleOutput](ctx, p.nc, handlers_iam.SubjectGetRole, handlers_iam.GetRoleRequest{
		AccountID: accountID,
		Input:     input,
	}, imdsRPCTimeout, accountID)
	if err != nil {
		return nil, err
	}
	p.roles.put(key, out)
	return out, nil
}

// NATSPublicKeyLookup is the NATS-backed publicKeyLookup with a success-only TTL cache.
type NATSPublicKeyLookup struct {
	nc    *nats.Conn
	cache *ttlCache[string]
}

var _ publicKeyLookup = (*NATSPublicKeyLookup)(nil)

// NewNATSPublicKeyLookup constructs a NATSPublicKeyLookup over nc.
func NewNATSPublicKeyLookup(nc *nats.Conn) *NATSPublicKeyLookup {
	return &NATSPublicKeyLookup{
		nc:    nc,
		cache: newTTLCache[string](pubKeyCacheTTL),
	}
}

func (p *NATSPublicKeyLookup) GetPublicKey(ctx context.Context, accountID, keyName string) (string, error) {
	key := accountID + "\x00" + keyName
	if v, ok := p.cache.get(key); ok {
		return v, nil
	}
	out, err := utils.NATSRequestCtx[handlers_ec2_key.GetPublicKeyResponse](ctx, p.nc, "imds.ec2.get_public_key", handlers_ec2_key.GetPublicKeyRequest{
		AccountID: accountID,
		KeyName:   keyName,
	}, imdsRPCTimeout, accountID)
	if err != nil {
		return "", err // errors not cached: transient miss must not pin key as absent
	}
	p.cache.put(key, out.OpenSSHKey)
	return out.OpenSSHKey, nil
}

// ttlCache is a concurrency-safe map with per-entry expiry; now is injectable for tests.
type ttlCache[V any] struct {
	mu  sync.Mutex
	ttl time.Duration
	now func() time.Time
	m   map[string]ttlCacheEntry[V]
}

type ttlCacheEntry[V any] struct {
	val       V
	expiresAt time.Time
}

func newTTLCache[V any](ttl time.Duration) *ttlCache[V] {
	return &ttlCache[V]{ttl: ttl, now: time.Now, m: make(map[string]ttlCacheEntry[V])}
}

func (c *ttlCache[V]) get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.m[key]
	if !ok || c.now().After(entry.expiresAt) {
		var zero V
		return zero, false
	}
	return entry.val, true
}

func (c *ttlCache[V]) put(key string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = ttlCacheEntry[V]{val: v, expiresAt: c.now().Add(c.ttl)}
}
