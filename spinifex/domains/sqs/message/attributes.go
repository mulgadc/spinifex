package message

import (
	"crypto/md5" //nolint:gosec // MD5OfMessageAttributes is an AWS wire checksum, not a security control.
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Message attribute limits from the SQS developer guide.
const (
	MaxAttributes        = 10
	MaxAttributeNameLen  = 256
	MaxAttributeTypeLen  = 256
	maxNumberDigits      = 38
	minNumberExponent    = -128
	maxNumberExponent    = 126
	attributeCodecFormat = 1
)

// Transport types of the attribute digest and encoding.
const (
	transportString byte = 1
	transportBinary byte = 2
)

// ErrAttributesCorrupt reports stored attribute bytes that do not decode.
var ErrAttributesCorrupt = errors.New("message: stored message attributes are corrupt")

// Attribute is one named message attribute as supplied by a client, kept
// as a list so a decoder can report a repeated name instead of losing it.
type Attribute struct {
	Name  string
	Value AttributeValue
}

// AttributeValue is one attribute's members. A nil StringValue or
// BinaryValue was not supplied; a non-nil empty one was supplied empty.
// The list members are reserved by AWS and refused whenever present.
type AttributeValue struct {
	DataType         string
	StringValue      *string
	BinaryValue      []byte
	StringListValues []string
	BinaryListValues [][]byte
}

// Attributes is a validated attribute set, sorted by name.
type Attributes struct {
	list []Attribute
}

// NewAttributes validates up to MaxAttributes attributes and sorts them.
// Errors name the attribute, never its value.
func NewAttributes(in []Attribute) (Attributes, error) {
	if len(in) == 0 {
		return Attributes{}, nil
	}
	if len(in) > MaxAttributes {
		return Attributes{}, fmt.Errorf("%w: a message can have at most %d message attributes, got %d", ErrInvalidParameter, MaxAttributes, len(in))
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]Attribute, 0, len(in))
	for _, a := range in {
		if err := validateAttributeName(a.Name); err != nil {
			return Attributes{}, err
		}
		if _, dup := seen[a.Name]; dup {
			return Attributes{}, fmt.Errorf("%w: message attribute name %q is repeated", ErrInvalidParameter, a.Name)
		}
		seen[a.Name] = struct{}{}
		v, err := validateAttributeValue(a.Name, a.Value)
		if err != nil {
			return Attributes{}, err
		}
		out = append(out, Attribute{Name: a.Name, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	attrs := Attributes{list: out}
	// No queue accepts more, and it keeps every encoded length within uint32.
	if attrs.Size() > MaxMessageSize {
		return Attributes{}, fmt.Errorf("%w: message attributes are %d bytes, over the %d byte maximum", ErrInvalidParameter, attrs.Size(), MaxMessageSize)
	}
	return attrs, nil
}

// Len is the number of attributes.
func (a Attributes) Len() int { return len(a.list) }

// All returns copies of the attributes in name order.
func (a Attributes) All() []Attribute {
	out := make([]Attribute, len(a.list))
	for i, at := range a.list {
		out[i] = Attribute{Name: at.Name, Value: copyValue(at.Value)}
	}
	return out
}

// Size is the bytes the attributes count toward a message's size limit:
// every name, data type and value.
func (a Attributes) Size() int {
	n := 0
	for _, at := range a.list {
		n += len(at.Name) + len(at.Value.DataType) + len(valueBytes(at.Value))
	}
	return n
}

// SuppliedSize measures attributes as supplied, valid or not: every name,
// data type and value, list members included. For a valid set it equals
// Attributes.Size.
func SuppliedSize(in []Attribute) int {
	n := 0
	for _, a := range in {
		n += len(a.Name) + len(a.Value.DataType) + len(a.Value.BinaryValue)
		if a.Value.StringValue != nil {
			n += len(*a.Value.StringValue)
		}
		for _, s := range a.Value.StringListValues {
			n += len(s)
		}
		for _, b := range a.Value.BinaryListValues {
			n += len(b)
		}
	}
	return n
}

// MD5 is MD5OfMessageAttributes by the developer guide's algorithm, or ""
// when there are no attributes.
func (a Attributes) MD5() string {
	if len(a.list) == 0 {
		return ""
	}
	sum := md5.Sum(a.digestBuffer()) //nolint:gosec // AWS wire checksum.
	return hex.EncodeToString(sum[:])
}

// Encode is the store-neutral form: a format byte then the digest buffer,
// which is length-prefixed and so decodes unambiguously. Nil when empty.
func (a Attributes) Encode() []byte {
	if len(a.list) == 0 {
		return nil
	}
	return append([]byte{attributeCodecFormat}, a.digestBuffer()...)
}

// DecodeAttributes reverses Encode and revalidates the result, so stored
// bytes that would not pass a send are ErrAttributesCorrupt.
func DecodeAttributes(b []byte) (Attributes, error) {
	if len(b) == 0 {
		return Attributes{}, nil
	}
	if b[0] != attributeCodecFormat {
		return Attributes{}, fmt.Errorf("%w: unknown format %d", ErrAttributesCorrupt, b[0])
	}
	r := b[1:]
	var in []Attribute
	for len(r) > 0 {
		name, rest, ok := readField(r)
		if !ok {
			return Attributes{}, fmt.Errorf("%w: truncated name", ErrAttributesCorrupt)
		}
		dataType, rest, ok := readField(rest)
		if !ok || len(rest) == 0 {
			return Attributes{}, fmt.Errorf("%w: truncated data type", ErrAttributesCorrupt)
		}
		transport := rest[0]
		value, rest, ok := readField(rest[1:])
		if !ok {
			return Attributes{}, fmt.Errorf("%w: truncated value", ErrAttributesCorrupt)
		}
		v := AttributeValue{DataType: string(dataType)}
		switch transport {
		case transportString:
			s := string(value)
			v.StringValue = &s
		case transportBinary:
			v.BinaryValue = value
		default:
			return Attributes{}, fmt.Errorf("%w: unknown transport type %d", ErrAttributesCorrupt, transport)
		}
		in = append(in, Attribute{Name: string(name), Value: v})
		r = rest
	}
	attrs, err := NewAttributes(in)
	if err != nil {
		return Attributes{}, fmt.Errorf("%w: %w", ErrAttributesCorrupt, err)
	}
	for i := range in {
		if in[i].Name != attrs.list[i].Name {
			return Attributes{}, fmt.Errorf("%w: attributes out of order", ErrAttributesCorrupt)
		}
	}
	return attrs, nil
}

// CheckMessageSize applies the combined limit: body bytes plus attribute
// bytes must not exceed the queue's maximum message size.
func CheckMessageSize(bodyLen int, attrs Attributes, maxSize int) error {
	if total := bodyLen + attrs.Size(); total > maxSize {
		return fmt.Errorf("%w: message body and attributes are %d bytes, over the queue's limit of %d", ErrInvalidParameter, total, maxSize)
	}
	return nil
}

// encodePayload frames a schema-2 body record: the attribute encoding's
// length, the encoding, then the body.
func encodePayload(attrs Attributes, body []byte) []byte {
	enc := attrs.Encode()
	out := make([]byte, 0, 4+len(enc)+len(body))
	out = appendField(out, enc)
	return append(out, body...)
}

// decodePayload reverses encodePayload and checks the attributes against
// the digest the send recorded.
func decodePayload(payload []byte, md5a string) ([]byte, Attributes, error) {
	enc, body, ok := readField(payload)
	if !ok {
		return nil, Attributes{}, fmt.Errorf("%w: truncated payload", ErrAttributesCorrupt)
	}
	attrs, err := DecodeAttributes(enc)
	if err != nil {
		return nil, Attributes{}, err
	}
	if attrs.MD5() != md5a {
		return nil, Attributes{}, fmt.Errorf("%w: attributes do not match their recorded digest", ErrAttributesCorrupt)
	}
	return body, attrs, nil
}

func (a Attributes) digestBuffer() []byte {
	var buf []byte
	for _, at := range a.list {
		buf = appendField(buf, []byte(at.Name))
		buf = appendField(buf, []byte(at.Value.DataType))
		if at.Value.StringValue != nil {
			buf = append(buf, transportString)
		} else {
			buf = append(buf, transportBinary)
		}
		buf = appendField(buf, valueBytes(at.Value))
	}
	return buf
}

func appendField(buf, b []byte) []byte {
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(b))) //nolint:gosec // NewAttributes bounds every field by MaxMessageSize.
	return append(buf, b...)
}

func readField(b []byte) (field, rest []byte, ok bool) {
	if len(b) < 4 {
		return nil, nil, false
	}
	n := binary.BigEndian.Uint32(b)
	if int64(n) > int64(len(b)-4) {
		return nil, nil, false
	}
	return b[4 : 4+n], b[4+n:], true
}

func valueBytes(v AttributeValue) []byte {
	if v.StringValue != nil {
		return []byte(*v.StringValue)
	}
	return v.BinaryValue
}

func copyValue(v AttributeValue) AttributeValue {
	out := AttributeValue{DataType: v.DataType}
	if v.StringValue != nil {
		s := *v.StringValue
		out.StringValue = &s
	}
	if v.BinaryValue != nil {
		out.BinaryValue = append([]byte{}, v.BinaryValue...)
	}
	return out
}

// validateAttributeName applies the documented name rules: 1 to 256 of
// A-Z a-z 0-9 _ - ., no leading, trailing or repeated period, and no
// AWS. or Amazon. prefix in any casing.
func validateAttributeName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: message attribute name must not be empty", ErrInvalidParameter)
	}
	if len(name) > MaxAttributeNameLen {
		return fmt.Errorf("%w: message attribute name is longer than %d characters", ErrInvalidParameter, MaxAttributeNameLen)
	}
	for i := 0; i < len(name); i++ {
		if !isNameByte(name[i]) {
			return fmt.Errorf("%w: message attribute name may contain only A-Z, a-z, 0-9, underscore, hyphen and period", ErrInvalidParameter)
		}
	}
	lower := strings.ToLower(name)
	switch {
	case strings.HasPrefix(lower, "aws.") || strings.HasPrefix(lower, "amazon."):
		return fmt.Errorf("%w: message attribute name %q uses a reserved prefix", ErrInvalidParameter, name)
	case name[0] == '.' || name[len(name)-1] == '.':
		return fmt.Errorf("%w: message attribute name %q starts or ends with a period", ErrInvalidParameter, name)
	case strings.Contains(name, ".."):
		return fmt.Errorf("%w: message attribute name %q has consecutive periods", ErrInvalidParameter, name)
	}
	return nil
}

