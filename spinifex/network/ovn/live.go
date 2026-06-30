package ovn

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/mulgadc/spinifex/spinifex/network/ovn/nbdb"
	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

// ensureRefetchTimeout bounds the post-conflict refetch wait. 200ms tolerates
// clustered NB raft follower lag under steady-state.
const ensureRefetchTimeout = 200 * time.Millisecond

// ensureRefetchInterval polls at libovsdb's monitor flush cadence.
const ensureRefetchInterval = 20 * time.Millisecond

// ensureWaitTimeoutMS=0 makes the wait-op fail fast so we fall through to the
// refetch path on conflict.
const ensureWaitTimeoutMS = 0

// inactivityTimeout is the Echo-probe cadence; a missed probe marks the NB peer
// dead and triggers reconnect. Must exceed reconnectTimeout (libovsdb invariant).
const inactivityTimeout = 10 * time.Second

// reconnectTimeout bounds each reconnect attempt's Connect context.
const reconnectTimeout = 5 * time.Second

// reconnectMaxInterval caps exponential backoff between reconnect attempts.
const reconnectMaxInterval = 30 * time.Second

// ensureNamedRowOps returns ops that insert createObj unless a row with the same
// Name exists. The wait-op serialises concurrent writers; `until=!=` is required
// because RFC 7047 §5.2.6 mandates a non-empty `rows` member.
func (c *LiveClient) ensureNamedRowOps(table, name string, createObj model.Model) ([]ovsdb.Operation, error) {
	createOps, err := c.client.Create(createObj)
	if err != nil {
		return nil, fmt.Errorf("create %s ops: %w", table, err)
	}
	timeout := ensureWaitTimeoutMS
	waitOp := ovsdb.Operation{
		Op:      ovsdb.OperationWait,
		Table:   table,
		Timeout: &timeout,
		Where:   []ovsdb.Condition{{Column: "name", Function: ovsdb.ConditionEqual, Value: name}},
		Columns: []string{"name"},
		Until:   string(ovsdb.WaitConditionNotEqual),
		Rows:    []ovsdb.Row{{"name": name}},
	}
	return append([]ovsdb.Operation{waitOp}, createOps...), nil
}

// transactOps runs ops as one transaction and checks per-op results.
func (c *LiveClient) transactOps(ctx context.Context, ops []ovsdb.Operation) error {
	results, err := c.client.Transact(ctx, ops...)
	if err != nil {
		return err
	}
	_, err = ovsdb.CheckOperationResults(results, ops)
	if err != nil {
		for i, r := range results {
			if r.Error != "" {
				opTable := ""
				if i < len(ops) {
					opTable = fmt.Sprintf("%s on %s", ops[i].Op, ops[i].Table)
				}
				slog.Error("OVSDB operation failed", "index", i, "op", opTable, "error", r.Error, "details", r.Details)
			}
		}
	}
	return err
}

// namedUUID builds a valid OVSDB named-uuid by replacing non-[_a-zA-Z0-9]
// bytes with `_`. Required for ACL UUIDs embedding match expressions.
func namedUUID(prefix, name string) string {
	s := prefix + name
	result := make([]byte, len(s))
	for i := range s {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_':
			result[i] = c
		default:
			result[i] = '_'
		}
	}
	return string(result)
}

// LiveClient implements Client using libovsdb against a real OVN NB DB.
type LiveClient struct {
	endpoint string
	client   client.Client
}

var _ Client = (*LiveClient)(nil)

// NewLiveClient creates a LiveClient targeting an OVN NB DB endpoint
// ("tcp:host:port" or "unix:/path/to/socket").
func NewLiveClient(endpoint string) *LiveClient {
	return &LiveClient{endpoint: endpoint}
}

func (c *LiveClient) Connect(ctx context.Context) error {
	dbModel, err := nbdb.FullDatabaseModel()
	if err != nil {
		return fmt.Errorf("failed to create database model: %w", err)
	}

	// Infinite backoff (MaxElapsedTime=0) so a daemon keeps reconnecting across a
	// long NB outage. WithInactivityCheck enables reconnect and adds the Echo
	// probe that detects a dead/half-open socket the OS never reports.
	bo := backoff.NewExponentialBackOff(
		backoff.WithMaxElapsedTime(0),
		backoff.WithMaxInterval(reconnectMaxInterval),
	)
	ovn, err := client.NewOVSDBClient(dbModel,
		client.WithEndpoint(c.endpoint),
		client.WithInactivityCheck(inactivityTimeout, reconnectTimeout, bo),
	)
	if err != nil {
		return fmt.Errorf("failed to create OVSDB client: %w", err)
	}

	if err := ovn.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect to OVN NB DB at %s: %w", c.endpoint, err)
	}

	_, err = ovn.MonitorAll(ctx)
	if err != nil {
		ovn.Close()
		return fmt.Errorf("failed to monitor OVN NB DB: %w", err)
	}

	c.client = ovn
	slog.Info("Connected to OVN NB DB", "endpoint", c.endpoint)
	return nil
}

func (c *LiveClient) Close() {
	if c.client != nil {
		c.client.Close()
		slog.Info("Disconnected from OVN NB DB")
	}
}

func (c *LiveClient) Connected() bool {
	return c.client != nil
}

func (c *LiveClient) CreateLogicalSwitch(ctx context.Context, ls *nbdb.LogicalSwitch) error {
	ops, err := c.client.Create(ls)
	if err != nil {
		return fmt.Errorf("create logical switch ops: %w", err)
	}
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("create logical switch transact: %w", err)
	}
	return nil
}

func (c *LiveClient) EnsureLogicalSwitch(ctx context.Context, ls *nbdb.LogicalSwitch) (*nbdb.LogicalSwitch, error) {
	if existing, err := c.GetLogicalSwitch(ctx, ls.Name); err == nil {
		return existing, nil
	}
	if ls.UUID == "" {
		ls.UUID = namedUUID("ls_", ls.Name)
	}
	ops, err := c.ensureNamedRowOps("Logical_Switch", ls.Name, ls)
	if err != nil {
		return nil, err
	}
	if err := c.transactOps(ctx, ops); err != nil {
		if existing, refErr := c.waitForCachedSwitch(ctx, ls.Name); refErr == nil {
			slog.Info("ovn: EnsureLogicalSwitch lost race, reusing existing",
				"name", ls.Name, "existing_uuid", existing.UUID)
			return existing, nil
		}
		return nil, fmt.Errorf("ensure logical switch %q: %w", ls.Name, err)
	}
	return ls, nil
}

