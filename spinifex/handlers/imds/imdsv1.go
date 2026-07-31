package handlers_imds

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// v1AllowTTL bounds how long a per-instance HttpTokens decision is reused. The
// decision only changes via ModifyInstanceMetadataOptions, so a short TTL costs
// little and keeps the untokened path off the NATS fan-out.
const v1AllowTTL = 30 * time.Second

type v1AllowEntry struct {
	allowed bool
	expires time.Time
}

// v1AllowCache memoises whether an instance permits IMDSv1. It exists for
// amplification control: the untokened path is reachable without credentials, so
// resolving HttpTokens per request would let any guest drive unbounded
// DescribeInstances fan-outs.
type v1AllowCache struct {
	mu      sync.Mutex
	entries map[string]v1AllowEntry
}

func newV1AllowCache() *v1AllowCache {
	return &v1AllowCache{entries: make(map[string]v1AllowEntry)}
}

func (c *v1AllowCache) get(instanceID string, now time.Time) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[instanceID]
	if !ok || now.After(e.expires) {
		return false, false
	}
	return e.allowed, true
}

func (c *v1AllowCache) put(instanceID string, allowed bool, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[instanceID] = v1AllowEntry{allowed: allowed, expires: now.Add(v1AllowTTL)}
}

func (c *v1AllowCache) sweep(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, e := range c.entries {
		if now.After(e.expires) {
			delete(c.entries, id)
		}
	}
}

// imdsV1Allowed reports whether this ENI's instance was launched or modified
// with HttpTokens=optional, which is the only case where an untokened GET is
// served. Defaults closed: an unattached ENI, a lookup failure or a missing
// instance all deny.
func (s *IMDSServiceImpl) imdsV1Allowed(ctx context.Context, eni *eniFacts) bool {
	if eni.instanceID == "" {
		return false
	}
	if allowed, ok := s.v1Allow.get(eni.instanceID, s.now()); ok {
		return allowed
	}

	inst, err := s.resolver.resolveInstance(ctx, eni)
	if err != nil {
		slog.WarnContext(ctx, "IMDS: HttpTokens lookup failed, denying untokened request",
			"instance_id", eni.instanceID, "err", err)
	}
	allowed := err == nil && inst != nil && inst.httpTokens == ec2.HttpTokensStateOptional

	// Cached even on failure: without that, a guest could replay a failing
	// lookup to keep the fan-out running.
	s.v1Allow.put(eni.instanceID, allowed, s.now())
	return allowed
}
