package vpcd

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecurity_CreatePortGroup(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	evt := SGEvent{
		GroupId: "sg-abc123",
		VpcId:   "vpc-test1",
	}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicCreateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "create SG port group")

	// Verify port group was created in the mock
	mock.Mu.Lock()
	pg, exists := mock.PortGroups["sg_abc123"]
	mock.Mu.Unlock()
	assert.True(t, exists, "port group sg_abc123 should exist")
	assert.NotNil(t, pg)

	// Verify the infrastructure ACL set was emitted: 2 default-deny at
	// priority 900 (logged for CMMC SC.L1-3.13.1) plus 2 DHCP-infra allows
	// at priority 1050 (unlogged — high-volume, low-signal).
	type aclSnapshot struct {
		action    string
		log       bool
		severity  string
		name      string
		priority  int
		direction string
		match     string
	}
	var snapshots []aclSnapshot
	mock.Mu.Lock()
	aclCount := len(pg.ACLs)
	for _, aclUUID := range pg.ACLs {
		a := mock.ACLs[aclUUID]
		if a == nil {
			continue
		}
		snap := aclSnapshot{
			action:    a.Action,
			log:       a.Log,
			priority:  a.Priority,
			direction: a.Direction,
			match:     a.Match,
		}
		if a.Severity != nil {
			snap.severity = *a.Severity
		}
		if a.Name != nil {
			snap.name = *a.Name
		}
		snapshots = append(snapshots, snap)
	}
	mock.Mu.Unlock()

	assert.Equal(t, 4, aclCount, "should have 4 infra ACLs (2 deny + 2 DHCP allow)")
	denyCount, dhcpAllowCount := 0, 0
	for _, s := range snapshots {
		switch s.action {
		case "drop":
			denyCount++
			assert.True(t, s.log, "default deny ACL must have log=true for boundary monitoring")
			assert.Equal(t, "info", s.severity, "default deny ACL must use info severity")
			assert.Contains(t, s.name, "sg_abc123-deny-", "default deny ACL must have a name for syslog correlation")
			assert.Equal(t, 900, s.priority, "default deny ACL must be priority 900")
		case "allow":
			dhcpAllowCount++
			assert.False(t, s.log, "DHCP infra allow must not be logged (high-volume, low-signal)")
			assert.Equal(t, 1050, s.priority, "DHCP infra allow must be priority 1050 (above tenant rules and default-deny)")
			assert.Contains(t, s.name, "sg_abc123-allow-dhcp-", "DHCP allow must have a name for operator correlation")
		}
	}
	assert.Equal(t, 2, denyCount, "should have 2 drop ACLs")
	assert.Equal(t, 2, dhcpAllowCount, "should have 2 DHCP allow ACLs (egress client→server, ingress server→client)")
}

// TestSecurity_DHCPInfraACLs locks the exact shape of the DHCPv4 infra-allow
// ACLs that vpcd emits on every port group. This is the fix for the
// "narrow-egress SG silently breaks DHCP" bug: OVN's DHCP responder runs
// inside the LS pipeline *after* the port-group ACL stage, so without these
// the guest's DHCPDISCOVER (dst=255.255.255.255) hits the priority-900
// default-deny under any non-0.0.0.0/0 egress rule and the VM never gets a
// lease.
func TestSecurity_DHCPInfraACLs(t *testing.T) {
	const pg = "sg_dhcp_shape"

	egress := dhcpEgressACL(pg)
	assert.Equal(t, "from-lport", egress.Direction)
	assert.Equal(t, 1050, egress.Priority, "must outrank tenant rules (1000) and default-deny (900)")
	assert.Equal(t, "allow", egress.Action, "plain allow — DHCP is single-shot, no conntrack")
	assert.Equal(t, "inport == @sg_dhcp_shape && udp && udp.src == 68 && udp.dst == 67", egress.Match)
	assert.Equal(t, "sg_dhcp_shape-allow-dhcp-egress", egress.Name)
	assert.False(t, egress.Log)

	ingress := dhcpIngressACL(pg)
	assert.Equal(t, "to-lport", ingress.Direction)
	assert.Equal(t, 1050, ingress.Priority)
	assert.Equal(t, "allow", ingress.Action)
	assert.Equal(t, "outport == @sg_dhcp_shape && udp && udp.src == 67 && udp.dst == 68", ingress.Match)
	assert.Equal(t, "sg_dhcp_shape-allow-dhcp-ingress", ingress.Name)
	assert.False(t, ingress.Log)
}

