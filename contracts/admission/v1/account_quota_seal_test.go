package admissionv1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSealAccountQuotaOverrideSubject(t *testing.T) {
	require.Equal(t, "admission.v1.SealAccountQuotaOverride", SealAccountQuotaOverrideSubject)
}

func TestSealAccountQuotaOverrideRequestJSONContract(t *testing.T) {
	data, err := json.Marshal(SealAccountQuotaOverrideRequest{AccountID: "111122223333", CorrelationRef: "req-1"})
	require.NoError(t, err)
	require.JSONEq(t, `{"account_id":"111122223333","correlation_ref":"req-1"}`, string(data))

	data, err = json.Marshal(SealAccountQuotaOverrideRequest{AccountID: "111122223333"})
	require.NoError(t, err)
	require.JSONEq(t, `{"account_id":"111122223333"}`, string(data))
}

func TestSealAccountQuotaOverrideReplyJSONContract(t *testing.T) {
	reply := SealAccountQuotaOverrideReply{
		AccountID:      "111122223333",
		CorrelationRef: "req-1",
		Disposition:    AccountQuotaOverrideDispositionSealed,
		Revision:       42,
	}
	data, err := json.Marshal(reply)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"account_id": "111122223333",
		"correlation_ref": "req-1",
		"disposition": "sealed",
		"revision": 42
	}`, string(data))

	data, err = json.Marshal(SealAccountQuotaOverrideReply{AccountID: "111122223333", Disposition: AccountQuotaOverrideDispositionSealed})
	require.NoError(t, err)
	require.JSONEq(t, `{"account_id":"111122223333","disposition":"sealed","revision":0}`, string(data))
}