func (c *LiveClient) waitForCachedSwitch(ctx context.Context, name string) (*nbdb.LogicalSwitch, error) {
	deadline := time.Now().Add(ensureRefetchTimeout)
	var lastErr error
	for {
		v, err := c.GetLogicalSwitch(ctx, name)
		if err == nil {
			return v, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(ensureRefetchInterval):
		}
	}
}

func (c *LiveClient) DeleteLogicalSwitch(ctx context.Context, name string) error {
	ls, err := c.GetLogicalSwitch(ctx, name)
	if err != nil {
		return fmt.Errorf("delete logical switch lookup: %w", err)
	}
	ops, err := c.client.Where(ls).Delete()
	if err != nil {
		return fmt.Errorf("delete logical switch ops: %w", err)
	}
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("delete logical switch transact: %w", err)
	}
	return nil
}

func (c *LiveClient) GetLogicalSwitch(ctx context.Context, name string) (*nbdb.LogicalSwitch, error) {
	var switches []nbdb.LogicalSwitch
	err := c.client.WhereCache(func(ls *nbdb.LogicalSwitch) bool {
		return ls.Name == name
	}).List(ctx, &switches)
	if err != nil {
		return nil, fmt.Errorf("get logical switch: %w", err)
	}
	if len(switches) == 0 {
		return nil, fmt.Errorf("logical switch %q not found", name)
	}
	return &switches[0], nil
}

func (c *LiveClient) ListLogicalSwitches(ctx context.Context) ([]nbdb.LogicalSwitch, error) {
	var switches []nbdb.LogicalSwitch
	err := c.client.List(ctx, &switches)
	if err != nil {
		return nil, fmt.Errorf("list logical switches: %w", err)
	}
	return switches, nil
}

func (c *LiveClient) CreateLogicalSwitchPort(ctx context.Context, switchName string, lsp *nbdb.LogicalSwitchPort) error {
	if lsp.UUID == "" {
		lsp.UUID = namedUUID("lsp_", lsp.Name)
	}

	createOps, err := c.client.Create(lsp)
	if err != nil {
		return fmt.Errorf("create logical switch port ops: %w", err)
	}

	ls, err := c.GetLogicalSwitch(ctx, switchName)
	if err != nil {
		return fmt.Errorf("get logical switch for port add: %w", err)
	}

	mutateOps, err := c.client.Where(ls).Mutate(ls, model.Mutation{
		Field:   &ls.Ports,
		Mutator: "insert",
		Value:   []string{lsp.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate logical switch ports ops: %w", err)
	}

	ops := append(createOps, mutateOps...)
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("create logical switch port transact: %w", err)
	}
	return nil
}

// CreateLogicalSwitchPortInGroups bundles LSP create, switch-ports mutate, and
// per-PG ports mutates into one transaction so the LSP's named UUID resolves across all ops.
func (c *LiveClient) CreateLogicalSwitchPortInGroups(ctx context.Context, switchName string, lsp *nbdb.LogicalSwitchPort, portGroupNames []string) error {
	if lsp.UUID == "" {
		lsp.UUID = namedUUID("lsp_", lsp.Name)
	}

	createOps, err := c.client.Create(lsp)
	if err != nil {
		return fmt.Errorf("create logical switch port ops: %w", err)
	}

	ls, err := c.GetLogicalSwitch(ctx, switchName)
	if err != nil {
		return fmt.Errorf("get logical switch for port add: %w", err)
	}

	switchMutateOps, err := c.client.Where(ls).Mutate(ls, model.Mutation{
		Field:   &ls.Ports,
		Mutator: "insert",
		Value:   []string{lsp.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate logical switch ports ops: %w", err)
	}

	ops := append(createOps, switchMutateOps...)

	for _, pgName := range portGroupNames {
		pg, err := c.getPortGroup(ctx, pgName)
		if err != nil {
			return fmt.Errorf("get port group %s for port add: %w", pgName, err)
		}
		pgMutateOps, err := c.client.Where(pg).Mutate(pg, model.Mutation{
			Field:   &pg.Ports,
			Mutator: ovsdb.MutateOperationInsert,
			Value:   []string{lsp.UUID},
		})
		if err != nil {
			return fmt.Errorf("mutate port group %s ops: %w", pgName, err)
		}
		ops = append(ops, pgMutateOps...)
	}

	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("create logical switch port in groups transact: %w", err)
	}
	return nil
}

func (c *LiveClient) DeleteLogicalSwitchPort(ctx context.Context, switchName string, portName string) error {
	lsp, err := c.GetLogicalSwitchPort(ctx, portName)
	if err != nil {
		return fmt.Errorf("get logical switch port for delete: %w", err)
	}

	ls, err := c.GetLogicalSwitch(ctx, switchName)
	if err != nil {
		return fmt.Errorf("get logical switch for port delete: %w", err)
	}

	mutateOps, err := c.client.Where(ls).Mutate(ls, model.Mutation{
		Field:   &ls.Ports,
		Mutator: "delete",
		Value:   []string{lsp.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate logical switch ports ops: %w", err)
	}

	deleteOps, err := c.client.Where(lsp).Delete()
	if err != nil {
		return fmt.Errorf("delete logical switch port ops: %w", err)
	}

	ops := append(mutateOps, deleteOps...)
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("delete logical switch port transact: %w", err)
	}
	return nil
}

func (c *LiveClient) GetLogicalSwitchPort(ctx context.Context, name string) (*nbdb.LogicalSwitchPort, error) {
	var ports []nbdb.LogicalSwitchPort
	err := c.client.WhereCache(func(lsp *nbdb.LogicalSwitchPort) bool {
		return lsp.Name == name
	}).List(ctx, &ports)
	if err != nil {
		return nil, fmt.Errorf("get logical switch port: %w", err)
	}
	if len(ports) == 0 {
		return nil, fmt.Errorf("logical switch port %q not found", name)
	}
	return &ports[0], nil
}

func (c *LiveClient) ListLogicalSwitchPorts(ctx context.Context) ([]nbdb.LogicalSwitchPort, error) {
	var ports []nbdb.LogicalSwitchPort
	if err := c.client.List(ctx, &ports); err != nil {
		return nil, fmt.Errorf("list logical switch ports: %w", err)
	}
	return ports, nil
}

func (c *LiveClient) UpdateLogicalSwitchPort(ctx context.Context, lsp *nbdb.LogicalSwitchPort) error {
	if lsp.UUID == "" {
		existing, err := c.GetLogicalSwitchPort(ctx, lsp.Name)
		if err != nil {
			return fmt.Errorf("get logical switch port for update: %w", err)
		}
		lsp.UUID = existing.UUID
	}
	ops, err := c.client.Where(lsp).Update(lsp)
	if err != nil {
		return fmt.Errorf("update logical switch port ops: %w", err)
	}
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("update logical switch port transact: %w", err)
	}
	return nil
}

func (c *LiveClient) CreateLogicalRouter(ctx context.Context, lr *nbdb.LogicalRouter) error {
	ops, err := c.client.Create(lr)
	if err != nil {
		return fmt.Errorf("create logical router ops: %w", err)
	}
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("create logical router transact: %w", err)
	}
	return nil
}

func (c *LiveClient) EnsureLogicalRouter(ctx context.Context, lr *nbdb.LogicalRouter) (*nbdb.LogicalRouter, error) {
	if existing, err := c.GetLogicalRouter(ctx, lr.Name); err == nil {
		return existing, nil
	}
	if lr.UUID == "" {
		lr.UUID = namedUUID("lr_", lr.Name)
	}
	ops, err := c.ensureNamedRowOps("Logical_Router", lr.Name, lr)
	if err != nil {
		return nil, err
	}
	if err := c.transactOps(ctx, ops); err != nil {
		if existing, refErr := c.waitForCachedRouter(ctx, lr.Name); refErr == nil {
			slog.Info("ovn: EnsureLogicalRouter lost race, reusing existing",
				"name", lr.Name, "existing_uuid", existing.UUID)
			return existing, nil
		}
		return nil, fmt.Errorf("ensure logical router %q: %w", lr.Name, err)
	}
	return lr, nil
}

func (c *LiveClient) UpdateLogicalRouterExternalIDs(ctx context.Context, name string, externalIDs map[string]string) error {
	lr, err := c.GetLogicalRouter(ctx, name)
	if err != nil {
		return fmt.Errorf("get logical router for external_ids update: %w", err)
	}
	lr.ExternalIDs = externalIDs
	ops, err := c.client.Where(lr).Update(lr, &lr.ExternalIDs)
	if err != nil {
		return fmt.Errorf("update logical router external_ids ops: %w", err)
	}
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("update logical router external_ids transact: %w", err)
	}
	return nil
}

