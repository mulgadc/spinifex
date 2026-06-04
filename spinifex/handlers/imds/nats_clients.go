package handlers_imds

import (
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/sts"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	handlers_sts "github.com/mulgadc/spinifex/spinifex/handlers/sts"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

const (
	// imdsRPCTimeout bounds each internal control-plane round-trip to awsgw.
	imdsRPCTimeout = 10 * time.Second

	// profileCacheTTL memoises profile/role lookups. Both mappings are effectively
	// static, so a short TTL keeps the iam/* GETs off a NATS round-trip per request.
	profileCacheTTL = 5 * time.Minute
)

// NATSSTSAssumer is the NATS-backed stsAssumer. It mints instance-role credentials
// via awsgw's SubjectAssumeRoleForInstance responder, keeping no local cache —
// credCache already memoises minted credentials, so STS stays off the hot path.
type NATSSTSAssumer struct {
	nc *nats.Conn
}

var _ stsAssumer = (*NATSSTSAssumer)(nil)

// NewNATSSTSAssumer constructs a NATSSTSAssumer over nc.
func NewNATSSTSAssumer(nc *nats.Conn) *NATSSTSAssumer {
	return &NATSSTSAssumer{nc: nc}
}

func (a *NATSSTSAssumer) AssumeRoleForInstance(accountID, roleARN, instanceID string, durationSeconds int64) (*sts.AssumeRoleOutput, error) {
	return utils.NATSRequest[sts.AssumeRoleOutput](a.nc, handlers_sts.SubjectAssumeRoleForInstance, handlers_sts.AssumeRoleForInstanceRequest{
		AccountID:       accountID,
		RoleARN:         roleARN,
		InstanceID:      instanceID,
		DurationSeconds: durationSeconds,
	}, imdsRPCTimeout, accountID)
}

// NATSProfileLookup is the NATS-backed profileLookup. It resolves instance
// profiles and roles via awsgw's IAM responders, fronted by a short-TTL cache
// so repeat iam/* GETs don't take a round-trip each.
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

func (p *NATSProfileLookup) ResolveInstanceProfile(accountID, nameOrARN string) (*handlers_iam.InstanceProfile, error) {
	key := accountID + "\x00" + nameOrARN
	if v, ok := p.profiles.get(key); ok {
		return v, nil
	}
	out, err := utils.NATSRequest[handlers_iam.InstanceProfile](p.nc, handlers_iam.SubjectResolveInstanceProfile, handlers_iam.ResolveInstanceProfileRequest{
		AccountID: accountID,
		NameOrARN: nameOrARN,
	}, imdsRPCTimeout, accountID)
	if err != nil {
		return nil, err
	}
	p.profiles.put(key, out)
	return out, nil
}

func (p *NATSProfileLookup) GetRole(accountID string, input *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	key := accountID + "\x00" + aws.StringValue(input.RoleName)
	if v, ok := p.roles.get(key); ok {
		return v, nil
	}
	out, err := utils.NATSRequest[iam.GetRoleOutput](p.nc, handlers_iam.SubjectGetRole, handlers_iam.GetRoleRequest{
		AccountID: accountID,
		Input:     input,
	}, imdsRPCTimeout, accountID)
	if err != nil {
		return nil, err
	}
	p.roles.put(key, out)
	return out, nil
}

// ttlCache is a minimal concurrency-safe map with per-entry expiry, used to
// keep effectively-static IAM lookups off the NATS round-trip. now is a field
// so tests can drive expiry deterministically.
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
