package route53

import (
	"errors"
	"fmt"
	"strings"
)

const maxCharacterString = 255

// characterStrings splits a TXT or SPF value into its character-strings, kept
// in presentation format so their escapes reach the wire unchanged. A value
// with no opening quote is one string, as Route 53 treats it.
func characterStrings(v string) ([]string, error) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, `"`) {
		n, err := decodedLength(v)
		if err != nil {
			return nil, err
		}
		if n > maxCharacterString {
			return nil, errTXTTooLong
		}
		return []string{v}, nil
	}
	var out []string
	for v != "" {
		if v[0] != '"' {
			return nil, errUnquoted
		}
		end := closingQuote(v)
		if end < 0 {
			return nil, errUnquoted
		}
		s := v[1:end]
		n, err := decodedLength(s)
		if err != nil {
			return nil, err
		}
		if n > maxCharacterString {
			return nil, errTXTTooLong
		}
		out = append(out, s)
		v = strings.TrimLeft(v[end+1:], " \t")
	}
	return out, nil
}

var (
	errTXTTooLong = errors.New("TXTRDATATooLong (TXT RDATA string too long)")
	errUnquoted   = errors.New("InvalidCharacterString (Value should be enclosed in quotation marks)")
)

// closingQuote returns the index of the quote closing the string opened at
// v[0], skipping escaped characters, or -1.
func closingQuote(v string) int {
	for i := 1; i < len(v); i++ {
		switch v[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// decodedLength is the wire length of a presentation-format string, in which
// \ddd is one byte and \c is the character c.
func decodedLength(s string) (int, error) {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			switch {
			case i+3 < len(s) && isDigit(s[i+1]) && isDigit(s[i+2]) && isDigit(s[i+3]):
				if int(s[i+1]-'0')*100+int(s[i+2]-'0')*10+int(s[i+3]-'0') > 255 {
					return 0, fmt.Errorf("escape \\%s is not a byte", s[i+1:i+4])
				}
				i += 3
			case i+1 < len(s):
				i++
			default:
				return 0, errors.New("value ends in a bare backslash")
			}
		}
		n++
	}
	return n, nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
