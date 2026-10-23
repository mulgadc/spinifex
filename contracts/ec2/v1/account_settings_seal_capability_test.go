package ec2v1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountSettingsSealCapabilitySubject(t *testing.T) {
	require.Equal(t, "ec2.v1.AccountSettingsSealCapability.node-1", AccountSettingsSealCapabilitySubject("node-1"))
	require.Equal(t, "ec2.v1.AccountSettingsSealCapability.node-2", AccountSettingsSealCapabilitySubject("node-2"))
}

func TestAccountSettingsSealCapabilityRequestJSONContract(t *testing.T) {
	data, err := json.Marshal(AccountSettingsSealCapabilityRequest{Node: "node-1", CorrelationRef: "req-1"})
	require.NoError(t, err)
	require.JSONEq(t, `{"node":"node-1","correlation_ref":"req-1"}`, string(data))

	data, err = json.Marshal(AccountSettingsSealCapabilityRequest{Node: "node-1"})
	require.NoError(t, err)
	require.JSONEq(t, `{"node":"node-1"}`, string(data))
}

func TestAccountSettingsSealCapabilityReplyJSONContract(t *testing.T) {
	reply := AccountSettingsSealCapabilityReply{Node: "node-1", CapabilityVersion: 1, CorrelationRef: "req-1"}
	data, err := json.Marshal(reply)
	require.NoError(t, err)
	require.JSONEq(t, `{"node":"node-1","capability_version":1,"correlation_ref":"req-1"}`, string(data))

	data, err = json.Marshal(AccountSettingsSealCapabilityReply{Node: "node-1", CapabilityVersion: 1})
	require.NoError(t, err)
	require.JSONEq(t, `{"node":"node-1","capability_version":1}`, string(data))
}

func TestValidSealCapabilityNode(t *testing.T) {
	for _, node := range []string{"node-1", "node_2", "n3"} {
		require.True(t, ValidSealCapabilityNode(node), node)
	}
	for _, node := range []string{"", "a.b", "*", ">", "node-*", "node >", "node\t1", "node\n1"} {
		require.False(t, ValidSealCapabilityNode(node), "%q", node)
	}
}
