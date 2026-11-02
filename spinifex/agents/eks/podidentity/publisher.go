package podidentity

import (
	"context"
	"errors"
	"maps"
	"time"
)

const (
	defaultResync     = 5 * time.Minute
	reconcileAttempts = 3
)

// Reconcile makes the CA bundle ConfigMap in one namespace match the bundle.
// Losing a create or update race to another publisher is not an error.
func (p *Publisher) Reconcile(ctx context.Context, namespace string) error {
	want := string(p.cfg.Bundle)
	var err error
	for range reconcileAttempts {
		var cm *ConfigMap
		cm, err = p.cms.Get(ctx, namespace, CAConfigMapName)
		switch {
		case errors.Is(err, ErrNotFound):
			err = p.cms.Create(ctx, ConfigMap{Namespace: namespace, Name: CAConfigMapName, Data: map[string]string{CABundleKey: want}})
			if err == nil {
				return nil
			}
			if !errors.Is(err, ErrAlreadyExists) {
				return err
			}
			continue
		case err != nil:
			return err
		case cm.Data[CABundleKey] == want:
			return nil
		}

		data := maps.Clone(cm.Data)
		if data == nil {
			data = map[string]string{}
		}
		data[CABundleKey] = want
		err = p.cms.Update(ctx, ConfigMap{Namespace: namespace, Name: CAConfigMapName, Data: data, ResourceVersion: cm.ResourceVersion})
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	return err
}

// Run publishes to every existing namespace, then to each new one, and
// re-checks every namespace each resync interval until ctx ends.
func (p *Publisher) Run(ctx context.Context, resync time.Duration) error {
	if resync <= 0 {
		resync = defaultResync
	}
	created, err := p.ns.Watch(ctx)
	if err != nil {
		return err
	}
	p.reconcileAll(ctx)

	tick := time.NewTicker(resync)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ns, ok := <-created:
			if !ok {
				created = nil
				continue
			}
			p.reconcileOne(ctx, ns)
		case <-tick.C:
			if created == nil {
				if ch, err := p.ns.Watch(ctx); err != nil {
					p.log.Warn("namespace watch restart failed", "error", err)
				} else {
					created = ch
				}
			}
			p.reconcileAll(ctx)
		}
	}
}

func (p *Publisher) reconcileAll(ctx context.Context) {
	names, err := p.ns.List(ctx)
	if err != nil {
		p.log.Warn("namespace list failed", "error", err)
		return
	}
	for _, ns := range names {
		p.reconcileOne(ctx, ns)
	}
}

func (p *Publisher) reconcileOne(ctx context.Context, ns string) {
	if err := p.Reconcile(ctx, ns); err != nil && ctx.Err() == nil {
		p.log.Warn("ca bundle publish failed", "namespace", ns, "error", err)
	}
}