// validateAttributeValue checks the data type, then that exactly the value
// member its base type uses is present and valid.
func validateAttributeValue(name string, v AttributeValue) (AttributeValue, error) {
	if v.StringListValues != nil || v.BinaryListValues != nil {
		return AttributeValue{}, fmt.Errorf("%w: message attribute %q list values", ErrUnsupported, name)
	}
	base, err := attributeBaseType(name, v.DataType)
	if err != nil {
		return AttributeValue{}, err
	}
	if base == "Binary" {
		switch {
		case v.StringValue != nil:
			return AttributeValue{}, fmt.Errorf("%w: message attribute %q of type Binary must not have a StringValue", ErrInvalidParameter, name)
		case v.BinaryValue == nil:
			return AttributeValue{}, fmt.Errorf("%w: message attribute %q is missing its BinaryValue", ErrInvalidParameter, name)
		case len(v.BinaryValue) == 0:
			return AttributeValue{}, fmt.Errorf("%w: message attribute %q has an empty BinaryValue", ErrInvalidParameter, name)
		}
		return copyValue(v), nil
	}
	switch {
	case v.BinaryValue != nil:
		return AttributeValue{}, fmt.Errorf("%w: message attribute %q of type %s must not have a BinaryValue", ErrInvalidParameter, name, base)
	case v.StringValue == nil:
		return AttributeValue{}, fmt.Errorf("%w: message attribute %q is missing its StringValue", ErrInvalidParameter, name)
	case *v.StringValue == "":
		return AttributeValue{}, fmt.Errorf("%w: message attribute %q has an empty StringValue", ErrInvalidParameter, name)
	}
	if err := validateContents([]byte(*v.StringValue)); err != nil {
		return AttributeValue{}, fmt.Errorf("%w: message attribute %q has a StringValue with a character SQS does not allow", ErrInvalidParameter, name)
	}
	if base == "Number" && !validNumber(*v.StringValue) {
		return AttributeValue{}, fmt.Errorf("%w: message attribute %q is not a number within 38 digits and 10^-128 to 10^126", ErrInvalidParameter, name)
	}
	return copyValue(v), nil
}

