package ovn

import (
	"context"
	"fmt"

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
		policies, err := c.ListLogicalRouterPolicies(ctx, lr.Name)
		if err != nil {
			return err
		}
		guardOps, err := c.upsertPeeringPolicyOps(lr, policies, end.Guard)
		if err != nil {
			return err
		}
		noRouteOps, err := c.upsertPeeringPolicyOps(lr, policies, end.NoRoute)
		if err != nil {
			return err
		}
		entryOps, err := c.upsertEntryRouteOps(ctx, lr, end.Entry)
		if err != nil {
			return err
		}
		ops = append(ops, portOps...)
		ops = append(ops, guardOps...)
		ops = append(ops, noRouteOps...)
		ops = append(ops, entryOps...)
	}
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("peering link transact: %w", err)
	}
	return nil
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

// upsertPeeringPolicyOps rewrites the connection's policy of want's role on
// lr in place, or creates it, and removes any other policy of that role.
func (c *LiveClient) upsertPeeringPolicyOps(lr *nbdb.LogicalRouter, policies []nbdb.LogicalRouterPolicy, want nbdb.LogicalRouterPolicy) ([]ovsdb.Operation, error) {
	pcxID, role := want.ExternalIDs[ExternalIDPcxID], want.ExternalIDs["spinifex:role"]
	var ops []ovsdb.Operation
	var stale []string
	for i := range policies {
		p := &policies[i]
		if p.ExternalIDs[ExternalIDPcxID] != pcxID || p.ExternalIDs["spinifex:role"] != role {
			continue
		}
		if want.UUID == "" {
			want.UUID = p.UUID
			continue
		}
		stale = append(stale, p.UUID)
		del, err := c.client.Where(p).Delete()
		if err != nil {
			return nil, fmt.Errorf("delete stale %s ops: %w", role, err)
		}
		ops = append(ops, del...)
	}
	if len(stale) > 0 {
		m, err := c.client.Where(lr).Mutate(lr, model.Mutation{Field: &lr.Policies, Mutator: "delete", Value: stale})
		if err != nil {
			return nil, fmt.Errorf("detach stale %s ops: %w", role, err)
		}
		ops = append(m, ops...)
	}
	if want.UUID != "" {
		upd, err := c.client.Where(&want).Update(&want)
		if err != nil {
			return nil, fmt.Errorf("update %s ops: %w", role, err)
		}
		return append(ops, upd...), nil
	}
	want.UUID = namedUUID("lrp_", fmt.Sprintf("%s:%s:%s", lr.Name, pcxID, role))
	create, err := c.client.Create(&want)
	if err != nil {
		return nil, fmt.Errorf("create %s ops: %w", role, err)
	}
	m, err := c.client.Where(lr).Mutate(lr, model.Mutation{Field: &lr.Policies, Mutator: "insert", Value: []string{want.UUID}})
	if err != nil {
		return nil, fmt.Errorf("attach %s ops: %w", role, err)
	}
	return append(append(ops, create...), m...), nil
}

