package state

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The record has no typed map with object values and no non-omitempty object
// pointer, so these shapes exercise the merge rules the record cannot reach.
type probeInner struct {
	Name string `json:"name"`
}

type probe struct {
	Entries  map[string]probeInner `json:"entries,omitempty"`
	Numbered map[int]probeInner    `json:"numbered,omitempty"`
	Ptr      *probeInner           `json:"ptr"`
}

func probeTree(t *testing.T, b []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	require.NoError(t, dec.Decode(&v))
	return v
}

func mergeProbe(t *testing.T, doc string, mutate func(*probe)) (any, error) {
	t.Helper()
	var p probe
	require.NoError(t, json.Unmarshal([]byte(doc), &p))
	snap, err := json.Marshal(&p)
	require.NoError(t, err)
	mutate(&p)
	cur, err := json.Marshal(&p)
	require.NoError(t, err)
	s := schemaOf(reflect.TypeFor[probe](), map[reflect.Type]*fieldSchema{})
	return mergeChanged(s, probeTree(t, []byte(doc)), probeTree(t, snap), probeTree(t, cur), "$")
}

func TestMergeChanged_ProbeShapes(t *testing.T) {
	cases := []struct {
		name, doc string
		mutate    func(*probe)
		conflict  string // container and member, or empty for success
		want      string
	}{
		{"map entry with an unknown member removed",
			`{"entries":{"a":{"name":"x","future":1},"b":{"name":"y"}},"ptr":null}`,
			func(p *probe) { delete(p.Entries, "a") },
			"changed object $.entries.a would drop unknown member $.entries.a.future", ""},
		{"map entry without unknown members removed",
			`{"entries":{"a":{"name":"x"},"b":{"name":"y","future":1}},"ptr":null}`,
			func(p *probe) { delete(p.Entries, "a") },
			"", `{"entries":{"b":{"name":"y","future":1}},"ptr":null}`},
		{"map entry changed in place keeps its unknown member",
			`{"entries":{"a":{"name":"x","future":1}},"ptr":null}`,
			func(p *probe) { p.Entries["a"] = probeInner{Name: "z"} },
			"", `{"entries":{"a":{"name":"z","future":1}},"ptr":null}`},
		{"map entry under a rewritten key replaced",
			`{"numbered":{"07":{"name":"x","future":1}},"ptr":null}`,
			func(p *probe) { p.Numbered[8] = probeInner{Name: "y"} },
			"changed object $.numbered.07 would drop unknown member $.numbered.07.future", ""},
		{"object with an unknown member replaced by null",
			`{"ptr":{"name":"x","future":{"deep":true}}}`,
			func(p *probe) { p.Ptr = nil },
			"changed object $.ptr would drop unknown member $.ptr.future", ""},
		{"object without unknown members replaced by null",
			`{"ptr":{"name":"x"}}`,
			func(p *probe) { p.Ptr = nil },
			"", `{"ptr":null}`},
		{"keys of a map are data, not extensions",
			`{"entries":{"anything":{"name":"x"}},"ptr":null}`,
			func(p *probe) { p.Entries = nil },
			"", `{"ptr":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mergeProbe(t, tc.doc, tc.mutate)
			if tc.conflict != "" {
				require.ErrorIs(t, err, ErrRecordExtensionConflict)
				assert.Contains(t, err.Error(), tc.conflict)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, probeTree(t, []byte(tc.want)), got)
		})
	}
}
