package awsapi

import (
	"encoding/json"
	"strconv"

	"github.com/aws/aws-sdk-go/aws"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// listQuery holds the query-string inputs of a GET action, which the gateway
// folds into the body as a JSON object of string lists.
type listQuery map[string][]string

// parseListQuery decodes the folded query of a GET action. An empty body means
// the request carried no query string.
func parseListQuery(body []byte) (listQuery, error) {
	q := listQuery{}
	if len(body) == 0 {
		return q, nil
	}
	if err := json.Unmarshal(body, (*map[string][]string)(&q)); err != nil {
		return nil, awserrors.Errorf(awserrors.ErrorEKSInvalidParameter, "malformed query string")
	}
	return q, nil
}

// str returns the first value of name, or nil when it is absent or empty.
func (q listQuery) str(name string) *string {
	if v := q[name]; len(v) > 0 && v[0] != "" {
		return aws.String(v[0])
	}
	return nil
}

// strs returns every non-empty value of name, or nil when there are none.
func (q listQuery) strs(name string) []*string {
	var out []string
	for _, v := range q[name] {
		if v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return aws.StringSlice(out)
}

// int64 returns the first value of name as an integer, or nil when absent.
func (q listQuery) int64(name string) (*int64, error) {
	s := q.str(name)
	if s == nil {
		return nil, nil
	}
	n, err := strconv.ParseInt(*s, 10, 64)
	if err != nil {
		return nil, awserrors.Errorf(awserrors.ErrorEKSInvalidParameter, "%s must be an integer", name)
	}
	return aws.Int64(n), nil
}
