//go:build e2e

package route53

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
)

const (
	// More zone objects than one ListObjectsV2 page holds (1,000), so a
	// listing that stops at the first page leaves zones unloaded.
	scaleZoneCount = 1100
	scaleZoneAddr  = "192.0.2.53"
	scaleWorkers   = 8

	// Two 30 s sync cycles plus reading 1,100 objects, with headroom.
	scaleSyncTimeout = 4 * time.Minute
)

// scaleZone sorts every zone ahead of the platform base zone, so a purge that
// trusts a truncated listing would take the base zone with it.
func scaleZone(i int) string {
	return fmt.Sprintf("a%04d.zonescale.test", i)
}

// Northstar loads every zone in its bucket, not just the first listing page,
// and a sync never purges a zone that is still there. The last zone written
// must resolve on every node, the platform base zone must survive the sync,
// and deleting the objects must stop them resolving.
func TestNorthstarServesZonesBeyondFirstListPage(t *testing.T) {
	env := harness.LoadEnv(t)
	targets := harness.DNSTargets(env)
	store := harness.NorthstarZoneStore(t, env, targets[0])
	base := harness.NorthstarBaseDomain(env)
	if base == "" && env.Mode == harness.ModeMultinode {
		base, _ = harness.PeerNorthstarDomains(t, targets[0])
	}
	require.NotEmpty(t, base, "northstar base domain is not configured")

	harness.Phase(t, "Northstar — write %d zone objects", scaleZoneCount)
	t.Cleanup(func() {
		if err := forEachZone(func(name string) error {
			return harness.DeleteTestZone(context.Background(), store, name)
		}); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})
	require.NoError(t, forEachZone(func(name string) error {
		return harness.WriteTestZone(store, name, scaleZoneAddr)
	}))

	first, last := scaleZone(1), scaleZone(scaleZoneCount)
	harness.Phase(t, "Northstar — every node serves the first and last zone")
	for _, node := range targets {
		harness.EventuallyErr(t, func() error {
			for _, name := range []string{first, last} {
				if err := wantAddr(node, name, scaleZoneAddr); err != nil {
					return err
				}
			}
			return nil
		}, scaleSyncTimeout, 5*time.Second)
	}

	harness.Phase(t, "Northstar — the platform base zone survives the sync")
	for _, node := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ns, err := harness.NodeResolver(node).LookupNS(ctx, base)
		cancel()
		require.NoError(t, err, "base zone %s on %s", base, node.Name)
		require.NotEmpty(t, ns, "base zone %s on %s has no NS records", base, node.Name)
	}

	harness.Phase(t, "Northstar — deleted zone objects stop resolving")
	require.NoError(t, forEachZone(func(name string) error {
		return harness.DeleteTestZone(context.Background(), store, name)
	}))
	for _, node := range targets {
		harness.EventuallyErr(t, func() error {
			for _, name := range []string{first, last} {
				if err := wantGone(node, name); err != nil {
					return err
				}
			}
			return nil
		}, scaleSyncTimeout, 5*time.Second)
	}
}

// forEachZone runs fn over every scale zone with a small worker pool and
// returns the first error.
func forEachZone(fn func(name string) error) error {
	names := make(chan string)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for range scaleWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for name := range names {
				if err := fn(name); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("%s: %w", name, err)
					}
					mu.Unlock()
				}
			}
		}()
	}
	for i := 1; i <= scaleZoneCount; i++ {
		names <- scaleZone(i)
	}
	close(names)
	wg.Wait()
	return firstErr
}

func wantAddr(node harness.Node, name, addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addrs, err := harness.ResolveViaNode(ctx, node, name)
	if err != nil {
		return err
	}
	for _, a := range addrs {
		if a == addr {
			return nil
		}
	}
	return fmt.Errorf("%s via %s = %v, want %s", name, node.Name, addrs, addr)
}

// wantGone accepts any definite answer other than an address: NXDOMAIN when
// the name is recursed upstream, REFUSED when the listener does not recurse
// for this client. A timeout proves nothing, so it is not accepted.
func wantGone(node harness.Node, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addrs, err := harness.NodeResolver(node).LookupHost(ctx, name)
	if err == nil {
		return fmt.Errorf("%s via %s still resolves to %v", name, node.Name, addrs)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsTimeout {
		return fmt.Errorf("%s via %s: %w", name, node.Name, err)
	}
	return nil
}
