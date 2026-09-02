//test:in-package — requestConditionKeys, principalContext and the principal type
// constants are unexported, and the gate exists to compare what they emit with
// the evaluator's registries.

package gateway

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The keys this door resolves. predastore's mirror of this list adds s3:prefix
// and nothing else; a key added at one door and not the other fails there.
var gatewayDoorKeys = []string{
	iampolicy.KeySecureTransport,
	iampolicy.KeyUsername,
	iampolicy.KeyPrincipalAccount,
	iampolicy.KeySourceIP,
}

// Every condition key the evaluator names, so the write-path gate below covers
// the whole namespace rather than the subset this door happens to supply.
var allConditionKeys = []string{
	iampolicy.KeySourceIP,
	iampolicy.KeyS3Prefix,
	iampolicy.KeySecureTransport,
	iampolicy.KeyUsername,
	iampolicy.KeyPrincipalAccount,
	iampolicy.KeyUserID,
	// Deliberately outside the allowlist: there is no MFA in the stack, so the
	// key could never be true. It is here to prove the validator says no.
	"aws:MultiFactorAuthPresent",
}

// One value per operator that the leaf validator accepts, so a rejection can
// only come from the operator/key allowlist and not from the value.
var operatorValues = map[string]string{
	iampolicy.OpStringEquals: "alice",
	iampolicy.OpStringLike:   "ali*",
	iampolicy.OpIPAddress:    "10.0.0.0/8",
	iampolicy.OpBool:         "true",
	// Operators the evaluator does not implement. Accepting one would store a
	// restriction that compares false forever.
	"StringNotEquals": "alice",
	"ArnLike":         "arn:aws:iam::000000000001:user/alice",
	"NumericLessThan": "3",
	"DateGreaterThan": "2026-01-01T00:00:00Z",
}

// emittedKeys drives requestConditionKeys with everything a request can carry,
// for both principal types, and returns the union.
func emittedKeys(t *testing.T) map[string]string {
	t.Helper()

	principals := map[string]principalContext{
		"user": {
			identity:      "alice",
			accountID:     "000000000001",
			principalType: principalTypeUser,
		},
		"assumed-role": {
			identity:       "session",
			accountID:      "000000000001",
			principalType:  principalTypeAssumedRole,
			assumedRoleARN: "arn:aws:sts::000000000001:assumed-role/SharedOps/session",
		},
	}

	union := make(map[string]string)
	for name, principal := range principals {
		r := httptest.NewRequest(http.MethodPost, "/?prefix=home/", nil)
		r.TLS = &tls.ConnectionState{}
		r.RemoteAddr = "10.4.1.9:52344"
		for key := range requestConditionKeys(r, principal) {
			union[key] = name
		}
	}
	return union
}

// A key this door supplies that no policy can name is carried to the evaluator
// and dropped there — the silent half of the write-path/evaluator disagreement.
func TestRequestConditionKeys_EveryEmittedKeyIsUsableInAPolicy(t *testing.T) {
	for key, principal := range emittedKeys(t) {
		supported := false
		for op := range operatorValues {
			if iampolicy.SupportedCondition(op, key) {
				supported = true
				break
			}
		}
		_, unresolvable := iampolicy.UnsupportedVariable("${" + key + "}")
		assert.True(t, supported || !unresolvable,
			"requestConditionKeys emits %q for a %s principal, but the evaluator neither enforces a "+
				"condition on it nor substitutes it: a policy naming it can never fire", key, principal)
	}
}

// The door's key set as a whole, so a key gained or lost here is a deliberate
// change made at both doors rather than a drift between them.
func TestRequestConditionKeys_MatchesTheDoorKeySet(t *testing.T) {
	emitted := make([]string, 0, len(gatewayDoorKeys))
	for key := range emittedKeys(t) {
		emitted = append(emitted, key)
	}
	assert.ElementsMatch(t, gatewayDoorKeys, emitted,
		"the AWS gateway door key set changed: update predastore's mirror in "+
			"internal/gate/conditionkeys_completeness_test.go and the door table in bluebottle's door_test.go")

	// s3:prefix is the one key the S3 gate adds over this set, and it is absent
	// here even when the request carries a prefix parameter.
	assert.NotContains(t, emitted, iampolicy.KeyS3Prefix)
}

// The write path and the evaluator are the same allowlist or they are a bug:
// a pair the validator accepts but the evaluator cannot enforce is stored as an
// inert restriction, and one it rejects that the evaluator does enforce is a
// policy an operator cannot write.
func TestValidatePolicyDocument_AcceptsExactlyTheSupportedConditions(t *testing.T) {
	for _, key := range allConditionKeys {
		for op, value := range operatorValues {
			doc := conditionDocument(t, op, key, value)
			_, err := handlers_iam.ValidatePolicyDocument(doc)

			if iampolicy.SupportedCondition(op, key) {
				assert.NoError(t, err,
					"the evaluator enforces %s on %q but the write path rejects it, so the policy "+
						"cannot be written", op, key)
				continue
			}
			assert.Error(t, err,
				"the write path accepts %s on %q, which the evaluator does not enforce: the condition "+
					"would be stored and then compare false forever", op, key)
		}
	}
}

func conditionDocument(t *testing.T, op, key, value string) string {
	t.Helper()

	doc, err := json.Marshal(map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect":    "Allow",
			"Action":    []string{"s3:GetObject"},
			"Resource":  []string{"arn:aws:s3:::reports/*"},
			"Condition": map[string]any{op: map[string]any{key: []string{value}}},
		}},
	})
	require.NoError(t, err, "marshalling the %s/%s document", op, key)
	return string(doc)
}