func (c *LiveClient) waitForCachedRouter(ctx context.Context, name string) (*nbdb.LogicalRouter, error) {
	deadline := time.Now().Add(ensureRefetchTimeout)
	var lastErr error
	for {
		v, err := c.GetLogicalRouter(ctx, name)
		if err == nil {
			return v, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(ensureRefetchInterval):
		}
	}
}

func (c *LiveClient) DeleteLogicalRouter(ctx context.Context, name string) error {
	lr, err := c.GetLogicalRouter(ctx, name)
	if err != nil {
		return fmt.Errorf("delete logical router lookup: %w", err)
	}
	ops, err := c.client.Where(lr).Delete()
	if err != nil {
		return fmt.Errorf("delete logical router ops: %w", err)
	}
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("delete logical router transact: %w", err)
	}
	return nil
}

func (c *LiveClient) GetLogicalRouter(ctx context.Context, name string) (*nbdb.LogicalRouter, error) {
	var routers []nbdb.LogicalRouter
	err := c.client.WhereCache(func(lr *nbdb.LogicalRouter) bool {
		return lr.Name == name
	}).List(ctx, &routers)
	if err != nil {
		return nil, fmt.Errorf("get logical router: %w", err)
	}
	if len(routers) == 0 {
		return nil, fmt.Errorf("logical router %q not found", name)
	}
	return &routers[0], nil
}

func (c *LiveClient) ListLogicalRouters(ctx context.Context) ([]nbdb.LogicalRouter, error) {
	var routers []nbdb.LogicalRouter
	err := c.client.List(ctx, &routers)
	if err != nil {
		return nil, fmt.Errorf("list logical routers: %w", err)
	}
	return routers, nil
}

func (c *LiveClient) CreateLogicalRouterPort(ctx context.Context, routerName string, lrp *nbdb.LogicalRouterPort) error {
	if lrp.UUID == "" {
		lrp.UUID = namedUUID("lrp_", lrp.Name)
	}

	createOps, err := c.client.Create(lrp)
	if err != nil {
		return fmt.Errorf("create logical router port ops: %w", err)
	}

	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return fmt.Errorf("get logical router for port add: %w", err)
	}

	mutateOps, err := c.client.Where(lr).Mutate(lr, model.Mutation{
		Field:   &lr.Ports,
		Mutator: "insert",
		Value:   []string{lrp.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate logical router ports ops: %w", err)
	}

	ops := append(createOps, mutateOps...)
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("create logical router port transact: %w", err)
	}
	return nil
}

func (c *LiveClient) DeleteLogicalRouterPort(ctx context.Context, routerName string, portName string) error {
	lrp, err := c.GetLogicalRouterPort(ctx, portName)
	if err != nil {
		return fmt.Errorf("get logical router port for delete: %w", err)
	}

	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return fmt.Errorf("get logical router for port delete: %w", err)
	}

	mutateOps, err := c.client.Where(lr).Mutate(lr, model.Mutation{
		Field:   &lr.Ports,
		Mutator: "delete",
		Value:   []string{lrp.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate logical router ports ops: %w", err)
	}

	deleteOps, err := c.client.Where(lrp).Delete()
	if err != nil {
		return fmt.Errorf("delete logical router port ops: %w", err)
	}

	ops := append(mutateOps, deleteOps...)
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("delete logical router port transact: %w", err)
	}
	return nil
}

func (c *LiveClient) GetLogicalRouterPort(ctx context.Context, name string) (*nbdb.LogicalRouterPort, error) {
	var ports []nbdb.LogicalRouterPort
	err := c.client.WhereCache(func(lrp *nbdb.LogicalRouterPort) bool {
		return lrp.Name == name
	}).List(ctx, &ports)
	if err != nil {
		return nil, fmt.Errorf("get logical router port: %w", err)
	}
	if len(ports) == 0 {
		return nil, fmt.Errorf("logical router port %q not found", name)
	}
	return &ports[0], nil
}

// UpdateLogicalRouterPort rewrites mutable columns on an existing LRP.
func (c *LiveClient) UpdateLogicalRouterPort(ctx context.Context, lrp *nbdb.LogicalRouterPort) error {
	if lrp.UUID == "" {
		existing, err := c.GetLogicalRouterPort(ctx, lrp.Name)
		if err != nil {
			return fmt.Errorf("get logical router port for update: %w", err)
		}
		lrp.UUID = existing.UUID
	}
	ops, err := c.client.Where(lrp).Update(lrp)
	if err != nil {
		return fmt.Errorf("update logical router port ops: %w", err)
	}
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("update logical router port transact: %w", err)
	}
	return nil
}

