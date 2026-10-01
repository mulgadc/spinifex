// Package paging pages a listing with opaque resume tokens. Each API keeps its
// own limits and error text; this package only owns the token and the slicing.
package paging

import (
	"encoding/base64"
	"errors"
	"slices"
	"strings"
)

// ErrInvalidToken reports a token this package did not issue.
var ErrInvalidToken = errors.New("paging: token was not issued by this service")

// Marks a token as ours, so arbitrary base64 is rejected rather than resumed.
const tokenPrefix = "spx-page-v1:" //nolint:gosec // a format marker, not a credential

// Request is a validated page request. The zero value asks for everything.
type Request struct {
	// Limit caps the page; zero or less means no cap.
	Limit int
	// From is the key to resume at, decoded from the caller's token.
	From string
}

// EncodeToken returns the opaque token that resumes a listing at key.
func EncodeToken(key string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(tokenPrefix + key))
}

// DecodeToken returns the key a token resumes at. An empty token is the start.
func DecodeToken(token string) (string, error) {
	if token == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", ErrInvalidToken
	}
	key, ok := strings.CutPrefix(string(decoded), tokenPrefix)
	if !ok || key == "" {
		return "", ErrInvalidToken
	}
	return key, nil
}

// Page sorts items by key in place and returns the page req selects, plus the
// token for the next page, or "" on the last page. A resume starts at the first
// key not below From, so deleting the item a token names skips nothing.
func Page[T any](items []T, key func(T) string, req Request) ([]T, string) {
	slices.SortStableFunc(items, func(a, b T) int { return strings.Compare(key(a), key(b)) })

	start := 0
	if req.From != "" {
		start, _ = slices.BinarySearchFunc(items, req.From, func(item T, target string) int {
			return strings.Compare(key(item), target)
		})
	}
	rest := items[start:]
	if req.Limit <= 0 || len(rest) <= req.Limit {
		return rest, ""
	}
	return rest[:req.Limit], EncodeToken(key(rest[req.Limit]))
}
