//test:in-package — projects an SGEvent through the unexported toSpec, which is
//how the vpc.update-sg handler itself reaches policy.UpdateSG.

package subscribers

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/spinifex/network/ovn/mock"
	"github.com/mulgadc/spinifex/spinifex/network/policy"
	"github.com/mulgadc/spinifex/spinifex/network/topology"
)

// aclFingerprints applies a vpc.update-sg payload through policy.UpdateSG and
// returns the resulting ACL set as sorted, UUID-free strings.
func aclFingerprints(t *testing.T, payload string) []string {
	t.Helper()
	var evt SGEvent
	require.NoError(t, json.Unmarshal([]byte(payload), &evt))

	m := mock.New()
	pg := topology.SecurityGroupPortGroup(evt.GroupId)
	require.NoError(t, m.CreatePortGroup(context.Background(), pg, nil))
	sg := policy.NewSecurityGroupManager(m, policy.EgressPolicy{})
	require.NoError(t, sg.UpdateSG(context.Background(), evt.toSpec()))

	stored, ok := m.PortGroups[pg]
	require.True(t, ok)
	out := make([]string, 0, len(stored.ACLs))
	for _, uuid := range stored.ACLs {
		acl, ok := m.ACLs[uuid]
		require.True(t, ok)
		out = append(out, fmt.Sprintf("%s|%d|%s|%s", acl.Direction, acl.Priority, acl.Match, acl.Action))
	}
	sort.Strings(out)
	return out
}

// TestUpdateSG_DescriptionDoesNotChangeACLSet pins that a rule description
// cannot reach the dataplane: the same rules with and without descriptions
// commit an identical ACL set, so re-tagging a rule is a metadata write.
func TestUpdateSG_DescriptionDoesNotChangeACLSet(t *testing.T) {
	const withoutDescriptions = `{
		"group_id": "sg-desc", "vpc_id": "vpc-1",
		"ingress_rules": [
			{"rule_id": "sgr-aaaaaaaaaaaaaaaaa", "ip_protocol": "tcp", "from_port": 443, "to_port": 443, "cidr_ip": "0.0.0.0/0"},
			{"rule_id": "sgr-bbbbbbbbbbbbbbbbb", "ip_protocol": "tcp", "from_port": 22, "to_port": 22, "source_sg": "sg-peer"}
		],
		"egress_rules": [
			{"rule_id": "sgr-ccccccccccccccccc", "ip_protocol": "-1", "from_port": 0, "to_port": 0, "cidr_ip": "0.0.0.0/0"}
		]
	}`
	const withDescriptions = `{
		"group_id": "sg-desc", "vpc_id": "vpc-1",
		"ingress_rules": [
			{"rule_id": "sgr-aaaaaaaaaaaaaaaaa", "ip_protocol": "tcp", "from_port": 443, "to_port": 443, "cidr_ip": "0.0.0.0/0", "description": "elbv2.k8s.aws/targetGroupBinding=shared"},
			{"rule_id": "sgr-bbbbbbbbbbbbbbbbb", "ip_protocol": "tcp", "from_port": 22, "to_port": 22, "source_sg": "sg-peer", "description": "bastion"}
		],
		"egress_rules": [
			{"rule_id": "sgr-ccccccccccccccccc", "ip_protocol": "-1", "from_port": 0, "to_port": 0, "cidr_ip": "0.0.0.0/0", "description": "all egress"}
		]
	}`

	before := aclFingerprints(t, withoutDescriptions)
	require.NotEmpty(t, before)
	assert.Equal(t, before, aclFingerprints(t, withDescriptions),
		"a description change must rebuild an identical ACL set")
}

// TestUpdateSG_ModifiedRuleMatchesAuthorizedRule pins that the dataplane cannot
// tell a modify from an authorize: a rule edited in place under its old ID
// commits the same ACL set as a fresh rule with the same content.
func TestUpdateSG_ModifiedRuleMatchesAuthorizedRule(t *testing.T) {
	const beforeModify = `{
		"group_id": "sg-mod", "vpc_id": "vpc-1",
		"ingress_rules": [
			{"rule_id": "sgr-aaaaaaaaaaaaaaaaa", "ip_protocol": "tcp", "from_port": 8080, "to_port": 8080, "cidr_ip": "192.0.2.0/24"}
		],
		"egress_rules": [
			{"rule_id": "sgr-ccccccccccccccccc", "ip_protocol": "-1", "from_port": 0, "to_port": 0, "cidr_ip": "0.0.0.0/0"}
		]
	}`
	const afterModify = `{
		"group_id": "sg-mod", "vpc_id": "vpc-1",
		"ingress_rules": [
			{"rule_id": "sgr-aaaaaaaaaaaaaaaaa", "ip_protocol": "tcp", "from_port": 8081, "to_port": 8081, "cidr_ip": "198.51.100.0/24"}
		],
		"egress_rules": [
			{"rule_id": "sgr-ccccccccccccccccc", "ip_protocol": "-1", "from_port": 0, "to_port": 0, "cidr_ip": "0.0.0.0/0"}
		]
	}`
	const freshlyAuthorized = `{
		"group_id": "sg-mod", "vpc_id": "vpc-1",
		"ingress_rules": [
			{"rule_id": "sgr-ddddddddddddddddd", "ip_protocol": "tcp", "from_port": 8081, "to_port": 8081, "cidr_ip": "198.51.100.0/24"}
		],
		"egress_rules": [
			{"rule_id": "sgr-ccccccccccccccccc", "ip_protocol": "-1", "from_port": 0, "to_port": 0, "cidr_ip": "0.0.0.0/0"}
		]
	}`

	modified := aclFingerprints(t, afterModify)
	require.NotEmpty(t, modified)
	assert.Equal(t, aclFingerprints(t, freshlyAuthorized), modified,
		"a modified rule must commit the same ACL set as an authorized one with its content")
	assert.NotEqual(t, aclFingerprints(t, beforeModify), modified,
		"a modify must change the committed ACL set")
}
