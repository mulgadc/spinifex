package accountlifecycle

//test:in-package — the probe's local wire shape is unexported; this pins that it
// still decodes both contracts, so a renamed contract tag cannot silently
// disable a seal.

import (
	"encoding/json"
	"testing"

	admissionv1 "github.com/mulgadc/spinifex/contracts/admission/v1"
	ec2v1 "github.com/mulgadc/spinifex/contracts/ec2/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The local shape must read exactly what each contract's responder writes.
func TestSealCapabilityLocalReplyDecodesBothContracts(t *testing.T) {
	want := sealCapabilityReply{Node: "node-1", CapabilityVersion: 7, CorrelationRef: "req-1"}

	ec2Data, err := json.Marshal(ec2v1.AccountSettingsSealCapabilityReply{Node: "node-1", CapabilityVersion: 7, CorrelationRef: "req-1"})
	require.NoError(t, err)
	admissionData, err := json.Marshal(admissionv1.AccountQuotaOverrideSealCapabilityReply{Node: "node-1", CapabilityVersion: 7, CorrelationRef: "req-1"})
	require.NoError(t, err)

	for name, data := range map[string][]byte{"ec2v1": ec2Data, "admissionv1": admissionData} {
		var got sealCapabilityReply
		require.NoError(t, json.Unmarshal(data, &got), name)
		assert.Equal(t, want, got, name)
	}
}

// Each contract's responder must read exactly what the local shape sends.
func TestSealCapabilityLocalRequestDecodesInBothContracts(t *testing.T) {
	data, err := json.Marshal(sealCapabilityRequest{Node: "node-1", CorrelationRef: "req-1"})
	require.NoError(t, err)

	var ec2Got ec2v1.AccountSettingsSealCapabilityRequest
	require.NoError(t, json.Unmarshal(data, &ec2Got))
	assert.Equal(t, ec2v1.AccountSettingsSealCapabilityRequest{Node: "node-1", CorrelationRef: "req-1"}, ec2Got)

	var admissionGot admissionv1.AccountQuotaOverrideSealCapabilityRequest
	require.NoError(t, json.Unmarshal(data, &admissionGot))
	assert.Equal(t, admissionv1.AccountQuotaOverrideSealCapabilityRequest{Node: "node-1", CorrelationRef: "req-1"}, admissionGot)
}