// SetGatewayChassis binds a chassis to an LRP for HA gateway scheduling.
// Idempotent: absent → create+mutate; same priority → no-op; different priority → update.
func (c *LiveClient) SetGatewayChassis(ctx context.Context, lrpName string, chassisName string, priority int) error {
	gcName := lrpName + "-" + chassisName
	existing, err := c.GetGatewayChassisByName(ctx, gcName)
	if err != nil {
		return fmt.Errorf("get existing gateway chassis: %w", err)
	}
	if existing != nil {
		if existing.Priority == priority {
			return nil
		}
		return c.updateGatewayChassisPriority(ctx, existing, priority)
	}

	lrp, err := c.GetLogicalRouterPort(ctx, lrpName)
	if err != nil {
		return fmt.Errorf("get logical router port for gateway chassis: %w", err)
	}

	gc := &nbdb.GatewayChassis{
		UUID:        namedUUID("gc_", gcName),
		Name:        gcName,
		ChassisName: chassisName,
		Priority:    priority,
		ExternalIDs: map[string]string{},
		Options:     map[string]string{},
	}

	createOps, err := c.client.Create(gc)
	if err != nil {
		return fmt.Errorf("create gateway chassis ops: %w", err)
	}

	mutateOps, err := c.client.Where(lrp).Mutate(lrp, model.Mutation{
		Field:   &lrp.GatewayChassis,
		Mutator: "insert",
		Value:   []string{gc.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate logical router port gateway_chassis ops: %w", err)
	}

	ops := append(createOps, mutateOps...)
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("set gateway chassis transact: %w", err)
	}
	return nil
}

func (c *LiveClient) updateGatewayChassisPriority(ctx context.Context, gc *nbdb.GatewayChassis, priority int) error {
	gc.Priority = priority
	ops, err := c.client.Where(gc).Update(gc, &gc.Priority)
	if err != nil {
		return fmt.Errorf("update gateway_chassis priority ops: %w", err)
	}
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("update gateway_chassis priority transact: %w", err)
	}
	return nil
}

// GetGatewayChassisByName looks up a Gateway_Chassis row by its `name` column
// (the "lrpName-chassisName" form). Returns (nil, nil) when no row matches.
func (c *LiveClient) GetGatewayChassisByName(ctx context.Context, name string) (*nbdb.GatewayChassis, error) {
	var rows []nbdb.GatewayChassis
	err := c.client.WhereCache(func(gc *nbdb.GatewayChassis) bool {
		return gc.Name == name
	}).List(ctx, &rows)
	if err != nil {
		return nil, fmt.Errorf("list gateway_chassis by name: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// ListGatewayChassis returns every Gateway_Chassis row.
func (c *LiveClient) ListGatewayChassis(ctx context.Context) ([]nbdb.GatewayChassis, error) {
	var rows []nbdb.GatewayChassis
	if err := c.client.List(ctx, &rows); err != nil {
		return nil, fmt.Errorf("list gateway_chassis: %w", err)
	}
	return rows, nil
}

// DeleteGatewayChassis removes a Gateway_Chassis row and detaches it from
// the owning LRP in one transaction.
func (c *LiveClient) DeleteGatewayChassis(ctx context.Context, lrpName string, gcUUID string) error {
	lrp, err := c.GetLogicalRouterPort(ctx, lrpName)
	if err != nil {
		return fmt.Errorf("get logical router port for gateway chassis delete: %w", err)
	}

	mutateOps, err := c.client.Where(lrp).Mutate(lrp, model.Mutation{
		Field:   &lrp.GatewayChassis,
		Mutator: "delete",
		Value:   []string{gcUUID},
	})
	if err != nil {
		return fmt.Errorf("mutate logical router port gateway_chassis (delete) ops: %w", err)
	}

	gc := &nbdb.GatewayChassis{UUID: gcUUID}
	deleteOps, err := c.client.Where(gc).Delete()
	if err != nil {
		return fmt.Errorf("delete gateway_chassis ops: %w", err)
	}

	ops := append(mutateOps, deleteOps...)
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("delete gateway_chassis transact: %w", err)
	}
	return nil
}

// ListLogicalRouterPorts returns every LRP across all routers.
func (c *LiveClient) ListLogicalRouterPorts(ctx context.Context) ([]nbdb.LogicalRouterPort, error) {
	var rows []nbdb.LogicalRouterPort
	if err := c.client.List(ctx, &rows); err != nil {
		return nil, fmt.Errorf("list logical_router_port: %w", err)
	}
	return rows, nil
}

func (c *LiveClient) CreateDHCPOptions(ctx context.Context, opts *nbdb.DHCPOptions) (string, error) {
	ops, err := c.client.Create(opts)
	if err != nil {
		return "", fmt.Errorf("create DHCP options ops: %w", err)
	}
	results, err := c.client.Transact(ctx, ops...)
	if err != nil {
		return "", fmt.Errorf("create DHCP options transact: %w", err)
	}
	if _, err := ovsdb.CheckOperationResults(results, ops); err != nil {
		return "", fmt.Errorf("create DHCP options check: %w", err)
	}
	if len(results) > 0 {
		return results[0].UUID.GoUUID, nil
	}
	return "", nil
}

func (c *LiveClient) DeleteDHCPOptions(ctx context.Context, uuid string) error {
	opts := &nbdb.DHCPOptions{UUID: uuid}
	ops, err := c.client.Where(opts).Delete()
	if err != nil {
		return fmt.Errorf("delete DHCP options ops: %w", err)
	}
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("delete DHCP options transact: %w", err)
	}
	return nil
}

func (c *LiveClient) FindDHCPOptionsByCIDR(ctx context.Context, cidr string) (*nbdb.DHCPOptions, error) {
	var options []nbdb.DHCPOptions
	err := c.client.WhereCache(func(o *nbdb.DHCPOptions) bool {
		return o.CIDR == cidr
	}).List(ctx, &options)
	if err != nil {
		return nil, fmt.Errorf("find DHCP options by CIDR: %w", err)
	}
	if len(options) == 0 {
		return nil, fmt.Errorf("DHCP options for CIDR %q not found", cidr)
	}
	return &options[0], nil
}

func (c *LiveClient) FindDHCPOptionsByExternalID(ctx context.Context, key, value string) (*nbdb.DHCPOptions, error) {
	var options []nbdb.DHCPOptions
	err := c.client.WhereCache(func(o *nbdb.DHCPOptions) bool {
		return o.ExternalIDs[key] == value
	}).List(ctx, &options)
	if err != nil {
		return nil, fmt.Errorf("find DHCP options by external_id %s=%s: %w", key, value, err)
	}
	if len(options) == 0 {
		return nil, fmt.Errorf("DHCP options with external_id %s=%s not found", key, value)
	}
	return &options[0], nil
}

func (c *LiveClient) ListDHCPOptions(ctx context.Context) ([]nbdb.DHCPOptions, error) {
	var options []nbdb.DHCPOptions
	err := c.client.List(ctx, &options)
	if err != nil {
		return nil, fmt.Errorf("list DHCP options: %w", err)
	}
	return options, nil
}

func (c *LiveClient) AddNAT(ctx context.Context, routerName string, nat *nbdb.NAT) error {
	if nat.UUID == "" {
		nat.UUID = namedUUID("nat_", nat.Type+"_"+nat.LogicalIP)
	}

	createOps, err := c.client.Create(nat)
	if err != nil {
		return fmt.Errorf("create NAT ops: %w", err)
	}

	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return fmt.Errorf("get logical router for NAT add: %w", err)
	}

	mutateOps, err := c.client.Where(lr).Mutate(lr, model.Mutation{
		Field:   &lr.NAT,
		Mutator: "insert",
		Value:   []string{nat.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate router NAT ops: %w", err)
	}

	ops := append(createOps, mutateOps...)
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("add NAT transact: %w", err)
	}
	return nil
}

// DeleteNAT removes every NAT rule matching (natType, logicalIP) from routerName.
// Scoped to routerName.NAT — AWS CIDRs repeat across VPCs so the pair is not globally
// unique. Deletes all matches so a duplicate row (e.g. minted by a racing reconcile)
// is fully cleaned on teardown rather than leaking past the first delete.
func (c *LiveClient) DeleteNAT(ctx context.Context, routerName string, natType, logicalIP string) error {
	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return fmt.Errorf("get logical router for NAT delete: %w", err)
	}
	routerNATs := make(map[string]struct{}, len(lr.NAT))
	for _, uuid := range lr.NAT {
		routerNATs[uuid] = struct{}{}
	}

	var nats []nbdb.NAT
	err = c.client.WhereCache(func(n *nbdb.NAT) bool {
		if _, ok := routerNATs[n.UUID]; !ok {
			return false
		}
		return n.Type == natType && n.LogicalIP == logicalIP
	}).List(ctx, &nats)
	if err != nil {
		return fmt.Errorf("find NAT: %w", err)
	}
	if len(nats) == 0 {
		return fmt.Errorf("NAT %s %s on %s: %w", natType, logicalIP, routerName, ErrNATNotFound)
	}

	var allOps []ovsdb.Operation
	for i := range nats {
		nat := &nats[i]
		mutateOps, mErr := c.client.Where(lr).Mutate(lr, model.Mutation{
			Field:   &lr.NAT,
			Mutator: "delete",
			Value:   []string{nat.UUID},
		})
		if mErr != nil {
			return fmt.Errorf("mutate router NAT ops: %w", mErr)
		}
		deleteOps, dErr := c.client.Where(nat).Delete()
		if dErr != nil {
			return fmt.Errorf("delete NAT ops: %w", dErr)
		}
		allOps = append(allOps, mutateOps...)
		allOps = append(allOps, deleteOps...)
	}

	if err := c.transactOps(ctx, allOps); err != nil {
		return fmt.Errorf("delete NAT transact: %w", err)
	}
	return nil
}

