package ovn

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

func (c *LiveClient) EnsureTransitGatewayLink(ctx context.Context, attachmentID string, ends [2]TransitGatewayLinkEnd) error {
	var ops []ovsdb.Operation
	for _, end := range ends {
		lr, err := c.GetLogicalRouter(ctx, end.Router)
		if err != nil {
			return fmt.Errorf("transit gateway link: %w", err)
		}
		portOps, err := c.syncRouterPortOps(ctx, lr, end.Port)
		if err != nil {
			return err
		}
		policies, err := rowsIn(ctx, c, lr.Policies, policyStamp)
		if err != nil {
			return fmt.Errorf("list policies on %s: %w", lr.Name, err)
		}
		routes, err := rowsIn(ctx, c, lr.StaticRoutes, routeStamp)
		if err != nil {
			return fmt.Errorf("list static routes on %s: %w", lr.Name, err)
		}
		owned := ownedSync{c: c, lr: lr, owner: attachmentID}
		policyOps, perr := syncOwnedOps(owned, &lr.Policies, policies, end.Policies, policyStamp, PolicyKey, samePolicy, "lrp_")
		routeOps, rerr := syncOwnedOps(owned, &lr.StaticRoutes, routes, end.Routes, routeStamp, StaticRouteKey, sameStaticRoute, "route_")
		if err := errors.Join(perr, rerr); err != nil {
			return err
		}
		ops = slices.Concat(ops, portOps, policyOps, routeOps)
	}
	if len(ops) == 0 {
		return nil
	}
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("transit gateway link %s transact: %w", attachmentID, err)
	}
	return nil
}

func (c *LiveClient) DeleteTransitGatewayLink(ctx context.Context, attachmentID string, routers []string) error {
	return c.deleteOwnedLink(ctx, ExternalIDTGWAttachmentID, attachmentID, routers)
}

// syncRouterPortOps writes port unless a port of its name already equals it.
func (c *LiveClient) syncRouterPortOps(ctx context.Context, lr *nbdb.LogicalRouter, port nbdb.LogicalRouterPort) ([]ovsdb.Operation, error) {
	var existing []nbdb.LogicalRouterPort
	if err := c.client.WhereCache(func(p *nbdb.LogicalRouterPort) bool { return p.Name == port.Name }).List(ctx, &existing); err != nil {
		return nil, fmt.Errorf("find port %s: %w", port.Name, err)
	}
	if len(existing) > 0 && slices.Contains(lr.Ports, existing[0].UUID) && SameRouterPort(&existing[0], &port) {
		return nil, nil
	}
	return c.upsertRouterPortOps(ctx, lr, port)
}

// PolicyKey is a policy's identity among one owner's rows on a router.
func PolicyKey(p *nbdb.LogicalRouterPolicy) string { return fmt.Sprintf("%d|%s", p.Priority, p.Match) }

// StaticRouteKey is a static route's identity among one owner's rows on a router.
func StaticRouteKey(r *nbdb.LogicalRouterStaticRoute) string { return r.IPPrefix }

// SameRouterPort reports whether a port already holds every column a link sets.
func SameRouterPort(cur, want *nbdb.LogicalRouterPort) bool {
	return cur.MAC == want.MAC && slices.Equal(cur.Networks, want.Networks) &&
		samePtr(cur.Peer, want.Peer) && maps.Equal(cur.ExternalIDs, want.ExternalIDs)
}

func samePolicy(cur, want *nbdb.LogicalRouterPolicy) bool {
	return cur.Priority == want.Priority && cur.Match == want.Match && cur.Action == want.Action &&
		slices.Equal(cur.Nexthops, want.Nexthops) && samePtr(cur.Nexthop, want.Nexthop) &&
		maps.Equal(cur.ExternalIDs, want.ExternalIDs)
}

func sameStaticRoute(cur, want *nbdb.LogicalRouterStaticRoute) bool {
	return cur.IPPrefix == want.IPPrefix && cur.Nexthop == want.Nexthop &&
		samePtr(cur.OutputPort, want.OutputPort) && maps.Equal(cur.ExternalIDs, want.ExternalIDs)
}

func samePtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// ownedSync is the router and owner one syncOwnedOps call works on.
type ownedSync struct {
	c     *LiveClient
	lr    *nbdb.LogicalRouter
	owner string
}

// syncOwnedOps makes the rows in rows stamped with o's owner, reroutes aside,
// exactly want,
// matched by key: equal rows are kept, drifted ones rewritten in place, and
// the rest created or deleted with their attach on *field.
func syncOwnedOps[T any](o ownedSync, field *[]string, rows, want []T, at stamp[T], key func(*T) string, same func(cur, want *T) bool, prefix string) ([]ovsdb.Operation, error) {
	wanted := make(map[string]bool, len(want))
	for i := range want {
		wanted[key(&want[i])] = true
	}
	have := map[string]*T{}
	var ops []ovsdb.Operation
	var stale []string
	for i := range rows {
		uuid, ext := at(&rows[i])
		if ext[ExternalIDTGWAttachmentID] != o.owner || ext[ExternalIDRole] == TransitGatewayRerouteRole {
			continue
		}
		k := key(&rows[i])
		if _, dup := have[k]; wanted[k] && !dup {
			have[k] = &rows[i]
			continue
		}
		del, err := o.c.client.Where(&rows[i]).Delete()
		if err != nil {
			return nil, fmt.Errorf("delete stale row %s of %s ops: %w", k, o.owner, err)
		}
		ops = append(ops, del...)
		stale = append(stale, *uuid)
	}
	if len(stale) > 0 {
		m, err := o.c.client.Where(o.lr).Mutate(o.lr, model.Mutation{Field: field, Mutator: "delete", Value: stale})
		if err != nil {
			return nil, fmt.Errorf("detach stale rows of %s ops: %w", o.owner, err)
		}
		ops = append(m, ops...)
	}
	var created []string
	for i := range want {
		row := want[i]
		k := key(&row)
		uuid, _ := at(&row)
		if cur, ok := have[k]; ok {
			if same(cur, &row) {
				continue
			}
			curUUID, _ := at(cur)
			*uuid = *curUUID
			upd, err := o.c.client.Where(&row).Update(&row)
			if err != nil {
				return nil, fmt.Errorf("update row %s of %s ops: %w", k, o.owner, err)
			}
			ops = append(ops, upd...)
			continue
		}
		*uuid = namedUUID(prefix, fmt.Sprintf("%s_%s_%d", o.lr.Name, o.owner, i))
		create, err := o.c.client.Create(&row)
		if err != nil {
			return nil, fmt.Errorf("create row %s of %s ops: %w", k, o.owner, err)
		}
		ops = append(ops, create...)
		created = append(created, *uuid)
	}
	if len(created) > 0 {
		m, err := o.c.client.Where(o.lr).Mutate(o.lr, model.Mutation{Field: field, Mutator: "insert", Value: created})
		if err != nil {
			return nil, fmt.Errorf("attach rows of %s ops: %w", o.owner, err)
		}
		ops = append(ops, m...)
	}
	return ops, nil
}
