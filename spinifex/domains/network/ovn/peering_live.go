package ovn

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/mulgadc/spinifex/spinifex/domains/network/ovn/nbdb"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

func (c *LiveClient) EnsurePeeringLink(ctx context.Context, ends [2]PeeringLinkEnd) error {
	var ops []ovsdb.Operation
	for _, end := range ends {
		lr, err := c.GetLogicalRouter(ctx, end.Router)
		if err != nil {
			return fmt.Errorf("peering link: %w", err)
		}
		portOps, err := c.upsertRouterPortOps(ctx, lr, end.Port)
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
		guardOps, gerr := upsertOwnedOps(c, lr, &lr.Policies, policies, end.Guard, policyStamp, "lrp_")
		noRouteOps, nerr := upsertOwnedOps(c, lr, &lr.Policies, policies, end.NoRoute, policyStamp, "lrp_")
		entryOps, eerr := upsertOwnedOps(c, lr, &lr.StaticRoutes, routes, end.Entry, routeStamp, "route_")
		if err := errors.Join(gerr, nerr, eerr); err != nil {
			return err
		}
		ops = slices.Concat(ops, portOps, guardOps, noRouteOps, entryOps)
	}
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("peering link transact: %w", err)
	}
	return nil
}

// stamp exposes an NB row's UUID and external IDs to the generic helpers below.
type stamp[T any] func(*T) (uuid *string, ext map[string]string)

func portStamp(r *nbdb.LogicalRouterPort) (*string, map[string]string) { return &r.UUID, r.ExternalIDs }

func policyStamp(r *nbdb.LogicalRouterPolicy) (*string, map[string]string) {
	return &r.UUID, r.ExternalIDs
}

func routeStamp(r *nbdb.LogicalRouterStaticRoute) (*string, map[string]string) {
	return &r.UUID, r.ExternalIDs
}

// rowsIn returns the cached rows of type T whose UUID is in uuids.
func rowsIn[T any](ctx context.Context, c *LiveClient, uuids []string, at stamp[T]) ([]T, error) {
	if len(uuids) == 0 {
		return nil, nil
	}
	set := uuidSet(uuids)
	var rows []T
	err := c.client.WhereCache(func(r *T) bool {
		uuid, _ := at(r)
		_, ok := set[*uuid]
		return ok
	}).List(ctx, &rows)
	return rows, err
}

// upsertRouterPortOps rewrites port in place when a port of its name exists,
// or creates it on lr.
func (c *LiveClient) upsertRouterPortOps(ctx context.Context, lr *nbdb.LogicalRouter, port nbdb.LogicalRouterPort) ([]ovsdb.Operation, error) {
	var existing []nbdb.LogicalRouterPort
	if err := c.client.WhereCache(func(p *nbdb.LogicalRouterPort) bool { return p.Name == port.Name }).List(ctx, &existing); err != nil {
		return nil, fmt.Errorf("find peering port %s: %w", port.Name, err)
	}
	if len(existing) > 0 {
		port.UUID = existing[0].UUID
		ops, err := c.client.Where(&port).Update(&port)
		if err != nil {
			return nil, fmt.Errorf("update peering port %s ops: %w", port.Name, err)
		}
		return ops, nil
	}
	port.UUID = namedUUID("lrp_", port.Name)
	createOps, err := c.client.Create(&port)
	if err != nil {
		return nil, fmt.Errorf("create peering port %s ops: %w", port.Name, err)
	}
	mutateOps, err := c.client.Where(lr).Mutate(lr, model.Mutation{Field: &lr.Ports, Mutator: "insert", Value: []string{port.UUID}})
	if err != nil {
		return nil, fmt.Errorf("attach peering port %s ops: %w", port.Name, err)
	}
	return append(createOps, mutateOps...), nil
}

