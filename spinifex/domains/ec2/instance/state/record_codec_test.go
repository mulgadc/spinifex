package state_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/state"
	"github.com/mulgadc/spinifex/spinifex/foundation/lifecycle/resource"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// goldenV1 is an unmarked record with unknown members at every level, in
// array elements and in the AWS SDK projection. terminated_at and
// hostfwd_port_map are in forms the typed encoder would rewrite.
const goldenV1 = `{
 "metadata":{"name":"i-0123456789abcdef0","account_id":"111122223333","generation":7,"observed_generation":6,
  "owner_refs":[{"kind":"cluster","name":"c-1","future_ref":"r-1"}],"tags":{"Name":"web"},
  "future_meta":{"a":1}},
 "spec":{"instance_type":"t3.micro","desired_state":"running",
  "config":{"future_cfg":true},
  "run_instances_input":{"MaxCount":1,"MinCount":1,"FutureInput":"y"},
  "future_spec":[1,2]},
 "status":{"status":"running","last_node":"node-1","az":"us-east-1a",
  "instance":{"InstanceId":"i-0123456789abcdef0","State":{"Name":"running","FutureState":1}},
  "terminated_at":"2026-10-01T09:00:00.000000000+10:00",
  "hostfwd_port_map":{"08080":22},
  "future_status":"z"},
 "future_top":{"nested":{"deep":[{"k":1}]}}
}`

// goldenV2 is a format-2 record: both new sections, the legacy mirror kept
// consistent with them, and unknown members inside each new section.
const goldenV2 = `{
 "format":2,
 "metadata":{"name":"i-0123456789abcdef0","account_id":"111122223333","generation":7,"observed_generation":6,"future_meta":"m"},
 "spec":{"instance_type":"t3.micro","desired_state":"running"},
 "status":{"status":"running","last_node":"node-1","az":"us-east-1a","future_status":"s"},
 "assignment":{"node":"node-1","generation":3,"epoch":2,"token":"tok-3","for_desired_generation":7,"reason":"launch",
  "future_assignment":{"x":1}},
 "observation":{"node":"node-1","assignment_epoch":2,"assignment_generation":3,"assignment_token":"tok-3",
  "desired_generation":6,"state":"running",
  "evidence":{"kind":"qmp","incarnation":"inc-1","taken_at":"2026-10-01T09:00:00Z","future_evidence":true},
  "reported_at":"2026-10-01T09:00:01Z","future_observation":[{"o":1}]},
 "future_top":"t"
}`

// canonical decodes JSON into a generic tree with numbers kept as literals,
// so two documents compare equal regardless of key order and whitespace.
func canonical(t testing.TB, b []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	require.NoError(t, dec.Decode(&v), "decode %s", b)
	return v
}

// edit returns the canonical form of doc with each dotted path set, or
// removed when the value is nil.
func edit(t *testing.T, doc string, changes map[string]any) any {
	t.Helper()
	tree := canonical(t, []byte(doc))
	for path, v := range changes {
		parts := strings.Split(path, ".")
		obj := tree.(map[string]any)
		for _, p := range parts[:len(parts)-1] {
			next, ok := obj[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				obj[p] = next
			}
			obj = next
		}
		last := parts[len(parts)-1]
		if v == nil {
			delete(obj, last)
		} else {
			obj[last] = v
		}
	}
	return tree
}

