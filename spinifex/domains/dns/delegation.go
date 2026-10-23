package dns

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	nsconfig "github.com/mulgadc/northstar/pkg/config"
	"github.com/nats-io/nats.go"

	reconcilelock "github.com/mulgadc/spinifex/spinifex/domains/network/reconcile"
)

// delegateLockPoll is how often Delegate retries the zone-writer election
// while another node holds it.
var delegateLockPoll = time.Second

// PlatformDelegation returns the NS set the base zone publishes for name and
// the zone it read. ok is false when name is not strictly below the base
// domain, so the zone's parent is somewhere else.
func (w *Writer) PlatformDelegation(name string) (servers []string, zone string, ok bool, err error) {
	if !w.Enabled() {
		return nil, "", false, nil
	}
	base := normalizeZone(w.baseDomain)
	n := normalizeZone(name)
	if base == "" || !strings.HasSuffix(n, "."+base) {
		return nil, "", false, nil
	}
	cfg, exists, err := nsconfig.ReadZoneRaw(w.s3cfg, base)
	if err != nil {
		return nil, base, true, fmt.Errorf("read zone %s: %w", base, err)
	}
	if !exists {
		return nil, base, true, nil
	}
	label := relativeLabel(n, base)
	for _, r := range cfg.Records {
		if r.Type == nsconfig.TypeNS && strings.EqualFold(r.Domain, label) {
			servers = append(servers, r.Address)
		}
	}
	return servers, base, true, nil
}

// Delegate writes, or with remove withdraws, an NS set in the base zone that
// delegates each name to this cluster's nameservers. It takes the zone-writer
// election first, so it never races the reconcile's own writes to the zone.
func (w *Writer) Delegate(ctx context.Context, nc *nats.Conn, holder string, names []string, remove bool) (*ChangeResult, error) {
	if !w.Enabled() {
		return nil, errors.New("northstar is not configured on this node")
	}
	base := normalizeZone(w.baseDomain)
	values := make([]string, 0, len(w.nameservers))
	for _, ns := range w.nameservers {
		values = append(values, ns.Host+"."+base+".")
	}
	batch := &ChangeBatch{}
	for _, name := range names {
		n := normalizeZone(name)
		if !strings.HasSuffix(n, "."+base) {
			return nil, fmt.Errorf("%s is not below the base domain %s", name, base)
		}
		if ReservedZoneName(n, base, w.internalDomain) {
			return nil, fmt.Errorf("%s is a platform name and cannot be delegated", name)
		}
		if remove {
			for _, v := range values {
				batch.Changes = append(batch.Changes, Change{Action: ActionDelete, Zone: base, Name: n + ".", Type: "NS", Value: v})
			}
			continue
		}
		batch.Changes = append(batch.Changes, Change{Action: ActionUpsertSet, Zone: base, Name: n + ".", Type: "NS", Values: values})
	}
	if len(batch.Changes) == 0 {
		return &ChangeResult{}, nil
	}
	release, err := w.awaitWriterElection(ctx, nc, holder)
	if err != nil {
		return nil, err
	}
	defer release()
	return w.ApplyBatch(batch)
}

func (w *Writer) awaitWriterElection(ctx context.Context, nc *nats.Conn, holder string) (func(), error) {
	if nc == nil {
		return func() {}, nil
	}
	tick := time.NewTicker(delegateLockPoll)
	defer tick.Stop()
	for {
		if release, ok := reconcilelock.AcquireLeader(ctx, nc, KVBucketDNSReconcile, holder); ok {
			return release, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("another node is writing the zone: %w", ctx.Err())
		case <-tick.C:
		}
	}
}