// TestSecurity_InfrastructureACLs_Order locks the deny-before-allow ordering
// that provisionSG/handleUpdateSG rely on when concatenating infrastructure
// ACLs with tenant rule ACLs.
func TestSecurity_InfrastructureACLs_Order(t *testing.T) {
	specs := infrastructureACLs("sg_order")
	require.Len(t, specs, 4)
	assert.Equal(t, "sg_order-deny-ingress", specs[0].Name)
	assert.Equal(t, "sg_order-deny-egress", specs[1].Name)
	assert.Equal(t, "sg_order-allow-dhcp-egress", specs[2].Name)
	assert.Equal(t, "sg_order-allow-dhcp-ingress", specs[3].Name)
}

func TestSecurity_DeletePortGroup(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Create first
	createEvt := SGEvent{GroupId: "sg-del1", VpcId: "vpc-test2"}
	data, _ := json.Marshal(createEvt)
	resp, err := nc.Request(TopicCreateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "create SG for delete test")

	// Verify it exists
	mock.Mu.Lock()
	_, exists := mock.PortGroups["sg_del1"]
	mock.Mu.Unlock()
	assert.True(t, exists)

	// Delete
	delEvt := SGEvent{GroupId: "sg-del1", VpcId: "vpc-test2"}
	data, _ = json.Marshal(delEvt)
	resp, err = nc.Request(TopicDeleteSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "delete SG port group")

	// Verify removed
	mock.Mu.Lock()
	_, exists = mock.PortGroups["sg_del1"]
	mock.Mu.Unlock()
	assert.False(t, exists, "port group sg_del1 should be deleted")
}

func TestSecurity_UpdateSGAddRules(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	// Create SG first (no rules)
	createEvt := SGEvent{GroupId: "sg-upd1", VpcId: "vpc-test3"}
	data, _ := json.Marshal(createEvt)
	resp, err := nc.Request(TopicCreateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "create SG for update test")

	// Update with ingress rules
	updateEvt := SGEvent{
		GroupId: "sg-upd1",
		VpcId:   "vpc-test3",
		IngressRules: []SGRuleForACL{
			{IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "10.0.0.0/8"},
			{IpProtocol: "tcp", FromPort: 443, ToPort: 443, CidrIp: "0.0.0.0/0"},
		},
	}
	data, _ = json.Marshal(updateEvt)
	resp, err = nc.Request(TopicUpdateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "update SG with ingress rules")

	// Verify ACLs were created: 2 default deny + 2 ingress allow = 4
	type aclSnapshot struct {
		action string
		match  string
		log    bool
	}
	var snapshots []aclSnapshot
	mock.Mu.Lock()
	pg := mock.PortGroups["sg_upd1"]
	aclCount := len(pg.ACLs)
	for _, aclUUID := range pg.ACLs {
		a := mock.ACLs[aclUUID]
		if a == nil {
			continue
		}
		snapshots = append(snapshots, aclSnapshot{action: a.Action, match: a.Match, log: a.Log})
	}
	mock.Mu.Unlock()

	assert.Equal(t, 6, aclCount, "should have 6 ACLs (2 deny + 2 DHCP infra allow + 2 ingress allow)")

	// Check that at least one match contains tcp.dst == 22. Also verify
	// logging policy: denies logged, allows not logged (CMMC SC.L1-3.13.1).
	foundSSH := false
	foundHTTPS := false
	for _, s := range snapshots {
		switch s.action {
		case "drop":
			assert.True(t, s.log, "deny ACL must be logged")
		case "allow-related":
			assert.False(t, s.log, "allow ACL must not be logged (high volume, low signal)")
		}
		if containsAll(s.match, "tcp.dst == 22", "ip4.src == 10.0.0.0/8") {
			foundSSH = true
		}
		if containsAll(s.match, "tcp.dst == 443") {
			foundHTTPS = true
		}
	}
	assert.True(t, foundSSH, "should have SSH ACL with source CIDR")
	assert.True(t, foundHTTPS, "should have HTTPS ACL")
}