// upsertEntryRouteOps rewrites the connection's static route on lr in place,
// or creates it, and removes any other static route the connection holds there.
func (c *LiveClient) upsertEntryRouteOps(ctx context.Context, lr *nbdb.LogicalRouter, want nbdb.LogicalRouterStaticRoute) ([]ovsdb.Operation, error) {
	routes, err := c.ListStaticRoutes(ctx, lr.Name)
	if err != nil {
		return nil, err
	}
	pcxID := want.ExternalIDs[ExternalIDPcxID]
	var ops []ovsdb.Operation
	var stale []string
	for i := range routes {
		r := &routes[i]
		if r.ExternalIDs[ExternalIDPcxID] != pcxID {
			continue
		}
		if want.UUID == "" {
			want.UUID = r.UUID
			continue
		}
		stale = append(stale, r.UUID)
		del, err := c.client.Where(r).Delete()
		if err != nil {
			return nil, fmt.Errorf("delete stale peering entry route ops: %w", err)
		}
		ops = append(ops, del...)
	}
	if len(stale) > 0 {
		m, err := c.client.Where(lr).Mutate(lr, model.Mutation{Field: &lr.StaticRoutes, Mutator: "delete", Value: stale})
		if err != nil {
			return nil, fmt.Errorf("detach stale peering entry routes ops: %w", err)
		}
		ops = append(m, ops...)
	}
	if want.UUID != "" {
		upd, err := c.client.Where(&want).Update(&want)
		if err != nil {
			return nil, fmt.Errorf("update peering entry route ops: %w", err)
		}
		return append(ops, upd...), nil
	}
	want.UUID = namedUUID("route_", lr.Name+":"+pcxID)
	create, err := c.client.Create(&want)
	if err != nil {
		return nil, fmt.Errorf("create peering entry route ops: %w", err)
	}
	m, err := c.client.Where(lr).Mutate(lr, model.Mutation{Field: &lr.StaticRoutes, Mutator: "insert", Value: []string{want.UUID}})
	if err != nil {
		return nil, fmt.Errorf("attach peering entry route ops: %w", err)
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
	owned := uuidSet(lr.Ports)
	var ports []nbdb.LogicalRouterPort
	if err := c.client.WhereCache(func(p *nbdb.LogicalRouterPort) bool {
		_, ok := owned[p.UUID]
		return ok && p.ExternalIDs[ExternalIDPcxID] == pcxID
	}).List(ctx, &ports); err != nil {
		return nil, fmt.Errorf("list peering ports on %s: %w", lr.Name, err)
	}
	policies, err := c.ListLogicalRouterPolicies(ctx, lr.Name)
	if err != nil {
		return nil, err
	}
	routes, err := c.ListStaticRoutes(ctx, lr.Name)
	if err != nil {
		return nil, err
	}
	var ops []ovsdb.Operation
	var portUUIDs, policyUUIDs, routeUUIDs []string
	for i := range ports {
		portUUIDs = append(portUUIDs, ports[i].UUID)
		del, err := c.client.Where(&ports[i]).Delete()
		if err != nil {
			return nil, fmt.Errorf("delete peering port ops: %w", err)
		}
		ops = append(ops, del...)
	}
	for i := range policies {
		if policies[i].ExternalIDs[ExternalIDPcxID] != pcxID {
			continue
		}
		policyUUIDs = append(policyUUIDs, policies[i].UUID)
		del, err := c.client.Where(&policies[i]).Delete()
		if err != nil {
			return nil, fmt.Errorf("delete peering policy ops: %w", err)
		}
		ops = append(ops, del...)
	}
	for i := range routes {
		if routes[i].ExternalIDs[ExternalIDPcxID] != pcxID {
			continue
		}
		routeUUIDs = append(routeUUIDs, routes[i].UUID)
		del, err := c.client.Where(&routes[i]).Delete()
		if err != nil {
			return nil, fmt.Errorf("delete peering entry route ops: %w", err)
		}
		ops = append(ops, del...)
	}
	var mutations []model.Mutation
	if len(routeUUIDs) > 0 {
		mutations = append(mutations, model.Mutation{Field: &lr.StaticRoutes, Mutator: "delete", Value: routeUUIDs})
	}
	if len(portUUIDs) > 0 {
		mutations = append(mutations, model.Mutation{Field: &lr.Ports, Mutator: "delete", Value: portUUIDs})
	}
	if len(policyUUIDs) > 0 {
		mutations = append(mutations, model.Mutation{Field: &lr.Policies, Mutator: "delete", Value: policyUUIDs})
	}
	if len(mutations) == 0 {
		return nil, nil
	}
	m, err := c.client.Where(lr).Mutate(lr, mutations...)
	if err != nil {
		return nil, fmt.Errorf("detach peering rows from %s ops: %w", lr.Name, err)
	}
	return append(m, ops...), nil
}
