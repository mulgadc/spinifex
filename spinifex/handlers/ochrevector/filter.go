package handlers_ochrevector

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// FilterOp is one Bedrock-KB-grade metadata filter operator (D9). Scope is
// exactly this set -- comparison, set, string and combinator operators --
// so a Filter can only ever compile to one of the fixed SQL shapes below,
// never arbitrary SQL.
type FilterOp string

// The Bedrock Knowledge Bases operator set (D9), verbatim: comparison,
// set, string, and combinator operators. .10 translates the KB API's
// operator names onto these directly.
const (
	FilterEquals             FilterOp = "equals"
	FilterNotEquals          FilterOp = "notEquals"
	FilterGreaterThan        FilterOp = "greaterThan"
	FilterGreaterThanOrEqual FilterOp = "greaterThanOrEquals"
	FilterLessThan           FilterOp = "lessThan"
	FilterLessThanOrEqual    FilterOp = "lessThanOrEquals"
	FilterIn                 FilterOp = "in"
	FilterNotIn              FilterOp = "notIn"
	FilterStartsWith         FilterOp = "startsWith"
	FilterStringContains     FilterOp = "stringContains"
	FilterListContains       FilterOp = "listContains"
	FilterAndAll             FilterOp = "andAll"
	FilterOrAll              FilterOp = "orAll"
)

