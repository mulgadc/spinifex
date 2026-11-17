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
		guardOps, err := c.upsertGuardOps(ctx, lr, end.Guard)
		if err != nil {
			return err
		}
		ops = append(ops, portOps...)
		ops = append(ops, guardOps...)
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

// upsertGuardOps rewrites guard in place when lr holds a policy at its
// priority and match, or creates it, and removes the connection's other guards.
func (c *LiveClient) upsertGuardOps(ctx context.Context, lr *nbdb.LogicalRouter, guard nbdb.LogicalRouterPolicy) ([]ovsdb.Operation, error) {
	policies, err := c.ListLogicalRouterPolicies(ctx, lr.Name)
	if err != nil {
		return nil, err
	}
	pcxID := guard.ExternalIDs[ExternalIDPcxID]
	var ops []ovsdb.Operation
	var stale []string
	for i := range policies {
		p := &policies[i]
		switch {
		case p.Priority == guard.Priority && p.Match == guard.Match:
			guard.UUID = p.UUID
		case p.ExternalIDs[ExternalIDPcxID] == pcxID && p.ExternalIDs["spinifex:role"] == PeeringGuardRole:
			stale = append(stale, p.UUID)
			del, err := c.client.Where(p).Delete()
			if err != nil {
				return nil, fmt.Errorf("delete stale peering guard ops: %w", err)
			}
			ops = append(ops, del...)
		}
	}
	if len(stale) > 0 {
		m, err := c.client.Where(lr).Mutate(lr, model.Mutation{Field: &lr.Policies, Mutator: "delete", Value: stale})
		if err != nil {
			return nil, fmt.Errorf("detach stale peering guards ops: %w", err)
		}
		ops = append(m, ops...)
	}
	if guard.UUID != "" {
		upd, err := c.client.Where(&guard).Update(&guard)
		if err != nil {
			return nil, fmt.Errorf("update peering guard ops: %w", err)
		}
		return append(ops, upd...), nil
	}
	guard.UUID = namedUUID("lrp_", fmt.Sprintf("%d:%s", guard.Priority, guard.Match))
	create, err := c.client.Create(&guard)
	if err != nil {
		return nil, fmt.Errorf("create peering guard ops: %w", err)
	}
	m, err := c.client.Where(lr).Mutate(lr, model.Mutation{Field: &lr.Policies, Mutator: "insert", Value: []string{guard.UUID}})
	if err != nil {
		return nil, fmt.Errorf("attach peering guard ops: %w", err)
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

// deletePeeringRowsOps detaches and deletes lr's ports and policies stamped pcxID.
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
	var ops []ovsdb.Operation
	var portUUIDs, policyUUIDs []string
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
	var mutations []model.Mutation
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
