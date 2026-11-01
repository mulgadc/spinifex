package admissionv1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountQuotaOverrideSealCapabilitySubject(t *testing.T) {
	require.Equal(t, "admission.v1.AccountQuotaOverrideSealCapability.node-1", AccountQuotaOverrideSealCapabilitySubject("node-1"))
	require.Equal(t, "admission.v1.AccountQuotaOverrideSealCapability.node-2", AccountQuotaOverrideSealCapabilitySubject("node-2"))
}

func TestAccountQuotaOverrideSealCapabilityRequestJSONContract(t *testing.T) {
	data, err := json.Marshal(AccountQuotaOverrideSealCapabilityRequest{Node: "node-1", CorrelationRef: "req-1"})
	require.NoError(t, err)
	require.JSONEq(t, `{"node":"node-1","correlation_ref":"req-1"}`, string(data))

	data, err = json.Marshal(AccountQuotaOverrideSealCapabilityRequest{Node: "node-1"})
	require.NoError(t, err)
	require.JSONEq(t, `{"node":"node-1"}`, string(data))
}

func TestAccountQuotaOverrideSealCapabilityReplyJSONContract(t *testing.T) {
	reply := AccountQuotaOverrideSealCapabilityReply{Node: "node-1", CapabilityVersion: 1, CorrelationRef: "req-1"}
	data, err := json.Marshal(reply)
	require.NoError(t, err)
	require.JSONEq(t, `{"node":"node-1","capability_version":1,"correlation_ref":"req-1"}`, string(data))

	data, err = json.Marshal(AccountQuotaOverrideSealCapabilityReply{Node: "node-1", CapabilityVersion: 1})
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
