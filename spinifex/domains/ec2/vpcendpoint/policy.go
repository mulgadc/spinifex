package vpcendpoint

import (
	"encoding/json"
	"reflect"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// DefaultPolicy is the policy AWS gives a gateway endpoint created without one.
const DefaultPolicy = `{"Version":"2008-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"*","Resource":"*"}]}`

var defaultStatements = mustStatements(DefaultPolicy)

func mustStatements(doc string) []any {
	st, ok := statements(doc)
	if !ok {
		panic("vpcendpoint: default policy does not parse")
	}
	return st
}

func invalidPolicy() error {
	return awserrors.Errorf(awserrors.ErrorInvalidPolicyDocument, "Please provide a valid VPC Endpoint policy")
}

// checkPolicy validates doc as AWS does for an endpoint policy, then refuses
// any policy other than the default, which nothing here enforces yet. It
// returns the document to store: "" for the default.
func checkPolicy(doc string) (string, error) {
	if len(doc) > maxPolicyLength {
		return "", invalidPolicy()
	}
	st, ok := statements(doc)
	if !ok {
		return "", invalidPolicy()
	}
	if !reflect.DeepEqual(st, defaultStatements) {
		return "", awserrors.Errorf(awserrors.ErrorInvalidParameterValue,
			"Custom VPC endpoint policies are not supported. Omit PolicyDocument or pass the default full-access policy.")
	}
	return "", nil
}

// statements returns doc's statements in a canonical form, or false when doc
// is not an object with at least one statement, each naming a Principal.
func statements(doc string) ([]any, bool) {
	var policy map[string]any
	if err := json.Unmarshal([]byte(doc), &policy); err != nil {
		return nil, false
	}
	var list []any
	switch st := policy["Statement"].(type) {
	case []any:
		list = st
	case map[string]any:
		list = []any{st}
	}
	if len(list) == 0 {
		return nil, false
	}
	out := make([]any, 0, len(list))
	for _, raw := range list {
		st, ok := raw.(map[string]any)
		if !ok {
			return nil, false
		}
		if _, ok := st["Principal"]; !ok {
			return nil, false
		}
		canon := make(map[string]any, len(st))
		for k, v := range st {
			canon[k] = canonicalValue(v)
		}
		out = append(out, canon)
	}
	return out, true
}

// canonicalValue reads a one-element list as its element, as IAM does.
func canonicalValue(v any) any {
	if l, ok := v.([]any); ok && len(l) == 1 {
		return l[0]
	}
	return v
}
