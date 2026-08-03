package handlers_bedrock

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateTransition(t *testing.T) {
	allStates := []EndpointState{StateAbsent, StateStarting, StateReady, StateDraining}

	legal := map[EndpointState]map[EndpointState]bool{
		StateAbsent:   {StateStarting: true},
		StateStarting: {StateReady: true, StateAbsent: true},
		StateReady:    {StateDraining: true},
		StateDraining: {StateAbsent: true},
	}

	for _, from := range allStates {
		for _, to := range allStates {
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				err := validateTransition(from, to)
				if legal[from][to] {
					assert.NoError(t, err)
				} else {
					assert.Error(t, err)
					assert.ErrorIs(t, err, ErrIllegalTransition)
				}
			})
		}
	}
}

// A few explicitly illegal transitions worth naming, so a future edit to the
// table's shape (e.g. adding a state) breaks a readable test, not just the
// generated matrix above.
func TestValidateTransition_IllegalCases(t *testing.T) {
	cases := []struct {
		name string
		from EndpointState
		to   EndpointState
	}{
		{"ready cannot restart", StateReady, StateStarting},
		{"absent cannot jump to ready", StateAbsent, StateReady},
		{"draining cannot go ready", StateDraining, StateReady},
		{"absent cannot self-transition", StateAbsent, StateAbsent},
		{"starting cannot re-enter starting", StateStarting, StateStarting},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTransition(tc.from, tc.to)
			assert.ErrorIs(t, err, ErrIllegalTransition)
		})
	}
}