func member(t *testing.T, doc []byte, path ...string) (json.RawMessage, bool) {
	t.Helper()
	var cur json.RawMessage = doc
	for _, p := range path {
		var obj map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(cur, &obj))
		next, ok := obj[p]
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

func decode(t *testing.T, doc string) *state.TransitionRecord {
	t.Helper()
	r, err := state.DecodeTransitionRecord([]byte(doc))
	require.NoError(t, err)
	return r
}

func encode(t *testing.T, r *state.TransitionRecord) []byte {
	t.Helper()
	out, err := r.Encode()
	require.NoError(t, err)
	return out
}

func TestTransitionRecord_V1WithoutMarkerRoundTripsAndEmitsNoMarker(t *testing.T) {
	r := decode(t, goldenV1)
	assert.Equal(t, state.RecordFormatLegacy, r.Format())
	assert.False(t, r.ReadOnly())
	assert.Equal(t, "i-0123456789abcdef0", r.Record.Metadata.Name)
	assert.Nil(t, r.Assignment)

	out := encode(t, r)
	assert.Equal(t, canonical(t, []byte(goldenV1)), canonical(t, out))
	_, has := member(t, out, "format")
	assert.False(t, has, "a format-1 record read without a marker must not gain one")
}

func TestTransitionRecord_ExplicitFormatOneIsKeptAsWritten(t *testing.T) {
	doc := `{"format":1,"metadata":{"name":"i-1"},"spec":{},"status":{"status":"running"}}`
	r := decode(t, doc)
	assert.Equal(t, state.RecordFormatLegacy, r.Format())
	r.Record.Status.LastNode = "node-1"
	assert.Equal(t, edit(t, doc, map[string]any{"status.last_node": "node-1"}), canonical(t, encode(t, r)))
}

// Each mutation must change exactly its path; every unknown member at every
// level, and every untouched known member, comes back as it was read.
func TestTransitionRecord_MutationChangesOnlyItsPath(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*state.TransitionRecord)
		want   map[string]any
	}{
		{"spec field beside unknown spec members", func(r *state.TransitionRecord) {
			r.Record.Spec.InstanceType = "t3.large"
		}, map[string]any{"spec.instance_type": "t3.large"}},
		{"metadata generation beside unknown metadata", func(r *state.TransitionRecord) {
			r.Record.Metadata.Generation = 8
		}, map[string]any{"metadata.generation": json.Number("8")}},
		{"status field beside raw-form siblings", func(r *state.TransitionRecord) {
			r.Record.Status.LastNode = "node-2"
		}, map[string]any{"status.last_node": "node-2"}},
		{"field the raw config omitted is added alone", func(r *state.TransitionRecord) {
			r.Record.Spec.Config.CPUCount = 2
		}, map[string]any{"spec.config.cpu_count": json.Number("2")}},
		{"field inside the AWS SDK projection", func(r *state.TransitionRecord) {
			r.Record.Status.Instance.State.Name = aws.String("stopped")
		}, map[string]any{"status.instance.State.Name": "stopped"}},
		{"map entry added", func(r *state.TransitionRecord) {
			r.Record.Metadata.Tags["Env"] = "prod"
		}, map[string]any{"metadata.tags.Env": "prod"}},
		{"desired state", func(r *state.TransitionRecord) {
			r.Record.Spec.DesiredState = vm.DesiredStopped
		}, map[string]any{"spec.desired_state": string(vm.DesiredStopped)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := decode(t, goldenV1)
			tc.mutate(r)
			assert.Equal(t, edit(t, goldenV1, tc.want), canonical(t, encode(t, r)))
		})
	}
}

func TestTransitionRecord_ZeroedOmitemptyFieldIsRemoved(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*state.TransitionRecord)
		gone   string
	}{
		{"string", func(r *state.TransitionRecord) { r.Record.Status.AZ = "" }, "status.az"},
		{"map", func(r *state.TransitionRecord) { r.Record.Metadata.Tags = nil }, "metadata.tags"},
		{"slice", func(r *state.TransitionRecord) { r.Record.Metadata.OwnerRefs = nil }, "metadata.owner_refs"},
		{"omitzero time", func(r *state.TransitionRecord) { r.Record.Status.TerminatedAt = time.Time{} }, "status.terminated_at"},
		{"pointer", func(r *state.TransitionRecord) { r.Record.Spec.RunInstancesInput = nil }, "spec.run_instances_input"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := decode(t, goldenV1)
			tc.mutate(r)
			assert.Equal(t, edit(t, goldenV1, map[string]any{tc.gone: nil}), canonical(t, encode(t, r)))
		})
	}
}

