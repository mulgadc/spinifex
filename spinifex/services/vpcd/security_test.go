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
	mock.mu.Lock()
	pg, exists := mock.portGroups["sg_abc123"]
	mock.mu.Unlock()
	assert.True(t, exists, "port group sg_abc123 should exist")
	assert.NotNil(t, pg)

	// Verify default deny ACLs were created (2 deny ACLs at priority 900)
	// and that each has logging enabled per CMMC SC.L1-3.13.1.
	type aclSnapshot struct {
		action   string
		log      bool
		severity string
		name     string
	}
	var snapshots []aclSnapshot
	mock.mu.Lock()
	aclCount := len(pg.ACLs)
	for _, aclUUID := range pg.ACLs {
		a := mock.acls[aclUUID]
		if a == nil {
			continue
		}
		snap := aclSnapshot{action: a.Action, log: a.Log}
		if a.Severity != nil {
			snap.severity = *a.Severity
		}
		if a.Name != nil {
			snap.name = *a.Name
		}
		snapshots = append(snapshots, snap)
	}
	mock.mu.Unlock()

	assert.Equal(t, 2, aclCount, "should have 2 default deny ACLs (ingress + egress)")
	denyCount := 0
	for _, s := range snapshots {
		if s.action != "drop" {
			continue
		}
		denyCount++
		assert.True(t, s.log, "default deny ACL must have log=true for boundary monitoring")
		assert.Equal(t, "info", s.severity, "default deny ACL must use info severity")
		assert.Contains(t, s.name, "sg_abc123-deny-", "default deny ACL must have a name for syslog correlation")
	}
	assert.Equal(t, 2, denyCount, "should have 2 drop ACLs")
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
	mock.mu.Lock()
	_, exists := mock.portGroups["sg_del1"]
	mock.mu.Unlock()
	assert.True(t, exists)

	// Delete
	delEvt := SGEvent{GroupId: "sg-del1", VpcId: "vpc-test2"}
	data, _ = json.Marshal(delEvt)
	resp, err = nc.Request(TopicDeleteSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "delete SG port group")

	// Verify removed
	mock.mu.Lock()
	_, exists = mock.portGroups["sg_del1"]
	mock.mu.Unlock()
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
	mock.mu.Lock()
	pg := mock.portGroups["sg_upd1"]
	aclCount := len(pg.ACLs)
	for _, aclUUID := range pg.ACLs {
		a := mock.acls[aclUUID]
		if a == nil {
			continue
		}
		snapshots = append(snapshots, aclSnapshot{action: a.Action, match: a.Match, log: a.Log})
	}
	mock.mu.Unlock()

	assert.Equal(t, 4, aclCount, "should have 4 ACLs (2 deny + 2 ingress allow)")

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

// TestSecurity_CreateSG_CreatesAddressSet locks the Phase 5.1 invariant:
// handleCreateSG must provision the SG's `<pg>_ip4` address set alongside
// the port group, otherwise SG-to-SG ACL match expressions reference a
// nonexistent set and OVN rejects them.
func TestSecurity_CreateSG_CreatesAddressSet(t *testing.T) {
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

	evt := SGEvent{GroupId: "sg-as001", VpcId: "vpc-as"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicCreateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "create SG")

	mock.mu.Lock()
	as, exists := mock.addressSets["sg_as001_ip4"]
	mock.mu.Unlock()
	assert.True(t, exists, "address set sg_as001_ip4 should exist after create")
	assert.NotNil(t, as)
	assert.Empty(t, as.Addresses, "newly created address set must be empty")
}

// TestSecurity_DeleteSG_DeletesAddressSet locks the Phase 5.2 invariant.
func TestSecurity_DeleteSG_DeletesAddressSet(t *testing.T) {
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

	evt := SGEvent{GroupId: "sg-as002", VpcId: "vpc-as"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicCreateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "create SG")

	resp, err = nc.Request(TopicDeleteSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "delete SG")

	mock.mu.Lock()
	_, exists := mock.addressSets["sg_as002_ip4"]
	mock.mu.Unlock()
	assert.False(t, exists, "address set sg_as002_ip4 should be deleted")
}

// handleDeleteSG must fail-fast on DeleteAddressSet errors. Once the port
// group is gone, the orphan-PG reconciler can no longer anchor cleanup of the
// matching address set — silently logging the AS error would leak it forever.
func TestSecurity_DeleteSG_FailsFastOnAddressSetError(t *testing.T) {
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

	evt := SGEvent{GroupId: "sg-asfail", VpcId: "vpc-asfail"}
	data, _ := json.Marshal(evt)
	resp, err := nc.Request(TopicCreateSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertSuccess(t, resp, "create SG")

	// Delete the address set out-of-band so handleDeleteSG's
	// DeleteAddressSet step errors. The handler must surface the error
	// rather than logging-and-continuing.
	mock.mu.Lock()
	delete(mock.addressSets, "sg_asfail_ip4")
	mock.mu.Unlock()

	resp, err = nc.Request(TopicDeleteSG, data, 5_000_000_000)
	require.NoError(t, err)
	assertFailure(t, resp, "delete SG with missing address set must fail")
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