// attributeBaseType returns String, Number or Binary from a data type with
// an optional ".label" suffix; the label follows the body's character rules.
func attributeBaseType(name, dataType string) (string, error) {
	if dataType == "" {
		return "", fmt.Errorf("%w: message attribute %q is missing its DataType", ErrInvalidParameter, name)
	}
	if utf8.RuneCountInString(dataType) > MaxAttributeTypeLen {
		return "", fmt.Errorf("%w: message attribute %q has a DataType longer than %d characters", ErrInvalidParameter, name, MaxAttributeTypeLen)
	}
	if err := validateContents([]byte(dataType)); err != nil {
		return "", fmt.Errorf("%w: message attribute %q has a DataType with a character SQS does not allow", ErrInvalidParameter, name)
	}
	base, label, custom := strings.Cut(dataType, ".")
	if base != "String" && base != "Number" && base != "Binary" {
		return "", fmt.Errorf("%w: message attribute %q has a DataType that is not String, Number or Binary", ErrInvalidParameter, name)
	}
	if custom && label == "" {
		return "", fmt.Errorf("%w: message attribute %q has an empty custom DataType label", ErrInvalidParameter, name)
	}
	return base, nil
}

// validNumber accepts a signed decimal with an optional exponent of up to
// 38 significant digits whose magnitude is zero or within 10^-128..10^126.
func validNumber(s string) bool {
	if s != "" && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	mant, exp := s, 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mant = s[:i]
		e, neg := s[i+1:], false
		if e != "" && (e[0] == '+' || e[0] == '-') {
			e, neg = e[1:], e[0] == '-'
		}
		// Past six digits any non-zero mantissa is out of range anyway.
		e = strings.TrimLeft(e, "0")
		if !allDigits(e) || s[i+1:] == "" || strings.HasSuffix(s, "+") || strings.HasSuffix(s, "-") || len(e) > 6 {
			return false
		}
		if e != "" {
			n, err := strconv.Atoi(e)
			if err != nil {
				return false
			}
			exp = n
		}
		if neg {
			exp = -exp
		}
	}
	intPart, frac, _ := strings.Cut(mant, ".")
	if intPart == "" && frac == "" || !allDigits(intPart) || !allDigits(frac) {
		return false
	}
	digits := strings.TrimLeft(intPart+frac, "0")
	if digits == "" {
		return true
	}
	// The leading significant digit's power of ten.
	lead := exp + len(intPart) - 1 - (len(intPart+frac) - len(digits))
	digits = strings.TrimRight(digits, "0")
	if len(digits) > maxNumberDigits {
		return false
	}
	return lead >= minNumberExponent && (lead < maxNumberExponent || lead == maxNumberExponent && digits == "1")
}

func isNameByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// AttributeSelector is a parsed ReceiveMessage MessageAttributeNames list.
// The zero value selects nothing.
type AttributeSelector struct {
	all      bool
	names    map[string]struct{}
	prefixes []string
}

// ParseAttributeSelector reads names, each All, .*, an attribute name, or
// an attribute name followed by .* for every attribute under that dotted
// prefix. A malformed entry is ErrInvalidParameter rather than ignored.
func ParseAttributeSelector(names []string) (AttributeSelector, error) {
	var s AttributeSelector
	for _, n := range names {
		switch {
		case n == "All" || n == ".*":
			s.all = true
		case strings.HasSuffix(n, ".*"):
			p := strings.TrimSuffix(n, ".*")
			if err := validateAttributeName(p); err != nil {
				return AttributeSelector{}, err
			}
			s.prefixes = append(s.prefixes, p+".")
		default:
			if err := validateAttributeName(n); err != nil {
				return AttributeSelector{}, err
			}
			if s.names == nil {
				s.names = map[string]struct{}{}
			}
			s.names[n] = struct{}{}
		}
	}
	return s, nil
}

func (s AttributeSelector) selects(name string) bool {
	if s.all {
		return true
	}
	if _, ok := s.names[name]; ok {
		return true
	}
	for _, p := range s.prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Select is the attributes s names, still in name order.
func (a Attributes) Select(s AttributeSelector) Attributes {
	var out []Attribute
	for _, at := range a.list {
		if s.selects(at.Name) {
			out = append(out, at)
		}
	}
	return Attributes{list: out}
}
