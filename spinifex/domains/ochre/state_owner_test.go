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
			_, err = json.Marshal(rec)
			require.ErrorIs(t, err, ochre.ErrLaunchOwnerInvalid, "a damaged owner is never rewritten")
		})
	}

	noNode := ownedRecord()
	noNode.NodeID = ""
	_, err := json.Marshal(noNode)
	require.ErrorIs(t, err, ochre.ErrLaunchOwnerInvalid, "an owner with no node is never written")
	_, err = json.Marshal(&noNode)
	require.ErrorIs(t, err, ochre.ErrLaunchOwnerInvalid, "nor through a pointer")

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

// legacyJSON is a record as the code before the owner field encoded it.
const legacyJSON = `{"account_id":"000000000000","model_id":"model","state":"READY","instance_id":"i-1","node_id":"node-1",` +
	`"base_url":"http://10.0.0.1:8000","members":{"model":{"port":8000,"family":"meta"}},` +
	`"created_at":"2026-11-01T00:00:00Z","health_hold":{"token":"hold-1","epoch":2,"reserved_at":"2026-11-02T00:00:00Z"},"in_flight":2,"generation":7}`

func TestEndpointRecord_LegacyEncodingIsUnchanged(t *testing.T) {
	rec := decode(t, legacyJSON)
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	assert.Equal(t, legacyJSON, string(data), "a legacy record encodes byte for byte as before")

	lenient := decode(t, `{"Model_ID":"model","state":"READY","future_field":{"x":1},"generation":7}`)
	assert.Equal(t, "model", lenient.ModelID, "unrelated fields keep case-insensitive matching")
	_, ok, err := lenient.Owner()
	require.NoError(t, err)
	assert.False(t, ok, "an unrelated unknown field leaves the record legacy")
}

func TestEndpointRecord_AmbiguousOwnerIsInvalid(t *testing.T) {
	const valid = `{"worker_nonce":"nonce-1","token_id":"launch-1","epoch":3}`
	const other = `{"worker_nonce":"nonce-2","token_id":"launch-2","epoch":4}`
	head := `{"account_id":"000000000000","model_id":"model","state":"STARTING","node_id":"node-1","generation":4,`
	cases := map[string]string{
		"owner repeated, valid first": head + `"launch_owner":` + valid + `,"launch_owner":` + other + `}`,
		"owner repeated, valid last":  head + `"launch_owner":` + other + `,"launch_owner":` + valid + `}`,
		"owner repeated identically":  head + `"launch_owner":` + valid + `,"launch_owner":` + valid + `}`,
		"owner key in another case":   head + `"Launch_Owner":` + valid + `}`,
		"owner key and a case alias":  head + `"launch_owner":` + valid + `,"LAUNCH_OWNER":` + other + `}`,
		"alias then owner key":        head + `"LAUNCH_OWNER":` + other + `,"launch_owner":` + valid + `}`,
		"owner field repeated, first": head + `"launch_owner":{"worker_nonce":"nonce-1","token_id":"launch-1","epoch":3,"epoch":4}}`,
		"owner field repeated, last":  head + `"launch_owner":{"epoch":4,"worker_nonce":"nonce-1","token_id":"launch-1","epoch":3}}`,
		"owner field in another case": head + `"launch_owner":{"worker_nonce":"nonce-1","Token_ID":"launch-1","epoch":3}}`,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			rec := decode(t, data)
			got, ok, err := rec.Owner()
			require.ErrorIs(t, err, ochre.ErrLaunchOwnerInvalid)
			assert.True(t, ok, "an ambiguous owner is damaged, not absent")
			assert.Zero(t, got)
			assert.Equal(t, "node-1", rec.NodeID, "the rest of the record is kept")
			assert.Equal(t, uint64(4), rec.Generation)
			_, err = json.Marshal(rec)
			require.ErrorIs(t, err, ochre.ErrLaunchOwnerInvalid, "an ambiguous owner is never rewritten")
		})
	}
}
