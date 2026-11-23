package ochre_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/ochre"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var completeOwner = ochre.LaunchOwner{WorkerNonce: "nonce-1", TokenID: "launch-1", Epoch: 3}

func ownedRecord() ochre.EndpointRecord {
	return ochre.EndpointRecord{
		AccountID: "000000000000", ModelID: "model", State: ochre.StateStarting,
		InstanceID: "i-1", NodeID: "node-1", Generation: 4,
		LaunchOwner: ochre.Owned(completeOwner),
	}
}

func decode(t *testing.T, data string) ochre.EndpointRecord {
	t.Helper()
	var rec ochre.EndpointRecord
	require.NoError(t, json.Unmarshal([]byte(data), &rec))
	return rec
}

func TestEndpointRecord_LegacyRecordHasNoOwner(t *testing.T) {
	legacy := ownedRecord()
	legacy.LaunchOwner = ochre.OwnerField{}
	data, err := json.Marshal(legacy)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "launch_owner", "a record with no owner omits the field")

	rec := decode(t, `{"account_id":"000000000000","model_id":"model","state":"STARTING","node_id":"node-1","generation":4}`)
	owner, ok, err := rec.Owner()
	require.NoError(t, err)
	assert.False(t, ok, "a record written before the field is legacy, not damaged")
	assert.Zero(t, owner)
}

func TestEndpointRecord_CompleteOwnerRoundTrips(t *testing.T) {
	data, err := json.Marshal(ownedRecord())
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &fields))
	assert.JSONEq(t, `{"worker_nonce":"nonce-1","token_id":"launch-1","epoch":3}`, string(fields["launch_owner"]))

	rec := decode(t, string(data))
	owner, ok, err := rec.Owner()
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, completeOwner, owner)
	assert.Equal(t, ownedRecord(), rec)
}

func TestEndpointRecord_PartialOrInvalidOwnerIsRefused(t *testing.T) {
	cases := map[string]string{
		"null":               `null`,
		"empty":              `{}`,
		"no nonce":           `{"token_id":"launch-1","epoch":3}`,
		"no token":           `{"worker_nonce":"nonce-1","epoch":3}`,
		"zero epoch":         `{"worker_nonce":"nonce-1","token_id":"launch-1","epoch":0}`,
		"no epoch":           `{"worker_nonce":"nonce-1","token_id":"launch-1"}`,
		"unknown field":      `{"worker_nonce":"nonce-1","token_id":"launch-1","epoch":3,"node":"node-2"}`,
		"wrong type":         `"launch-1"`,
		"negative epoch":     `{"worker_nonce":"nonce-1","token_id":"launch-1","epoch":-1}`,
		"epoch as a string":  `{"worker_nonce":"nonce-1","token_id":"launch-1","epoch":"3"}`,
		"owner on no node":   `{"worker_nonce":"nonce-1","token_id":"launch-1","epoch":3}`,
		"array of an owner":  `[{"worker_nonce":"nonce-1","token_id":"launch-1","epoch":3}]`,
		"fractional epoch":   `{"worker_nonce":"nonce-1","token_id":"launch-1","epoch":3.5}`,
	}
	for name, owner := range cases {
		t.Run(name, func(t *testing.T) {
			node := `"node_id":"node-1",`
			if name == "owner on no node" {
				node = ""
			}
			rec := decode(t, `{"account_id":"000000000000","model_id":"model","state":"STARTING",`+node+`"generation":4,"launch_owner":`+owner+`}`)
			got, ok, err := rec.Owner()
			require.ErrorIs(t, err, ochre.ErrLaunchOwnerInvalid)
			assert.True(t, ok, "a damaged owner is never read as a legacy record")
			assert.Zero(t, got)
			if name != "owner on no node" {
				_, err = json.Marshal(rec)
				require.ErrorIs(t, err, ochre.ErrLaunchOwnerInvalid, "a damaged owner is never rewritten")
			}
		})
	}

	for name, owner := range map[string]ochre.LaunchOwner{
		"no nonce":   {TokenID: "launch-1", Epoch: 3},
		"no token":   {WorkerNonce: "nonce-1", Epoch: 3},
		"zero epoch": {WorkerNonce: "nonce-1", TokenID: "launch-1"},
	} {
		rec := ownedRecord()
		rec.LaunchOwner = ochre.Owned(owner)
		_, err := json.Marshal(rec)
		require.ErrorIs(t, err, ochre.ErrLaunchOwnerInvalid, "a partial owner is never written: %s", name)
	}
}

// n1Record has the shape an N-1 reader decodes into: every EndpointRecord
// field and tag except the launch owner.
func n1Record(t *testing.T) reflect.Type {
	t.Helper()
	full := reflect.TypeFor[ochre.EndpointRecord]()
	var fields []reflect.StructField
	for i := range full.NumField() {
		if f := full.Field(i); f.Name != "LaunchOwner" {
			fields = append(fields, f)
		}
	}
	require.Len(t, fields, full.NumField()-1)
	return reflect.StructOf(fields)
}

// An N-1 reader ignores the owner and keeps every other field, but a record it
// rewrites loses the owner and then reads as legacy. This shows the decode
// shape only; it is not a mixed-version rollout test.
func TestEndpointRecord_N1ReaderIgnoresTheOwner(t *testing.T) {
	data, err := json.Marshal(ownedRecord())
	require.NoError(t, err)

	old := reflect.New(n1Record(t))
	require.NoError(t, json.Unmarshal(data, old.Interface()), "an N-1 reader accepts a record carrying an owner")
	assert.Equal(t, "node-1", old.Elem().FieldByName("NodeID").String())
	assert.Equal(t, uint64(4), old.Elem().FieldByName("Generation").Uint())

	rewritten, err := json.Marshal(old.Interface())
	require.NoError(t, err)
	assert.NotContains(t, string(rewritten), "launch_owner", "an N-1 rewrite drops the owner")
	_, ok, err := decode(t, string(rewritten)).Owner()
	require.NoError(t, err)
	assert.False(t, ok, "after an N-1 rewrite the record reads as legacy")

	want := ownedRecord()
	want.LaunchOwner = ochre.OwnerField{}
	assert.Equal(t, want, decode(t, string(rewritten)), "every other field survives")
}
