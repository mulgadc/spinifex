package identifiers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A describe that reads one document per named ID pays for every entry it is
// given, so a repeated or nil ID has to be dropped before the fan-out and the
// surviving order has to be the request's.
func TestDistinct(t *testing.T) {
	first, second := "vol-1", "vol-2"
	tests := []struct {
		name string
		ids  []*string
		want []string
	}{
		{name: "nil slice", ids: nil, want: []string{}},
		{name: "only nil entries", ids: []*string{nil, nil}, want: []string{}},
		{name: "request order", ids: []*string{&second, &first}, want: []string{"vol-2", "vol-1"}},
		{name: "duplicates", ids: []*string{&first, &second, &first}, want: []string{"vol-1", "vol-2"}},
		{name: "nil among values", ids: []*string{&first, nil, &second}, want: []string{"vol-1", "vol-2"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Distinct(tt.ids))
		})
	}
}

// An empty string is kept so a request naming "" matches nothing rather than
// widening into an unfiltered describe.
func TestSet(t *testing.T) {
	first, second, empty := "vpc-1", "vpc-2", ""
	tests := []struct {
		name string
		ids  []*string
		want map[string]bool
	}{
		{name: "nil slice", ids: nil, want: map[string]bool{}},
		{name: "nil entries dropped", ids: []*string{nil, &first, nil}, want: map[string]bool{"vpc-1": true}},
		{name: "duplicates collapse", ids: []*string{&first, &second, &first}, want: map[string]bool{"vpc-1": true, "vpc-2": true}},
		{name: "empty string kept", ids: []*string{&empty}, want: map[string]bool{"": true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Set(tt.ids))
		})
	}
}