// TestSecurity_CreateSG_FailsFastOnAddACLError locks the Phase 1.1 invariant
// that handleCreateSG returns an AddACLs error to the caller and tears down
// the half-built SG. The mock fails the batched AddACLs call, and the test
// asserts the handler responds with failure AND that the port group is
// gone — so the next reconciler scan recreates from scratch instead of
// observing a half-built PG with no ACLs.
func TestSecurity_CreateSG_FailsFastOnAddACLError(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	mock.AddACLErrAfter = 1
	_ = mock.Connect(context.Background())

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	evt := SGEvent{GroupId: "sg-failacl", VpcId: "vpc-failacl"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicCreateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertFailure(t, resp, "create SG must fail when AddACL fails")

	mock.Mu.Lock()
	_, pgExists := mock.PortGroups["sg_failacl"]
	mock.Mu.Unlock()
	assert.False(t, pgExists, "port group must be torn down on failed provisionSG")
}

// TestSecurity_UpdateSG_FailsFastOnAddACLError locks the same fail-fast policy
// in handleUpdateSG: clear ACLs, re-add deny ACLs, then add allow ACLs — any
// failure must propagate to the caller instead of being slog.Warn-ed away.
func TestSecurity_UpdateSG_FailsFastOnAddACLError(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	createEvt := SGEvent{GroupId: "sg-updfail", VpcId: "vpc-updfail"}
	data, _ := json.Marshal(createEvt)
	resp, err := nc.Request(TopicCreateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "create SG before update")

	mock.Mu.Lock()
	mock.AddACLCalls = 0
	mock.AddACLErrAfter = 1
	mock.Mu.Unlock()

	updateEvt := SGEvent{
		GroupId: "sg-updfail",
		VpcId:   "vpc-updfail",
		IngressRules: []SGRuleForACL{
			{IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "10.0.0.0/8"},
			{IpProtocol: "tcp", FromPort: 443, ToPort: 443, CidrIp: "0.0.0.0/0"},
		},
	}
	data, _ = json.Marshal(updateEvt)
	resp, err = nc.Request(TopicUpdateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertFailure(t, resp, "update SG must fail when AddACL fails mid-batch")
}

// TestSecurity_CreateSG_WithRules_FailsFastOnAddACLError locks the fail-fast
// policy when handleCreateSG carries allow rules: the batched AddACLs call
// includes both deny ACLs and rule ACLs, and a failure must still tear down
// the PG/AS so the reconciler doesn't mistake a half-built PG as healthy.
func TestSecurity_CreateSG_WithRules_FailsFastOnAddACLError(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	mock.AddACLErrAfter = 1
	_ = mock.Connect(context.Background())

	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	evt := SGEvent{
		GroupId: "sg-rulefail",
		VpcId:   "vpc-rulefail",
		IngressRules: []SGRuleForACL{
			{IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "10.0.0.0/8"},
			{IpProtocol: "tcp", FromPort: 443, ToPort: 443, CidrIp: "0.0.0.0/0"},
		},
	}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicCreateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertFailure(t, resp, "create SG with rules must fail when AddACLs fails")

	mock.Mu.Lock()
	_, pgExists := mock.PortGroups["sg_rulefail"]
	mock.Mu.Unlock()
	assert.False(t, pgExists, "port group must be torn down when AddACLs fails")
}

// TestSecurity_HandleCreateSG_OVNNotConnected, ...HandleDeleteSG_..., and
// ...HandleUpdateSG_... lock the defensive guard at the top of each handler:
// when vpcd's OVN client failed to connect (or has been Closed), the SG event
// must surface a clear error to the caller instead of nil-deref'ing.
func TestSecurity_HandleCreateSG_OVNNotConnected(t *testing.T) {
	_, nc := startTestNATS(t)
	topo := NewTopologyHandler(nil)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	data, _ := json.Marshal(SGEvent{GroupId: "sg-noovn", VpcId: "vpc-noovn"})
	resp, err := nc.Request(TopicCreateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertFailure(t, resp, "create SG must fail when ovn client is nil")
}

func TestSecurity_HandleDeleteSG_OVNNotConnected(t *testing.T) {
	_, nc := startTestNATS(t)
	topo := NewTopologyHandler(nil)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	data, _ := json.Marshal(SGEvent{GroupId: "sg-noovn", VpcId: "vpc-noovn"})
	resp, err := nc.Request(TopicDeleteSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertFailure(t, resp, "delete SG must fail when ovn client is nil")
}

func TestSecurity_HandleUpdateSG_OVNNotConnected(t *testing.T) {
	_, nc := startTestNATS(t)
	topo := NewTopologyHandler(nil)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	data, _ := json.Marshal(SGEvent{GroupId: "sg-noovn", VpcId: "vpc-noovn"})
	resp, err := nc.Request(TopicUpdateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertFailure(t, resp, "update SG must fail when ovn client is nil")
}

// TestSecurity_HandleCreateSG_BadJSON / ..._HandleDeleteSG_BadJSON /
// ..._HandleUpdateSG_BadJSON: malformed event payloads must surface as
// failures, not be silently dropped — the handler-side caller relies on the
// ack to know the request was processed.
func TestSecurity_HandleCreateSG_BadJSON(t *testing.T) {
	_, nc := startTestNATS(t)
	topo := NewTopologyHandler(NewMockOVNClient())
	_ = topo.ovn.Connect(context.Background())
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	resp, err := nc.Request(TopicCreateSG, []byte("not json"), 5_000_000_000)
	require.NoError(t, err)
	assertFailure(t, resp, "create SG with malformed payload must fail")
}

func TestSecurity_HandleDeleteSG_BadJSON(t *testing.T) {
	_, nc := startTestNATS(t)
	topo := NewTopologyHandler(NewMockOVNClient())
	_ = topo.ovn.Connect(context.Background())
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	resp, err := nc.Request(TopicDeleteSG, []byte("not json"), 5_000_000_000)
	require.NoError(t, err)
	assertFailure(t, resp, "delete SG with malformed payload must fail")
}

func TestSecurity_HandleUpdateSG_BadJSON(t *testing.T) {
	_, nc := startTestNATS(t)
	topo := NewTopologyHandler(NewMockOVNClient())
	_ = topo.ovn.Connect(context.Background())
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	resp, err := nc.Request(TopicUpdateSG, []byte("not json"), 5_000_000_000)
	require.NoError(t, err)
	assertFailure(t, resp, "update SG with malformed payload must fail")
}

// TestSecurity_HandleDeleteSG_FailsFastOnClearACLsError: handleDeleteSG must
// surface the ClearACLs error rather than swallow it. We trigger this by
// requesting a delete for an SG that was never created — the mock's ClearACLs
// returns "port group not found", which must propagate as a failed response.
func TestSecurity_HandleDeleteSG_FailsFastOnClearACLsError(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	data, _ := json.Marshal(SGEvent{GroupId: "sg-never-created", VpcId: "vpc-x"})
	resp, err := nc.Request(TopicDeleteSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertFailure(t, resp, "delete must fail when port group does not exist")
}

// TestSecurity_HandleUpdateSG_FailsFastOnClearACLsError: handleUpdateSG must
// also surface the ClearACLs error — silently swallowing it would leave the
// caller thinking the rule update succeeded when in fact the old ACLs are
// still in place.
func TestSecurity_HandleUpdateSG_FailsFastOnClearACLsError(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	data, _ := json.Marshal(SGEvent{
		GroupId:      "sg-never-created",
		VpcId:        "vpc-x",
		IngressRules: []SGRuleForACL{{IpProtocol: "tcp", FromPort: 22, ToPort: 22, CidrIp: "0.0.0.0/0"}},
	})
	resp, err := nc.Request(TopicUpdateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertFailure(t, resp, "update must fail when port group does not exist")
}

// TestSecurity_HandleDeleteSG_Idempotent_AddressSetMissing: if the address set
// has already been cleaned up out-of-band but the port group still exists, the
// delete must NOT propagate the AS-not-found error — the goal state is "both
// gone", and we've already achieved it for the AS. Locks the matching
// fail-fast invariant in TestSecurity_DeleteSG_FailsFastOnAddressSetError
// (already covered) by exercising the inverse direction: a fresh delete after
// a successful create must succeed end-to-end without errors leaking.
//
// (kept here to avoid duplicating the create→delete fixture; the existing
// happy-path test exercises the success branch but does not assert that
// neither half-state lingers.)
func TestSecurity_HandleDeleteSG_LeavesNoResidualState(t *testing.T) {
	_, nc := startTestNATS(t)
	mock := NewMockOVNClient()
	_ = mock.Connect(context.Background())
	topo := NewTopologyHandler(mock)
	subs, err := topo.Subscribe(nc)
	require.NoError(t, err)
	defer func() {
		for _, s := range subs {
			_ = s.Unsubscribe()
		}
	}()

	data, _ := json.Marshal(SGEvent{GroupId: "sg-residual", VpcId: "vpc-x"})
	resp, err := nc.Request(TopicCreateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "create SG")

	resp, err = nc.Request(TopicDeleteSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "delete SG")

	mock.Mu.Lock()
	_, pgExists := mock.PortGroups["sg_residual"]
	leftoverACLs := 0
	for _, a := range mock.ACLs {
		if a != nil && (a.Match == "outport == @sg_residual && ip4" || a.Match == "inport == @sg_residual && ip4") {
			leftoverACLs++
		}
	}
	mock.Mu.Unlock()
	assert.False(t, pgExists, "port group must be gone")
	assert.Zero(t, leftoverACLs, "no ACL rows must reference the deleted PG")
}

// containsAll checks if s contains all substrings.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && searchString(s, sub)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
