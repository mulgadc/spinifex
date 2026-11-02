package queue

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
)

// ListQueues bounds. Without MaxResults a call returns at most
// defaultListLimit queues and no token; both are provisional until
// characterised against AWS.
const (
	maxListResults   = 1000
	defaultListLimit = 1000
	maxPrefixLen     = 80
	listTokenVersion = 1
)

// ErrInvalidListRequest rejects a bad MaxResults, prefix or NextToken; its
// wire code is provisional.
var ErrInvalidListRequest = errors.New("invalid ListQueues request")

// ListRequest is a ListQueues call for one account and Region.
type ListRequest struct {
	Account, Region string
	Prefix          string
	// MaxResults is nil when the caller supplied none.
	MaxResults *int
	NextToken  string
}

// ListResult is one page; NextToken is empty on the last page.
type ListResult struct {
	Queues    []Record
	NextToken string
}

// listToken is the opaque pagination cursor. It is bound to the caller's
// account, Region and prefix, so it cannot page another account's names.
type listToken struct {
	V      int    `json:"v"`
	Acct   string `json:"a"`
	Region string `json:"r"`
	Prefix string `json:"p"`
	After  string `json:"after"`
}

// List returns active queues in name order whose binding and record agree.
// Names come from one key listing that may lag recent writes; each is then
// read from the leader. One traversal never repeats a name.
func (s *Store) List(ctx context.Context, req ListRequest) (ListResult, error) {
	if err := validIdentity(req.Account, req.Region); err != nil {
		return ListResult{}, err
	}
	limit, after, err := req.validate()
	if err != nil {
		return ListResult{}, err
	}
	base := "n." + req.Account + "." + req.Region + "."
	keys, err := s.kv.keys(ctx, base+">")
	if err != nil {
		return ListResult{}, fmt.Errorf("%w: list queue names: %w", ErrUnavailable, err)
	}
	names := make([]string, 0, len(keys))
	for _, k := range keys {
		name := strings.TrimPrefix(k, base)
		if ValidateName(name) != nil {
			return ListResult{}, fmt.Errorf("%w: malformed binding key %s", ErrUnavailable, k)
		}
		if strings.HasPrefix(name, req.Prefix) && name > after {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)

	var out ListResult
	for i, name := range names {
		if len(out.Queues) == limit {
			if req.MaxResults != nil {
				if out.NextToken, err = req.token(names[i-1]); err != nil {
					return ListResult{}, err
				}
			}
			break
		}
		r, ok, err := s.listed(ctx, req.Account, req.Region, name)
		if err != nil {
			return ListResult{}, err
		}
		if ok {
			out.Queues = append(out.Queues, r)
		}
	}
	return out, nil
}

// listed reads one name as Resolve would: absent or non-active queues are
// skipped, and any read or decode failure fails the call rather than
// silently omitting a queue that may exist.
func (s *Store) listed(ctx context.Context, account, region, name string) (Record, bool, error) {
	r, _, err := s.resolve(ctx, account, region, name)
	switch {
	case errors.Is(err, ErrQueueDoesNotExist):
		return Record{}, false, nil
	case err != nil:
		return Record{}, false, err
	case r.State != StateActive:
		return Record{}, false, nil
	}
	return r, true, nil
}

// Validate rejects a bad MaxResults, prefix or NextToken without reading the
// store, so an adapter can refuse the request before authorizing it.
func (req *ListRequest) Validate() error {
	_, _, err := req.validate()
	return err
}

func (req *ListRequest) validate() (limit int, after string, err error) {
	switch {
	case req.MaxResults != nil && (*req.MaxResults < 1 || *req.MaxResults > maxListResults):
		return 0, "", fmt.Errorf("%w: MaxResults must be from 1 to %d", ErrInvalidListRequest, maxListResults)
	case len(req.Prefix) > maxPrefixLen || (req.Prefix != "" && ValidateName(req.Prefix) != nil):
		return 0, "", fmt.Errorf("%w: invalid QueueNamePrefix", ErrInvalidListRequest)
	}
	limit = defaultListLimit
	if req.MaxResults != nil {
		limit = *req.MaxResults
	}
	if req.NextToken == "" {
		return limit, "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(req.NextToken)
	if err != nil {
		return 0, "", fmt.Errorf("%w: invalid NextToken", ErrInvalidListRequest)
	}
	var t listToken
	if err := json.Unmarshal(b, &t); err != nil ||
		t.V != listTokenVersion || t.Acct != req.Account || t.Region != req.Region ||
		t.Prefix != req.Prefix || ValidateName(t.After) != nil || !strings.HasPrefix(t.After, req.Prefix) {
		return 0, "", fmt.Errorf("%w: invalid NextToken", ErrInvalidListRequest)
	}
	return limit, t.After, nil
}

func (req *ListRequest) token(after string) (string, error) {
	b, err := json.Marshal(listToken{V: listTokenVersion, Acct: req.Account, Region: req.Region, Prefix: req.Prefix, After: after})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// errListIncomplete reports a key listing that ended before the server
// signalled it had delivered every current key.
var errListIncomplete = errors.New("key listing ended early")

// keys lists keys matching filter through an ordered consumer, which may lag
// the leader. It fails unless the watcher reaches its end-of-initial-values
// marker, so a cut-short listing is never mistaken for a complete one.
func (k jsKV) keys(ctx context.Context, filter string) ([]string, error) {
	w, err := k.kv.WatchFiltered(ctx, []string{filter}, jetstream.IgnoreDeletes(), jetstream.MetaOnly())
	if err != nil {
		return nil, err
	}
	defer func() { _ = w.Stop() }()
	return drainKeys(ctx, w.Updates())
}

// drainKeys collects keys until the nil end-of-initial-values marker; a
// channel closed before it, or a cancelled context, is an incomplete listing.
func drainKeys(ctx context.Context, updates <-chan jetstream.KeyValueEntry) ([]string, error) {
	var out []string
	for {
		select {
		case e, ok := <-updates:
			switch {
			case !ok:
				return nil, errListIncomplete
			case e == nil:
				return out, nil
			}
			out = append(out, e.Key())
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