// filterKeyPattern is the strict identifier allowlist a metadata key must
// pass before it is ever written into SQL text. A jsonb key has no
// parameterized form -- `metadata->>$1` is not valid Postgres, the key
// must appear in the query string itself -- so this regex is the only
// thing standing between a filter key and injection. It is deliberately
// conservative: letters, digits, underscore, must start with a letter or
// underscore, max 64 chars.
var filterKeyPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,63}$`)

// Filter is one node of the Bedrock-KB metadata filter AST (D9): either a
// leaf comparison of Key against Value under Op, or a combinator (andAll/
// orAll) over Children. Construct via the Equals/GreaterThan/.../AndAll
// helpers rather than the struct literal directly.
type Filter struct {
	Op       FilterOp
	Key      string
	Value    any
	Children []*Filter
}

// Equals builds an `equals` leaf filter: metadata[key] as text equals value.
func Equals(key string, value any) *Filter { return &Filter{Op: FilterEquals, Key: key, Value: value} }

// NotEquals builds a `notEquals` leaf filter.
func NotEquals(key string, value any) *Filter {
	return &Filter{Op: FilterNotEquals, Key: key, Value: value}
}

// GreaterThan builds a `greaterThan` leaf filter: metadata[key] cast to
// numeric compared against value.
func GreaterThan(key string, value any) *Filter {
	return &Filter{Op: FilterGreaterThan, Key: key, Value: value}
}

// GreaterThanOrEquals builds a `greaterThanOrEquals` leaf filter.
func GreaterThanOrEquals(key string, value any) *Filter {
	return &Filter{Op: FilterGreaterThanOrEqual, Key: key, Value: value}
}

// LessThan builds a `lessThan` leaf filter.
func LessThan(key string, value any) *Filter {
	return &Filter{Op: FilterLessThan, Key: key, Value: value}
}

// LessThanOrEquals builds a `lessThanOrEquals` leaf filter.
func LessThanOrEquals(key string, value any) *Filter {
	return &Filter{Op: FilterLessThanOrEqual, Key: key, Value: value}
}

// In builds an `in` leaf filter: metadata[key] as text must equal one of values.
func In(key string, values []string) *Filter {
	return &Filter{Op: FilterIn, Key: key, Value: values}
}

// NotIn builds a `notIn` leaf filter: metadata[key] as text must equal none of values.
func NotIn(key string, values []string) *Filter {
	return &Filter{Op: FilterNotIn, Key: key, Value: values}
}

// StartsWith builds a `startsWith` leaf filter: metadata[key] as text
// starts with prefix.
func StartsWith(key, prefix string) *Filter {
	return &Filter{Op: FilterStartsWith, Key: key, Value: prefix}
}

// StringContains builds a `stringContains` leaf filter: metadata[key] as
// text contains substr.
func StringContains(key, substr string) *Filter {
	return &Filter{Op: FilterStringContains, Key: key, Value: substr}
}

// ListContains builds a `listContains` leaf filter: metadata[key], a jsonb
// array, contains value as a top-level element.
func ListContains(key, value string) *Filter {
	return &Filter{Op: FilterListContains, Key: key, Value: value}
}

// AndAll builds an `andAll` combinator over children, all of which must match.
func AndAll(children ...*Filter) *Filter {
	return &Filter{Op: FilterAndAll, Children: children}
}

// OrAll builds an `orAll` combinator over children, any of which may match.
func OrAll(children ...*Filter) *Filter {
	return &Filter{Op: FilterOrAll, Children: children}
}

// compile renders f into a parameterized SQL boolean expression starting at
// bind parameter $startParam, returning the expression, the bound args in
// order, and the next free parameter index for the caller to continue
// numbering from. Every metadata key is validated against filterKeyPattern
// before it reaches the returned SQL string; every value is returned only
// as a bound arg, never interpolated.
func (f *Filter) compile(startParam int) (whereSQL string, args []any, nextParam int, err error) {
	if f == nil {
		return "", nil, startParam, fmt.Errorf("ochrevector: nil filter")
	}
	switch f.Op {
	case FilterEquals, FilterNotEquals, FilterStartsWith, FilterStringContains:
		return f.compileText(startParam)
	case FilterGreaterThan, FilterGreaterThanOrEqual, FilterLessThan, FilterLessThanOrEqual:
		return f.compileNumeric(startParam)
	case FilterIn, FilterNotIn:
		return f.compileSet(startParam)
	case FilterListContains:
		return f.compileListContains(startParam)
	case FilterAndAll, FilterOrAll:
		return f.compileCombinator(startParam)
	default:
		return "", nil, startParam, fmt.Errorf("ochrevector: unknown filter operator %q", f.Op)
	}
}

// sanitizeFilterKey validates key against filterKeyPattern, the sole gate
// between a caller-supplied metadata key and raw SQL text.
func sanitizeFilterKey(key string) (string, error) {
	if !filterKeyPattern.MatchString(key) {
		return "", fmt.Errorf("ochrevector: invalid filter key %q", key)
	}
	return key, nil
}

// compileText handles equals/notEquals/startsWith/stringContains: every one
// compares metadata[key] extracted as text (`->>'`) against a single bound
// text value.
func (f *Filter) compileText(startParam int) (string, []any, int, error) {
	key, err := sanitizeFilterKey(f.Key)
	if err != nil {
		return "", nil, startParam, err
	}
	value, err := filterTextValue(f.Value)
	if err != nil {
		return "", nil, startParam, fmt.Errorf("ochrevector: filter %q on %q: %w", f.Op, f.Key, err)
	}

	var sql string
	switch f.Op {
	case FilterEquals:
		sql = fmt.Sprintf(`metadata->>'%s' = $%d`, key, startParam)
	case FilterNotEquals:
		sql = fmt.Sprintf(`metadata->>'%s' != $%d`, key, startParam)
	case FilterStartsWith:
		sql = fmt.Sprintf(`starts_with(metadata->>'%s', $%d)`, key, startParam)
	case FilterStringContains:
		sql = fmt.Sprintf(`position($%d IN metadata->>'%s') > 0`, startParam, key)
	}
	return sql, []any{value}, startParam + 1, nil
}

// compileNumeric handles greaterThan(OrEquals)/lessThan(OrEquals): each
// casts metadata[key] to numeric and compares against a bound numeric value.
func (f *Filter) compileNumeric(startParam int) (string, []any, int, error) {
	key, err := sanitizeFilterKey(f.Key)
	if err != nil {
		return "", nil, startParam, err
	}
	value, err := filterNumericValue(f.Value)
	if err != nil {
		return "", nil, startParam, fmt.Errorf("ochrevector: filter %q on %q: %w", f.Op, f.Key, err)
	}

	var op string
	switch f.Op {
	case FilterGreaterThan:
		op = ">"
	case FilterGreaterThanOrEqual:
		op = ">="
	case FilterLessThan:
		op = "<"
	case FilterLessThanOrEqual:
		op = "<="
	}
	sql := fmt.Sprintf(`(metadata->>'%s')::numeric %s $%d`, key, op, startParam)
	return sql, []any{value}, startParam + 1, nil
}

