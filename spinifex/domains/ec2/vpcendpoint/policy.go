package vpcendpoint

import (
	"encoding/json"
	"reflect"

	"github.com/mulgadc/bluebottle/pkg/iampolicy"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// DefaultPolicy is the policy AWS gives a gateway endpoint created without one.
const DefaultPolicy = `{"Version":"2008-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"*","Resource":"*"}]}`

// defaultStatements is DefaultPolicy's statements as statements reads them; a
// create given DefaultPolicy explicitly proves the two agree.
var defaultStatements = []any{map[string]any{"Effect": "Allow", "Principal": "*", "Action": "*", "Resource": "*"}}

func invalidPolicy() error {
	return awserrors.Errorf(awserrors.ErrorInvalidPolicyDocument, "Please provide a valid VPC Endpoint policy")
}

// checkPolicy validates doc as AWS does for an endpoint policy, then refuses one
// the S3 gate would not enforce as written, so no policy is stored that fails
// closed there. It returns the document to store: "" for the default.
func checkPolicy(doc string) (string, error) {
	if len(doc) > maxPolicyLength {
		return "", invalidPolicy()
	}
	st, ok := statements(doc)
	if !ok {
		return "", invalidPolicy()
	}
	if reflect.DeepEqual(st, defaultStatements) {
		return "", nil
	}
	parsed, err := iampolicy.ParseEndpointPolicy([]byte(doc))
	if err != nil {
		return "", invalidPolicy()
	}
	if err := iampolicy.CheckEndpointPolicy(parsed); err != nil {
		return "", awserrors.Errorf(awserrors.ErrorInvalidPolicyDocument,
			"This VPC endpoint policy cannot be enforced: %s", err)
	}
	return doc, nil
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