// terminated_at and hostfwd_port_map are read in forms that the typed encoder
// rewrites, so these cases tell retention of the read value from re-encoding.
func TestTransitionRecord_UnchangedKnownFieldKeepsItsRawForm(t *testing.T) {
	var typed vm.InstanceRecord
	require.NoError(t, json.Unmarshal([]byte(goldenV1), &typed))
	plain, err := json.Marshal(&typed)
	require.NoError(t, err)
	plainAt, _ := member(t, plain, "status", "terminated_at")
	plainMap, _ := member(t, plain, "status", "hostfwd_port_map")
	require.JSONEq(t, `"2026-10-01T09:00:00+10:00"`, string(plainAt), "precondition: re-encoding rewrites the time")
	require.JSONEq(t, `{"8080":22}`, string(plainMap), "precondition: re-encoding rewrites the map key")

	r := decode(t, goldenV1)
	r.Record.Status.LastNode = "node-2"
	out := encode(t, r)
	at, _ := member(t, out, "status", "terminated_at")
	ports, _ := member(t, out, "status", "hostfwd_port_map")
	assert.JSONEq(t, `"2026-10-01T09:00:00.000000000+10:00"`, string(at))
	assert.JSONEq(t, `{"08080":22}`, string(ports))
}

func TestTransitionRecord_ExplicitMutationShadowsRawForm(t *testing.T) {
	r := decode(t, goldenV1)
	r.Record.Status.TerminatedAt = time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	r.Record.Status.HostfwdPortMap = map[int]int{9090: 22}
	want := edit(t, goldenV1, map[string]any{
		"status.terminated_at":    "2026-10-02T00:00:00Z",
		"status.hostfwd_port_map": map[string]any{"9090": json.Number("22")},
	})
	assert.Equal(t, want, canonical(t, encode(t, r)))
}

// An unchanged array is written back as read, unknown element members and
// all. A changed array is replaced whole, unless a read element carries an
// unknown member at any depth: then Encode refuses rather than drop it.
func TestTransitionRecord_Arrays(t *testing.T) {
	t.Run("unchanged array keeps unknown element members", func(t *testing.T) {
		r := decode(t, goldenV1)
		r.Record.Metadata.Name = "i-renamed"
		out := encode(t, r)
		refs, _ := member(t, out, "metadata", "owner_refs")
		assert.JSONEq(t, `[{"kind":"cluster","name":"c-1","future_ref":"r-1"}]`, string(refs))
	})
	t.Run("changed array with known members only is replaced whole", func(t *testing.T) {
		doc := `{"metadata":{"name":"i-1","owner_refs":[{"kind":"cluster","name":"c-1"}]},"future":[{"x":1}]}`
		r := decode(t, doc)
		r.Record.Metadata.OwnerRefs = append(r.Record.Metadata.OwnerRefs, resource.OwnerRef{Kind: "asg", Name: "a-1"})
		want := edit(t, doc, map[string]any{"metadata.owner_refs": []any{
			map[string]any{"kind": "cluster", "name": "c-1"},
			map[string]any{"kind": "asg", "name": "a-1"},
		}})
		assert.Equal(t, want, canonical(t, encode(t, r)))
	})
	t.Run("changed array of scalars is replaced whole", func(t *testing.T) {
		doc := `{"metadata":{"name":"i-1","finalizers":["eni","volume"]}}`
		r := decode(t, doc)
		r.Record.Metadata.Finalizers = []string{"volume"}
		want := edit(t, doc, map[string]any{"metadata.finalizers": []any{"volume"}})
		assert.Equal(t, want, canonical(t, encode(t, r)))
	})

	conflicts := []struct {
		name, doc, array, member string
		mutate                   func(*state.TransitionRecord)
	}{
		{
			"unknown member in an element",
			goldenV1,
			"$.metadata.owner_refs",
			"$.metadata.owner_refs[0].future_ref",
			func(r *state.TransitionRecord) {
				r.Record.Metadata.OwnerRefs = append(r.Record.Metadata.OwnerRefs, resource.OwnerRef{Kind: "asg", Name: "a-1"})
			},
		},
		{
			"unknown member one level deeper in an element",
			`{"status":{"instance":{"NetworkInterfaces":[{"NetworkInterfaceId":"eni-1",` +
				`"Attachment":{"DeviceIndex":0,"FutureAttach":"x"}}]}}}`,
			"$.status.instance.NetworkInterfaces",
			"$.status.instance.NetworkInterfaces[0].Attachment.FutureAttach",
			func(r *state.TransitionRecord) {
				r.Record.Status.Instance.NetworkInterfaces[0].NetworkInterfaceId = aws.String("eni-2")
			},
		},
	}
	for _, tc := range conflicts {
		t.Run("changed array refused: "+tc.name, func(t *testing.T) {
			r := decode(t, tc.doc)
			tc.mutate(r)
			out, err := r.Encode()
			require.ErrorIs(t, err, state.ErrRecordExtensionConflict)
			assert.Contains(t, err.Error(), "changed array "+tc.array+" ")
			assert.Contains(t, err.Error(), tc.member)
			assert.Nil(t, out)
		})
	}
}