// upsertOwnedOps rewrites the row of want's connection and role in rows in
// place, or creates it and attaches it to *field on lr, and removes any other
// row of that owner.
func upsertOwnedOps[T any](c *LiveClient, lr *nbdb.LogicalRouter, field *[]string, rows []T, want T, at stamp[T], prefix string) ([]ovsdb.Operation, error) {
	wantUUID, wantExt := at(&want)
	label := fmt.Sprintf("%s:%s:%s", lr.Name, wantExt[ExternalIDPcxID], wantExt[ExternalIDRole])
	var ops []ovsdb.Operation
	var stale []string
	for i := range rows {
		uuid, ext := at(&rows[i])
		if !SamePeeringOwner(ext, wantExt) {
			continue
		}
		if *wantUUID == "" {
			*wantUUID = *uuid
			continue
		}
		stale = append(stale, *uuid)
		del, err := c.client.Where(&rows[i]).Delete()
		if err != nil {
			return nil, fmt.Errorf("delete stale peering row %s ops: %w", label, err)
		}
		ops = append(ops, del...)
	}
	if len(stale) > 0 {
		m, err := c.client.Where(lr).Mutate(lr, model.Mutation{Field: field, Mutator: "delete", Value: stale})
		if err != nil {
			return nil, fmt.Errorf("detach stale peering rows %s ops: %w", label, err)
		}
		ops = append(m, ops...)
	}
	if *wantUUID != "" {
		upd, err := c.client.Where(&want).Update(&want)
		if err != nil {
			return nil, fmt.Errorf("update peering row %s ops: %w", label, err)
		}
		return append(ops, upd...), nil
	}
	*wantUUID = namedUUID(prefix, label)
	create, err := c.client.Create(&want)
	if err != nil {
		return nil, fmt.Errorf("create peering row %s ops: %w", label, err)
	}
	m, err := c.client.Where(lr).Mutate(lr, model.Mutation{Field: field, Mutator: "insert", Value: []string{*wantUUID}})
	if err != nil {
		return nil, fmt.Errorf("attach peering row %s ops: %w", label, err)
	}
	return append(append(ops, create...), m...), nil
}

func (c *LiveClient) DeletePeeringLink(ctx context.Context, pcxID string, routers []string) error {
	var ops []ovsdb.Operation
	for _, name := range routers {
		var found []nbdb.LogicalRouter
		if err := c.client.WhereCache(func(lr *nbdb.LogicalRouter) bool { return lr.Name == name }).List(ctx, &found); err != nil {
			return fmt.Errorf("find router %s: %w", name, err)
		}
		if len(found) == 0 {
			continue
		}
		lr := &found[0]
		routerOps, err := c.deletePeeringRowsOps(ctx, lr, pcxID)
		if err != nil {
			return err
		}
		ops = append(ops, routerOps...)
	}
	if len(ops) == 0 {
		return nil
	}
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("delete peering link transact: %w", err)
	}
	return nil
}

// deletePeeringRowsOps detaches and deletes lr's ports, policies and static routes stamped pcxID.
func (c *LiveClient) deletePeeringRowsOps(ctx context.Context, lr *nbdb.LogicalRouter, pcxID string) ([]ovsdb.Operation, error) {
	ports, err := rowsIn(ctx, c, lr.Ports, portStamp)
	if err != nil {
		return nil, fmt.Errorf("list ports on %s: %w", lr.Name, err)
	}
	policies, err := rowsIn(ctx, c, lr.Policies, policyStamp)
	if err != nil {
		return nil, fmt.Errorf("list policies on %s: %w", lr.Name, err)
	}
	routes, err := rowsIn(ctx, c, lr.StaticRoutes, routeStamp)
	if err != nil {
		return nil, fmt.Errorf("list static routes on %s: %w", lr.Name, err)
	}
	var d rowDeletes
	if err := errors.Join(
		deleteOwned(c, &d, &lr.Ports, ports, portStamp, pcxID),
		deleteOwned(c, &d, &lr.Policies, policies, policyStamp, pcxID),
		deleteOwned(c, &d, &lr.StaticRoutes, routes, routeStamp, pcxID),
	); err != nil {
		return nil, err
	}
	if len(d.mutations) == 0 {
		return nil, nil
	}
	m, err := c.client.Where(lr).Mutate(lr, d.mutations...)
	if err != nil {
		return nil, fmt.Errorf("detach peering rows from %s ops: %w", lr.Name, err)
	}
	return append(m, d.ops...), nil
}

// rowDeletes collects the delete ops and router detach mutations of one router's rows.
type rowDeletes struct {
	ops       []ovsdb.Operation
	mutations []model.Mutation
}

// deleteOwned adds the deletion of each row in rows stamped pcxID, and its
// detach from *field, to d.
func deleteOwned[T any](c *LiveClient, d *rowDeletes, field *[]string, rows []T, at stamp[T], pcxID string) error {
	var uuids []string
	for i := range rows {
		uuid, ext := at(&rows[i])
		if ext[ExternalIDPcxID] != pcxID {
			continue
		}
		del, err := c.client.Where(&rows[i]).Delete()
		if err != nil {
			return fmt.Errorf("delete peering row ops: %w", err)
		}
		d.ops = append(d.ops, del...)
		uuids = append(uuids, *uuid)
	}
	if len(uuids) > 0 {
		d.mutations = append(d.mutations, model.Mutation{Field: field, Mutator: "delete", Value: uuids})
	}
	return nil
}
