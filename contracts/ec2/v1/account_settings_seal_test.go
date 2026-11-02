package ec2v1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSealAccountSettingsSubject(t *testing.T) {
	require.Equal(t, "ec2.v1.SealAccountSettings", SealAccountSettingsSubject)
}

func TestSealAccountSettingsRequestJSONContract(t *testing.T) {
	data, err := json.Marshal(SealAccountSettingsRequest{AccountID: "111122223333", CorrelationRef: "req-1"})
	require.NoError(t, err)
	require.JSONEq(t, `{"account_id":"111122223333","correlation_ref":"req-1"}`, string(data))

	data, err = json.Marshal(SealAccountSettingsRequest{AccountID: "111122223333"})
	require.NoError(t, err)
	require.JSONEq(t, `{"account_id":"111122223333"}`, string(data))
}

func TestSealAccountSettingsReplyJSONContract(t *testing.T) {
	reply := SealAccountSettingsReply{
		AccountID:      "111122223333",
		CorrelationRef: "req-1",
		Disposition:    AccountSettingsDispositionSealed,
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

	data, err = json.Marshal(SealAccountSettingsReply{AccountID: "111122223333", Disposition: AccountSettingsDispositionSealed})
	require.NoError(t, err)
	require.JSONEq(t, `{"account_id":"111122223333","disposition":"sealed","revision":0}`, string(data))
}