func TestTransitionRecord_V2DecodesSectionsAndKeepsLegacyMirror(t *testing.T) {
	r := decode(t, goldenV2)
	assert.Equal(t, state.RecordFormatTransition, r.Format())
	assert.False(t, r.ReadOnly())
	assert.Equal(t, &state.InstanceAssignment{
		Node: "node-1", Generation: 3, Epoch: 2, Token: "tok-3", ForDesiredGeneration: 7, Reason: state.AssignmentLaunch,
	}, r.Assignment)
	require.NotNil(t, r.Observation)
	assert.Equal(t, int64(6), r.Observation.DesiredGeneration)
	require.NotNil(t, r.Observation.Evidence)
	assert.Equal(t, state.EvidenceQMP, r.Observation.Evidence.Kind)

	// The legacy mirror an R0 reader still consumes.
	assert.Equal(t, "node-1", r.Record.Status.LastNode)
	assert.Equal(t, "us-east-1a", r.Record.Status.AZ)
	assert.Equal(t, vm.StateRunning, r.Record.Status.Status)
	assert.Equal(t, int64(6), r.Record.Metadata.ObservedGeneration)

	out := encode(t, r)
	assert.Equal(t, canonical(t, []byte(goldenV2)), canonical(t, out))
	format, _ := member(t, out, "format")
	assert.JSONEq(t, `2`, string(format))
}

func TestTransitionRecord_V2SectionMutationKeepsNestedUnknownMembers(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*state.TransitionRecord)
		want   map[string]any
	}{
		{"assignment", func(r *state.TransitionRecord) {
			r.Assignment.Generation = 4
			r.Assignment.Token = "tok-4"
		}, map[string]any{"assignment.generation": json.Number("4"), "assignment.token": "tok-4"}},
		{"observation evidence", func(r *state.TransitionRecord) {
			r.Observation.Evidence.Incarnation = "inc-2"
		}, map[string]any{"observation.evidence.incarnation": "inc-2"}},
		{"observation state", func(r *state.TransitionRecord) {
			r.Observation.State = state.ObservedUnconfirmed
		}, map[string]any{"observation.state": "unconfirmed"}},
		{"assignment removed", func(r *state.TransitionRecord) {
			r.Assignment = nil
		}, map[string]any{"assignment": nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := decode(t, goldenV2)
			tc.mutate(r)
			assert.Equal(t, edit(t, goldenV2, tc.want), canonical(t, encode(t, r)))
		})
	}
}

func TestTransitionRecord_V1RefusesNewSections(t *testing.T) {
	r := decode(t, goldenV1)
	r.Assignment = &state.InstanceAssignment{Node: "node-1"}
	_, err := r.Encode()
	require.ErrorIs(t, err, state.ErrRecordSectionsNeedFormat)
}

// A format-1 record has no typed sections, so a member named like one is an
// unknown member and survives as such.
func TestTransitionRecord_V1KeepsSectionNamedMemberAsUnknown(t *testing.T) {
	doc := `{"metadata":{"name":"i-1"},"spec":{},"status":{},"assignment":{"node":"node-1","generation":3}}`
	r := decode(t, doc)
	assert.Nil(t, r.Assignment)
	r.Record.Status.LastNode = "node-2"
	assert.Equal(t, edit(t, doc, map[string]any{"status.last_node": "node-2"}), canonical(t, encode(t, r)))
}