// compileSet handles in/notIn: metadata[key] as text compared against a
// single bound array parameter via ANY/ALL.
func (f *Filter) compileSet(startParam int) (string, []any, int, error) {
	key, err := sanitizeFilterKey(f.Key)
	if err != nil {
		return "", nil, startParam, err
	}
	values, err := filterStringSlice(f.Value)
	if err != nil {
		return "", nil, startParam, fmt.Errorf("ochrevector: filter %q on %q: %w", f.Op, f.Key, err)
	}

	var sql string
	switch f.Op {
	case FilterIn:
		sql = fmt.Sprintf(`metadata->>'%s' = ANY($%d)`, key, startParam)
	case FilterNotIn:
		sql = fmt.Sprintf(`metadata->>'%s' != ALL($%d)`, key, startParam)
	}
	return sql, []any{values}, startParam + 1, nil
}

// compileListContains handles listContains: metadata[key] is treated as a
// jsonb array and tested for value as a top-level element via the `?`
// jsonb-exists operator.
func (f *Filter) compileListContains(startParam int) (string, []any, int, error) {
	key, err := sanitizeFilterKey(f.Key)
	if err != nil {
		return "", nil, startParam, err
	}
	value, ok := f.Value.(string)
	if !ok {
		return "", nil, startParam, fmt.Errorf("ochrevector: filter %q on %q requires a string value, got %T", f.Op, f.Key, f.Value)
	}
	sql := fmt.Sprintf(`metadata->'%s' ? $%d`, key, startParam)
	return sql, []any{value}, startParam + 1, nil
}

// compileCombinator handles andAll/orAll: every child is compiled in order,
// each consuming the next contiguous block of bind parameters, and the
// results are joined and parenthesized as one boolean expression.
func (f *Filter) compileCombinator(startParam int) (string, []any, int, error) {
	if len(f.Children) == 0 {
		return "", nil, startParam, fmt.Errorf("ochrevector: %q filter requires at least one child", f.Op)
	}
	joiner := " AND "
	if f.Op == FilterOrAll {
		joiner = " OR "
	}

	parts := make([]string, 0, len(f.Children))
	var args []any
	next := startParam
	for _, child := range f.Children {
		sql, childArgs, n, err := child.compile(next)
		if err != nil {
			return "", nil, startParam, err
		}
		parts = append(parts, sql)
		args = append(args, childArgs...)
		next = n
	}
	return "(" + strings.Join(parts, joiner) + ")", args, next, nil
}

// filterTextValue renders v as the single text value a text-comparison
// filter binds. Scalars are formatted, not interpolated -- the returned
// string is always used as a bound arg, never written into SQL.
func filterTextValue(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case int:
		return strconv.Itoa(t), nil
	case int32:
		return strconv.FormatInt(int64(t), 10), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	default:
		return "", fmt.Errorf("unsupported filter value type %T", v)
	}
}

// filterNumericValue coerces v to float64 for a numeric-comparison filter's
// bound parameter, rejecting non-numeric types.
func filterNumericValue(v any) (float64, error) {
	switch t := v.(type) {
	case float64:
		return t, nil
	case float32:
		return float64(t), nil
	case int:
		return float64(t), nil
	case int32:
		return float64(t), nil
	case int64:
		return float64(t), nil
	default:
		return 0, fmt.Errorf("value must be numeric, got %T", v)
	}
}

// filterStringSlice coerces v to a []string for an in/notIn filter's bound
// array parameter, rejecting an empty or non-list value.
func filterStringSlice(v any) ([]string, error) {
	var out []string
	switch t := v.(type) {
	case []string:
		out = t
	case []any:
		out = make([]string, len(t))
		for i, item := range t {
			s, err := filterTextValue(item)
			if err != nil {
				return nil, err
			}
			out[i] = s
		}
	default:
		return nil, fmt.Errorf("value must be a list, got %T", v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("value must be a non-empty list")
	}
	return out, nil
}