// DeleteNATByExternalIP removes a NAT rule matching (natType, externalIP) from
// routerName only. Returns ErrNATNotFound if absent on this router.
func (c *LiveClient) DeleteNATByExternalIP(ctx context.Context, routerName string, natType, externalIP string) error {
	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return fmt.Errorf("get logical router for NAT delete: %w", err)
	}
	routerNATs := make(map[string]struct{}, len(lr.NAT))
	for _, uuid := range lr.NAT {
		routerNATs[uuid] = struct{}{}
	}

	var nats []nbdb.NAT
	err = c.client.WhereCache(func(n *nbdb.NAT) bool {
		if _, ok := routerNATs[n.UUID]; !ok {
			return false
		}
		return n.Type == natType && n.ExternalIP == externalIP
	}).List(ctx, &nats)
	if err != nil {
		return fmt.Errorf("find NAT by external IP: %w", err)
	}
	if len(nats) == 0 {
		return fmt.Errorf("NAT %s external_ip=%s on %s: %w", natType, externalIP, routerName, ErrNATNotFound)
	}

	var allOps []ovsdb.Operation
	for i := range nats {
		nat := &nats[i]
		mutateOps, mErr := c.client.Where(lr).Mutate(lr, model.Mutation{
			Field:   &lr.NAT,
			Mutator: "delete",
			Value:   []string{nat.UUID},
		})
		if mErr != nil {
			return fmt.Errorf("mutate router NAT ops: %w", mErr)
		}
		deleteOps, dErr := c.client.Where(nat).Delete()
		if dErr != nil {
			return fmt.Errorf("delete NAT ops: %w", dErr)
		}
		allOps = append(allOps, mutateOps...)
		allOps = append(allOps, deleteOps...)
	}

	if err := c.transactOps(ctx, allOps); err != nil {
		return fmt.Errorf("delete NAT by external IP transact: %w", err)
	}
	return nil
}

// DeleteAllNATsByExternalIP removes NAT rules matching externalIP across all routers.
// Handles cross-VPC stale rules. Returns the count deleted.
func (c *LiveClient) DeleteAllNATsByExternalIP(ctx context.Context, natType, externalIP string) (int, error) {
	var nats []nbdb.NAT
	if err := c.client.WhereCache(func(n *nbdb.NAT) bool {
		return n.Type == natType && n.ExternalIP == externalIP
	}).List(ctx, &nats); err != nil {
		return 0, fmt.Errorf("find NAT by external IP: %w", err)
	}
	if len(nats) == 0 {
		return 0, nil
	}

	routers, err := c.ListLogicalRouters(ctx)
	if err != nil {
		return 0, fmt.Errorf("list routers for stale NAT cleanup: %w", err)
	}

	staleUUIDs := make(map[string]struct{}, len(nats))
	for _, n := range nats {
		staleUUIDs[n.UUID] = struct{}{}
	}

	var allOps []ovsdb.Operation
	for i := range routers {
		lr := &routers[i]
		for _, natUUID := range lr.NAT {
			if _, stale := staleUUIDs[natUUID]; !stale {
				continue
			}
			ops, err := c.client.Where(lr).Mutate(lr, model.Mutation{
				Field: &lr.NAT, Mutator: "delete", Value: []string{natUUID},
			})
			if err != nil {
				return 0, fmt.Errorf("mutate router %s: %w", lr.Name, err)
			}
			allOps = append(allOps, ops...)
		}
	}
	for i := range nats {
		ops, err := c.client.Where(&nats[i]).Delete()
		if err != nil {
			return 0, fmt.Errorf("delete NAT row: %w", err)
		}
		allOps = append(allOps, ops...)
	}
	if err := c.transactOps(ctx, allOps); err != nil {
		return 0, fmt.Errorf("delete all NATs by external IP: %w", err)
	}
	return len(nats), nil
}