func TestTransitionRecord_NewerFormatIsReadOnly(t *testing.T) {
	for _, marker := range []string{"3", "99999999999999999999"} {
		t.Run(marker, func(t *testing.T) {
			doc := `{"format":` + marker + `,"metadata":{"name":"i-1"},"spec":{},"status":{"status":"running","last_node":"node-1"},` +
				`"assignment":{"node":"node-1"}}`
			r, err := state.DecodeTransitionRecord([]byte(doc))
			require.NoError(t, err, "a newer format is readable")
			assert.True(t, r.ReadOnly())
			assert.Greater(t, r.Format(), state.RecordFormatTransition)
			assert.Equal(t, "node-1", r.Record.Status.LastNode, "legacy fields are decoded")
			assert.Nil(t, r.Assignment, "sections of a newer format are not interpreted")

			out, err := r.Encode()
			require.ErrorIs(t, err, state.ErrRecordFormatAhead)
			assert.Nil(t, out)
		})
	}
}

func TestTransitionRecord_Rejections(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want error
	}{
		{"truncated", `{"metadata":`, state.ErrRecordMalformed},
		{"trailing comma", `{"metadata":{},}`, state.ErrRecordMalformed},
		{"empty input", ``, state.ErrRecordMalformed},
		{"invalid UTF-8", "{\"metadata\":{\"name\":\"\xff\"}}", state.ErrRecordMalformed},
		{"known field of the wrong type", `{"metadata":{"generation":"7"}}`, state.ErrRecordMalformed},
		{"array top level", `[]`, state.ErrRecordNotObject},
		{"string top level", `"i-1"`, state.ErrRecordNotObject},
		{"null top level", `null`, state.ErrRecordNotObject},
		{"second value", `{} {}`, state.ErrRecordTrailingData},
		{"trailing garbage", `{}x`, state.ErrRecordTrailingData},
		{"top-level duplicate", `{"spec":{},"spec":{}}`, state.ErrRecordDuplicateKey},
		{"nested known duplicate", `{"status":{"instance":{"InstanceId":"a","InstanceId":"b"}}}`, state.ErrRecordDuplicateKey},
		{"duplicate inside unknown member", `{"future":{"x":1,"x":2}}`, state.ErrRecordDuplicateKey},
		{"duplicate inside array element", `{"metadata":{"owner_refs":[{"kind":"a","kind":"b"}]}}`, state.ErrRecordDuplicateKey},
		{"duplicate format marker", `{"format":1,"format":2}`, state.ErrRecordDuplicateKey},
		{"top-level case alias", `{"Metadata":{}}`, state.ErrRecordCaseAliasKey},
		{"format marker case alias", `{"Format":2}`, state.ErrRecordCaseAliasKey},
		{"nested case alias", `{"spec":{"Instance_Type":"t3.micro"}}`, state.ErrRecordCaseAliasKey},
		{"case alias in AWS SDK struct", `{"status":{"instance":{"instanceId":"i-1"}}}`, state.ErrRecordCaseAliasKey},
		{"case alias in array element", `{"metadata":{"owner_refs":[{"Kind":"a"}]}}`, state.ErrRecordCaseAliasKey},
		{"Unicode fold alias", `{"ſpec":{}}`, state.ErrRecordCaseAliasKey},
		{"format zero", `{"format":0}`, state.ErrRecordFormatOutOfRange},
		{"format negative", `{"format":-1}`, state.ErrRecordFormatOutOfRange},
		{"format negative zero", `{"format":-0}`, state.ErrRecordFormatOutOfRange},
		{"format fraction", `{"format":1.5}`, state.ErrRecordFormatNotInteger},
		{"format with a fraction part", `{"format":2.0}`, state.ErrRecordFormatNotInteger},
		{"format exponent", `{"format":2e0}`, state.ErrRecordFormatNotInteger},
		{"format string", `{"format":"2"}`, state.ErrRecordFormatNotInteger},
		{"format null", `{"format":null}`, state.ErrRecordFormatNotInteger},
		{"format object", `{"format":{"v":2}}`, state.ErrRecordFormatNotInteger},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := state.DecodeTransitionRecord([]byte(tc.doc))
			require.ErrorIs(t, err, tc.want)
			assert.Nil(t, r)
		})
	}
}