// FindNATByExternalIP returns the first NAT rule matching (natType, externalIP), or nil.
func (c *LiveClient) FindNATByExternalIP(ctx context.Context, natType, externalIP string) (*nbdb.NAT, error) {
	var nats []nbdb.NAT
	if err := c.client.WhereCache(func(n *nbdb.NAT) bool {
		return n.Type == natType && n.ExternalIP == externalIP
	}).List(ctx, &nats); err != nil {
		return nil, fmt.Errorf("find NAT by external IP: %w", err)
	}
	if len(nats) == 0 {
		return nil, nil
	}
	return &nats[0], nil
}

// FindNATByLogicalIP returns the first owned NAT rule matching (natType, logicalIP)
// on routerName, or (nil, nil). Router-scoped because AWS CIDRs repeat across VPCs,
// so the pair is not globally unique (mirrors FindStaticRoute and DeleteNAT's key).
func (c *LiveClient) FindNATByLogicalIP(ctx context.Context, routerName, natType, logicalIP string) (*nbdb.NAT, error) {
	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return nil, fmt.Errorf("get logical router for NAT lookup: %w", err)
	}
	owned := make(map[string]struct{}, len(lr.NAT))
	for _, u := range lr.NAT {
		owned[u] = struct{}{}
	}
	var nats []nbdb.NAT
	if err := c.client.WhereCache(func(n *nbdb.NAT) bool {
		if _, ok := owned[n.UUID]; !ok {
			return false
		}
		return n.Type == natType && n.LogicalIP == logicalIP
	}).List(ctx, &nats); err != nil {
		return nil, fmt.Errorf("find NAT by logical IP: %w", err)
	}
	if len(nats) == 0 {
		return nil, nil
	}
	return &nats[0], nil
}

func (c *LiveClient) AddStaticRoute(ctx context.Context, routerName string, route *nbdb.LogicalRouterStaticRoute) error {
	if route.UUID == "" {
		route.UUID = namedUUID("route_", route.IPPrefix)
	}

	createOps, err := c.client.Create(route)
	if err != nil {
		return fmt.Errorf("create static route ops: %w", err)
	}

	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return fmt.Errorf("get logical router for route add: %w", err)
	}

	mutateOps, err := c.client.Where(lr).Mutate(lr, model.Mutation{
		Field:   &lr.StaticRoutes,
		Mutator: "insert",
		Value:   []string{route.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate router static routes ops: %w", err)
	}

	ops := append(createOps, mutateOps...)
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("add static route transact: %w", err)
	}
	return nil
}

// FindStaticRoute returns the first owned static route matching ipPrefix on routerName,
// or (nil, nil). Prevents duplicate rows from AddStaticRoute retries.
func (c *LiveClient) FindStaticRoute(ctx context.Context, routerName, ipPrefix string) (*nbdb.LogicalRouterStaticRoute, error) {
	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return nil, fmt.Errorf("get logical router for static route lookup: %w", err)
	}
	owned := make(map[string]struct{}, len(lr.StaticRoutes))
	for _, u := range lr.StaticRoutes {
		owned[u] = struct{}{}
	}
	var routes []nbdb.LogicalRouterStaticRoute
	if err := c.client.WhereCache(func(r *nbdb.LogicalRouterStaticRoute) bool {
		if r.IPPrefix != ipPrefix {
			return false
		}
		_, ok := owned[r.UUID]
		return ok
	}).List(ctx, &routes); err != nil {
		return nil, fmt.Errorf("list static routes: %w", err)
	}
	if len(routes) == 0 {
		return nil, nil
	}
	return &routes[0], nil
}

func (c *LiveClient) DeleteStaticRoute(ctx context.Context, routerName string, ipPrefix string) error {
	var routes []nbdb.LogicalRouterStaticRoute
	err := c.client.WhereCache(func(r *nbdb.LogicalRouterStaticRoute) bool {
		return r.IPPrefix == ipPrefix
	}).List(ctx, &routes)
	if err != nil {
		return fmt.Errorf("find static route: %w", err)
	}
	if len(routes) == 0 {
		return fmt.Errorf("static route %s not found", ipPrefix)
	}

	route := &routes[0]
	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return fmt.Errorf("get logical router for route delete: %w", err)
	}

	mutateOps, err := c.client.Where(lr).Mutate(lr, model.Mutation{
		Field:   &lr.StaticRoutes,
		Mutator: "delete",
		Value:   []string{route.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate router static routes ops: %w", err)
	}

	deleteOps, err := c.client.Where(route).Delete()
	if err != nil {
		return fmt.Errorf("delete static route ops: %w", err)
	}

	ops := append(mutateOps, deleteOps...)
	err = c.transactOps(ctx, ops)
	if err != nil {
		return fmt.Errorf("delete static route transact: %w", err)
	}
	return nil
}

// Logical Router Policies (per-subnet egress steering).

func (c *LiveClient) AddLogicalRouterPolicy(ctx context.Context, routerName string, policy *nbdb.LogicalRouterPolicy) error {
	if policy.UUID == "" {
		policy.UUID = namedUUID("lrp_", fmt.Sprintf("%d:%s", policy.Priority, policy.Match))
	}
	if policy.ExternalIDs == nil {
		policy.ExternalIDs = map[string]string{}
	}
	if policy.Options == nil {
		policy.Options = map[string]string{}
	}

	createOps, err := c.client.Create(policy)
	if err != nil {
		return fmt.Errorf("create LR policy ops: %w", err)
	}

	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return fmt.Errorf("get logical router for policy add: %w", err)
	}

	mutateOps, err := c.client.Where(lr).Mutate(lr, model.Mutation{
		Field:   &lr.Policies,
		Mutator: "insert",
		Value:   []string{policy.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate router policies ops: %w", err)
	}

	if err := c.transactOps(ctx, append(createOps, mutateOps...)); err != nil {
		return fmt.Errorf("add LR policy transact: %w", err)
	}
	return nil
}

func (c *LiveClient) FindLogicalRouterPolicy(ctx context.Context, routerName string, priority int, match string) (*nbdb.LogicalRouterPolicy, error) {
	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return nil, fmt.Errorf("get logical router for policy lookup: %w", err)
	}
	owned := make(map[string]struct{}, len(lr.Policies))
	for _, u := range lr.Policies {
		owned[u] = struct{}{}
	}
	var policies []nbdb.LogicalRouterPolicy
	if err := c.client.WhereCache(func(p *nbdb.LogicalRouterPolicy) bool {
		if p.Priority != priority || p.Match != match {
			return false
		}
		_, ok := owned[p.UUID]
		return ok
	}).List(ctx, &policies); err != nil {
		return nil, fmt.Errorf("list LR policies: %w", err)
	}
	if len(policies) == 0 {
		return nil, nil
	}
	return &policies[0], nil
}

func (c *LiveClient) ListLogicalRouterPolicies(ctx context.Context, routerName string) ([]nbdb.LogicalRouterPolicy, error) {
	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return nil, fmt.Errorf("get logical router for policy list: %w", err)
	}
	if len(lr.Policies) == 0 {
		return nil, nil
	}
	owned := make(map[string]struct{}, len(lr.Policies))
	for _, u := range lr.Policies {
		owned[u] = struct{}{}
	}
	var policies []nbdb.LogicalRouterPolicy
	if err := c.client.WhereCache(func(p *nbdb.LogicalRouterPolicy) bool {
		_, ok := owned[p.UUID]
		return ok
	}).List(ctx, &policies); err != nil {
		return nil, fmt.Errorf("list LR policies: %w", err)
	}
	return policies, nil
}

func (c *LiveClient) DeleteLogicalRouterPolicy(ctx context.Context, routerName string, priority int, match string) error {
	existing, err := c.FindLogicalRouterPolicy(ctx, routerName, priority, match)
	if err != nil {
		return err
	}
	if existing == nil {
		return nil
	}

	lr, err := c.GetLogicalRouter(ctx, routerName)
	if err != nil {
		return fmt.Errorf("get logical router for policy delete: %w", err)
	}

	mutateOps, err := c.client.Where(lr).Mutate(lr, model.Mutation{
		Field:   &lr.Policies,
		Mutator: "delete",
		Value:   []string{existing.UUID},
	})
	if err != nil {
		return fmt.Errorf("mutate router policies ops: %w", err)
	}

	deleteOps, err := c.client.Where(existing).Delete()
	if err != nil {
		return fmt.Errorf("delete LR policy ops: %w", err)
	}

	if err := c.transactOps(ctx, append(mutateOps, deleteOps...)); err != nil {
		return fmt.Errorf("delete LR policy transact: %w", err)
	}
	return nil
}

// Port Groups

func (c *LiveClient) CreatePortGroup(ctx context.Context, name string, ports []string) error {
	pg := &nbdb.PortGroup{
		UUID:        namedUUID("pg_", name),
		Name:        name,
		Ports:       ports,
		ExternalIDs: map[string]string{},
	}
	ops, err := c.client.Create(pg)
	if err != nil {
		return fmt.Errorf("create port group ops: %w", err)
	}
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("create port group transact: %w", err)
	}
	return nil
}

func (c *LiveClient) EnsurePortGroup(ctx context.Context, name string, ports []string) (*nbdb.PortGroup, error) {
	if existing, err := c.getPortGroup(ctx, name); err == nil {
		return existing, nil
	}
	pg := &nbdb.PortGroup{
		UUID:        namedUUID("pg_", name),
		Name:        name,
		Ports:       ports,
		ExternalIDs: map[string]string{},
	}
	ops, err := c.ensureNamedRowOps("Port_Group", name, pg)
	if err != nil {
		return nil, err
	}
	if err := c.transactOps(ctx, ops); err != nil {
		if existing, refErr := c.waitForCachedPortGroup(ctx, name); refErr == nil {
			slog.Info("ovn: EnsurePortGroup lost race, reusing existing",
				"name", name, "existing_uuid", existing.UUID)
			return existing, nil
		}
		return nil, fmt.Errorf("ensure port group %q: %w", name, err)
	}
	return pg, nil
}

func (c *LiveClient) waitForCachedPortGroup(ctx context.Context, name string) (*nbdb.PortGroup, error) {
	deadline := time.Now().Add(ensureRefetchTimeout)
	var lastErr error
	for {
		v, err := c.getPortGroup(ctx, name)
		if err == nil {
			return v, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(ensureRefetchInterval):
		}
	}
}

func (c *LiveClient) DeletePortGroup(ctx context.Context, name string) error {
	pg, err := c.getPortGroup(ctx, name)
	if err != nil {
		return fmt.Errorf("delete port group lookup: %w", err)
	}
	ops, err := c.client.Where(pg).Delete()
	if err != nil {
		return fmt.Errorf("delete port group ops: %w", err)
	}
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("delete port group transact: %w", err)
	}
	return nil
}

func (c *LiveClient) UpdatePortGroupMemberships(ctx context.Context, lspName string, addPGs, removePGs []string) error {
	if len(addPGs) == 0 && len(removePGs) == 0 {
		return nil
	}
	lsp, err := c.GetLogicalSwitchPort(ctx, lspName)
	if err != nil {
		return fmt.Errorf("update port group memberships lsp lookup: %w", err)
	}

	var ops []ovsdb.Operation
	appendPGOps := func(pgName, mutator string) error {
		pg, err := c.getPortGroup(ctx, pgName)
		if err != nil {
			return fmt.Errorf("port group %s lookup: %w", pgName, err)
		}
		pgOps, err := c.client.Where(pg).Mutate(pg, model.Mutation{
			Field:   &pg.Ports,
			Mutator: ovsdb.Mutator(mutator),
			Value:   []string{lsp.UUID},
		})
		if err != nil {
			return fmt.Errorf("mutate port group %s ops: %w", pgName, err)
		}
		ops = append(ops, pgOps...)
		return nil
	}

	for _, pgName := range addPGs {
		if err := appendPGOps(pgName, "insert"); err != nil {
			return err
		}
	}
	for _, pgName := range removePGs {
		if err := appendPGOps(pgName, "delete"); err != nil {
			return err
		}
	}

	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("update port group memberships transact: %w", err)
	}
	return nil
}

func (c *LiveClient) getPortGroup(ctx context.Context, name string) (*nbdb.PortGroup, error) {
	var pgs []nbdb.PortGroup
	err := c.client.WhereCache(func(pg *nbdb.PortGroup) bool {
		return pg.Name == name
	}).List(ctx, &pgs)
	if err != nil {
		return nil, fmt.Errorf("get port group: %w", err)
	}
	if len(pgs) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrPortGroupNotFound, name)
	}
	return &pgs[0], nil
}

// GetPortGroup returns the named port group.
func (c *LiveClient) GetPortGroup(ctx context.Context, name string) (*nbdb.PortGroup, error) {
	return c.getPortGroup(ctx, name)
}