// Unknown members whose names differ only in case from each other are both
// kept: neither aliases a typed field.
func TestTransitionRecord_UnknownMembersDifferingInCaseAreKept(t *testing.T) {
	doc := `{"metadata":{"name":"i-1"},"future":1,"Future":2}`
	assert.Equal(t, canonical(t, []byte(doc)), canonical(t, encode(t, decode(t, doc))))
}

func TestTransitionRecord_LargeIntegersKeepPrecision(t *testing.T) {
	const big = 9007199254740993 // 2^53+1, not representable as float64
	doc := `{"metadata":{"name":"i-1","generation":9007199254740993},"future":123456789012345678901234567890.25}`

	r := decode(t, doc)
	assert.Equal(t, int64(big), r.Record.Metadata.Generation)
	out := encode(t, r)
	assert.Equal(t, canonical(t, []byte(doc)), canonical(t, out))
	future, _ := member(t, out, "future")
	assert.Equal(t, "123456789012345678901234567890.25", string(future))

	r.Record.Metadata.Generation = big + 2
	gen, _ := member(t, encode(t, r), "metadata", "generation")
	assert.Equal(t, "9007199254740995", string(gen))
}

func TestTransitionRecord_ZeroValueEncodesNewFormatOneRecord(t *testing.T) {
	r := &state.TransitionRecord{}
	r.Record.Metadata.Name = "i-1"
	r.Record.Status.Status = vm.StateRunning
	plain, err := json.Marshal(&r.Record)
	require.NoError(t, err)

	out := encode(t, r)
	assert.Equal(t, canonical(t, plain), canonical(t, out))
	_, has := member(t, out, "format")
	assert.False(t, has)
}

// The codec protects capable writers only. A binary already deployed decodes
// into the typed record and drops what it does not know, which is why the
// activation gate, not this codec, must exclude such binaries from writing.
func TestTransitionRecord_CapableWriterPreservesWhatPlainTypedRoundTripStrips(t *testing.T) {
	newer := []byte(`{"metadata":{"name":"i-1","generation":4,"assignment_fence":"f-9"},` +
		`"spec":{"instance_type":"t3.micro","desired_generation":4},` +
		`"status":{"status":"running","last_node":"node-1","observed_assignment":3},` +
		`"assignment":{"node":"node-1","generation":3}}`)

	var typed vm.InstanceRecord
	require.NoError(t, json.Unmarshal(newer, &typed))
	stripped, err := json.Marshal(&typed)
	require.NoError(t, err)
	_, has := member(t, stripped, "metadata", "assignment_fence")
	assert.False(t, has, "plain encoding/json typed round trip strips unknown members")

	r, err := state.DecodeTransitionRecord(newer)
	require.NoError(t, err)
	r.Record.Status.LastNode = "node-2"
	kept := encode(t, r)
	for _, path := range [][]string{
		{"metadata", "assignment_fence"},
		{"spec", "desired_generation"},
		{"status", "observed_assignment"},
		{"assignment"},
	} {
		_, has := member(t, kept, path...)
		assert.True(t, has, "codec round trip keeps %v", path)
	}
}

// An unmutated record cannot raise an extension conflict: that needs a
// changed array, so every accepted, writable input must round-trip.
func FuzzTransitionRecordRoundTrip(f *testing.F) {
	for _, seed := range []string{
		goldenV1, goldenV2,
		`{}`, `{"format":3}`, `{"format":1}`,
		`{"metadata":{"generation":9007199254740993}}`,
		`{"spec":{},"spec":{}}`, `{"Metadata":{}}`, `{"format":1.5}`, `{} {}`, `[]`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := state.DecodeTransitionRecord(data)
		if err != nil {
			return
		}
		out, err := r.Encode()
		if r.ReadOnly() {
			require.ErrorIs(t, err, state.ErrRecordFormatAhead)
			return
		}
		require.NoError(t, err)
		assert.Equal(t, canonical(t, data), canonical(t, out))
	})
}