func (c *LiveClient) ListPortGroups(ctx context.Context) ([]nbdb.PortGroup, error) {
	var pgs []nbdb.PortGroup
	if err := c.client.List(ctx, &pgs); err != nil {
		return nil, fmt.Errorf("list port groups: %w", err)
	}
	return pgs, nil
}

// ListPortGroupsForPort returns port group names containing the LSP. Empty (not error) when none.
func (c *LiveClient) ListPortGroupsForPort(ctx context.Context, lspName string) ([]string, error) {
	lsp, err := c.GetLogicalSwitchPort(ctx, lspName)
	if err != nil {
		return nil, fmt.Errorf("list port groups for port lookup: %w", err)
	}
	var pgs []nbdb.PortGroup
	if err := c.client.List(ctx, &pgs); err != nil {
		return nil, fmt.Errorf("list port groups: %w", err)
	}
	names := make([]string, 0)
	for i := range pgs {
		if slices.Contains(pgs[i].Ports, lsp.UUID) {
			names = append(names, pgs[i].Name)
		}
	}
	return names, nil
}

// ACLs

func (c *LiveClient) AddACLs(ctx context.Context, portGroupName string, specs []ACLSpec) error {
	if len(specs) == 0 {
		return nil
	}
	pg, err := c.getPortGroup(ctx, portGroupName)
	if err != nil {
		return fmt.Errorf("add ACLs port group lookup: %w", err)
	}

	var ops []ovsdb.Operation
	uuids := make([]string, 0, len(specs))
	for i, spec := range specs {
		// Index i disambiguates ACLs with the same (direction, match).
		acl := &nbdb.ACL{
			UUID:        namedUUID("acl_", fmt.Sprintf("%s_%d_%s", portGroupName, i, spec.Direction)),
			Direction:   spec.Direction,
			Priority:    spec.Priority,
			Match:       spec.Match,
			Action:      spec.Action,
			Log:         spec.Log,
			ExternalIDs: map[string]string{},
		}
		if spec.Name != "" {
			name := spec.Name
			acl.Name = &name
		}
		if spec.Severity != "" {
			severity := spec.Severity
			acl.Severity = &severity
		}
		createOps, err := c.client.Create(acl)
		if err != nil {
			return fmt.Errorf("create ACL ops: %w", err)
		}
		ops = append(ops, createOps...)
		uuids = append(uuids, acl.UUID)
	}

	mutateOps, err := c.client.Where(pg).Mutate(pg, model.Mutation{
		Field:   &pg.ACLs,
		Mutator: ovsdb.MutateOperationInsert,
		Value:   uuids,
	})
	if err != nil {
		return fmt.Errorf("mutate port group ACLs ops: %w", err)
	}
	ops = append(ops, mutateOps...)

	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("add ACLs transact: %w", err)
	}
	return nil
}

// ClearACLs removes every ACL row referenced by the port group and detaches
// them from the port group's ACLs set in one transaction.
func (c *LiveClient) ClearACLs(ctx context.Context, portGroupName string) error {
	pg, err := c.getPortGroup(ctx, portGroupName)
	if err != nil {
		return fmt.Errorf("clear ACLs port group lookup: %w", err)
	}
	if len(pg.ACLs) == 0 {
		return nil
	}

	mutateOps, err := c.client.Where(pg).Mutate(pg, model.Mutation{
		Field:   &pg.ACLs,
		Mutator: ovsdb.MutateOperationDelete,
		Value:   pg.ACLs,
	})
	if err != nil {
		return fmt.Errorf("mutate port group ACLs (delete) ops: %w", err)
	}

	ops := mutateOps
	for _, aclUUID := range pg.ACLs {
		acl := &nbdb.ACL{UUID: aclUUID}
		deleteOps, dErr := c.client.Where(acl).Delete()
		if dErr != nil {
			return fmt.Errorf("delete ACL ops: %w", dErr)
		}
		ops = append(ops, deleteOps...)
	}

	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("clear ACLs transact: %w", err)
	}
	return nil
}

// ReplaceACLs atomically swaps the port group's ACL set in one transaction.
// Avoids the zero-ACL window that ClearACLs+AddACLs would expose (default-drop).
func (c *LiveClient) ReplaceACLs(ctx context.Context, portGroupName string, specs []ACLSpec) error {
	pg, err := c.getPortGroup(ctx, portGroupName)
	if err != nil {
		return fmt.Errorf("replace ACLs port group lookup: %w", err)
	}

	var ops []ovsdb.Operation

	if len(pg.ACLs) > 0 {
		detachOps, dErr := c.client.Where(pg).Mutate(pg, model.Mutation{
			Field:   &pg.ACLs,
			Mutator: ovsdb.MutateOperationDelete,
			Value:   pg.ACLs,
		})
		if dErr != nil {
			return fmt.Errorf("mutate port group ACLs (detach) ops: %w", dErr)
		}
		ops = append(ops, detachOps...)
		for _, aclUUID := range pg.ACLs {
			delOps, dErr := c.client.Where(&nbdb.ACL{UUID: aclUUID}).Delete()
			if dErr != nil {
				return fmt.Errorf("delete ACL ops: %w", dErr)
			}
			ops = append(ops, delOps...)
		}
	}

	uuids := make([]string, 0, len(specs))
	for i, spec := range specs {
		acl := &nbdb.ACL{
			UUID:        namedUUID("acl_", fmt.Sprintf("%s_%d_%s", portGroupName, i, spec.Direction)),
			Direction:   spec.Direction,
			Priority:    spec.Priority,
			Match:       spec.Match,
			Action:      spec.Action,
			Log:         spec.Log,
			ExternalIDs: map[string]string{},
		}
		if spec.Name != "" {
			name := spec.Name
			acl.Name = &name
		}
		if spec.Severity != "" {
			severity := spec.Severity
			acl.Severity = &severity
		}
		createOps, cErr := c.client.Create(acl)
		if cErr != nil {
			return fmt.Errorf("create ACL ops: %w", cErr)
		}
		ops = append(ops, createOps...)
		uuids = append(uuids, acl.UUID)
	}

	if len(uuids) > 0 {
		attachOps, aErr := c.client.Where(pg).Mutate(pg, model.Mutation{
			Field:   &pg.ACLs,
			Mutator: ovsdb.MutateOperationInsert,
			Value:   uuids,
		})
		if aErr != nil {
			return fmt.Errorf("mutate port group ACLs (attach) ops: %w", aErr)
		}
		ops = append(ops, attachOps...)
	}

	if len(ops) == 0 {
		return nil
	}
	if err := c.transactOps(ctx, ops); err != nil {
		return fmt.Errorf("replace ACLs transact: %w", err)
	}
	return nil
}
